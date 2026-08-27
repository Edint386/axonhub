package biz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
)

// newLatencyStatsTestService builds a ChannelService backed by its own in-memory
// database. SystemService is left nil so the lookback falls back to the documented
// 24-hour default.
func newLatencyStatsTestService(t *testing.T, name string) (*ChannelService, *ent.Client, context.Context) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:channel-latency-%s?mode=memory&_fk=0", name))
	t.Cleanup(func() { _ = client.Close() })

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))
	svc := &ChannelService{AbstractService: &AbstractService{db: client}}

	return svc, client, ctx
}

func newLatencyStatsChannel(t *testing.T, ctx context.Context, client *ent.Client, name string, status channel.Status) *ent.Channel {
	t.Helper()

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenaiFake).
		SetName(name).
		SetStatus(status).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetCredentials(objects.ChannelCredentials{}).
		Save(ctx)
	require.NoError(t, err)

	return ch
}

type latencySampleSpec struct {
	source    request.Source
	stream    bool
	latencyMs int64
	// measured is false for a completed execution that produced no latency metric at
	// all, which must stay out of the window rather than count as a fast sample.
	measured bool
	status   requestexecution.Status
	at       time.Time
}

func seedLatencySample(t *testing.T, ctx context.Context, client *ent.Client, channelID int, spec latencySampleSpec) {
	t.Helper()

	req, err := client.Request.Create().
		SetModelID("gpt-4").
		SetSource(spec.source).
		SetRequestBody(objects.JSONRawMessage(`{}`)).
		SetStatus(request.StatusCompleted).
		SetChannelID(channelID).
		SetStream(spec.stream).
		SetCreatedAt(spec.at).
		SetUpdatedAt(spec.at).
		Save(ctx)
	require.NoError(t, err)

	create := client.RequestExecution.Create().
		SetRequestID(req.ID).
		SetChannelID(channelID).
		SetModelID("gpt-4").
		SetRequestBody(objects.JSONRawMessage(`{}`)).
		SetStatus(spec.status).
		SetStream(spec.stream).
		SetCreatedAt(spec.at).
		SetUpdatedAt(spec.at)

	if spec.measured {
		create = create.SetMetricsLatencyMs(spec.latencyMs)
		// Only streaming responses expose a first-token boundary; a non-streaming row
		// carries total latency alone, which the query falls back to.
		if spec.stream {
			create = create.SetMetricsFirstTokenLatencyMs(spec.latencyMs)
		}
	}

	_, err = create.Save(ctx)
	require.NoError(t, err)
}

func streamingSample(source request.Source, latencyMs int64, at time.Time) latencySampleSpec {
	return latencySampleSpec{
		source:    source,
		stream:    true,
		latencyMs: latencyMs,
		measured:  true,
		status:    requestexecution.StatusCompleted,
		at:        at,
	}
}

func TestChannelLatencyStatsScopeSeparatesProbeFromRealTraffic(t *testing.T) {
	// The switch an API key profile exposes is exactly this: which scope the ceiling
	// reads. Both are the same statistic over the same table, so there is no merge
	// rule and no way for one to shadow the other.
	svc, client, ctx := newLatencyStatsTestService(t, "scopes")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceTest, 100, recent.Add(time.Duration(i)*time.Minute)))
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceAPI, 9_000, recent.Add(time.Duration(i)*time.Minute)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	probeOnly, ok := svc.ChannelLatencyStats(ch.ID, true, false)
	require.True(t, ok)
	require.Equal(t, int64(3), probeOnly.SampleCount)
	require.InDelta(t, 100.0, probeOnly.P95Ms, 0.001)

	includingTraffic, ok := svc.ChannelLatencyStats(ch.ID, true, true)
	require.True(t, ok)
	require.Equal(t, int64(6), includingTraffic.SampleCount)
	require.InDelta(t, 9_000.0, includingTraffic.P95Ms, 0.001)

	computedAt, lookback := svc.ChannelLatencyStatsComputedAt()
	require.False(t, computedAt.IsZero())
	require.Equal(t, channelLatencyStatsDefaultLookback, lookback)
}

func TestChannelLatencyStatsUsesNearestRankPercentile(t *testing.T) {
	// 20 samples put the nearest-rank P95 at ceil(20 * 0.95) = the 19th smallest.
	// This is the definition PostgreSQL's PERCENTILE_DISC computes natively and the
	// one the portable ROW_NUMBER fallback reproduces, so both dialects agree.
	svc, client, ctx := newLatencyStatsTestService(t, "percentile")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-2 * time.Hour)
	for i := 1; i <= 20; i++ {
		seedLatencySample(t, ctx, client, ch.ID,
			streamingSample(request.SourceAPI, int64(i*100), recent.Add(time.Duration(i)*time.Second)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	sample, ok := svc.ChannelLatencyStats(ch.ID, true, true)
	require.True(t, ok)
	require.Equal(t, int64(20), sample.SampleCount)
	require.InDelta(t, 1_900.0, sample.P95Ms, 0.001)
	require.InDelta(t, 1_050.0, sample.AvgMs, 0.001)
}

func TestChannelLatencyStatsSeparatesStreamingModes(t *testing.T) {
	// Streaming rows measure a first-token boundary and non-streaming rows a total
	// response time. Mixing them would compare two different measurements against one
	// ceiling, so they are separate windows.
	svc, client, ctx := newLatencyStatsTestService(t, "modes")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		at := recent.Add(time.Duration(i) * time.Minute)
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceAPI, 100, at))
		seedLatencySample(t, ctx, client, ch.ID, latencySampleSpec{
			source:    request.SourceAPI,
			latencyMs: 5_000,
			measured:  true,
			status:    requestexecution.StatusCompleted,
			at:        at,
		})
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	streaming, ok := svc.ChannelLatencyStats(ch.ID, true, true)
	require.True(t, ok)
	require.Equal(t, int64(3), streaming.SampleCount)
	require.InDelta(t, 100.0, streaming.P95Ms, 0.001)

	nonStreaming, ok := svc.ChannelLatencyStats(ch.ID, false, true)
	require.True(t, ok)
	require.Equal(t, int64(3), nonStreaming.SampleCount)
	require.InDelta(t, 5_000.0, nonStreaming.P95Ms, 0.001)
}

