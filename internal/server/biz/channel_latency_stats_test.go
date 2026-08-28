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
// default gate window.
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

// testLatencyModel is the model every fixture uses unless it is specifically
// exercising the per-model split.
const testLatencyModel = "gpt-4"

type latencySampleSpec struct {
	source request.Source
	// modelID is the model the request asked for, which is the dimension the window is
	// grouped by. Empty means testLatencyModel.
	modelID   string
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

	modelID := spec.modelID
	if modelID == "" {
		modelID = testLatencyModel
	}

	// request_executions carries its OWN model_id -- the upstream/actual model, not the
	// one the caller asked for. The gate keys on the requested name, so the two are
	// deliberately different here: reading the execution's column instead of the
	// request's would be a real bug that identical fixtures could never catch.
	executionModelID := modelID + "-upstream"

	req, err := client.Request.Create().
		SetModelID(modelID).
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
		SetModelID(executionModelID).
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

func streamingSampleForModel(modelID string, source request.Source, latencyMs int64, at time.Time) latencySampleSpec {
	spec := streamingSample(source, latencyMs, at)
	spec.modelID = modelID

	return spec
}

func TestChannelLatencyStatsScopeSeparatesProbeFromRealTraffic(t *testing.T) {
	// The switch an API key profile exposes is exactly this: which scope the ceiling
	// reads. Both are the same statistic over the same table, so there is no merge
	// rule and no way for one to shadow the other.
	svc, client, ctx := newLatencyStatsTestService(t, "scopes")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-5 * time.Minute)
	for i := range 3 {
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceTest, 100, recent.Add(time.Duration(i)*time.Minute)))
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceAPI, 9_000, recent.Add(time.Duration(i)*time.Minute)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	probeOnly, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, false)
	require.True(t, ok)
	require.Equal(t, int64(3), probeOnly.SampleCount)
	require.InDelta(t, 100.0, probeOnly.P95Ms, 0.001)

	includingTraffic, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, true)
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

	recent := time.Now().UTC().Add(-10 * time.Minute)
	for i := 1; i <= 20; i++ {
		seedLatencySample(t, ctx, client, ch.ID,
			streamingSample(request.SourceAPI, int64(i*100), recent.Add(time.Duration(i)*time.Second)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	sample, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, true)
	require.True(t, ok)
	require.Equal(t, int64(20), sample.SampleCount)
	require.InDelta(t, 1_900.0, sample.P95Ms, 0.001)
	require.InDelta(t, 1_050.0, sample.AvgMs, 0.001)
}

func TestChannelLatencyStatsSeparatesModels(t *testing.T) {
	// The reason the model is a grouping key. One channel serving two models holds two
	// windows, and asking about one never returns the other's number -- a reasoning
	// model and a plain one differ by more than two channels typically do, so a single
	// per-channel figure would judge one by evidence about the other.
	//
	// This test executes the real SQL, so it is what proves the GROUP BY, not just the
	// key struct.
	svc, client, ctx := newLatencyStatsTestService(t, "models")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	const (
		fastModel = "gpt-fast"
		slowModel = "gpt-slow"
	)

	recent := time.Now().UTC().Add(-5 * time.Minute)
	for i := range 3 {
		at := recent.Add(time.Duration(i) * time.Minute)
		seedLatencySample(t, ctx, client, ch.ID, streamingSampleForModel(fastModel, request.SourceTest, 100, at))
		seedLatencySample(t, ctx, client, ch.ID, streamingSampleForModel(slowModel, request.SourceTest, 9_000, at))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	fast, ok := svc.ChannelLatencyStats(ch.ID, fastModel, false)
	require.True(t, ok)
	require.Equal(t, int64(3), fast.SampleCount)
	require.InDelta(t, 100.0, fast.AvgMs, 0.001)

	slow, ok := svc.ChannelLatencyStats(ch.ID, slowModel, false)
	require.True(t, ok)
	require.Equal(t, int64(3), slow.SampleCount)
	require.InDelta(t, 9_000.0, slow.AvgMs, 0.001)

	// A model this channel has no measurements for is UNKNOWN rather than inheriting a
	// sibling's number, which is what keeps an unprobed model unjudged instead of
	// wrongly excluded.
	_, ok = svc.ChannelLatencyStats(ch.ID, "gpt-never-probed", false)
	require.False(t, ok)
}

func TestChannelLatencyStatsFoldsProbeModesTogether(t *testing.T) {
	// Streaming mode is NOT a grouping key. It was, and the split bought nothing the
	// ceiling could act on: one global probe stream flag meant one bucket held every
	// probe sample and the other held none, which forced a cross-mode fallback just to
	// keep the ceiling working.
	//
	// So both modes land in one window for the model. Here three streaming probes at
	// 100ms and three non-streaming at 5000ms make one window of six averaging 2550ms.
	// The non-streaming rows are admitted because a probe response is length-capped,
	// which is what makes its total latency a sound stand-in for a first token.
	svc, client, ctx := newLatencyStatsTestService(t, "modes")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-5 * time.Minute)
	for i := range 3 {
		at := recent.Add(time.Duration(i) * time.Minute)
		seedLatencySample(t, ctx, client, ch.ID, streamingSample(request.SourceTest, 100, at))
		seedLatencySample(t, ctx, client, ch.ID, latencySampleSpec{
			source:    request.SourceTest,
			latencyMs: 5_000,
			measured:  true,
			status:    requestexecution.StatusCompleted,
			at:        at,
		})
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	sample, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, false)
	require.True(t, ok)
	require.Equal(t, int64(6), sample.SampleCount, "both probe modes belong to one window")
	require.InDelta(t, 2_550.0, sample.AvgMs, 0.001)
}

func TestChannelLatencyStatsRefusesNonStreamingRealTraffic(t *testing.T) {
	// A non-streaming execution records no first-token metric at all, only its total
	// completion time. For a probe those are nearly the same number; for real traffic
	// they are not, because completion time grows with the length of the answer. Left
	// in, a "maximum first-token latency" ceiling would exclude a channel for being
	// verbose -- so such a row is not a sample, whatever the scope.
	//
	// The streaming rows here are fast and the non-streaming ones slow, so a window
	// that admitted both would average 2550ms instead of 100ms.
	svc, client, ctx := newLatencyStatsTestService(t, "nonstreaming-traffic")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-5 * time.Minute)
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

	sample, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, true)
	require.True(t, ok)
	require.Equal(t, int64(3), sample.SampleCount, "only the rows that measured a first token count")
	require.InDelta(t, 100.0, sample.AvgMs, 0.001)
}

