package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelhealthproberun"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
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
