package biz

import (
	"context"
	"database/sql"
	"errors"
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

// channelLatencyStatsDefaultLookback is the window used when no valid gate window is
// configured. It matches defaultActiveHealthProbeScanSetting.GateWindowMinutes so an
// unconfigured install and a default-configured one judge identically.
const channelLatencyStatsDefaultLookback = 30 * time.Minute

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

// ChannelLatencyGateMinimumSamples is the smallest window the routing ceiling will
// act on. Below it the statistic reads UNKNOWN, which keeps the candidate.
const ChannelLatencyGateMinimumSamples int64 = 3

// UsableForGate reports whether the routing ceiling will act on this statistic.
//
// It lives on the sample, not beside the ceiling, because two consumers must agree on
// it: the ceiling itself and the dashboard, which now displays the very number the
// ceiling judges by. A dashboard applying its own idea of "enough samples" would show
// a figure the ceiling is ignoring -- the same "the number you read is not the number
// that decides" defect this whole line of work exists to remove.
//
// The positivity check is on AvgMs because that is the value consumers read; every
// sample in the window is a positive latency by construction, so a non-positive mean
// means the row carried nothing usable.
func (s ChannelLatencySample) UsableForGate() bool {
	return s.SampleCount >= ChannelLatencyGateMinimumSamples && s.AvgMs > 0
}

// channelLatencyStatsKey identifies one statistic: a channel, the model the request
// asked for, and whether real traffic was admitted alongside synthetic probes.
//
// The model is part of the key because it is what the ceiling is asked about. A
// per-channel figure would judge one model by another model's measurements, and on a
// channel serving both a reasoning model and a plain one those differ by more than
// two channels typically do.
type channelLatencyStatsKey struct {
	channelID          int
	modelID            string
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
	modelID     string
	sampleCount int64
	avgMs       float64
	p95Ms       float64
}

// ChannelLatencyStats reports a channel's windowed first-token latency for one model,
// or false when the window holds nothing for that pair.
//
// This is a single map lookup against the most recent snapshot: the aggregation runs
// on a schedule, never on the request path, so a routing decision never waits on a
// database query.
//
// A missing entry means UNKNOWN, not fast and not slow. Callers must keep such a
// channel eligible. A model that is never probed and carries no real traffic is
// therefore unjudged by design: the ceiling declines to rank it rather than borrowing
// another model's number.
func (svc *ChannelService) ChannelLatencyStats(
	channelID int,
	modelID string,
	includeRealTraffic bool,
) (ChannelLatencySample, bool) {
	if modelID == "" {
		return ChannelLatencySample{}, false
	}

	snapshot := svc.channelLatencyStats.Load()
	if snapshot == nil {
		return ChannelLatencySample{}, false
	}

	sample, ok := snapshot.samples[channelLatencyStatsKey{
		channelID:          channelID,
		modelID:            modelID,
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
//
// One scope failing does not discard the other. Returning early published nothing at
// all, which left the ROUTING CEILING reading a snapshot that never expired for as
// long as the error lasted -- a stale number presented as current, which is the exact
// failure mode the windowed statistic was introduced to remove. Instead every scope
// that succeeded is published, a failed scope carries its previous values forward only
// while they still overlap the current window, and the error is returned so the
// scheduler logs it.
func (svc *ChannelService) RefreshChannelLatencyStats(ctx context.Context) error {
	now := xtime.UTCNow()

	channelIDs, err := svc.enabledChannelIDs(ctx)
	if err != nil {
		return fmt.Errorf("failed to list enabled channels for latency stats: %w", err)
	}

	lookback := svc.channelLatencyStatsLookback(ctx)
	// One entry per (channel, model, scope); the model count is unknown here, so this is
	// only a starting size.
	samples := make(map[channelLatencyStatsKey]ChannelLatencySample, len(channelIDs)*2)

	var scopeErrs error

	if len(channelIDs) > 0 {
		since := now.Add(-lookback)

		for _, scope := range []qb.ChannelLatencyStatsScope{
			qb.ChannelLatencyStatsScopeProbe,
			qb.ChannelLatencyStatsScopeAll,
		} {
			includeRealTraffic := scope == qb.ChannelLatencyStatsScopeAll

			rows, err := svc.queryChannelLatencyStats(ctx, scope, channelIDs, since, now)
			if err != nil {
				scopeErrs = errors.Join(scopeErrs, fmt.Errorf("scope %s: %w", scope, err))
				svc.carryForwardLatencyScope(samples, includeRealTraffic, now, lookback)

				continue
			}

			for _, row := range rows {
				samples[channelLatencyStatsKey{
					channelID:          row.channelID,
					modelID:            row.modelID,
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

	return scopeErrs
}

// carryForwardLatencyScope copies one scope's previous values into the snapshot being
// built, for a scope whose query just failed.
//
// The carry-forward is BOUNDED by the lookback: once the previous snapshot is older
// than one window, the samples it holds describe a period that has entirely passed, so
// dropping them is the honest answer. A dropped entry reads as UNKNOWN, which keeps
// the candidate eligible -- an admitted absence rather than a silent wrong answer.
func (svc *ChannelService) carryForwardLatencyScope(
	samples map[channelLatencyStatsKey]ChannelLatencySample,
	includeRealTraffic bool,
	now time.Time,
	lookback time.Duration,
) {
	previous := svc.channelLatencyStats.Load()
	if previous == nil || previous.computedAt.IsZero() {
		return
	}

	if now.Sub(previous.computedAt) > lookback {
		return
	}

	for key, sample := range previous.samples {
		if key.includeRealTraffic == includeRealTraffic {
			samples[key] = sample
		}
	}
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

// channelLatencyStatsLookback is the window the routing ceiling judges by.
//
// It reads GateWindowMinutes, NOT the dashboard's P95 window. The gate answers "is
// this channel fast right now", and over a window measured in hours a slowdown that
// started minutes ago is diluted by hundreds of older samples -- the gate would react
// slowest at exactly the moment it exists for. The dashboard's longer window answers
// a different question and keeps its own setting.
//
// A time window rather than "the newest N samples" is deliberate, and it is about how
// each one FAILS. A time window that holds too few samples fails to UNKNOWN, and
// unknown has a safe exit: the candidate is kept. A sample-count window always finds
// N samples, so when probing is sparse those N span hours and a stale number is used
// as though it were current -- a silent wrong answer instead of an admitted absence.
func (svc *ChannelService) channelLatencyStatsLookback(ctx context.Context) time.Duration {
	if svc.SystemService == nil {
		return channelLatencyStatsDefaultLookback
	}

	setting := svc.SystemService.ChannelSettingOrDefault(ctx)
	if setting == nil || setting.ActiveHealthProbeScan == nil || setting.ActiveHealthProbeScan.GateWindowMinutes <= 0 {
		return channelLatencyStatsDefaultLookback
	}

	return time.Duration(setting.ActiveHealthProbeScan.GateWindowMinutes) * time.Minute
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

	// PostgreSQL takes $N placeholders and has PERCENTILE_DISC. SQLite and MySQL/TiDB
	// both take ? and need the portable nearest-rank fallback, but they disagree on
	// integer division: SQLite truncates with `/`, MySQL returns a DECIMAL and needs
	// `DIV`, without which the rank comparison matches no row and the whole statistic
	// silently comes back empty.
	postgres := sqlDriver.Dialect() == dialect.Postgres
	mysql := sqlDriver.Dialect() == dialect.MySQL

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
		UseIntegerDivOperator:  mysql,
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
			modelID     string
			sampleCount int64
			avgMs       sql.NullFloat64
			p95Ms       sql.NullFloat64
		)

		if err := rows.Scan(&channelID, &modelID, &sampleCount, &avgMs, &p95Ms); err != nil {
			return nil, fmt.Errorf("failed to scan channel latency stats: %w", err)
		}

		// Both statistics come from the same non-empty group over latencies the query
		// already constrained to be positive, so they are valid and positive together.
		// The guard names the mean because that is what the routing ceiling reads.
		if !avgMs.Valid || avgMs.Float64 <= 0 {
			continue
		}

		// A blank model cannot be matched by a lookup, so the group is unusable rather
		// than a bucket every unnamed request would silently share.
		if modelID == "" {
			continue
		}

		results = append(results, channelLatencyStatsRow{
			channelID:   channelID,
			modelID:     modelID,
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
