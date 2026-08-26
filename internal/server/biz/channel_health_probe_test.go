package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelhealthproberun"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestNormalizeAndValidateChannelHealthProbeSettings(t *testing.T) {
	// Per-channel settings now carry only the probe opt-in; the cadence moved to
	// the global policy. An unset opt-in normalizes to enabled.
	settings := &objects.ChannelHealthProbeSettings{}
	require.NoError(t, normalizeAndValidateChannelHealthProbeSettings(settings))
	require.NotNil(t, settings.ProbeEnabled)
	require.True(t, *settings.ProbeEnabled)

	require.Error(t, normalizeAndValidateChannelHealthProbeSettings(nil))
}

func TestChannelHealthProbeService_ClaimScheduledRunIsUnique(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-claim?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "scheduled", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})
	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	policy := ActiveHealthProbeScanSetting{Enabled: true, Models: []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}}}
	targets, err := svc.DueTargetsWithPolicy(ctx, time.Date(2026, 8, 12, 2, 30, 10, 0, time.UTC), policy)
	require.NoError(t, err)
	require.Len(t, targets, 1)

	first, claimed, err := svc.ClaimScheduledRun(ctx, targets[0], time.Now().UTC())
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotNil(t, first)
	second, claimed, err := svc.ClaimScheduledRun(ctx, targets[0], time.Now().UTC())
	require.NoError(t, err)
	require.False(t, claimed)
	require.Nil(t, second)
	require.Equal(t, 1, client.ChannelHealthProbeRun.Query().Where(channelhealthproberun.ChannelIDEQ(ch.ID)).CountX(ctx))
}

func TestChannelHealthProbeService_DueTargetsIgnoreRecentActivityAndBucketByGlobalInterval(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-activity?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "activity", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})
	policy := ActiveHealthProbeScanSetting{Enabled: true, IntervalMinutes: 7, Models: []ActiveHealthProbeModelSetting{
		{ModelID: "gpt-4", Enabled: true},
		{ModelID: "gpt-3.5", Enabled: true},
	}}
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	// Real traffic and a just-finished probe used to drop a channel out of the scan
	// for one interval. That is exactly what rotated the head of the priority chain
	// and defeated the extra-channel limit, so neither may filter anything now.
	createHealthProbeTestRequest(t, ctx, client, ch.ID, "gpt-4", request.SourceAPI, base)
	_, err := client.ChannelHealthProbeRun.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-3.5").
		SetSource(channelhealthproberun.SourceManual).
		SetStatus(channelhealthproberun.StatusHealthy).
		SetStream(false).
		SetStartedAt(base).
		SetCreatedAt(base).
		SetTotalMs(10).
		Save(ctx)
	require.NoError(t, err)

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	targets, err := svc.DueTargetsWithPolicy(ctx, base.Add(time.Second), policy)
	require.NoError(t, err)
	require.Len(t, targets, 2, "both models stay in the scan despite activity a second ago")

	// Once-per-interval comes from the schedule bucket instead: two times inside the
	// same bucket produce the same ScheduleKey, whose unique constraint is what stops
	// the second pass from claiming anything.
	keysAt := func(at time.Time) []string {
		list, err := svc.DueTargetsWithPolicy(ctx, at, policy)
		require.NoError(t, err)
		keys := make([]string, 0, len(list))
		for _, target := range list {
			keys = append(keys, target.ScheduleKey)
		}

		return keys
	}
	bucketStart := base.Truncate(7 * time.Minute)
	require.Equal(t, keysAt(bucketStart), keysAt(bucketStart.Add(6*time.Minute)))
	require.NotEqual(t, keysAt(bucketStart), keysAt(bucketStart.Add(7*time.Minute)))
}

