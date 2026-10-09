package gql

import (
	"context"
	"fmt"
	"testing"

	"github.com/99designs/gqlgen/client"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestUpdateChannelQuotaOmittedVersusExplicitNull(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer db.Close()

	ctx := authz.WithTestBypass(context.Background())
	ch, err := db.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Channel quota").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetSettings(&objects.ChannelSettings{
			Quota: &objects.APIKeyQuota{
				Requests: lo.ToPtr(int64(100)),
				Period:   objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	channelService := biz.NewChannelServiceForTest(db)
	defer channelService.Stop()
	handler := NewGraphqlHandlers(Dependencies{
		Ent:            db,
		ChannelService: channelService,
	})
	graphqlClient := client.New(handler.Graphql, func(request *client.Request) {
		request.HTTP = request.HTTP.WithContext(authz.WithTestBypass(request.HTTP.Context()))
	})

	mutation := `
		mutation UpdateChannel($id: ID!, $input: UpdateChannelInput!) {
			updateChannel(id: $id, input: $input) {
				id
			}
		}
	`
	channelID := fmt.Sprintf("gid://axonhub/%s/%d", ent.TypeChannel, ch.ID)

	t.Run("omitted quota keeps the stored quota", func(t *testing.T) {
		var response map[string]any
		err := graphqlClient.Post(mutation, &response,
			client.Var("id", channelID),
			client.Var("input", map[string]any{
				"settings": map[string]any{"extraModelPrefix": "partial-"},
			}),
		)
		require.NoError(t, err)

		stored, err := db.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		require.Equal(t, "partial-", stored.Settings.ExtraModelPrefix)
		require.NotNil(t, stored.Settings.Quota)
		require.Equal(t, int64(100), *stored.Settings.Quota.Requests)
	})

	t.Run("explicit null quota clears the stored quota", func(t *testing.T) {
		var response map[string]any
		err := graphqlClient.Post(mutation, &response,
			client.Var("id", channelID),
			client.Var("input", map[string]any{
				"settings": map[string]any{"extraModelPrefix": "partial-", "quota": nil},
			}),
		)
		require.NoError(t, err)

		stored, err := db.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		require.Nil(t, stored.Settings.Quota)
	})

	t.Run("inline literal arguments follow the same rule", func(t *testing.T) {
		_, err := db.Channel.UpdateOneID(ch.ID).SetSettings(&objects.ChannelSettings{
			Quota: &objects.APIKeyQuota{
				Requests: lo.ToPtr(int64(100)),
				Period:   objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
			},
		}).Save(ctx)
		require.NoError(t, err)

		var response map[string]any
		err = graphqlClient.Post(fmt.Sprintf(`mutation { updateChannel(id: %q, input: {settings: {extraModelPrefix: "inline-"}}) { id } }`, channelID), &response)
		require.NoError(t, err)

		stored, err := db.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		require.Equal(t, "inline-", stored.Settings.ExtraModelPrefix)
		require.NotNil(t, stored.Settings.Quota)

		err = graphqlClient.Post(fmt.Sprintf(`mutation { updateChannel(id: %q, input: {settings: {quota: null}}) { id } }`, channelID), &response)
		require.NoError(t, err)

		stored, err = db.Channel.Get(ctx, ch.ID)
		require.NoError(t, err)
		require.Nil(t, stored.Settings.Quota)
	})
}
