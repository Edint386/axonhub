package biz

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/transformer/openai/codex/turnstate"
)

func fakeTurnState(n int) string {
	prefix := "gAAAAA"
	buf := make([]byte, n)
	copy(buf, prefix)
	for i := len(prefix); i < n; i++ {
		buf[i] = 'A'
	}
	return string(buf)
}

func TestRotateHarvestProxyExpandsSid(t *testing.T) {
	rotated, err := rotateHarvestProxy("socks5h://user-sid-{sid}:pass@proxy.example:1080")
	require.NoError(t, err)
	require.Contains(t, rotated, "socks5h://")
	require.NotContains(t, rotated, "{sid}")
}

func TestProxyConfigFromHarvestURL(t *testing.T) {
	cfg, err := proxyConfigFromHarvestURL("socks5h://alice:secret@proxy.example:1080")
	require.NoError(t, err)
	require.Equal(t, "alice", cfg.Username)
	require.Equal(t, "secret", cfg.Password)
	require.Contains(t, cfg.URL, "socks5h://proxy.example:1080")
}

func TestCodexTurnStateAcquireAndInvalidate(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	svc := NewChannelServiceForTest(client)
	defer svc.Stop()

	strict := true
	live := &Channel{
		Channel: &ent.Channel{
			ID:      7,
			Type:    channel.TypeCodex,
			BaseURL: "https://chatgpt.com/backend-api/codex#",
			Credentials: objects.ChannelCredentials{
				OAuth: &objects.OAuthCredentials{AccessToken: "not-a-jwt"},
			},
			Settings: &objects.ChannelSettings{CodexTurnState: &objects.CodexTurnStateSettings{
				Enabled: true,
				Plan:    "pro",
				Models:  []string{"gpt-6-astra"},
				Strict:  &strict,
			}},
		},
	}
	svc.SetEnabledChannelsForTest([]*Channel{live})
	mgr := svc.turnState
	require.NotNil(t, mgr)

	key := turnStateKey(7, "gpt-6-astra")
	now := time.Now()
	cfg := live.Settings.CodexTurnState.Normalized()
	mgr.tickets[key] = &storedTurnTicket{
		Ticket: turnstate.Ticket{
			ChannelID: 7,
			Model:     "gpt-6-astra",
			Plan:      "pro",
			State:     fakeTurnState(turnstate.ProLength),
			Version:   3,
			Issued:    now,
			ExpiresAt: now.Add(time.Hour),
		},
		Fingerprint: turnStateFingerprint(live, cfg),
		Identity:    turnStateIdentity(live),
	}

	ticket, err := mgr.Acquire(context.Background(), turnstate.Request{ChannelID: 7, Model: "gpt-6-astra"})
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, fakeTurnState(turnstate.ProLength), ticket.State)

	rt := mgr.Runtime(live.Channel)
	require.NotNil(t, rt)
	require.True(t, rt.Enabled)
	require.Equal(t, "ready", rt.Phase)
	require.Len(t, rt.Models, 1)
	require.Equal(t, "ready", rt.Models[0].Phase)

	headers := http.Header{}
	headers.Set(turnstate.Header, fakeTurnState(turnstate.Signal312))
	mgr.Observe(ticket, headers, "")
	rt = mgr.Runtime(live.Channel)
	require.NotNil(t, rt)
	require.Equal(t, "failed", rt.Phase)
	require.NotEmpty(t, rt.RecentEvents)
	require.Equal(t, "invalidated", rt.RecentEvents[0].Kind)
	require.Equal(t, "state_312", rt.RecentEvents[0].Reason)

	_, err = mgr.Acquire(context.Background(), turnstate.Request{ChannelID: 7, Model: "gpt-6-astra"})
	require.ErrorIs(t, err, turnstate.ErrUnavailable)
}
