package biz

// Custom guard for realtime per-second billing (the duration_seconds price item,
// added for Bailian Paraformer). Its hooks live in upstream files, so these tests
// sit in a custom-only file: a merge that drops the duration_seconds case in
// computeUsageCostWithItems or its per-second scaling fails here.
// See CUSTOM_BRANCH_RULES.md section 5.

import (
	"context"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm"
)

func TestComputeUsageCost_DurationSeconds(t *testing.T) {
	usage := &llm.Usage{
		DurationSeconds: 4,
	}

	price := objects.ModelPrice{
		Items: []objects.ModelPriceItem{
			{
				ItemCode: objects.PriceItemCodeDurationSeconds,
				Pricing: objects.Pricing{
					Mode:         objects.PricingModeUsagePerUnit,
					UsagePerUnit: mustDecimalPtr("0.00008"), // ¥0.00008 per second
				},
			},
		},
	}

	items, total := ComputeUsageCost(usage, price, time.Now())
	require.Len(t, items, 1)
	require.Equal(t, objects.PriceItemCodeDurationSeconds, items[0].ItemCode)
	require.Equal(t, int64(4), items[0].Quantity)
	// 4 * 0.00008 = 0.00032 (NOT divided by 1e6)
	require.InDelta(t, 0.00032, total.InexactFloat64(), 0.0000001)
	require.InDelta(t, 0.00032, items[0].Subtotal.InexactFloat64(), 0.0000001)
}

func TestComputeUsageCost_DurationSeconds_RoundsFraction(t *testing.T) {
	price := objects.ModelPrice{
		Items: []objects.ModelPriceItem{
			{
				ItemCode: objects.PriceItemCodeDurationSeconds,
				Pricing: objects.Pricing{
					Mode:         objects.PricingModeUsagePerUnit,
					UsagePerUnit: mustDecimalPtr("0.01"),
				},
			},
		},
	}

	_, total := ComputeUsageCost(&llm.Usage{DurationSeconds: 0.4}, price, time.Now())
	require.InDelta(t, 0.01, total.InexactFloat64(), 0.0000001, "positive fraction below 0.5 bills 1s")

	_, total = ComputeUsageCost(&llm.Usage{DurationSeconds: 1.6}, price, time.Now())
	require.InDelta(t, 0.02, total.InexactFloat64(), 0.0000001, "1.6 rounds to 2s")

	_, total = ComputeUsageCost(&llm.Usage{DurationSeconds: 0}, price, time.Now())
	require.True(t, total.IsZero())
}

func TestComputeUsageCost_DurationSeconds_IgnoresTokenOnlyUsage(t *testing.T) {
	// Token usage without duration_seconds price item must not invent duration cost.
	usage := &llm.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		DurationSeconds:  10,
	}
	price := objects.ModelPrice{
		Items: []objects.ModelPriceItem{
			{
				ItemCode: objects.PriceItemCodeUsage,
				Pricing: objects.Pricing{
					Mode:         objects.PricingModeUsagePerUnit,
					UsagePerUnit: mustDecimalPtr("1"),
				},
			},
		},
	}
	items, total := ComputeUsageCost(usage, price, time.Now())
	require.Len(t, items, 1)
	require.Equal(t, objects.PriceItemCodeUsage, items[0].ItemCode)
	require.InDelta(t, 0.001, total.InexactFloat64(), 0.0000001)
}