func TestChannelLatencyStatsExcludesFailedAndUnmeasuredExecutions(t *testing.T) {
	// A failed attempt says nothing about latency, and a completed attempt with no
	// metric must stay unknown rather than enter the window as a fast sample.
	svc, client, ctx := newLatencyStatsTestService(t, "eligibility")
	ch := newLatencyStatsChannel(t, ctx, client, "c1", channel.StatusEnabled)

	recent := time.Now().UTC().Add(-5 * time.Minute)
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

	sample, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, true)
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

	_, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, true)
	require.False(t, ok, "samples older than the lookback must not be known at all")

	recent := time.Now().UTC().Add(-time.Minute)
	for i := range 3 {
		seedLatencySample(t, ctx, client, ch.ID,
			streamingSample(request.SourceAPI, 3_000, recent.Add(time.Duration(i)*time.Second)))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	sample, ok := svc.ChannelLatencyStats(ch.ID, testLatencyModel, true)
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

	recent := time.Now().UTC().Add(-5 * time.Minute)
	for i := range 3 {
		at := recent.Add(time.Duration(i) * time.Minute)
		seedLatencySample(t, ctx, client, enabled.ID, streamingSample(request.SourceAPI, 100, at))
		seedLatencySample(t, ctx, client, disabled.ID, streamingSample(request.SourceAPI, 100, at))
	}

	require.NoError(t, svc.RefreshChannelLatencyStats(ctx))

	_, ok := svc.ChannelLatencyStats(enabled.ID, testLatencyModel, true)
	require.True(t, ok)

	_, ok = svc.ChannelLatencyStats(disabled.ID, testLatencyModel, true)
	require.False(t, ok)
}

func TestChannelLatencyStatsUnknownBeforeFirstRefresh(t *testing.T) {
	// Before any snapshot exists every channel must read as unknown, so a ceiling
	// filters nothing rather than filtering everything.
	svc, _, _ := newLatencyStatsTestService(t, "cold")

	_, ok := svc.ChannelLatencyStats(1, testLatencyModel, false)
	require.False(t, ok)

	computedAt, lookback := svc.ChannelLatencyStatsComputedAt()
	require.True(t, computedAt.IsZero())
	require.Zero(t, lookback)
}

// The routing ceiling's window must come from GateWindowMinutes ALONE. Sharing the
// dashboard's P95 window is the original defect: it made a gate that is supposed to
// answer "is this channel fast right now" average over a whole day, so a slowdown
// that started minutes ago was diluted by hundreds of older samples and the gate
// reacted slowest at exactly the moment it exists for.
func TestChannelLatencyStatsLookbackIsIndependentOfTheDashboardP95Window(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel-latency-window-source?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))
	svc := newTestChannelService(client)

	// A deliberately extreme P95 window beside a short gate window: if the two are ever
	// coupled again, the assertion below reads 720h instead of 20 minutes.
	require.NoError(t, svc.SystemService.UpdateChannelSetting(ctx, UpdateSystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{
			Enabled:             true,
			IntervalMinutes:     5,
			AcceptableLatencyMs: 60_000,
			ExtraChannels:       1,
			P95LookbackHours:    720,
			GateWindowMinutes:   20,
			Models:              []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}},
		},
	}))

	require.Equal(t, 20*time.Minute, svc.channelLatencyStatsLookback(ctx))
}

// A stored policy written before gate_window_minutes existed carries no such key, so
// the zero value must fall back to the default rather than collapse the window to
// nothing (which would leave every channel unknown and the ceiling inert).
func TestChannelLatencyStatsLookbackFallsBackWhenTheGateWindowIsUnset(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel-latency-window-unset?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))
	svc := newTestChannelService(client)

	require.Equal(t, channelLatencyStatsDefaultLookback, svc.channelLatencyStatsLookback(ctx))
	require.Equal(t,
		time.Duration(defaultActiveHealthProbeScanSetting.GateWindowMinutes)*time.Minute,
		channelLatencyStatsDefaultLookback,
		"the nil-SystemService fallback and the configured default must agree",
	)
}
