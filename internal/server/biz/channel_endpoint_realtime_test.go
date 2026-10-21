//nolint:exhaustruct_v5 // Test fixtures intentionally set only fields relevant to each scenario.
package biz

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

const realtimeTestAccountID = "acct_realtime_test"

// realtimeTestJWT builds an unsigned-verification JWT carrying the ChatGPT account id claim
// that codex.ExtractChatGPTAccountIDFromJWT reads.
func realtimeTestJWT(t *testing.T) string {
	t.Helper()

	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": realtimeTestAccountID},
		"exp":                         time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("test-secret"))
	require.NoError(t, err)

	return token
}

// buildRealtimeTestChannel builds a channel with its outbounds through the regular
// ChannelService path so that Codex channels get a real *codex.OutboundTransformer.
func buildRealtimeTestChannel(t *testing.T, svc *ChannelService, c *ent.Channel) *Channel {
	t.Helper()

	ch, err := svc.buildChannelWithOutbounds(c)
	require.NoError(t, err)
	require.NotNil(t, ch)

	return ch
}

func realtimeExpectedKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))

	return "key:" + hex.EncodeToString(sum[:])[:12]
}

func TestChannel_RealtimeTarget(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_realtime_target?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	realtimeEndpoint := func(baseURL, path string) []objects.ChannelEndpoint {
		return []objects.ChannelEndpoint{{
			APIFormat: llm.APIFormatOpenAIRealtime.String(),
			BaseURL:   baseURL,
			Path:      path,
		}}
	}

	tests := []struct {
		name    string
		channel *ent.Channel
		build   bool
		mode    RealtimeMode
		want    RealtimeTarget
		wantErr string
	}{
		{
			name: "explicit endpoint with https base",
			channel: &ent.Channel{
				ID: 1, Name: "openai-explicit", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
				Endpoints:   realtimeEndpoint("https://x.example.com/api-ws/v1", "/realtime"),
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://x.example.com/api-ws/v1", WSURL: "wss://x.example.com/api-ws/v1/realtime",
				Dialect: RealtimeDialectGA, Explicit: true, RecordHealth: true,
			},
		},
		{
			name: "explicit endpoint with '#' base keeps the path and defaults /realtime",
			channel: &ent.Channel{
				ID: 2, Name: "openai-hash", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
				Endpoints:   realtimeEndpoint("https://x.example.com/custom#", ""),
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://x.example.com/custom", WSURL: "wss://x.example.com/custom/realtime",
				Dialect: RealtimeDialectGA, Explicit: true, RecordHealth: true,
			},
		},
		{
			name: "explicit endpoint with '##' base is raw",
			channel: &ent.Channel{
				ID: 3, Name: "openai-raw", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
				Endpoints:   realtimeEndpoint("https://x.example.com/raw/##", "/rt"),
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://x.example.com/raw", WSURL: "wss://x.example.com/raw/rt",
				Dialect: RealtimeDialectGA, Explicit: true, RecordHealth: true,
			},
		},
		{
			name: "explicit endpoint without base falls back to the channel base",
			channel: &ent.Channel{
				ID: 4, Name: "deepseek-explicit", Type: channel.TypeDeepseek,
				BaseURL:     "https://relay.example.com",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
				Endpoints:   realtimeEndpoint("", ""),
			},
			mode: RealtimeModeWebRTC,
			want: RealtimeTarget{
				APIBase: "https://relay.example.com/v1", WSURL: "wss://relay.example.com/v1/realtime",
				Dialect: RealtimeDialectGA, Explicit: true, RecordHealth: true,
			},
		},
		{
			name: "explicit endpoint on a bailian channel uses the bailian dialect",
			channel: &ent.Channel{
				ID: 5, Name: "bailian-explicit", Type: channel.TypeBailianResponses,
				BaseURL:     "https://dashscope.aliyuncs.com/compatible-mode/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
				Endpoints:   realtimeEndpoint("https://ws-123.cn-beijing.maas.aliyuncs.com/api-ws/v1", "/realtime"),
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://ws-123.cn-beijing.maas.aliyuncs.com/api-ws/v1",
				WSURL:   "wss://ws-123.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime",
				Dialect: RealtimeDialectBailian, Explicit: true, RecordHealth: true,
			},
		},
		{
			name: "derived openai base without /v1",
			channel: &ent.Channel{
				ID: 6, Name: "openai-no-v1", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://api.openai.com/v1", WSURL: "wss://api.openai.com/v1/realtime",
				Dialect: RealtimeDialectGA, RecordHealth: true,
			},
		},
		{
			name: "derived openai base with /v1",
			channel: &ent.Channel{
				ID: 7, Name: "openai-v1", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com/v1/",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://api.openai.com/v1", WSURL: "wss://api.openai.com/v1/realtime",
				Dialect: RealtimeDialectGA, RecordHealth: true,
			},
		},
		{
			name: "derived openai_responses base with '#' skips /v1",
			channel: &ent.Channel{
				ID: 8, Name: "openai-responses-hash", Type: channel.TypeOpenaiResponses,
				BaseURL:     "https://relay.example.com/openai#",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode: RealtimeModeWebRTC,
			want: RealtimeTarget{
				APIBase: "https://relay.example.com/openai", WSURL: "wss://relay.example.com/openai/realtime",
				Dialect: RealtimeDialectGA, RecordHealth: true,
			},
		},
		{
			name: "derived openai base with wss scheme is rewritten to https",
			channel: &ent.Channel{
				ID: 9, Name: "openai-wss", Type: channel.TypeOpenai,
				BaseURL:     "wss://relay.example.com/v1##",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://relay.example.com/v1", WSURL: "wss://relay.example.com/v1/realtime",
				Dialect: RealtimeDialectGA, RecordHealth: true,
			},
		},
		{
			name: "derived bailian cn",
			channel: &ent.Channel{
				ID: 10, Name: "bailian-cn", Type: channel.TypeBailian,
				BaseURL:     "https://dashscope.aliyuncs.com/compatible-mode/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://dashscope.aliyuncs.com/api-ws/v1",
				WSURL:   "wss://dashscope.aliyuncs.com/api-ws/v1/realtime",
				Dialect: RealtimeDialectBailian, RecordHealth: true,
			},
		},
		{
			name: "derived bailian intl",
			channel: &ent.Channel{
				ID: 11, Name: "bailian-intl", Type: channel.TypeBailianAnthropic,
				BaseURL:     "https://dashscope-intl.aliyuncs.com/apps/anthropic",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode: RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://dashscope-intl.aliyuncs.com/api-ws/v1",
				WSURL:   "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime",
				Dialect: RealtimeDialectBailian, RecordHealth: true,
			},
		},
		{
			name: "codex webrtc uses the chatgpt backend live base",
			channel: &ent.Channel{
				ID: 12, Name: "codex-pat", Type: channel.TypeCodex,
				Credentials:     objects.ChannelCredentials{APIKey: "pat-key"},
				SupportedModels: []string{"gpt-live-1-codex"},
			},
			build: true,
			mode:  RealtimeModeWebRTC,
			want: RealtimeTarget{
				APIBase: "https://chatgpt.com/backend-api/codex", Dialect: RealtimeDialectGA,
				Codex: true, Backend: true, LiveBase: "https://chatgpt.com/backend-api/codex",
			},
		},
		{
			name: "codex webrtc through a relay is not the backend shape",
			channel: &ent.Channel{
				ID: 13, Name: "fenno-relay", Type: channel.TypeFenno,
				BaseURL:         "https://api.fenno.ai",
				Credentials:     objects.ChannelCredentials{APIKey: "pat-key"},
				SupportedModels: []string{"gpt-live-1-codex"},
			},
			build: true,
			mode:  RealtimeModeWebRTC,
			want: RealtimeTarget{
				APIBase: "https://api.fenno.ai", Dialect: RealtimeDialectGA,
				Codex: true, Backend: false, LiveBase: "https://api.fenno.ai",
			},
		},
		{
			name: "codex websocket without an explicit endpoint is rejected",
			channel: &ent.Channel{
				ID: 14, Name: "codex-ws", Type: channel.TypeCodex,
				Credentials:     objects.ChannelCredentials{APIKey: "pat-key"},
				SupportedModels: []string{"gpt-live-1-codex"},
			},
			build:   true,
			mode:    RealtimeModeWebSocket,
			wantErr: "only supports WebRTC calls",
		},
		{
			name: "codex websocket with an explicit endpoint and an api key relays",
			channel: &ent.Channel{
				ID: 15, Name: "codex-pat-ws", Type: channel.TypeCodex,
				BaseURL:         "https://relay.example.com/v1",
				Credentials:     objects.ChannelCredentials{APIKey: "pat-key"},
				SupportedModels: []string{"gpt-realtime"},
				Endpoints:       realtimeEndpoint("https://relay.example.com/v1", "/realtime"),
			},
			build: true,
			mode:  RealtimeModeWebSocket,
			want: RealtimeTarget{
				APIBase: "https://relay.example.com/v1", WSURL: "wss://relay.example.com/v1/realtime",
				Dialect: RealtimeDialectGA, Explicit: true, RecordHealth: true,
				Codex: true, Backend: false, LiveBase: "https://relay.example.com/v1",
			},
		},
		{
			name: "codex oauth websocket is rejected even with an explicit endpoint",
			channel: &ent.Channel{
				ID: 16, Name: "codex-oauth-ws", Type: channel.TypeCodex,
				Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
					AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
				}},
				SupportedModels: []string{"gpt-realtime"},
				Endpoints:       realtimeEndpoint("https://relay.example.com/v1", "/realtime"),
			},
			build:   true,
			mode:    RealtimeModeWebSocket,
			wantErr: "OAuth credentials",
		},
		{
			name: "codex oauth webrtc is accepted",
			channel: &ent.Channel{
				ID: 17, Name: "codex-oauth-rtc", Type: channel.TypeCodex,
				Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
					AccessToken: "token", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
				}},
				SupportedModels: []string{"gpt-live-1-codex"},
			},
			build: true,
			mode:  RealtimeModeWebRTC,
			want: RealtimeTarget{
				APIBase: "https://chatgpt.com/backend-api/codex", Dialect: RealtimeDialectGA,
				Codex: true, Backend: true, LiveBase: "https://chatgpt.com/backend-api/codex",
			},
		},
		{
			name: "unsupported channel type without an endpoint errors",
			channel: &ent.Channel{
				ID: 18, Name: "deepseek", Type: channel.TypeDeepseek,
				BaseURL:     "https://api.deepseek.com",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode:    RealtimeModeWebSocket,
			wantErr: "channel type deepseek has no realtime endpoint; add an openai/realtime endpoint",
		},
		{
			name: "unknown mode errors",
			channel: &ent.Channel{
				ID: 19, Name: "openai-mode", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-test"},
			},
			mode:    RealtimeMode("sip"),
			wantErr: "unsupported mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ch *Channel
			if tt.build {
				ch = buildRealtimeTestChannel(t, svc, tt.channel)
			} else {
				ch = &Channel{Channel: tt.channel}
			}

			got, err := ch.RealtimeTarget(tt.mode)
			if tt.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}

	t.Run("nil channel errors", func(t *testing.T) {
		var ch *Channel

		_, err := ch.RealtimeTarget(RealtimeModeWebSocket)
		require.Error(t, err)
	})
}