func TestUsageLogService_CreateUsageLog_DurationSecondsCost(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	p, err := client.Project.Create().
		SetName("paraformer-project").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	ch, err := client.Channel.Create().
		SetName("bailian-paraformer").
		SetType("openai").
		SetBaseURL("https://dashscope.aliyuncs.com").
		SetSupportedModels([]string{"paraformer-realtime-v2"}).
		SetDefaultTestModel("paraformer-realtime-v2").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.ChannelModelPrice.Create().
		SetChannelID(ch.ID).
		SetModelID("paraformer-realtime-v2").
		SetReferenceID("paraformer-duration-ref").
		SetPrice(objects.ModelPrice{
			Items: []objects.ModelPriceItem{
				{
					ItemCode: objects.PriceItemCodeDurationSeconds,
					Pricing: objects.Pricing{
						Mode:         objects.PricingModeUsagePerUnit,
						UsagePerUnit: toDecimalPtr("0.00008"),
					},
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	req, err := client.Request.Create().
		SetProjectID(p.ID).
		SetChannelID(ch.ID).
		SetModelID("paraformer-realtime-v2").
		SetStatus(request.StatusCompleted).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{},
		Ent:         client,
	})
	channelService := NewChannelServiceForTest(client)

	enabledCh, err := channelService.buildChannelWithTransformer(ch)
	require.NoError(t, err)
	channelService.preloadModelPrices(ctx, enabledCh)
	channelService.SetEnabledChannelsForTest([]*Channel{enabledCh})

	svc := NewUsageLogService(client, systemService, channelService)

	// Simulate accounting copying Paraformer UsageRecord.DurationSeconds onto llm.Usage.
	usage := &llm.Usage{DurationSeconds: 4}

	created, err := svc.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     req.ID,
		ProjectID:     p.ID,
		ChannelID:     ch.ID,
		ActualModelID: "paraformer-realtime-v2",
		Usage:         usage,
		Source:        usagelog.SourceAPI,
		Format:        "openai/realtime",
		APIKeyID:      nil,
	})
	require.NoError(t, err)
	require.NotNil(t, created)
	require.Equal(t, "paraformer-duration-ref", created.CostPriceReferenceID)
	require.NotNil(t, created.TotalCost)
	require.InDelta(t, 0.00032, *created.TotalCost, 0.0000001)
	require.Len(t, created.CostItems, 1)
	require.Equal(t, objects.PriceItemCodeDurationSeconds, created.CostItems[0].ItemCode)
	require.Equal(t, int64(4), created.CostItems[0].Quantity)
}

func TestComputeUsageCost_DurationSeconds_RoundsToNearest(t *testing.T) {
	price := objects.ModelPrice{
		Items: []objects.ModelPriceItem{
			{
				ItemCode: objects.PriceItemCodeDurationSeconds,
				Pricing: objects.Pricing{
					Mode:         objects.PricingModeUsagePerUnit,
					UsagePerUnit: mustDecimalPtr("0.01"),
				},
			},
		},
	}

	_, total := ComputeUsageCost(&llm.Usage{DurationSeconds: 1.4}, price, time.Now())
	require.InDelta(t, 0.01, total.InexactFloat64(), 0.0000001, "1.4 rounds down to 1s")
}

// The price editors offer the tiered and volume modes for every item code, so the
// per-second divisor must reach those branches too.
func TestComputeUsageCost_DurationSeconds_TieredAndVolume(t *testing.T) {
	upTo := int64(2)
	tiers := &objects.TieredPricing{
		Tiers: []objects.PriceTier{
			{UpTo: &upTo, PricePerUnit: decimal.RequireFromString("0.01")},
			{PricePerUnit: decimal.RequireFromString("0.02")},
		},
	}
	priceFor := func(mode objects.PricingMode) objects.ModelPrice {
		return objects.ModelPrice{
			Items: []objects.ModelPriceItem{
				{
					ItemCode: objects.PriceItemCodeDurationSeconds,
					Pricing:  objects.Pricing{Mode: mode, UsageTiered: tiers},
				},
			},
		}
	}
	usage := &llm.Usage{DurationSeconds: 4}

	_, total := ComputeUsageCost(usage, priceFor(objects.PricingModeTiered), time.Now())
	require.InDelta(t, 0.06, total.InexactFloat64(), 0.0000001, "2s at 0.01 plus 2s at 0.02")

	_, total = ComputeUsageCost(usage, priceFor(objects.PricingModeVolume), time.Now())
	require.InDelta(t, 0.08, total.InexactFloat64(), 0.0000001, "all 4s at the matched tier's 0.02")
}

// Upstream pricing branches merged into computeItemSubtotalScaled must scale with
// scaleUnits(..., unitDivisor); a call to unitsInMillionTokens there would divide
// per-second prices by a million.
func TestComputeItemSubtotalScaled_ScalesOnlyByUnitDivisor(t *testing.T) {
	src, err := os.ReadFile("cost_calc.go")
	require.NoError(t, err)

	body := regexp.MustCompile(`(?s)\nfunc computeItemSubtotalScaled\(.*?\n\}\n`).Find(src)
	require.NotNil(t, body, "computeItemSubtotalScaled should exist in cost_calc.go")
	require.NotContains(t, string(body), "unitsInMillionTokens(")
}
