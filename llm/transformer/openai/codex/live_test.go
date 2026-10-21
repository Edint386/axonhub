package codex

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/oauth"
)

func TestOutboundTransformer_LiveBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "default base with raw marker",
			baseURL: "https://chatgpt.com/backend-api/codex#",
			want:    "https://chatgpt.com/backend-api/codex",
		},
		{
			name:    "empty base resolves to the chatgpt backend",
			baseURL: "",
			want:    "https://chatgpt.com/backend-api/codex",
		},
		{
			name:    "legacy openai base resolves to the chatgpt backend",
			baseURL: "https://api.openai.com/v1",
			want:    "https://chatgpt.com/backend-api/codex",
		},
		{
			name:    "double raw marker is stripped",
			baseURL: "https://relay.example.com/v1##",
			want:    "https://relay.example.com/v1",
		},
		{
			name:    "wss scheme becomes https",
			baseURL: "wss://relay.example.com/v1#",
			want:    "https://relay.example.com/v1",
		},
		{
			name:    "ws scheme becomes http",
			baseURL: "ws://relay.local:8080/v1",
			want:    "http://relay.local:8080/v1",
		},
		{
			name:    "trailing slash is trimmed",
			baseURL: "https://relay.example.com/v1/",
			want:    "https://relay.example.com/v1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := NewOutboundTransformer(Params{
				TokenProvider: oauth.NewStaticTokenProvider(&oauth.OAuthCredentials{AccessToken: "token"}),
				BaseURL:       tt.baseURL,
			})
			require.NoError(t, err)
			require.Equal(t, tt.want, out.LiveBaseURL())
		})
	}

	t.Run("nil receiver", func(t *testing.T) {
		var out *OutboundTransformer
		require.Equal(t, "", out.LiveBaseURL())
	})
}