func TestChannelHealthProbeService_OrdersOverviewAndDueTargetsByPriority(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-priority?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	settings := &objects.ChannelHealthProbeSettings{}
	low := createHealthProbeTestChannel(t, ctx, client, "low", channel.StatusEnabled, settings)
	high := createHealthProbeTestChannel(t, ctx, client, "high", channel.StatusEnabled, settings)
	client.Channel.UpdateOneID(low.ID).SetPriority(10).SetOrderingWeight(100).ExecX(ctx)
	client.Channel.UpdateOneID(high.ID).SetPriority(20).SetOrderingWeight(1).ExecX(ctx)

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	policy := ActiveHealthProbeScanSetting{Enabled: true, Models: []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}}}
	overview, err := svc.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, []int{high.ID, low.ID}, []int{overview[0].ChannelID.ID, overview[1].ChannelID.ID})
	require.Equal(t, []int{20, 10}, []int{overview[0].Priority, overview[1].Priority})

	targets, err := svc.DueTargetsWithPolicy(ctx, time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC), policy)
	require.NoError(t, err)
	require.Equal(t, []int{high.ID, low.ID}, []int{targets[0].ChannelID, targets[1].ChannelID})
}

func TestChannelHealthProbeService_SkippedRunKeepsLatestHealthAndBlocksItsBucket(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-skipped?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "skipped", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})
	policy := ActiveHealthProbeScanSetting{Enabled: true, IntervalMinutes: 10, Models: []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}}}
	systemService, systemClient := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer systemClient.Close()
	require.NoError(t, systemService.UpdateChannelSetting(ctx, UpdateSystemChannelSettings{
		ActiveHealthProbeScan: &policy,
	}))
	base := time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC)
	client.ChannelHealthProbeRun.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetSource(channelhealthproberun.SourceScheduled).
		SetStatus(channelhealthproberun.StatusHealthy).
		SetStream(true).
		SetStartedAt(base).
		SetCompletedAt(base).
		SetTotalMs(50).
		ExecX(ctx)

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}, systemService: systemService}
	targets, err := svc.DueTargetsWithPolicy(ctx, base.Add(5*time.Minute), policy)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	target := targets[0]

	require.NoError(t, svc.SkipScheduledTargets(ctx, []ChannelHealthProbeTarget{target}, base.Add(5*time.Minute)))

	// A skip placeholder must not become the channel's reported health.
	overview, err := svc.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, channelhealthproberun.StatusHealthy.String(), overview[0].Models[0].LatestRun.Status)

	// The target stays in the scan, but its bucket is already taken, so a second pass
	// cannot claim it. That is what makes one scan per interval hold now.
	stillListed, err := svc.DueTargetsWithPolicy(ctx, base.Add(6*time.Minute), policy)
	require.NoError(t, err)
	require.Len(t, stillListed, 1)
	require.Equal(t, target.ScheduleKey, stillListed[0].ScheduleKey)

	_, claimed, err := svc.ClaimScheduledRun(ctx, target, base.Add(6*time.Minute))
	require.NoError(t, err)
	require.False(t, claimed, "the bucket already holds a record for this channel/model")

	// The next bucket is a different key and can be claimed again.
	nextBucket, err := svc.DueTargetsWithPolicy(ctx, base.Add(15*time.Minute), policy)
	require.NoError(t, err)
	require.Len(t, nextBucket, 1)
	require.NotEqual(t, target.ScheduleKey, nextBucket[0].ScheduleKey)
	_, claimed, err = svc.ClaimScheduledRun(ctx, nextBucket[0], base.Add(15*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestChannelHealthProbeService_PolicyUsesStrictestEnabledAPIKeyLimit(t *testing.T) {
	systemService, client := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(t.Context(), client))

	projectEntity := client.Project.Create().
		SetName("health-probe-policy").
		SetStatus(project.StatusActive).
		SaveX(ctx)
	for index, testCase := range []struct {
		status  apikey.Status
		latency int64
	}{
		{status: apikey.StatusEnabled, latency: 30_000},
		{status: apikey.StatusEnabled, latency: 20_000},
		{status: apikey.StatusDisabled, latency: 10_000},
	} {
		latency := testCase.latency
		client.APIKey.Create().
			SetName(fmt.Sprintf("health-probe-policy-%d", index)).
			SetKey(fmt.Sprintf("ah-health-probe-policy-%d", index)).
			SetProjectID(projectEntity.ID).
			SetStatus(testCase.status).
			SetProfiles(&objects.APIKeyProfiles{
				ActiveProfile: "default",
				Profiles: []objects.APIKeyProfile{{
					Name:                   "default",
					MaxFirstTokenLatencyMs: &latency,
				}},
			}).
			ExecX(ctx)
	}

	svc := &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		systemService:   systemService,
	}
	updated, err := svc.UpdatePolicy(ctx, UpdateChannelHealthProbePolicyInput{
		Enabled:             true,
		AcceptableLatencyMs: 60_000,
		ExtraChannels:       2,
		P95LookbackHours:    24,
	})
	require.NoError(t, err)
	require.True(t, updated.Enabled)
	require.Equal(t, 60_000, updated.AcceptableLatencyMs)
	require.Equal(t, 2, updated.ExtraChannels)
	require.NotNil(t, updated.APIKeyMaxFirstTokenLatencyMs)
	require.Equal(t, 20_000.0, *updated.APIKeyMaxFirstTokenLatencyMs)
}

