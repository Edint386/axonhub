package codex

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
	"github.com/looplj/axonhub/llm/transformer/openai/codex/turnstate"
)

type staticTickets struct {
	ticket *turnstate.Ticket
	err    error
	seen   int
}

func (s *staticTickets) Acquire(_ context.Context, _ turnstate.Request) (*turnstate.Ticket, error) {
	return s.ticket, s.err
}

func (s *staticTickets) Observe(_ *turnstate.Ticket, _ http.Header, _ string) {
	s.seen++
}

func TestApplyTicketInjectsHeader(t *testing.T) {
	src := &staticTickets{ticket: &turnstate.Ticket{
		ChannelID: 1,
		Model:     "gpt-6-astra",
		State:     "gAAAAA" + strings.Repeat("A", turnstate.ProLength-6),
		Version:   1,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	outbound, err := NewOutboundTransformer(Params{
		BaseURL: "https://chatgpt.com/backend-api/codex#",
		TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{
			AccessToken: testAccessTokenWithAccountID(t),
			ExpiresAt:   time.Now().Add(time.Hour),
		}},
		ChannelID:    1,
		TicketSource: src,
	})
	require.NoError(t, err)
	exec := &codexExecutor{transformer: outbound}
	req := &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "https://chatgpt.com/backend-api/codex/responses",
		Headers: http.Header{"X-Codex-Turn-State": []string{"client-state"}},
		Body:    []byte(`{"model":"gpt-6-astra"}`),
	}
	ticket, err := exec.applyTicket(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	require.Equal(t, src.ticket.State, req.Headers.Get(turnstate.Header))
}

func TestApplyTicketUnavailable(t *testing.T) {
	src := &staticTickets{err: turnstate.ErrUnavailable}
	outbound, err := NewOutboundTransformer(Params{
		BaseURL: "https://chatgpt.com/backend-api/codex#",
		TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{
			AccessToken: testAccessTokenWithAccountID(t),
			ExpiresAt:   time.Now().Add(time.Hour),
		}},
		ChannelID:    1,
		TicketSource: src,
	})
	require.NoError(t, err)
	exec := &codexExecutor{transformer: outbound}
	req := &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "https://chatgpt.com/backend-api/codex/responses",
		Headers: make(http.Header),
		Body:    []byte(`{"model":"gpt-6-astra"}`),
	}
	_, err = exec.applyTicket(context.Background(), req)
	require.Error(t, err)
	var httpErr *httpclient.Error
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, http.StatusServiceUnavailable, httpErr.StatusCode)
}

func TestApplyTicketSkipsRelays(t *testing.T) {
	src := &staticTickets{err: turnstate.ErrUnavailable}
	outbound, err := NewOutboundTransformer(Params{
		BaseURL: "https://example.invalid/v1",
		TokenProvider: staticTokenGetter{creds: &oauth.OAuthCredentials{
			AccessToken: testAccessTokenWithAccountID(t),
			ExpiresAt:   time.Now().Add(time.Hour),
		}},
		ChannelID:    1,
		TicketSource: src,
	})
	require.NoError(t, err)
	exec := &codexExecutor{transformer: outbound}
	req := &httpclient.Request{
		Method:  http.MethodPost,
		URL:     "https://example.invalid/v1/responses",
		Headers: make(http.Header),
		Body:    []byte(`{"model":"gpt-6-astra"}`),
	}
	ticket, err := exec.applyTicket(context.Background(), req)
	require.NoError(t, err)
	require.Nil(t, ticket)
}
