package qb

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildChannelLatencyStatsQueryPostgresUsesPercentileAggregate(t *testing.T) {
	query := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
		UseDollarPlaceholders:  true,
		UsePercentileAggregate: true,
		Scope:                  ChannelLatencyStatsScopeAll,
		ChannelIDFilter:        "AND se.channel_id IN ($3,$4)",
	})

	// The native ordered-set aggregate folds into the GROUP BY, so no row numbering
	// is needed at all on PostgreSQL.
	require.Contains(t, query, "PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY latency_ms)")
	require.NotContains(t, query, "ROW_NUMBER()")
	require.Contains(t, query, "GROUP BY channel_id, is_stream")

	require.Contains(t, query, "se.created_at >= $1")
	require.Contains(t, query, "se.created_at < $2")
	require.Contains(t, query, "AND se.channel_id IN ($3,$4)")
	require.NotContains(t, query, "?")
}

func TestBuildChannelLatencyStatsQuerySQLiteUsesNearestRankFallback(t *testing.T) {
	query := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
		Scope:           ChannelLatencyStatsScopeAll,
		ChannelIDFilter: "AND se.channel_id IN (?,?)",
	})

	// SQLite has no percentile function, so the same value comes from ROW_NUMBER over
	// the same partition. The rank must be integer-ceiling arithmetic rather than
	// CEIL(), which is a compile-time option in SQLite.
	require.NotContains(t, query, "PERCENTILE_DISC")
	require.Contains(t, query, "ROW_NUMBER() OVER (PARTITION BY channel_id, is_stream ORDER BY latency_ms)")
	require.Contains(t, query, "WHERE latency_rank = (sample_count * 95 + 99) / 100")
	require.NotContains(t, query, "CEIL")

	require.Contains(t, query, "se.created_at >= ?")
	require.Contains(t, query, "se.created_at < ?")
	require.NotContains(t, query, "$1")
}

func TestBuildChannelLatencyStatsQueryScopeControlsTheSourceFilter(t *testing.T) {
	for _, useAggregate := range []bool{true, false} {
		probeScoped := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
			UsePercentileAggregate: useAggregate,
			UseDollarPlaceholders:  useAggregate,
			Scope:                  ChannelLatencyStatsScopeProbe,
		})
		allScoped := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
			UsePercentileAggregate: useAggregate,
			UseDollarPlaceholders:  useAggregate,
			Scope:                  ChannelLatencyStatsScopeAll,
		})

		// The entire difference between "probe only" and "probe plus real traffic" is
		// one join and one predicate -- this is what replaced merging two averages.
		require.Contains(t, probeScoped, "JOIN requests r ON r.id = se.request_id")
		require.Contains(t, probeScoped, "AND r.source = 'test'")

		// The all-sources scope needs nothing from requests, so it stays single-table.
		require.NotContains(t, allScoped, "JOIN requests")
		require.NotContains(t, allScoped, "r.source")
	}
}

func TestBuildChannelLatencyStatsQuerySharesSampleEligibility(t *testing.T) {
	// Both dialects must agree on which rows are samples at all, so the shared
	// projection is asserted for each of them rather than for one.
	for _, useAggregate := range []bool{true, false} {
		query := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
			UsePercentileAggregate: useAggregate,
			UseDollarPlaceholders:  useAggregate,
			Scope:                  ChannelLatencyStatsScopeAll,
		})

		require.Contains(t, query, "AND se.status = 'completed'")
		require.Contains(t, query, "AND se.channel_id IS NOT NULL")
		// First-token latency, falling back to total latency for non-streaming rows.
		require.Contains(t, query, "COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) AS latency_ms")
		// A missing or zero metric leaves the pair unknown instead of adding a fast sample.
		require.Contains(t, query, "AND COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) > 0")
		// Streaming mode is projected as an integer so one scan works on both dialects.
		require.Contains(t, query, "CASE WHEN se.stream THEN 1 ELSE 0 END AS is_stream")
	}
}

func TestBuildChannelLatencyStatsQueryColumnOrderMatchesAcrossDialects(t *testing.T) {
	// One Go scan reads either dialect's rows, so the select list order is part of the
	// contract: channel_id, is_stream, sample_count, avg_ms, p95_ms.
	postgres := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
		UseDollarPlaceholders:  true,
		UsePercentileAggregate: true,
		Scope:                  ChannelLatencyStatsScopeAll,
	})
	require.Contains(t, postgres, `SELECT
    channel_id,
    is_stream,
    COUNT(*) AS sample_count,
    AVG(latency_ms)::double precision AS avg_ms,
    PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY latency_ms) AS p95_ms
FROM samples`)

	sqlite := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
		Scope: ChannelLatencyStatsScopeAll,
	})
	require.Contains(t, sqlite, `SELECT
    channel_id,
    is_stream,
    sample_count,
    avg_ms,
    latency_ms AS p95_ms
FROM ranked`)
}