func TestChannelLatencyStatsExcludesFailedAndUnmeasuredExecutions(t *testing.T) {
	// A failed attempt says nothing about latency, and a completed attempt with no
	// metric must stay unknown rather than enter the window as a fast sample.
	svc, client, ctx := newLatencyStatsTestService(t, "eligibility")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		seedLatencySample(t, ctx, client, ch.ID,
			streamingSample(request.SourceAPI, 100, recent.Add(time.Duration(i)*time.Minute)))
	}

	seedLatencySample(t, ctx, client, ch.ID, latencySampleSpec{
		source:    request.SourceAPI,
		stream:    true,
		latencyMs: 90_000,
		measured:  true,
		status:    requestexecution.StatusFailed,
		at:        recent,
	})
	seedLatencySample(t, ctx, client, ch.ID, latencySampleSpec{
		source: request.SourceAPI,
		stream: true,
		status: requestexecution.StatusCompleted,
		at:     recent,
	})

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	sample, ok := svc.ChannelLatencyStats(ch.ID, true, true)
	require.True(t, ok)
	require.Equal(t, int64(3), sample.SampleCount)
	require.InDelta(t, 100.0, sample.P95Ms, 0.001)
}

func TestChannelLatencyStatsDropsSamplesOutsideTheWindow(t *testing.T) {
	// The property that makes a lockout impossible. Old slow samples age out on their
	// own: the channel returns to UNKNOWN (which keeps it eligible), and once it is
	// measured again the fresh value is the only one there is -- no stale number can
	// outvote it, and no expiry bookkeeping is needed to retire one.
	svc, client, ctx := newLatencyStatsTestService(t, "window")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	stale := time.Now().UTC().Add(-30 * time.Hour)
	for i := range 3 {
		seedLatencySample(t, ctx, client, ch.ID,
			streamingSample(request.SourceAPI, 40_000, stale.Add(time.Duration(i)*time.Minute)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	_, ok := svc.ChannelLatencyStats(ch.ID, true, true)
	require.False(t, ok, "samples older than the lookback must not be known at all")

	recent := time.Now().UTC().Add(-time.Minute)
	for i := range 3 {
		seedLatencySample(t, ctx, client, ch.ID,
			streamingSample(request.SourceAPI, 3_000, recent.Add(time.Duration(i)*time.Second)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	sample, ok := svc.ChannelLatencyStats(ch.ID, true, true)
	require.True(t, ok)
	require.Equal(t, int64(3), sample.SampleCount)
	require.InDelta(t, 3_000.0, sample.P95Ms, 0.001)
}

func TestChannelLatencyStatsSkipsDisabledChannels(t *testing.T) {
	// Only enabled channels can be routing candidates, and the channel predicate is
	// also what keeps the aggregation on the (channel_id, created_at) index.
	svc, client, ctx := newLatencyStatsTestService(t, "disabled")
	enabled := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)
	disabled := newLatencyStatsChannel(t, ctx, client, "c2", channel.StatusDisabled)

	recent := time.Now().UTC().Add(-time.Hour)
	for i := range 3 {
		at := recent.Add(time.Duration(i) * time.Minute)
		seedLatencySample(t, ctx, client, enabled.ID, streamingSample(request.SourceAPI, 100, at))
		seedLatencySample(t, ctx, client, disabled.ID, streamingSample(request.SourceAPI, 100, at))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	_, ok := svc.ChannelLatencyStats(enabled.ID, true, true)
	require.True(t, ok)

	_, ok = svc.ChannelLatencyStats(disabled.ID, true, true)
	require.False(t, ok)
}

func TestChannelLatencyStatsUnknownBeforeFirstRefresh(t *testing.T) {
	// Before any snapshot exists every channel must read as unknown, so a ceiling
	// filters nothing rather than filtering everything.
	svc, _, _ := newLatencyStatsTestService(t, "cold")

	_, ok := svc.ChannelLatencyStats(1, true, false)
	require.False(t, ok)

	computedAt, lookback := svc.ChannelLatencyStatsComputedAt()
	require.True(t, computedAt.IsZero())
	require.Zero(t, lookback)
}