func TestChannelHealthProbeService_HistoryPaginationAndFilters(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-history?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch := createHealthProbeTestChannel(t, ctx, client, "history", channel.StatusEnabled, nil)

	for i, modelID := range []string{"gpt-4", "gpt-4", "other"} {
		source := channelhealthproberun.SourceManual
		status := channelhealthproberun.StatusHealthy
		if i == 1 {
			source = channelhealthproberun.SourceScheduled
			status = channelhealthproberun.StatusUnhealthy
		}
		_, err := client.ChannelHealthProbeRun.Create().
			SetChannelID(ch.ID).
			SetModelID(modelID).
			SetSource(source).
			SetStatus(status).
			SetStream(i != 2).
			SetStartedAt(time.Now().Add(time.Duration(i) * time.Minute)).
			SetTotalMs(float64(i + 1)).
			Save(ctx)
		require.NoError(t, err)
	}

	modelID := "gpt-4"
	page, err := (&ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}).History(ctx, ChannelHealthProbeHistoryInput{
		ChannelID: &objects.GUID{Type: "Channel", ID: ch.ID},
		ModelID:   &modelID,
		Offset:    0,
		Limit:     1,
	})
	require.NoError(t, err)
	require.Equal(t, 2, page.TotalCount)
	require.Len(t, page.Items, 1)
	require.Equal(t, "gpt-4", page.Items[0].ModelID)

	status := "unhealthy"
	page, err = (&ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}).History(ctx, ChannelHealthProbeHistoryInput{
		Status: &status,
		Offset: 0,
		Limit:  50,
	})
	require.NoError(t, err)
	require.Equal(t, 1, page.TotalCount)
	require.Equal(t, channelhealthproberun.StatusUnhealthy.String(), page.Items[0].Status)
}

func TestChannelHealthProbeService_OverviewUsesLatestRunPerModel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-overview?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch := createHealthProbeTestChannel(t, ctx, client, "overview", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})
	// Overview reads the global model policy from SystemService. This test uses
	// the service-backed path so model rows are rendered from the same source as
	// production.
	systemService, systemClient := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer systemClient.Close()
	require.NoError(t, systemService.UpdateChannelSetting(ctx, UpdateSystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{
			Enabled: true,
			Models: []ActiveHealthProbeModelSetting{
				{ModelID: "gpt-4", Enabled: true},
				{ModelID: "gpt-3.5", Enabled: false},
			},
		},
	}))
	for i, modelID := range []string{"gpt-4", "gpt-4", "gpt-3.5"} {
		_, err := client.ChannelHealthProbeRun.Create().
			SetChannelID(ch.ID).
			SetModelID(modelID).
			SetSource(channelhealthproberun.SourceManual).
			SetStatus(channelhealthproberun.StatusHealthy).
			SetStream(true).
			SetStartedAt(time.Now().Add(time.Duration(i) * time.Minute)).
			SetTotalMs(float64(i + 1)).
			Save(ctx)
		require.NoError(t, err)
	}

	items, err := (&ChannelHealthProbeService{AbstractService: &AbstractService{db: client}, systemService: systemService}).Overview(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, items[0].Enabled)
	latestByModel := make(map[string]*ChannelHealthProbeRunRecord)
	for _, model := range items[0].Models {
		latestByModel[model.ModelID] = model.LatestRun
	}
	require.Equal(t, 2.0, latestByModel["gpt-4"].TotalMs)
	require.Equal(t, 3.0, latestByModel["gpt-3.5"].TotalMs)
}

