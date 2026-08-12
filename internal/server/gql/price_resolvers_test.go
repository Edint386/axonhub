package gql

import (
	"context"
	"fmt"
	"testing"

	"github.com/99designs/gqlgen/client"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestSaveChannelModelPricesPersistsIndependentMultiplier(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer db.Close()

	ctx := authz.WithTestBypass(context.Background())
	ch, err := db.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("Price multiplier channel").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
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
		mutation SavePrices($channelId: ID!, $multiplier: Float!, $input: [SaveChannelModelPriceInput!]!) {
			saveChannelModelPrices(channelId: $channelId, multiplier: $multiplier, input: $input) {
				modelID
			}
		}
	`
	input := []map[string]any{
		{
			"modelId": "gpt-4",
			"price": map[string]any{
				"items": []map[string]any{
					{
						"itemCode": "prompt_tokens",
						"pricing": map[string]any{
							"mode":         "usage_per_unit",
							"usagePerUnit": "2",
						},
					},
				},
			},
		},
	}
	var response struct {
		SaveChannelModelPrices []struct {
			ModelID string
		}
	}
	err = graphqlClient.Post(
		mutation,
		&response,
		client.Var("channelId", fmt.Sprintf("gid://axonhub/%s/%d", ent.TypeChannel, ch.ID)),
		client.Var("multiplier", 1.5),
		client.Var("input", input),
	)
	require.NoError(t, err)
	require.Len(t, response.SaveChannelModelPrices, 1)
	require.Equal(t, "gpt-4", response.SaveChannelModelPrices[0].ModelID)

	persistedChannel, err := db.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, 1.5, persistedChannel.ModelPriceMultiplier)
	persistedPrice, err := persistedChannel.QueryChannelModelPrices().Only(ctx)
	require.NoError(t, err)
	require.Equal(t, "2", persistedPrice.Price.Items[0].Pricing.UsagePerUnit.String())
}
