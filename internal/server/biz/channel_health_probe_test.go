package biz

import (
	"context"
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
	settings := &objects.ChannelHealthProbeSettings{
		Enabled: true,
		Models:  []objects.ChannelHealthProbeModel{{ModelID: " gpt-4 "}, {ModelID: "gpt-4"}},
	}
	err := normalizeAndValidateChannelHealthProbeSettings(settings, func(modelID string) bool { return modelID == "gpt-4" })
	require.Error(t, err)

	settings = &objects.ChannelHealthProbeSettings{
		Enabled: true,
		Models:  []objects.ChannelHealthProbeModel{{ModelID: " gpt-4 ", Enabled: true, Stream: true}},
	}
	require.NoError(t, normalizeAndValidateChannelHealthProbeSettings(settings, func(modelID string) bool { return modelID == "gpt-4" }))
	require.Equal(t, objects.DefaultChannelHealthProbeIntervalMinutes, settings.IntervalMinutes)
	require.Equal(t, "gpt-4", settings.Models[0].ModelID)
}

func TestChannelHealthProbeService_ClaimScheduledRunIsUnique(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-claim?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "scheduled", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		Enabled:         true,
		IntervalMinutes: 5,
		Models:          []objects.ChannelHealthProbeModel{{ModelID: "gpt-4", Enabled: true, Stream: true}},
	})
	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	targets, err := svc.DueTargets(ctx, time.Date(2026, 8, 12, 2, 30, 10, 0, time.UTC))
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

func TestChannelHealthProbeService_DueTargetsUsesLatestActivityAndConfiguredInterval(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-activity?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "activity", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		Enabled:         true,
		IntervalMinutes: 7,
		Models: []objects.ChannelHealthProbeModel{
			{ModelID: "gpt-4", Enabled: true, Stream: true},
			{ModelID: "gpt-3.5", Enabled: true, Stream: false},
		},
	})
	base := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	createHealthProbeTestRequest(t, ctx, client, ch.ID, "gpt-4", request.SourceAPI, base)
	createHealthProbeTestRequest(t, ctx, client, ch.ID, "gpt-4", request.SourcePlayground, base.Add(4*time.Minute))
	// Test-channel traffic must not postpone a scheduled active probe.
	createHealthProbeTestRequest(t, ctx, client, ch.ID, "gpt-4", request.SourceTest, base.Add(9*time.Minute))
	_, err := client.ChannelHealthProbeRun.Create().
		SetChannelID(ch.ID).
		SetModelID("gpt-3.5").
		SetSource(channelhealthproberun.SourceManual).
		SetStatus(channelhealthproberun.StatusHealthy).
		SetStream(false).
		SetStartedAt(base.Add(3 * time.Minute)).
		SetCreatedAt(base.Add(3 * time.Minute)).
		SetTotalMs(10).
		Save(ctx)
	require.NoError(t, err)

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	targets, err := svc.DueTargets(ctx, base.Add(10*time.Minute-time.Second))
	require.NoError(t, err)
	require.Empty(t, targets)

	targets, err = svc.DueTargets(ctx, base.Add(10*time.Minute))
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, "gpt-3.5", targets[0].ModelID)

	targets, err = svc.DueTargets(ctx, base.Add(11*time.Minute))
	require.NoError(t, err)
	require.Len(t, targets, 2)
}

func TestChannelHealthProbeService_OrdersOverviewAndDueTargetsByPriority(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-priority?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	settings := &objects.ChannelHealthProbeSettings{
		Enabled:         true,
		IntervalMinutes: 10,
		Models:          []objects.ChannelHealthProbeModel{{ModelID: "gpt-4", Enabled: true, Stream: true}},
	}
	low := createHealthProbeTestChannel(t, ctx, client, "low", channel.StatusEnabled, settings)
	high := createHealthProbeTestChannel(t, ctx, client, "high", channel.StatusEnabled, settings)
	client.Channel.UpdateOneID(low.ID).SetPriority(10).SetOrderingWeight(100).ExecX(ctx)
	client.Channel.UpdateOneID(high.ID).SetPriority(20).SetOrderingWeight(1).ExecX(ctx)

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	overview, err := svc.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, []int{high.ID, low.ID}, []int{overview[0].ChannelID.ID, overview[1].ChannelID.ID})
	require.Equal(t, []int{20, 10}, []int{overview[0].Priority, overview[1].Priority})

	targets, err := svc.DueTargets(ctx, time.Date(2026, 8, 12, 14, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, []int{high.ID, low.ID}, []int{targets[0].ChannelID, targets[1].ChannelID})
}

func TestChannelHealthProbeService_SkippedRunDelaysScheduleWithoutReplacingLatestHealth(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-skipped?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	ch := createHealthProbeTestChannel(t, ctx, client, "skipped", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		Enabled:         true,
		IntervalMinutes: 10,
		Models:          []objects.ChannelHealthProbeModel{{ModelID: "gpt-4", Enabled: true, Stream: true}},
	})
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

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	target := ChannelHealthProbeTarget{
		ChannelID:       ch.ID,
		ModelID:         "gpt-4",
		Stream:          true,
		IntervalMinutes: 10,
		ScheduleKey:     "skipped:gpt-4:1",
	}
	require.NoError(t, svc.SkipScheduledTargets(ctx, []ChannelHealthProbeTarget{target}, base.Add(5*time.Minute)))

	overview, err := svc.Overview(ctx)
	require.NoError(t, err)
	require.Equal(t, channelhealthproberun.StatusHealthy.String(), overview[0].Models[0].LatestRun.Status)

	targets, err := svc.DueTargets(ctx, base.Add(14*time.Minute))
	require.NoError(t, err)
	require.Empty(t, targets)
	targets, err = svc.DueTargets(ctx, base.Add(15*time.Minute))
	require.NoError(t, err)
	require.Len(t, targets, 1)
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
	ch := createHealthProbeTestChannel(t, ctx, client, "overview", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		Enabled:         true,
		IntervalMinutes: 10,
		Models: []objects.ChannelHealthProbeModel{
			{ModelID: "gpt-4", Enabled: true, Stream: true},
			{ModelID: "gpt-3.5", Enabled: false, Stream: false},
		},
	})
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

	items, err := (&ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}).Overview(ctx)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, items[0].Enabled)
	require.Equal(t, 10, items[0].IntervalMinutes)
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
	ch := createHealthProbeTestChannel(t, ctx, client, "global-model", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		Enabled:         false,
		IntervalMinutes: 5,
		Models:          []objects.ChannelHealthProbeModel{{ModelID: "gpt-4", Enabled: false, Stream: true}},
	})

	svc := &ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}
	targets, err := svc.DueTargetsWithPolicy(ctx, time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC), ActiveHealthProbeScanSetting{
		Models: []ActiveHealthProbeModelSetting{{ModelID: "gpt-4", Enabled: true, Stream: false}},
	})
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, ch.ID, targets[0].ChannelID)
	require.Equal(t, "gpt-4", targets[0].ModelID)
	require.False(t, targets[0].Stream)
}

func TestChannelHealthProbeOverviewIncludesP95AndLatestFirstToken(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:health-probe-metrics?mode=memory&_fk=0")
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch := createHealthProbeTestChannel(t, ctx, client, "metrics", channel.StatusEnabled, &objects.ChannelHealthProbeSettings{
		Enabled:         true,
		IntervalMinutes: 5,
		Models:          []objects.ChannelHealthProbeModel{{ModelID: "gpt-4", Enabled: true, Stream: false}},
	})
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

	overview, err := (&ChannelHealthProbeService{AbstractService: &AbstractService{db: client}}).Overview(ctx)
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
