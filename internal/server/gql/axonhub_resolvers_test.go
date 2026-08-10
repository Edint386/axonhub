package gql

import (
	"context"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func setupTestQueryResolver(t *testing.T) (*queryResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	resolver := &queryResolver{&Resolver{client: client}}

	return resolver, ctx, client
}

func TestQueryResolver_ChannelQuotaUsage(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	quota := &objects.APIKeyQuota{
		Requests: lo.ToPtr(int64(100)),
		Period: objects.APIKeyQuotaPeriod{
			Type: objects.APIKeyQuotaPeriodTypeAllTime,
		},
	}
	channelEntity, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Quota Channel").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpt-4o-mini"}).
		SetDefaultTestModel("gpt-4o-mini").
		SetStatus(channel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{Quota: quota}).
		Save(ctx)
	require.NoError(t, err)

	channelService := biz.NewChannelServiceForTest(client)
	defer channelService.Stop()
	quotaService := biz.NewQuotaService(
		client,
		biz.NewSystemService(biz.SystemServiceParams{Ent: client}),
	)
	resolver := &queryResolver{&Resolver{
		client:         client,
		channelService: channelService,
		quotaService:   quotaService,
	}}
	channelID := objects.GUID{Type: ent.TypeChannel, ID: channelEntity.ID}

	usage, err := resolver.ChannelQuotaUsage(ctx, channelID)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, channelID, usage.ChannelID)
	require.Equal(t, quota, usage.Quota)
	require.NotNil(t, usage.Window)
	require.Nil(t, usage.Window.Start)
	require.NotNil(t, usage.Window.End)
	require.NotNil(t, usage.Usage)
	require.Zero(t, usage.Usage.RequestCount)
	require.Zero(t, usage.Usage.TotalTokens)
	require.True(t, usage.Usage.TotalCost.IsZero())

	unconfiguredChannel, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Unconfigured Channel").
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpt-4o-mini"}).
		SetDefaultTestModel("gpt-4o-mini").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	usage, err = resolver.ChannelQuotaUsage(ctx, objects.GUID{
		Type: ent.TypeChannel,
		ID:   unconfiguredChannel.ID,
	})
	require.NoError(t, err)
	require.Nil(t, usage)
}

func TestQueryResolver_AllChannelSummarys_ProjectProfileUsesIntersection(t *testing.T) {
	resolver, ctx, client := setupTestQueryResolver(t)
	defer client.Close()

	idOnlyChannel, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("ID Only").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-1"}).
		SetSupportedModels([]string{"id-only-model"}).
		SetDefaultTestModel("id-only-model").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	matchingChannel, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Matching").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-2"}).
		SetSupportedModels([]string{"matching-model"}).
		SetDefaultTestModel("matching-model").
		SetStatus(channel.StatusEnabled).
		SetPriority(10).
		SetTags([]string{"allowed"}).
		Save(ctx)
	require.NoError(t, err)

	highPriorityChannel, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("High Priority Matching").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-3"}).
		SetSupportedModels([]string{"matching-model-high"}).
		SetDefaultTestModel("matching-model-high").
		SetStatus(channel.StatusEnabled).
		SetPriority(100).
		SetTags([]string{"allowed"}).
		Save(ctx)
	require.NoError(t, err)

	projectEntity, err := client.Project.Create().
		SetName("Project A").
		SetDescription("test project").
		SetProfiles(&objects.ProjectProfiles{
			ActiveProfile: "production",
			Profiles: []objects.ProjectProfile{
				{
					Name:        "production",
					ChannelIDs:  []int{idOnlyChannel.ID, matchingChannel.ID, highPriorityChannel.ID},
					ChannelTags: []string{"allowed"},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	projectCtx := contexts.WithProjectID(ctx, projectEntity.ID)

	channels, err := resolver.AllChannelSummarys(projectCtx, nil)
	require.NoError(t, err)
	require.Len(t, channels, 2)
	require.Equal(t, highPriorityChannel.ID, channels[0].ID)
	require.Equal(t, matchingChannel.ID, channels[1].ID)
}

func TestQueryResolver_AllChannelSummarys_RequiresChannelReadScopeWithoutProject(t *testing.T) {
	resolver, ctx, client := setupTestQueryResolver(t)
	defer client.Close()

	_, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Protected").
		SetCredentials(objects.ChannelCredentials{APIKey: "key"}).
		SetSupportedModels([]string{"protected-model"}).
		SetDefaultTestModel("protected-model").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	unauthorizedCtx := ent.NewContext(context.Background(), client)
	_, err = resolver.AllChannelSummarys(unauthorizedCtx, nil)
	require.Error(t, err)
}

func TestQueryResolver_AllChannelTags_ProjectProfileFiltersVisibleTags(t *testing.T) {
	resolver, ctx, client := setupTestQueryResolver(t)
	defer client.Close()

	_, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Visible Channel").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-visible"}).
		SetSupportedModels([]string{"visible-model"}).
		SetDefaultTestModel("visible-model").
		SetStatus(channel.StatusEnabled).
		SetTags([]string{"shared", "visible"}).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Hidden Channel").
		SetCredentials(objects.ChannelCredentials{APIKey: "key-hidden"}).
		SetSupportedModels([]string{"hidden-model"}).
		SetDefaultTestModel("hidden-model").
		SetStatus(channel.StatusEnabled).
		SetTags([]string{"shared", "hidden"}).
		Save(ctx)
	require.NoError(t, err)

	projectEntity, err := client.Project.Create().
		SetName("Project B").
		SetDescription("test project").
		SetProfiles(&objects.ProjectProfiles{
			ActiveProfile: "production",
			Profiles: []objects.ProjectProfile{
				{
					Name:        "production",
					ChannelTags: []string{"visible"},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	projectCtx := contexts.WithProjectID(ctx, projectEntity.ID)

	tags, err := resolver.AllChannelTags(projectCtx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"shared", "visible"}, lo.Uniq(tags))
}
