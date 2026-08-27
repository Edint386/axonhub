package biz

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xtime"
	"github.com/looplj/axonhub/internal/server/gql/qb"
)

// channelLatencyStatsDefaultLookback is the window used when no valid P95 lookback
// is configured.
const channelLatencyStatsDefaultLookback = 24 * time.Hour

// ChannelLatencySample is one channel's windowed first-token latency, for one
// streaming mode and one source scope.
//
// Unlike the in-memory EWMAs this is a WINDOWED statistic recomputed from the
// requests table, which is what removes three separate defects at once:
//   - no merge rule is needed for "probe plus real traffic": the scope is a WHERE
//     clause, so there is only ever one number to compare against the ceiling
//   - a channel cannot be locked out forever by a frozen average, because samples
//     leave the window on their own once they age out
//   - a restart no longer zeroes the signal, because the samples live in the
//     database rather than in process memory
type ChannelLatencySample struct {
	// AvgMs is the mean first-token latency over the window, and the value the
	// routing ceiling compares against. A ceiling labelled "maximum first-token
	// latency" is read as a statement about how fast a channel usually is, which is
	// the question a mean answers; over a lookback window measured in hours a
	// percentile answers a different and much stricter one.
	AvgMs float64

	// P95Ms is the nearest-rank 95th percentile over the same samples. Nothing reads
	// it today: the routing ceiling takes the mean, and the dashboard's own P95 is a
	// separate figure computed from the probe-run table. It is kept because it costs
	// nothing -- the same scan produces it -- and it is what a consumer asking about
	// the tail rather than the typical case would need.
	P95Ms float64

	// SampleCount is how many samples the window held. The routing ceiling applies
	// its own minimum before trusting the statistic.
	SampleCount int64
}

// channelLatencyStatsKey identifies one statistic: a channel, a streaming mode, and
// whether real traffic was admitted alongside synthetic probes.
type channelLatencyStatsKey struct {
	channelID          int
	streaming          bool
	includeRealTraffic bool
}

// channelLatencyStatsSnapshot is an immutable result set. Refresh always stores a
// brand-new map, never mutating a published one, so readers on the request path
// need no lock.
type channelLatencyStatsSnapshot struct {
	computedAt time.Time
	lookback   time.Duration
	samples    map[channelLatencyStatsKey]ChannelLatencySample
}

type channelLatencyStatsRow struct {
	channelID   int
	streaming   bool
	sampleCount int64
	avgMs       float64
	p95Ms       float64
}

// ChannelLatencyStats reports a channel's windowed first-token latency, or false
// when the window holds nothing for it.
//
// This is a single map lookup against the most recent snapshot: the aggregation runs
// on a schedule, never on the request path, so a routing decision never waits on a
// database query.
//
// A missing entry means UNKNOWN, not fast and not slow. Callers must keep such a
// channel eligible.
func (svc *ChannelService) ChannelLatencyStats(
	channelID int,
	streaming bool,
	includeRealTraffic bool,
) (ChannelLatencySample, bool) {
	snapshot := svc.channelLatencyStats.Load()
	if snapshot == nil {
		return ChannelLatencySample{}, false
	}

	sample, ok := snapshot.samples[channelLatencyStatsKey{
		channelID:          channelID,
		streaming:          streaming,
		includeRealTraffic: includeRealTraffic,
	}]

	return sample, ok
}

// ChannelLatencyStatsComputedAt reports when the current snapshot was produced, and
// the window it covered. Zero values mean nothing has been computed yet.
func (svc *ChannelService) ChannelLatencyStatsComputedAt() (time.Time, time.Duration) {
	snapshot := svc.channelLatencyStats.Load()
	if snapshot == nil {
		return time.Time{}, 0
	}

	return snapshot.computedAt, snapshot.lookback
}

// RefreshChannelLatencyStats recomputes the snapshot for every enabled channel.
//
// Two aggregations run per refresh, one per scope. A mean could in principle be
// combined after the fact from two means and their counts, but the percentile cannot
// -- P95(probe ∪ traffic) is not derivable from P95(probe) and P95(traffic) -- and
// both statistics are published per scope. Each aggregation is a single batch query
// over all channels, so channel count does not change the query count.
func (svc *ChannelService) RefreshChannelLatencyStats(ctx context.Context) error {
	now := xtime.UTCNow()

	channelIDs, err := svc.enabledChannelIDs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list enabled channels for latency stats: %w", err)
	}

	lookback := svc.channelLatencyStatsLookback(ctx)
	samples := make(map[channelLatencyStatsKey]ChannelLatencySample, len(channelIDs)*4)

	if len(channelIDs) > 0 {
		since := now.Add(-lookback)

		for _, scope := range []qb.ChannelLatencyStatsScope{
			qb.ChannelLatencyStatsScopeProbe,
			qb.ChannelLatencyStatsScopeAll,
		} {
			rows, err := svc.queryChannelLatencyStats(ctx, scope, channelIDs, since, now)
			if err != nil {
				return err
			}

			includeRealTraffic := scope == qb.ChannelLatencyStatsScopeAll
			for _, row := range rows {
				samples[channelLatencyStatsKey{
					channelID:          row.channelID,
					streaming:          row.streaming,
					includeRealTraffic: includeRealTraffic,
				}] = ChannelLatencySample{
					P95Ms:       row.p95Ms,
					AvgMs:       row.avgMs,
					SampleCount: row.sampleCount,
				}
			}
		}
	}

	svc.channelLatencyStats.Store(&channelLatencyStatsSnapshot{
		computedAt: now,
		lookback:   lookback,
		samples:    samples,
	})

	return nil
}

