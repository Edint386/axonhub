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
	require.Contains(t, query, "GROUP BY channel_id, model_id")

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
	require.Contains(t, query, "ROW_NUMBER() OVER (PARTITION BY channel_id, model_id ORDER BY latency_ms)")
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
		// one predicate -- this is what replaced merging two averages. The join itself
		// is unconditional now: it supplies the model, which is a grouping key.
		require.Contains(t, probeScoped, "JOIN requests r ON r.id = se.request_id")
		require.Contains(t, probeScoped, "AND r.source = 'test'\n")

		require.Contains(t, allScoped, "JOIN requests r ON r.id = se.request_id")
		// The all-sources scope must not carry the scope filter. It still mentions
		// r.source inside the row-eligibility clause, which admits a probe's total
		// latency as a first-token stand-in, so assert on the filter line itself rather
		// than on the column name.
		require.NotContains(t, allScoped, "AND r.source = 'test'\n")
		require.Contains(t, allScoped, "AND (se.metrics_first_token_latency_ms IS NOT NULL OR r.source = 'test')")
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
		// First-token latency, falling back to total latency ONLY for probes: the
		// fallback is sound for a length-capped probe and wrong for real traffic, where
		// total latency is completion time and a verbose answer would read as a slow
		// first token.
		require.Contains(t, query, "COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) AS latency_ms")
		require.Contains(t, query, "AND (se.metrics_first_token_latency_ms IS NOT NULL OR r.source = 'test')")
		// A missing or zero metric leaves the pair unknown instead of adding a fast sample.
		require.Contains(t, query, "AND COALESCE(se.metrics_first_token_latency_ms, se.metrics_latency_ms) > 0")
		// The model the request asked for is the grouping dimension, and streaming mode
		// is deliberately not one -- a mode column reappearing here means the split that
		// judged model B by model A's samples has come back.
		require.Contains(t, query, "r.model_id AS model_id")
		require.NotContains(t, query, "is_stream")
	}
}

func TestBuildChannelLatencyStatsQueryColumnOrderMatchesAcrossDialects(t *testing.T) {
	// One Go scan reads either dialect's rows, so the select list order is part of the
	// contract: channel_id, model_id, sample_count, avg_ms, p95_ms.
	postgres := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
		UseDollarPlaceholders:  true,
		UsePercentileAggregate: true,
		Scope:                  ChannelLatencyStatsScopeAll,
	})
	require.Contains(t, postgres, `SELECT
    channel_id,
    model_id,
    COUNT(*) AS sample_count,
    AVG(latency_ms)::double precision AS avg_ms,
    PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY latency_ms) AS p95_ms
FROM samples`)

	sqlite := BuildChannelLatencyStatsQuery(ChannelLatencyStatsQuery{
		Scope: ChannelLatencyStatsScopeAll,
	})
	require.Contains(t, sqlite, `SELECT
    channel_id,
    model_id,
    sample_count,
    avg_ms,
    latency_ms AS p95_ms
FROM ranked`)
}
