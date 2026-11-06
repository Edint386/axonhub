package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/events"
)

// SelectRealtime copies the PersistenceState that Process builds (CUSTOM_BRANCH_RULES.md section 3),
// including upstream's EventBus (#2912), so a realtime quota rejection publishes the same
// api_key.quota_exceeded plugin event a chat request does.
func TestSelectRealtime_QuotaRejectionPublishesPluginEvent(t *testing.T) {
	f := newRealtimeTestFixture(t)
	bus := events.NewBus()
	defer bus.Close(context.Background())

	received := make(chan events.Event, 4)
	bus.Subscribe("fork-test", []string{events.TypeAPIKeyQuotaExceeded}, func(_ context.Context, e events.Event) { received <- e })

	orch := f.orchestrator.WithEventBus(bus)
	orch.QuotaService = biz.NewQuotaService(f.client, newTestSystemService(f.client))

	zero := decimal.Zero
	ctx := PrepareRealtimeContext(f.ctx)
	ctx = contexts.WithAPIKey(ctx, realtimeTestAPIKey(objects.APIKeyProfile{
		Quota: &objects.APIKeyQuota{Cost: &zero, Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}},
	}))

	_, err := orch.SelectRealtimeCandidates(ctx, "gpt-realtime", biz.RealtimeModeWebSocket, nil)
	require.Error(t, err)

	select {
	case e := <-received:
		require.Equal(t, events.TypeAPIKeyQuotaExceeded, e.Type)
	case <-time.After(2 * time.Second):
		t.Fatal("a realtime quota rejection published no api_key.quota_exceeded event")
	}
}
