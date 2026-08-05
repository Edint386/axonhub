package biz

import (
	"context"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

// TestChannel_ModelPriceCache_ConcurrentRefreshAndRead guards against the data
// race between preloadModelPrices and the cost calculation read path.
//
// SaveChannelModelPrices refreshes the price cache on a Channel instance that
// has already been published to the shared enabled-channel cache, while
// in-flight requests read the very same field from computeUsageCost. Both
// sides must go through the channel's price mutex.
//
// Run with -race to make the check meaningful.
func TestChannel_ModelPriceCache_ConcurrentRefreshAndRead(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenaiFake).
		SetName("race-channel").
		SetSupportedModels([]string{"m1"}).
		SetDefaultTestModel("m1").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{}).
		Save(ctx)
	require.NoError(t, err)

	unit := decimal.NewFromFloat(0.01)
	_, err = client.ChannelModelPrice.Create().
		SetChannelID(ch.ID).
		SetModelID("m1").
		SetPrice(objects.ModelPrice{
			Items: []objects.ModelPriceItem{
				{
					ItemCode: objects.PriceItemCodeUsage,
					Pricing:  objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &unit},
				},
			},
		}).
		SetReferenceID("ref-race").
		Save(ctx)
	require.NoError(t, err)

	systemService := NewSystemService(SystemServiceParams{Ent: client})
	channelService := NewChannelServiceForTest(client)

	built, err := channelService.GetChannel(ctx, ch.ID)
	require.NoError(t, err)

	channelService.preloadModelPrices(ctx, built)
	channelService.SetEnabledChannelsForTest([]*Channel{built})

	usageLogService := NewUsageLogService(client, systemService, channelService)
	usage := &llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}

	const iterations = 50

	var wg sync.WaitGroup

	// Writer: mimics SaveChannelModelPrices refreshing the live channel.
	wg.Add(1)

	go func() {
		defer wg.Done()

		for range iterations {
			channelService.preloadModelPrices(ctx, built)
		}
	}()

	// Readers: mimic concurrent in-flight requests computing their cost.
	for range 4 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range iterations {
				_, _, _ = usageLogService.computeUsageCost(ctx, ch.ID, "m1", usage)
			}
		}()
	}

	wg.Wait()

	require.Equal(t, 1, built.ModelPriceCount())

	price, ok := built.ModelPrice("m1")
	require.True(t, ok)
	require.Equal(t, "ref-race", price.ReferenceID)
}

// TestChannel_ModelEntryCache_ConcurrentLazyInit pins down the lazy-init
// contract of the model entry caches.
//
// buildChannel warms both caches before a channel is published, so the hot
// path never initializes them concurrently. That invariant is incidental
// though: any Channel built outside buildChannel (ListModels, the model
// association matcher, tests) initializes them on first access. sync.Once
// turns "safe because of the caller" into "safe by construction".
func TestChannel_ModelEntryCache_ConcurrentLazyInit(t *testing.T) {
	ch := &Channel{
		Channel: &ent.Channel{
			SupportedModels: []string{"gpt-4o-mini", "gpt-4.1-mini", "gpt-4o"},
			Settings: &objects.ChannelSettings{
				ExtraModelPrefix: "vendor",
				ModelMappings: []objects.ModelMapping{
					{From: "fast", To: "gpt-4o-mini"},
					{From: "fast", To: "gpt-4.1-mini"},
					{From: "premium", To: "gpt-4o"},
				},
			},
		},
	}

	const goroutines = 8

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		groups []map[string][]ChannelModelEntry
	)

	for range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			got := ch.GetModelEntryGroups()
			_ = ch.GetModelEntries()

			mu.Lock()
			groups = append(groups, got)
			mu.Unlock()
		}()
	}

	wg.Wait()

	require.Len(t, groups, goroutines)

	// Every goroutine must observe the exact same map, not a per-goroutine
	// recomputation, otherwise the cache is being rebuilt under contention.
	for _, got := range groups {
		require.Len(t, got["fast"], 2)
		require.Equal(t, groups[0]["fast"][0], got["fast"][0])
		require.Equal(t, groups[0]["premium"], got["premium"])
	}
}