func TestChannel_RealtimeCredential(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_realtime_credential?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	t.Run("single api key", func(t *testing.T) {
		ch := buildRealtimeTestChannel(t, svc, &ent.Channel{
			ID: 1, Name: "openai", Type: channel.TypeOpenai,
			BaseURL:     "https://api.openai.com/v1",
			Credentials: objects.ChannelCredentials{APIKey: "sk-one"},
		})

		ctx := contexts.EnsureContainer(t.Context())

		cred, err := ch.RealtimeCredential(ctx)
		require.NoError(t, err)
		require.Equal(t, RealtimeCredential{Bearer: "sk-one", Fingerprint: realtimeExpectedKeyFingerprint("sk-one")}, cred)

		recorded, ok := contexts.GetChannelAPIKey(ctx)
		require.True(t, ok, "the api key provider records the channel api key for execution rows")
		require.Equal(t, "sk-one", recorded)

		again, err := ch.RealtimeCredentialByFingerprint(t.Context(), cred.Fingerprint)
		require.NoError(t, err)
		require.Equal(t, cred, again)
	})

	t.Run("two api keys have stable fingerprints", func(t *testing.T) {
		ch := buildRealtimeTestChannel(t, svc, &ent.Channel{
			ID: 2, Name: "openai-two", Type: channel.TypeOpenai,
			BaseURL:     "https://api.openai.com/v1",
			Credentials: objects.ChannelCredentials{APIKeys: []string{"sk-a", "sk-b"}},
		})

		cred, err := ch.RealtimeCredential(t.Context())
		require.NoError(t, err)
		require.Contains(t, []string{"sk-a", "sk-b"}, cred.Bearer)
		require.Equal(t, realtimeExpectedKeyFingerprint(cred.Bearer), cred.Fingerprint)
		require.False(t, cred.OAuth)
		require.Empty(t, cred.AccountID)

		for range 5 {
			again, err := ch.RealtimeCredentialByFingerprint(t.Context(), cred.Fingerprint)
			require.NoError(t, err)
			require.Equal(t, cred, again)
		}

		other := "sk-a"
		if cred.Bearer == "sk-a" {
			other = "sk-b"
		}

		otherCred, err := ch.RealtimeCredentialByFingerprint(t.Context(), realtimeExpectedKeyFingerprint(other))
		require.NoError(t, err)
		require.Equal(t, other, otherCred.Bearer)
		require.NotEqual(t, cred.Fingerprint, otherCred.Fingerprint)
	})

	t.Run("disabled key is no longer resolvable by fingerprint", func(t *testing.T) {
		ch := buildRealtimeTestChannel(t, svc, &ent.Channel{
			ID: 3, Name: "openai-disabled", Type: channel.TypeOpenai,
			BaseURL:         "https://api.openai.com/v1",
			Credentials:     objects.ChannelCredentials{APIKeys: []string{"sk-a", "sk-b"}},
			DisabledAPIKeys: []objects.DisabledAPIKey{{Key: "sk-b", DisabledAt: time.Now()}},
		})

		cred, err := ch.RealtimeCredential(t.Context())
		require.NoError(t, err)
		require.Equal(t, "sk-a", cred.Bearer)

		_, err = ch.RealtimeCredentialByFingerprint(t.Context(), realtimeExpectedKeyFingerprint("sk-b"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "no longer enabled")

		_, err = ch.RealtimeCredentialByFingerprint(t.Context(), "")
		require.Error(t, err)
	})

	t.Run("zero keys errors without panicking", func(t *testing.T) {
		ch := &Channel{Channel: &ent.Channel{ID: 4, Name: "empty", Type: channel.TypeOpenai}}

		require.NotPanics(t, func() {
			_, err := ch.RealtimeCredential(t.Context())
			require.Error(t, err)
			require.Contains(t, err.Error(), "no enabled api key")

			_, err = ch.RealtimeCredentialByFingerprint(t.Context(), realtimeExpectedKeyFingerprint("sk-missing"))
			require.Error(t, err)
		})
	})

	t.Run("codex PAT relay channel", func(t *testing.T) {
		ch := buildRealtimeTestChannel(t, svc, &ent.Channel{
			ID: 5, Name: "codex-pat", Type: channel.TypeCodex,
			BaseURL:         "https://relay.example.com/v1",
			Credentials:     objects.ChannelCredentials{APIKey: "pat-key"},
			SupportedModels: []string{"gpt-realtime"},
		})

		_, ok := ch.Outbound.(*codex.OutboundTransformer)
		require.True(t, ok)

		cred, err := ch.RealtimeCredential(t.Context())
		require.NoError(t, err)
		require.Equal(t, RealtimeCredential{Bearer: "pat-key", Fingerprint: realtimeExpectedKeyFingerprint("pat-key")}, cred)

		again, err := ch.RealtimeCredentialByFingerprint(t.Context(), cred.Fingerprint)
		require.NoError(t, err)
		require.Equal(t, cred, again)

		_, err = ch.RealtimeCredentialByFingerprint(t.Context(), "oauth:5")
		require.Error(t, err, "a PAT relay channel has no oauth credential")
	})

	t.Run("codex OAuth channel", func(t *testing.T) {
		token := realtimeTestJWT(t)
		ch := buildRealtimeTestChannel(t, svc, &ent.Channel{
			ID: 6, Name: "codex-oauth", Type: channel.TypeCodex,
			Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
				AccessToken: token, RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
			}},
			SupportedModels: []string{"gpt-live-1-codex"},
		})

		cred, err := ch.RealtimeCredential(t.Context())
		require.NoError(t, err)
		require.Equal(t, RealtimeCredential{
			Bearer: token, AccountID: realtimeTestAccountID, Fingerprint: "oauth:6", OAuth: true,
		}, cred)

		again, err := ch.RealtimeCredentialByFingerprint(t.Context(), "oauth:6")
		require.NoError(t, err)
		require.Equal(t, cred, again)

		_, err = ch.RealtimeCredentialByFingerprint(t.Context(), "oauth:999")
		require.Error(t, err)

		_, err = ch.RealtimeTarget(RealtimeModeWebSocket)
		require.Error(t, err, "websocket mode rejects oauth credentials")
	})

	t.Run("api key override wins", func(t *testing.T) {
		ch := buildRealtimeTestChannel(t, svc, &ent.Channel{
			ID: 7, Name: "openai-override", Type: channel.TypeOpenai,
			BaseURL:     "https://api.openai.com/v1",
			Credentials: objects.ChannelCredentials{APIKey: "sk-stored"},
		})
		ch.apiKeyOverride = "sk-override"

		cred, err := ch.RealtimeCredential(t.Context())
		require.NoError(t, err)
		require.Equal(t, "sk-override", cred.Bearer)

		again, err := ch.RealtimeCredentialByFingerprint(t.Context(), cred.Fingerprint)
		require.NoError(t, err)
		require.Equal(t, cred, again)
	})

	t.Run("nil channel errors", func(t *testing.T) {
		var ch *Channel

		_, err := ch.RealtimeCredential(t.Context())
		require.Error(t, err)

		_, err = ch.RealtimeCredentialByFingerprint(t.Context(), "key:abc")
		require.Error(t, err)
	})
}

