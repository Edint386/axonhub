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

	// UseIntegerDivOperator selects MySQL's `DIV` over `/` for the nearest-rank
	// arithmetic.
	//
	// It is not a style choice. `/` is integer division in SQLite and PostgreSQL but
	// DECIMAL division in MySQL, and the rank compares against an integer ROW_NUMBER:
	// (n*95 + 99) / 100 is never a whole number for any n >= 1, so on MySQL the
	// comparison matched no row at all and the query returned an empty result set --
	// silently, because an empty set is indistinguishable from "no samples yet".
	UseIntegerDivOperator bool

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
// for every channel in the filter, split by the MODEL the request asked for.
//
// The model is the grouping dimension because it is the dimension the ceiling is
// asked about: a routing decision is always "may this request, for THIS model, use
// this channel". Two models on one channel do not share a first-token latency -- a
// reasoning model and a plain one differ by more than two channels typically do -- so
// a per-channel figure answers a question nobody asked and silently judges model B by
// model A's measurements.
//
// Streaming mode is deliberately NOT part of the key. It was, and that split
// subdivided every group without adding information the ceiling could act on: the
// probe cadence has one global stream flag, so one bucket held every probe sample and
// the other held none, which forced a cross-mode fallback to keep the ceiling working
// at all. Folding the modes together removes that machinery. The cost is that a
// streaming TTFT and a non-streaming total latency can now land in the same mean --
// acceptable because the sample projection already coalesces the two metrics, and a
// probe response is capped short enough that they are close.
//
// Returned columns, identical in both dialects so one scan handles either:
//
//	channel_id, model_id, sample_count, avg_ms, p95_ms
//
// Two dialect implementations, one definition. PostgreSQL gets PERCENTILE_DISC,
// which is an ordered-set aggregate: it folds into the existing GROUP BY without
// numbering rows, and it is the form a Postgres deployment should be running.
// SQLite and MySQL have no percentile function, so they get the portable equivalent
// -- ROW_NUMBER over the same partition, picking the nearest-rank row -- differing
// only in the integer division operator.
//
// The two agree by construction because PERCENTILE_DISC is defined as the first
// value whose cumulative distribution reaches p, i.e. the element at 1-based index
// ceil(p*n) -- exactly the row the fallback selects via integer-ceiling arithmetic
// ((n*95 + 99) / 100, with a dialect-correct integer division operator). That is also
// the definition the pre-existing in-memory probe P95 uses (ceil(n*0.95) - 1 into a
// sorted slice), so no consumer sees the statistic change meaning.
func BuildChannelLatencyStatsQuery(query ChannelLatencyStatsQuery) string {
	samples := channelLatencySampleSource(query)

	if query.UsePercentileAggregate {
		return buildChannelLatencyStatsQueryPostgres(samples)
	}

	// MySQL's `/` yields a DECIMAL, which can never equal an integer ROW_NUMBER for
	// this arithmetic; `DIV` is its integer division operator.
	integerDiv := "/"
	if query.UseIntegerDivOperator {
		integerDiv = "DIV"
	}

	return buildChannelLatencyStatsQueryPortable(samples, integerDiv)
}

// channelLatencySampleSource renders the sample projection shared by both dialects,
// so the row-eligibility rules cannot drift between them.
//
// Sample eligibility:
//   - completed executions only -- a failed attempt says nothing about latency
//   - a positive latency: a missing or zero metric leaves the pair UNKNOWN rather
//     than contributing a spuriously fast sample.
//   - an execution that reaches a request row. The join is what supplies the model,
//     so an execution with no request is dropped rather than attributed to a guess.
//   - a row that actually measures a FIRST TOKEN. metrics_first_token_latency_ms is
//     written only for streaming executions, so a non-streaming row carries nothing
//     but its total completion time. For a length-capped probe those are nearly the
//     same number and the fallback is sound; for real traffic they are not remotely
//     the same -- a long generation takes tens of seconds after a fast first token --
//     and a ceiling named "maximum first-token latency" would then exclude a channel
//     for being verbose. So the fallback is admitted for probes and refused for
//     everything else, which is a filter on the ROW rather than on the scope: the
//     probe-only scope is unaffected, and the all-sources scope keeps every probe
//     plus every streaming request.
func channelLatencySampleSource(query ChannelLatencyStatsQuery) string {
	windowStart, windowEnd := "?", "?"
	if query.UseDollarPlaceholders {
		windowStart, windowEnd = "$1", "$2"
	}

	// The join is unconditional now that the model comes from it; only the source
	// predicate varies by scope. It is a primary-key lookup per execution row.
	sourceFilter := ""
	if query.Scope == ChannelLatencyStatsScopeProbe {
		sourceFilter = "\n        AND r.source = 'test'"
	}

	return fmt.Sprintf(`    SELECT
        se.channel_id AS channel_id,
        r.model_id AS model_id,
        COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) AS latency_ms
    FROM request_executions se
    JOIN requests r ON r.id = se.request_id
    WHERE se.created_at >= %s
        AND se.created_at < %s
        AND se.status = 'completed'
        AND se.channel_id IS NOT NULL
        AND (se.metrics_first_token_latency_ms IS NOT NULL OR r.source = 'test')
        AND COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) > 0%s
        %s`,
		windowStart, windowEnd, sourceFilter, query.ChannelIDFilter)
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
    model_id,
    COUNT(*) AS sample_count,
    AVG(latency_ms)::double precision AS avg_ms,
    PERCENTILE_DISC(%s) WITHIN GROUP (ORDER BY latency_ms) AS p95_ms
FROM samples
GROUP BY channel_id, model_id
ORDER BY channel_id, model_id`, samples, channelLatencyStatsPercentileFraction)
}

// buildChannelLatencyStatsQueryPortable computes the same nearest-rank percentile
// without any percentile function, for dialects that have none (SQLite, MySQL).
//
// The rank is integer-ceiling arithmetic rather than CEIL(n * 0.95): SQLite's math
// functions are a compile-time option, so relying on CEIL would make the query fail
// on builds that omit them. (n*95 + 99) / 100 is ceil(n * 0.95) for every n >= 1
// PROVIDED the division truncates, which is why the operator is dialect-supplied:
// SQLite and PostgreSQL truncate with `/`, MySQL needs `DIV`.
func buildChannelLatencyStatsQueryPortable(samples string, integerDiv string) string {
	return fmt.Sprintf(`
WITH samples AS (
%s
), ranked AS (
    SELECT
        channel_id,
        model_id,
        latency_ms,
        ROW_NUMBER() OVER (PARTITION BY channel_id, model_id ORDER BY latency_ms) AS latency_rank,
        COUNT(*) OVER (PARTITION BY channel_id, model_id) AS sample_count,
        AVG(latency_ms) OVER (PARTITION BY channel_id, model_id) AS avg_ms
    FROM samples
)
SELECT
    channel_id,
    model_id,
    sample_count,
    avg_ms,
    latency_ms AS p95_ms
FROM ranked
WHERE latency_rank = (sample_count * %d + 99) %s 100
ORDER BY channel_id, model_id`, samples, channelLatencyStatsPercentilePercent, integerDiv)
}