func TestChannelHealthProbeFirstTokenMsUsesModeSpecificMetric(t *testing.T) {
	ttfb := 120.0
	ttft := 80.0
	value, ok := channelHealthProbeFirstTokenMs(&ChannelHealthProbeRunRecord{TTFBMs: &ttfb, TTFTMs: &ttft, Stream: true})
	require.True(t, ok)
	require.Equal(t, 80.0, value)

	value, ok = channelHealthProbeFirstTokenMs(&ChannelHealthProbeRunRecord{TTFBMs: &ttfb, TTFTMs: &ttft, Stream: false})
	require.True(t, ok)
	require.Equal(t, 120.0, value)

	zero := 0.0
	value, ok = channelHealthProbeFirstTokenMs(&ChannelHealthProbeRunRecord{TTFBMs: &zero})
	require.True(t, ok)
	require.Equal(t, 0.0, value)
}

func TestChannelHealthProbeDueTargetsUseGlobalModelSettings(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-global-models?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch := createHealthProbeTestChannel(t, ctx, client, "global-model", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	targets, err := svc.DueTargetsWithPolicy(ctx, time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC), ActiveHealthProbeScanSetting{
		Enabled: true,
		Models:  []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}},
	})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, ch.ID, targets[0].ChannelID)
	require.Equal(t, "gpt-4", targets[0].ModelID)
	require.False(t, targets[0].Stream)
}

func TestChannelHealthProbePrimaryModelFollowsGlobalOrder(t *testing.T) {
	globalModels := []ActiveHealthProbeModelSetting{
		{ModelID: "model-1", Enabled: true},
		{ModelID: "model-2", Enabled: true},
		{ModelID: "model-3", Enabled: true},
	}
	recentRuns := []*ChannelHealthProbeRunRecord{
		{ModelID: "model-1", StartedAt: time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)},
		{ModelID: "model-2", StartedAt: time.Date(2026, 8, 26, 2, 0, 0, 0, time.UTC)},
	}

	channelOne := &ent.Channel{
		ID:               1,
		Name:             "channel-1",
		Status:           channel.StatusEnabled,
		SupportedModels:  []string{"model-1", "model-2"},
		DefaultTestModel: "model-1",
	}
	channelTwo := &ent.Channel{
		ID:               2,
		Name:             "channel-2",
		Status:           channel.StatusEnabled,
		SupportedModels:  []string{"model-2", "model-3"},
		DefaultTestModel: "model-2",
	}

	first := buildChannelHealthProbeOverview(channelOne, nil, nil, recentRuns, globalModels, false)
	second := buildChannelHealthProbeOverview(channelTwo, nil, nil, recentRuns, globalModels, false)
	require.Equal(t, "model-1", *first.PrimaryModelID)
	require.Equal(t, []string{"model-1", "model-2"}, modelOverviewIDs(first.Models))
	require.Equal(t, []string{"model-1"}, runModelIDs(first.RecentRuns))
	require.Equal(t, "model-2", *second.PrimaryModelID)
	require.Equal(t, []string{"model-2", "model-3"}, modelOverviewIDs(second.Models))
	require.Equal(t, []string{"model-2"}, runModelIDs(second.RecentRuns))

	globalModels[0].Enabled = false
	disabledFirst := buildChannelHealthProbeOverview(channelOne, nil, nil, recentRuns, globalModels, false)
	require.Equal(t, "model-2", *disabledFirst.PrimaryModelID)
}

func modelOverviewIDs(models []*ChannelHealthProbeModelOverview) []string {
	result := make([]string, 0, len(models))
	for _, model := range models {
		result = append(result, model.ModelID)
	}
	return result
}

func runModelIDs(runs []*ChannelHealthProbeRunRecord) []string {
	result := make([]string, 0, len(runs))
	for _, run := range runs {
		result = append(result, run.ModelID)
	}
	return result
}

