package qb

import "fmt"

// ChannelLatencyStatsScope selects which request sources feed the statistic.
//
// This is the whole point of computing latency from the requests table rather than
// from two separate in-memory averages: "probe only" and "probe plus real traffic"
// stop being two data pipelines that must somehow be merged, and become one query
// with one extra WHERE clause.
type ChannelLatencyStatsScope string

const (
	// ChannelLatencyStatsScopeProbe counts only synthetic health-probe traffic,
	// which is recorded with requests.source = 'test'.
	ChannelLatencyStatsScopeProbe ChannelLatencyStatsScope = "probe"

	// ChannelLatencyStatsScopeAll counts every source: probes, API traffic and
	// playground traffic alike.
	ChannelLatencyStatsScopeAll ChannelLatencyStatsScope = "all"
)

// channelLatencyStatsPercentile is the percentile the routing ceiling compares
// against, expressed for both the PostgreSQL ordered-set aggregate (0.95) and the
// portable integer nearest-rank arithmetic (95).
const (
	channelLatencyStatsPercentileFraction = "0.95"
	channelLatencyStatsPercentilePercent  = 95
)

// ChannelLatencyStatsQuery describes one channel-latency aggregation.
type ChannelLatencyStatsQuery struct {
	// UseDollarPlaceholders selects $1/$2 (PostgreSQL) over ? (SQLite).
	UseDollarPlaceholders bool

	// UsePercentileAggregate selects PostgreSQL's native PERCENTILE_DISC ordered-set
	// aggregate over the portable ROW_NUMBER nearest-rank fallback. Both compute the
	// same value; see BuildChannelLatencyStatsQuery.
	UsePercentileAggregate bool

	// Scope selects which request sources are counted.
	Scope ChannelLatencyStatsScope

	// ChannelIDFilter is an SQL fragment restricting the channels aggregated, e.g.
	// "AND se.channel_id IN ($3, $4)".
	//
	// SECURITY WARNING: this fragment is concatenated into the SQL text. Callers MUST
	// pass parameterized placeholders only, never raw user input.
	//
	// It is not merely an optimization: request_executions is indexed on
	// (channel_id, created_at) with no standalone created_at index, so a window
	// filter WITHOUT a channel predicate degrades into a full scan.
	ChannelIDFilter string
}

// BuildChannelLatencyStatsQuery builds the windowed first-token latency aggregation
// for every channel in the filter, split by streaming mode.
//
// Returned columns, identical in both dialects so one scan handles either:
//
//	channel_id, is_stream, sample_count, avg_ms, p95_ms
//
// Two dialect implementations, one definition. PostgreSQL gets PERCENTILE_DISC,
// which is an ordered-set aggregate: it folds into the existing GROUP BY without
// numbering rows, and it is the form a Postgres deployment should be running.
// SQLite has no percentile function at all, so it gets the portable equivalent --
// ROW_NUMBER over the same partition, picking the nearest-rank row.
//
// The two agree by construction because PERCENTILE_DISC is defined as the first
// value whose cumulative distribution reaches p, i.e. the element at 1-based index
// ceil(p*n) -- exactly the row the fallback selects via integer-ceiling arithmetic
// ((n*95 + 99) / 100). That is also the definition the pre-existing in-memory probe
// P95 uses (ceil(n*0.95) - 1 into a sorted slice), so no consumer sees the
// statistic change meaning.
func BuildChannelLatencyStatsQuery(query ChannelLatencyStatsQuery) string {
	samples := channelLatencySampleSource(query)

	if query.UsePercentileAggregate {
		return buildChannelLatencyStatsQueryPostgres(samples)
	}

	return buildChannelLatencyStatsQueryPortable(samples)
}

// channelLatencySampleSource renders the sample projection shared by both dialects,
// so the row-eligibility rules cannot drift between them.
//
// Sample eligibility:
//   - completed executions only -- a failed attempt says nothing about latency
//   - first-token latency, falling back to total latency. Non-streaming responses
//     expose no separate first-token boundary, and their total latency is the
//     closest analogue (the whole response arrives at once). This mirrors what the
//     health-probe metric already does.
//   - a positive latency: a missing or zero metric leaves the pair UNKNOWN rather
//     than contributing a spuriously fast sample.
func channelLatencySampleSource(query ChannelLatencyStatsQuery) string {
	windowStart, windowEnd := "?", "?"
	if query.UseDollarPlaceholders {
		windowStart, windowEnd = "$1", "$2"
	}

	// The join exists only to reach requests.source, so the all-sources scope skips
	// it entirely and stays a single-table scan.
	sourceJoin, sourceFilter := "", ""
	if query.Scope == ChannelLatencyStatsScopeProbe {
		sourceJoin = "\n    JOIN requests r ON r.id = se.request_id"
		sourceFilter = "\n        AND r.source = 'test'"
	}

	return fmt.Sprintf(`    SELECT
        se.channel_id AS channel_id,
        CASE WHEN se.stream THEN 1 ELSE 0 END AS is_stream,
        COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) AS latency_ms
    FROM request_executions se%s
    WHERE se.created_at >= %s
        AND se.created_at < %s
        AND se.status = 'completed'
        AND se.channel_id IS NOT NULL
        AND COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) > 0%s
        %s`,
		sourceJoin, windowStart, windowEnd, sourceFilter, query.ChannelIDFilter)
}

// buildChannelLatencyStatsQueryPostgres uses the native ordered-set aggregate.
//
// AVG over a bigint column yields NUMERIC in PostgreSQL, which reaches database/sql
// as text and would only become a float64 by way of a parse. The explicit float8
// cast makes the returned type the one the scan expects.
func buildChannelLatencyStatsQueryPostgres(samples string) string {
	return fmt.Sprintf(`
WITH samples AS (
%s
)
SELECT
    channel_id,
    is_stream,
    COUNT(*) AS sample_count,
    AVG(latency_ms)::double precision AS avg_ms,
    PERCENTILE_DISC(%s) WITHIN GROUP (ORDER BY latency_ms) AS p95_ms
FROM samples
GROUP BY channel_id, is_stream
ORDER BY channel_id, is_stream`, samples, channelLatencyStatsPercentileFraction)
}

// buildChannelLatencyStatsQueryPortable computes the same nearest-rank percentile
// without any percentile function, for dialects that have none (SQLite).
//
// The rank is integer-ceiling arithmetic rather than CEIL(n * 0.95): SQLite's math
// functions are a compile-time option, so relying on CEIL would make the query fail
// on builds that omit them. Integer division truncates in both dialects, so
// (n*95 + 99) / 100 is ceil(n * 0.95) for every n >= 1.
func buildChannelLatencyStatsQueryPortable(samples string) string {
	return fmt.Sprintf(`
WITH samples AS (
%s
), ranked AS (
    SELECT
        channel_id,
        is_stream,
        latency_ms,
        ROW_NUMBER() OVER (PARTITION BY channel_id, is_stream ORDER BY latency_ms) AS latency_rank,
        COUNT(*) OVER (PARTITION BY channel_id, is_stream) AS sample_count,
        AVG(latency_ms) OVER (PARTITION BY channel_id, is_stream) AS avg_ms
    FROM samples
)
SELECT
    channel_id,
    is_stream,
    sample_count,
    avg_ms,
    latency_ms AS p95_ms
FROM ranked
WHERE latency_rank = (sample_count * %d + 99) / 100
ORDER BY channel_id, is_stream`, samples, channelLatencyStatsPercentilePercent)
}