func TestChannel_RealtimeHeaderOverrides(t *testing.T) {
	ch := &Channel{Channel: &ent.Channel{
		ID: 1, Name: "bailian", Type: channel.TypeBailian,
		Settings: &objects.ChannelSettings{HeaderOverrideOperations: []objects.OverrideOperation{
			{Op: objects.OverrideOpSet, Path: "X-DashScope-WorkSpace", Value: "ws-123"},
			{Op: objects.OverrideOpSet, Path: "X-Templated", Value: "{{ .Model }}"},
			{Op: objects.OverrideOpDelete, Path: "X-Remove"},
		}},
	}}

	headers := http.Header{}
	headers.Set("X-Remove", "gone")

	ch.RealtimeHeaderOverrides(headers)

	require.Equal(t, "ws-123", headers.Get("X-DashScope-WorkSpace"))
	require.Empty(t, headers.Get("X-Templated"), "templated values are skipped outside the pipeline")
	require.Empty(t, headers.Get("X-Remove"))

	t.Run("nil safe", func(t *testing.T) {
		var nilCh *Channel

		require.NotPanics(t, func() {
			nilCh.RealtimeHeaderOverrides(headers)
			ch.RealtimeHeaderOverrides(nil)
		})
	})
}

func TestRealtimeEndpointOutbound_Stub(t *testing.T) {
	out := newRealtimeEndpointOutbound()
	require.Equal(t, llm.APIFormatOpenAIRealtime, out.APIFormat())

	_, err := out.TransformRequest(t.Context(), &llm.Request{Model: "gpt-realtime"})
	require.EqualError(t, err, "api_format openai/realtime only serves /v1/realtime; remove it from model protocol overrides")

	_, err = out.TransformResponse(t.Context(), nil)
	require.ErrorIs(t, err, errRealtimeEndpointOutbound)

	_, err = out.TransformStream(t.Context(), nil, nil)
	require.ErrorIs(t, err, errRealtimeEndpointOutbound)

	_, _, err = out.AggregateStreamChunks(t.Context(), nil, nil)
	require.ErrorIs(t, err, errRealtimeEndpointOutbound)

	respErr := out.TransformError(t.Context(), nil)
	require.NotNil(t, respErr)
	require.Equal(t, http.StatusInternalServerError, respErr.StatusCode)
	require.Equal(t, errRealtimeEndpointOutbound.Error(), respErr.Detail.Message)
}