// runChannelLatencyStatsPeriodically is the scheduled entry point.
func (svc *ChannelService) runChannelLatencyStatsPeriodically(ctx context.Context) {
	ctx = authz.WithSystemBypass(ctx, "channel-latency-stats")

	if err := svc.RefreshChannelLatencyStats(ctx); err != nil {
		log.Warn(ctx, "failed to refresh channel latency stats", log.Cause(err))
		return
	}

	if log.DebugEnabled(ctx) {
		computedAt, lookback := svc.ChannelLatencyStatsComputedAt()
		log.Debug(ctx, "refreshed channel latency stats",
			log.Time("computed_at", computedAt),
			log.Duration("lookback", lookback),
		)
	}
}

// channelLatencyStatsLookback reuses the active-probe P95 window, so the number the
// routing ceiling compares against covers the same period the dashboard shows.
func (svc *ChannelService) channelLatencyStatsLookback(ctx context.Context) time.Duration {
	if svc.SystemService == nil {
		return channelLatencyStatsDefaultLookback
	}

	setting := svc.SystemService.ChannelSettingOrDefault(ctx)
	if setting == nil || setting.ActiveHealthProbeScan == nil || setting.ActiveHealthProbeScan.P95LookbackHours <= 0 {
		return channelLatencyStatsDefaultLookback
	}

	return time.Duration(setting.ActiveHealthProbeScan.P95LookbackHours) * time.Hour
}

func (svc *ChannelService) enabledChannelIDs(ctx context.Context) ([]int, error) {
	entities, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.StatusEQ(channel.StatusEnabled)).
		Select(channel.FieldID).
		All(ctx)
	if err != nil {
		return nil, err
	}

	channelIDs := make([]int, len(entities))
	for i, entity := range entities {
		channelIDs[i] = entity.ID
	}

	return channelIDs, nil
}

func (svc *ChannelService) queryChannelLatencyStats(
	ctx context.Context,
	scope qb.ChannelLatencyStatsScope,
	channelIDs []int,
	since time.Time,
	until time.Time,
) ([]channelLatencyStatsRow, error) {
	sqlDriver, ok := svc.db.Driver().(*entsql.Driver)
	if !ok {
		return nil, fmt.Errorf("failed to get underlying SQL driver for channel latency stats")
	}

	// PostgreSQL takes $N placeholders and has PERCENTILE_DISC; every other dialect
	// we support (SQLite) takes ? and needs the portable nearest-rank fallback.
	postgres := sqlDriver.Dialect() == dialect.Postgres

	args := make([]any, 0, len(channelIDs)+2)
	args = append(args, since.UTC(), until.UTC())

	placeholders := make([]string, len(channelIDs))

	for i, channelID := range channelIDs {
		if postgres {
			// $1 and $2 are the window bounds.
			placeholders[i] = fmt.Sprintf("$%d", i+3)
		} else {
			placeholders[i] = "?"
		}

		args = append(args, channelID)
	}

	query := qb.BuildChannelLatencyStatsQuery(qb.ChannelLatencyStatsQuery{
		UseDollarPlaceholders:  postgres,
		UsePercentileAggregate: postgres,
		Scope:                  scope,
		ChannelIDFilter:        fmt.Sprintf("AND se.channel_id IN (%s)", strings.Join(placeholders, ",")),
	})

	rows, err := sqlDriver.DB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query channel latency stats (scope %s): %w", scope, err)
	}

	defer func() { _ = rows.Close() }()

	var results []channelLatencyStatsRow

	for rows.Next() {
		var (
			channelID   int
			isStream    int64
			sampleCount int64
			avgMs       sql.NullFloat64
			p95Ms       sql.NullFloat64
		)

		if err := rows.Scan(&channelID, &isStream, &sampleCount, &avgMs, &p95Ms); err != nil {
			return nil, fmt.Errorf("failed to scan channel latency stats: %w", err)
		}

		// Both statistics come from the same non-empty group over latencies the query
		// already constrained to be positive, so they are valid and positive together.
		// The guard names the mean because that is what the routing ceiling reads.
		if !avgMs.Valid || avgMs.Float64 <= 0 {
			continue
		}

		results = append(results, channelLatencyStatsRow{
			channelID:   channelID,
			streaming:   isStream != 0,
			sampleCount: sampleCount,
			avgMs:       avgMs.Float64,
			p95Ms:       p95Ms.Float64,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating channel latency stats: %w", err)
	}

	return results, nil
}
