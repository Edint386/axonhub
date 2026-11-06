package biz

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/llm/httpclient"
)

// Custom DisableAPIKey sends the channel auto-disabled webhook after the caller's transaction
// commits. Since upstream #2900 a settings read on a context that still carries a (committed)
// transaction queries that transaction, so the notify must run on a detached context or the
// webhook config read fails and nothing is sent. See CUSTOM_BRANCH_RULES.md section 5.
func TestDisableAPIKey_AfterCommitWebhookFires(t *testing.T) {
	for _, warm := range []bool{true, false} {
		name := "cold settings cache"
		if warm {
			name = "warm settings cache"
		}
		t.Run(name, func(t *testing.T) {
			client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
			defer client.Close()

			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			cfg := WebhookNotifierConfig{
				Targets:       []WebhookTarget{{Name: "default", Enabled: true, URL: server.URL, TimeoutMs: 1000, Body: `{"event":"{{.Event}}"}`}},
				Subscriptions: []WebhookSubscription{{Event: EventChannelAutoDisabled, TargetNames: []string{"default"}}},
			}

			ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
			svc := newTestChannelService(client)
			defer svc.enabledChannelsCache.Stop()
			sys := newTestSystemServiceWithWebhookConfig(t, client, cfg)
			svc.SystemService = sys
			svc.WebhookNotifier = NewWebhookNotifier(sys, httpclient.NewHttpClient())
			svc.channelNotifier = &channelSyncNotifierSpy{}
			previous := asyncReloadDisabled
			asyncReloadDisabled = false
			t.Cleanup(func() { asyncReloadDisabled = previous })

			_ = sys.TimeLocation(ctx)
			if warm {
				_, err := sys.WebhookNotifierConfig(ctx)
				require.NoError(t, err)
			}

			ch := createTestChannelWithAPIKeys(t, client, ctx, "manual-last-key", []string{"key1"})

			// A GraphQL mutation runs inside a transaction; disabling the last key disables the channel.
			tx, err := client.Tx(ctx)
			require.NoError(t, err)
			txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), tx.Client())
			require.NoError(t, svc.DisableAPIKey(txCtx, ch.ID, "key1", 0, "Manually disabled by user"))
			require.NoError(t, tx.Commit())

			require.Eventually(t, func() bool { return hits.Load() == 1 }, 3*time.Second, 20*time.Millisecond,
				"the channel auto-disabled webhook was not sent after commit")
		})
	}
}