func TestChannelHealthProbeOverviewIncludesP95AndLatestFirstToken(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-metrics?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch := createHealthProbeTestChannel(t, ctx, client, "metrics", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})
	systemService, systemClient := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer systemClient.Close()
	require.NoError(t, systemService.UpdateChannelSetting(ctx, UpdateSystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{
			Enabled:          true,
			P95LookbackHours: 24,
			Models:           []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}, {ModelID: "other", Enabled: true}, {ModelID: "gpt-3.5", Enabled: true}},
		},
	}))
	base := time.Now().UTC().Add(-5 * time.Minute)
	for index, latency := range []float64{100, 200, 300, 400, 500} {
		startedAt := base.Add(time.Duration(index) * time.Second)
		_, err := client.ChannelHealthProbeRun.Create().
			SetChannelID(ch.ID).
			SetModelID("gpt-4").
			SetSource(channelhealthproberun.SourceScheduled).
			SetStatus(channelhealthproberun.StatusHealthy).
			SetStream(false).
			SetTtfbMs(latency).
			SetStartedAt(startedAt).
			SetCompletedAt(startedAt.Add(time.Millisecond * time.Duration(latency))).
			SetTotalMs(latency + 20).
			Save(ctx)
		require.NoError(t, err)
	}

	overview, err := (&ChannelHealthProbeService{AbstractService: &AbstractService{db: client}, systemService: systemService}).Overview(ctx)
	require.NoError(t, err)
	require.Len(t, overview, 1)
	require.Len(t, overview[0].Models, 3)
	var model *ChannelHealthProbeModelOverview
	for _, item := range overview[0].Models {
		if item.ModelID == "gpt-4" {
			model = item
			break
		}
	}
	require.NotNil(t, model)
	require.NotNil(t, model.FirstTokenMs)
	require.Equal(t, 500.0, *model.FirstTokenMs)
	require.NotNil(t, model.P95Ms)
	require.Equal(t, 500.0, *model.P95Ms)
	require.Equal(t, 5, model.SampleCount)
	require.NotNil(t, model.LastProbedAt)
}

func TestChannelHealthProbeOverviewUsesConfiguredP95WindowAndPerChannelRecentLimit(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-window?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	systemService, systemClient := setupTestSystemService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer systemClient.Close()
	require.NoError(t, systemService.UpdateChannelSetting(ctx, UpdateSystemChannelSettings{
		ActiveHealthProbeScan: &ActiveHealthProbeScanSetting{
			Enabled:          true,
			P95LookbackHours: 1,
			Models:           []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}},
		},
	}))

	inside := createHealthProbeTestChannel(t, ctx, client, "inside", channel.StatusEnabled, nil)
	outside := createHealthProbeTestChannel(t, ctx, client, "outside", channel.StatusEnabled, nil)
	base := time.Now().UTC()
	for index := 0; index < 16; index++ {
		startedAt := base.Add(-time.Duration(index) * time.Minute)
		if index == 15 {
			startedAt = base.Add(-2 * time.Hour)
		}
		for _, channelID := range []int{inside.ID, outside.ID} {
			builder := client.ChannelHealthProbeRun.Create().
				SetChannelID(channelID).
				SetModelID("gpt-4").
				SetSource(channelhealthproberun.SourceScheduled).
				SetStatus(channelhealthproberun.StatusHealthy).
				SetStream(false).
				SetTtfbMs(float64(index + 1)).
				SetStartedAt(startedAt).
				SetCompletedAt(startedAt.Add(time.Second)).
				SetTotalMs(float64(index + 2))
			if index == 15 {
				builder = builder.SetTtfbMs(10_000)
			}
			builder.SaveX(ctx)
		}
	}

	// The oldest sample is outside the one-hour window and must not affect P95.
	// The most recent 15 runs are independently retained for both channels.
	overview, err := (&ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		systemService:   systemService,
	}).Overview(ctx)
	require.NoError(t, err)
	require.Len(t, overview, 2)
	for _, item := range overview {
		require.Len(t, item.RecentRuns, channelHealthProbeRecentRunsLimit)
		require.True(t, item.RecentRuns[0].StartedAt.Before(item.RecentRuns[len(item.RecentRuns)-1].StartedAt))
		require.Len(t, item.Models, 1)
		require.NotNil(t, item.Models[0].P95Ms)
		require.Equal(t, 15, item.Models[0].SampleCount)
		require.Less(t, *item.Models[0].P95Ms, 10_000.0)
	}
}