func TestChannel_HasRealtimeCredential(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:channel_realtime_has_credential?mode=memory&_fk=0")
	t.Cleanup(func() { client.Close() })

	svc := NewChannelServiceForTest(client)
	t.Cleanup(svc.Stop)

	tests := []struct {
		name    string
		channel *ent.Channel
		want    bool
	}{
		{
			name: "single api key",
			channel: &ent.Channel{
				ID: 1, Name: "openai", Type: channel.TypeOpenai,
				BaseURL:     "https://api.openai.com/v1",
				Credentials: objects.ChannelCredentials{APIKey: "sk-one"},
			},
			want: true,
		},
		{
			name: "codex PAT relay channel",
			channel: &ent.Channel{
				ID: 2, Name: "codex-pat", Type: channel.TypeCodex,
				BaseURL:         "https://relay.example.com/v1",
				Credentials:     objects.ChannelCredentials{APIKey: "pat-key"},
				SupportedModels: []string{"gpt-realtime"},
			},
			want: true,
		},
		{
			name: "codex OAuth channel",
			channel: &ent.Channel{
				ID: 3, Name: "codex-oauth", Type: channel.TypeCodex,
				Credentials: objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{
					AccessToken: realtimeTestJWT(t), RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour),
				}},
				SupportedModels: []string{"gpt-live-1-codex"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := buildRealtimeTestChannel(t, svc, tt.channel)
			ctx := contexts.EnsureContainer(t.Context())

			require.Equal(t, tt.want, ch.HasRealtimeCredential())

			_, recorded := contexts.GetChannelAPIKey(ctx)
			require.False(t, recorded, "checking availability must not record a channel api key")
		})
	}

	t.Run("zero keys", func(t *testing.T) {
		ch := &Channel{Channel: &ent.Channel{ID: 4, Name: "empty", Type: channel.TypeOpenai}}

		require.NotPanics(t, func() {
			require.False(t, ch.HasRealtimeCredential())
		})
	})

	t.Run("api key override wins over missing keys", func(t *testing.T) {
		ch := &Channel{Channel: &ent.Channel{ID: 5, Name: "override", Type: channel.TypeOpenai}}
		ch.apiKeyOverride = "sk-override"

		require.True(t, ch.HasRealtimeCredential())
	})

	t.Run("nil channel", func(t *testing.T) {
		var ch *Channel

		require.False(t, ch.HasRealtimeCredential())
		require.False(t, (&Channel{}).HasRealtimeCredential())
	})
}
