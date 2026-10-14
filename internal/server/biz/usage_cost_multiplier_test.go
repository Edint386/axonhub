package biz

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

// TestUsageCost_ModelLibraryFallbackAppliesChannelMultiplier covers the custom
// channel price multiplier (XSN-11) on the upstream model library fallback: the
// fallback price is the channel's base price, so the multiplier scales it too.
func TestUsageCost_ModelLibraryFallbackAppliesChannelMultiplier(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	createModelLibraryPriceEntry(t, ctx, client, "m-fallback-multiplied", model.StatusEnabled, objects.ModelCardCost{
		Input:  1,
		Output: 2,
	})

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenaiFake).
		SetName("fallback-multiplied").
		SetSupportedModels([]string{"m-fallback-multiplied"}).
		SetDefaultTestModel("m-fallback-multiplied").
		SetModelPriceMultiplier(0.5).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{}).
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{Ent: client})
	channelService := NewChannelServiceForTest(client)
	built, err := channelService.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	channelService.SetEnabledChannelsForTest([]*Channel{built})

	usageLogService := NewUsageLogService(client, systemService, channelService)
	ul, err := usageLogService.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     1,
		ProjectID:     1,
		ChannelID:     ch.ID,
		ActualModelID: "m-fallback-multiplied",
		Usage: &llm.Usage{
			PromptTokens:     100,
			CompletionTokens: 200,
			TotalTokens:      300,
		},
		Source: "api",
		Format: "openai/chat_completions",
	})
	require.NoError(t, err)

	// Unmultiplied: (100/1e6)*1 + (200/1e6)*2 = 0.0005; the 0.5 multiplier halves it.
	require.NotNil(t, ul.TotalCost)
	require.InDelta(t, 0.00025, *ul.TotalCost, 1e-12)
	require.Len(t, ul.CostItems, 2)
	require.Equal(t, modelLibraryPriceReferenceID("m-fallback-multiplied"), ul.CostPriceReferenceID)
}

// TestUsageCost_ZeroChannelMultiplierSurvivesChannelBuild covers a channel whose
// stored price multiplier is 0 (a free channel). Building the channel from the
// database must keep 0 rather than falling back to the default of 1, on both
// the explicit channel price and the model library fallback.
func TestUsageCost_ZeroChannelMultiplierSurvivesChannelBuild(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	createModelLibraryPriceEntry(t, ctx, client, "m-zero-fallback", model.StatusEnabled, objects.ModelCardCost{
		Input:  1,
		Output: 2,
	})

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenaiFake).
		SetName("zero-multiplier").
		SetSupportedModels([]string{"m-zero-priced", "m-zero-fallback"}).
		SetDefaultTestModel("m-zero-priced").
		SetModelPriceMultiplier(0).
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{}).
		Save(ctx)
	require.NoError(t, err)

	unitPrice := decimal.NewFromInt(2)
	_, err = client.ChannelModelPrice.Create().
		SetChannelID(ch.ID).
		SetModelID("m-zero-priced").
		SetPrice(objects.ModelPrice{Items: []objects.ModelPriceItem{{
			ItemCode: objects.PriceItemCodeUsage,
			Pricing:  objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &unitPrice},
		}}}).
		SetReferenceID("zero-ref").
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{Ent: client})
	channelService := NewChannelServiceForTest(client)
	built, err := channelService.GetChannel(ctx, ch.ID)
	require.NoError(t, err)
	require.Zero(t, built.ModelPriceMultiplier())
	channelService.preloadModelPrices(ctx, built)
	channelService.SetEnabledChannelsForTest([]*Channel{built})

	usageLogService := NewUsageLogService(client, systemService, channelService)
	for _, modelID := range []string{"m-zero-priced", "m-zero-fallback"} {
		ul, err := usageLogService.CreateUsageLog(ctx, CreateUsageLogParams{
			RequestID:     1,
			ProjectID:     1,
			ChannelID:     ch.ID,
			ActualModelID: modelID,
			Usage: &llm.Usage{
				PromptTokens:     1_000_000,
				CompletionTokens: 1_000_000,
				TotalTokens:      2_000_000,
			},
			Source: "api",
			Format: "openai/chat_completions",
		})
		require.NoError(t, err)
		require.NotNil(t, ul.TotalCost, modelID)
		require.Zero(t, *ul.TotalCost, modelID)
	}
}