func TestChannelHealthProbeMetricsCacheStoresFinalValuesAndInvalidatesOnCompletion(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-metrics-cache?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch := createHealthProbeTestChannel(t, ctx, client, "metrics-cache", channel.StatusEnabled, nil)
	base := time.Now().UTC()
	client.ChannelHealthProbeRun.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetSource(channelhealthproberun.SourceScheduled).
		SetStatus(channelhealthproberun.StatusHealthy).
		SetStream(false).
		SetTtfbMs(100).
		SetStartedAt(base.Add(-time.Minute)).
		SetCompletedAt(base.Add(-time.Minute + time.Second)).
		SetTotalMs(100).
		SaveX(ctx)

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	metrics, err := svc.metricsByChannelAndModel(ctx, []int{ch.ID}, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 100.0, *metrics[channelHealthProbeModelKey(ch.ID, "gpt-4")].P95Ms)

	pending := client.ChannelHealthProbeRun.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-4").
		SetSource(channelhealthproberun.SourceManual).
		SetStatus(channelhealthproberun.StatusPending).
		SetStream(false).
		SetStartedAt(base).
		SaveX(ctx)
	secondLatency := 200.0
	_, err = svc.CompleteRun(ctx, pending.ID, true, &secondLatency, nil, 200, nil, base.Add(time.Second))
	require.NoError(t, err)

	metrics, err = svc.metricsByChannelAndModel(ctx, []int{ch.ID}, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, 200.0, *metrics[channelHealthProbeModelKey(ch.ID, "gpt-4")].P95Ms)
}

func createHealthProbeTestChannel(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
	name string,
	status channel.Status,
	settings *objects.ChannelHealthProbeSettings,
) *ent.Channel {
	t.Helper()
	builder := client.Channel.Create().
		SetType(channel.TypeOpenaiFake).
		SetName(name).
		SetStatus(status).
		SetSupportedModels([]string{"gpt-4", "gpt-3.5", "other"}).
		SetDefaultTestModel("gpt-4").
		SetCredentials(objects.ChannelCredentials{})
	if settings != nil {
		builder.SetSettings(&objects.ChannelSettings{HealthProbe: settings})
	}
	return builder.SaveX(ctx)
}

func createHealthProbeTestRequest(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
	channelID int,
	modelID string,
	source request.Source,
	createdAt time.Time,
) *ent.Request {
	t.Helper()
	return client.Request.Create().
		SetChannelID(channelID).
		SetModelID(modelID).
		SetSource(source).
		SetStatus(request.StatusCompleted).
		SetStream(false).
		SetRequestBody([]byte("{}")).
		SetCreatedAt(createdAt).
		SetUpdatedAt(createdAt).
		SaveX(ctx)
}

func TestChannelHealthProbeSettings_NormalizeDefaultsProbeEnabledToTrue(t *testing.T) {
	// A settings blob persisted before probeEnabled existed contains only
	// intervalMinutes; it must normalize to enabled=true so existing channels
	// keep being probed.
	var raw = []byte(`{"intervalMinutes":9}`)
	var settings objects.ChannelHealthProbeSettings
	require.NoError(t, json.Unmarshal(raw, &settings))
	require.Nil(t, settings.ProbeEnabled)
	require.True(t, settings.IsProbeEnabled())

	settings.Normalize()
	require.NotNil(t, settings.ProbeEnabled)
	require.True(t, *settings.ProbeEnabled)
	require.True(t, settings.IsProbeEnabled())

	// A nil receiver is treated as enabled.
	var nilSettings *objects.ChannelHealthProbeSettings
	require.True(t, nilSettings.IsProbeEnabled())

	// An explicit false is honored.
	disabled := false
	off := objects.ChannelHealthProbeSettings{ProbeEnabled: &disabled}
	off.Normalize()
	require.False(t, off.IsProbeEnabled())
}

func TestChannelHealthProbeService_DueTargetsSkipsProbeDisabledChannel(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-optout?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	probeOn := true
	probeOff := false
	enabledCh := createHealthProbeTestChannel(t, ctx, client, "probe-on", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		ProbeEnabled: &probeOn,
	})
	optedOut := createHealthProbeTestChannel(t, ctx, client, "probe-off", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		ProbeEnabled: &probeOff,
	})

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	policy := ActiveHealthProbeScanSetting{Enabled: true, Models: []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true}}}
	targets, err := svc.DueTargetsWithPolicy(ctx, time.Date(2026, 8, 12, 2, 30, 10, 0, time.UTC), policy)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, enabledCh.ID, targets[0].ChannelID)

	// The opted-out channel must never appear as a scheduled target.
	for _, target := range targets {
		require.NotEqual(t, optedOut.ID, target.ChannelID)
	}

	// A manual probe on the opted-out channel must still work: the model is
	// supported, so CreateManualRun succeeds despite probeEnabled=false. It needs
	// a channelService to load the channel, so build one for this assertion.
	manualSvc := &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		channelService:  newTestChannelService(client),
	}
	run, err := manualSvc.CreateManualRun(ctx, RunChannelHealthProbeInput{
		ChannelID: objects.GUID{Type: "Channel", ID: optedOut.ID},
		ModelID:   "gpt-4",
		Stream:    true,
	}, time.Now().UTC())
	require.NoError(t, err)
	require.NotNil(t, run)
}

func TestChannelHealthProbeService_UpdateSettingsRoundTripsProbeEnabled(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-roundtrip?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	channelSvc := newTestChannelService(client)
	defer channelSvc.Stop()

	probeOn := true
	ch := createHealthProbeTestChannel(t, ctx, client, "roundtrip", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		ProbeEnabled: &probeOn,
	})

	probeSvc := &ChannelHealthProbeService{
		AbstractService: &AbstractService{db: client},
		channelService:  channelSvc,
	}

	overview, err := probeSvc.UpdateSettings(ctx, UpdateChannelHealthProbeSettingsInput{
		ChannelID:    objects.GUID{Type: "Channel", ID: ch.ID},
		ProbeEnabled: false,
	})
	require.NoError(t, err)
	require.False(t, overview.ProbeEnabled)

	// Persisted flag round-trips through the stored settings blob.
	reloaded := client.Channel.GetX(ctx, ch.ID)
	require.NotNil(t, reloaded.Settings)
	require.NotNil(t, reloaded.Settings.HealthProbe)
	require.NotNil(t, reloaded.Settings.HealthProbe.ProbeEnabled)
	require.False(t, *reloaded.Settings.HealthProbe.ProbeEnabled)

	// Turning it back on round-trips too.
	overview, err = probeSvc.UpdateSettings(ctx, UpdateChannelHealthProbeSettingsInput{
		ChannelID:    objects.GUID{Type: "Channel", ID: ch.ID},
		ProbeEnabled: true,
	})
	require.NoError(t, err)
	require.True(t, overview.ProbeEnabled)
}

func TestChannelHealthProbeService_ReapStalePendingRuns(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-reap?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "reap", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{})
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	newRun := func(status channelhealthproberun.Status, startedAt time.Time, key string) int {
		run := client.ChannelHealthProbeRun.Create().
			SetChannelID(ch.ID).
			SetModelID("gpt-4").
			SetSource(channelhealthproberun.SourceScheduled).
			SetStatus(status).
			SetStream(true).
			SetScheduleKey(key).
			SetStartedAt(startedAt).
			SetTotalMs(0).
			SaveX(ctx)

		return run.ID
	}

	// Abandoned: claimed well before the staleness window and never reported.
	stale := newRun(channelhealthproberun.StatusPending, now.Add(-ChannelHealthProbeStaleAfter-time.Minute), "reap:stale")
	// Still legitimately in flight -- a probe is allowed to take minutes.
	fresh := newRun(channelhealthproberun.StatusPending, now.Add(-time.Second), "reap:fresh")
	// Already finished; the sweep must not rewrite a real result.
	done := newRun(channelhealthproberun.StatusHealthy, now.Add(-time.Hour), "reap:done")

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	reaped, err := svc.ReapStalePendingRuns(ctx, now)
	require.NoError(t, err)
	require.Equal(t, 1, reaped)

	closed := client.ChannelHealthProbeRun.GetX(ctx, stale)
	require.Equal(t, channelhealthproberun.StatusUnhealthy, closed.Status)
	require.NotNil(t, closed.CompletedAt, "a closed run must carry a completion time")
	require.NotEmpty(t, closed.ErrorMessage, "the row must say why it was closed")

	require.Equal(t, channelhealthproberun.StatusPending, client.ChannelHealthProbeRun.GetX(ctx, fresh).Status)
	require.Equal(t, channelhealthproberun.StatusHealthy, client.ChannelHealthProbeRun.GetX(ctx, done).Status)

	// Idempotent: a second sweep finds nothing left to close.
	reaped, err = svc.ReapStalePendingRuns(ctx, now)
	require.NoError(t, err)
	require.Zero(t, reaped)
}
