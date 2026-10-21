package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
)

// runRealtimeSubprotocolAuth sends req through WithRealtimeSubprotocolAuth and returns the
// response recorder plus the request headers as seen by the downstream handler (nil when the
// middleware aborted).
func runRealtimeSubprotocolAuth(t *testing.T, req *http.Request) (*httptest.ResponseRecorder, http.Header) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var seen http.Header

	router := gin.New()
	router.Use(WithRealtimeSubprotocolAuth())
	router.GET("/v1/realtime", func(c *gin.Context) {
		seen = c.Request.Header.Clone()
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	return recorder, seen
}

// newUpgradeRequest builds a GET /v1/realtime request carrying the WebSocket upgrade headers.
func newUpgradeRequest(t *testing.T) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	return req
}

func TestWithRealtimeSubprotocolAuth(t *testing.T) {
	const key = "sk-test-subprotocol-key"

	tests := []struct {
		name              string
		protocols         []string
		authorization     string
		wantStatus        int
		wantAuthorization string
		wantProtocols     []string
		wantHeaderAbsent  bool
	}{
		{
			name:              "single line with key and realtime",
			protocols:         []string{"realtime, " + RealtimeInsecureAPIKeyPrefix + key},
			wantStatus:        http.StatusOK,
			wantAuthorization: "Bearer " + key,
			wantProtocols:     []string{"realtime"},
		},
		{
			name:              "key on second header line",
			protocols:         []string{"realtime", RealtimeInsecureAPIKeyPrefix + key},
			wantStatus:        http.StatusOK,
			wantAuthorization: "Bearer " + key,
			wantProtocols:     []string{"realtime"},
		},
		{
			name:              "key only deletes the header",
			protocols:         []string{RealtimeInsecureAPIKeyPrefix + key},
			wantStatus:        http.StatusOK,
			wantAuthorization: "Bearer " + key,
			wantHeaderAbsent:  true,
		},
		{
			name:              "authorization already present wins and key is stripped",
			protocols:         []string{"realtime, " + RealtimeInsecureAPIKeyPrefix + key},
			authorization:     "Bearer sk-header-key",
			wantStatus:        http.StatusOK,
			wantAuthorization: "Bearer sk-header-key",
			wantProtocols:     []string{"realtime"},
		},
		{
			name:       "two different keys are rejected",
			protocols:  []string{"realtime, " + RealtimeInsecureAPIKeyPrefix + key, RealtimeInsecureAPIKeyPrefix + "sk-other"},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:              "same key offered twice counts once",
			protocols:         []string{"realtime, " + RealtimeInsecureAPIKeyPrefix + key, RealtimeInsecureAPIKeyPrefix + key},
			wantStatus:        http.StatusOK,
			wantAuthorization: "Bearer " + key,
			wantProtocols:     []string{"realtime"},
		},
		{
			name:              "remaining tokens are joined on one line",
			protocols:         []string{"realtime, openai-organization.org_1", RealtimeInsecureAPIKeyPrefix + key + " , openai-project.proj_1"},
			wantStatus:        http.StatusOK,
			wantAuthorization: "Bearer " + key,
			wantProtocols:     []string{"realtime, openai-organization.org_1, openai-project.proj_1"},
		},
		{
			name:          "no key leaves authorization empty",
			protocols:     []string{"realtime"},
			wantStatus:    http.StatusOK,
			wantProtocols: []string{"realtime"},
		},
		{
			name:             "empty key token is dropped without setting authorization",
			protocols:        []string{RealtimeInsecureAPIKeyPrefix},
			wantStatus:       http.StatusOK,
			wantHeaderAbsent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newUpgradeRequest(t)
			for _, line := range tt.protocols {
				req.Header.Add("Sec-WebSocket-Protocol", line)
			}

			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}

			recorder, seen := runRealtimeSubprotocolAuth(t, req)
			require.Equal(t, tt.wantStatus, recorder.Code)

			if tt.wantStatus != http.StatusOK {
				require.Nil(t, seen, "handler must not run after abort")

				var body objects.ErrorResponse
				require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
				require.Equal(t, "multiple openai-insecure-api-key subprotocols", body.Error.Message)
				require.NotContains(t, recorder.Body.String(), key, "response must never echo the key")

				return
			}

			require.NotNil(t, seen)
			require.Equal(t, tt.wantAuthorization, seen.Get("Authorization"))

			if tt.wantHeaderAbsent {
				require.Empty(t, seen.Values("Sec-WebSocket-Protocol"))
			} else {
				require.Equal(t, tt.wantProtocols, seen.Values("Sec-WebSocket-Protocol"))
			}

			for _, line := range seen.Values("Sec-WebSocket-Protocol") {
				require.NotContains(t, line, RealtimeInsecureAPIKeyPrefix)
			}
		})
	}
}

func TestWithRealtimeSubprotocolAuth_NonUpgradeUntouched(t *testing.T) {
	const key = "sk-test-subprotocol-key"

	req := httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-realtime", nil)
	req.Header.Add("Sec-WebSocket-Protocol", "realtime, "+RealtimeInsecureAPIKeyPrefix+key)
	req.Header.Add("Sec-WebSocket-Protocol", RealtimeInsecureAPIKeyPrefix+"sk-other")

	recorder, seen := runRealtimeSubprotocolAuth(t, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.NotNil(t, seen)
	require.Empty(t, seen.Get("Authorization"))
	require.Equal(t, []string{
		"realtime, " + RealtimeInsecureAPIKeyPrefix + key,
		RealtimeInsecureAPIKeyPrefix + "sk-other",
	}, seen.Values("Sec-WebSocket-Protocol"))
}

func TestIsRealtimeBetaRequest(t *testing.T) {
	tests := []struct {
		name      string
		beta      []string
		protocols []string
		want      bool
	}{
		{
			name: "no beta markers",
			want: false,
		},
		{
			name: "header realtime=v1",
			beta: []string{"realtime=v1"},
			want: true,
		},
		{
			name: "header with realtime=v1 after another value",
			beta: []string{"foo, realtime=v1"},
			want: true,
		},
		{
			name: "header is case-insensitive",
			beta: []string{"Realtime=V1"},
			want: true,
		},
		{
			name: "header on a second line",
			beta: []string{"assistants=v2", "realtime=v1"},
			want: true,
		},
		{
			name:      "beta subprotocol token",
			protocols: []string{"realtime, " + RealtimeBetaSubprotocol},
			want:      true,
		},
		{
			name:      "beta subprotocol token on its own line",
			protocols: []string{"realtime", RealtimeBetaSubprotocol},
			want:      true,
		},
		{
			name: "unrelated beta header value",
			beta: []string{"assistants=v2"},
			want: false,
		},
		{
			name: "realtime=v2 is not the removed beta",
			beta: []string{"realtime=v2"},
			want: false,
		},
		{
			name: "realtime=v1 must be a whole token",
			beta: []string{"xrealtime=v1"},
			want: false,
		},
		{
			name:      "plain realtime subprotocol is not beta",
			protocols: []string{"realtime"},
			want:      false,
		},
		{
			name:      "beta subprotocol must be a whole token",
			protocols: []string{"realtime, " + RealtimeBetaSubprotocol + "-extra"},
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			for _, line := range tt.beta {
				req.Header.Add("Openai-Beta", line)
			}

			for _, line := range tt.protocols {
				req.Header.Add("Sec-WebSocket-Protocol", line)
			}

			require.Equal(t, tt.want, IsRealtimeBetaRequest(req))
		})
	}
}

func TestRealtimeSubprotocols(t *testing.T) {
	tests := []struct {
		name      string
		protocols []string
		want      []string
	}{
		{
			name: "no header",
			want: nil,
		},
		{
			name:      "single token",
			protocols: []string{"realtime"},
			want:      []string{"realtime"},
		},
		{
			name:      "comma list with surrounding whitespace",
			protocols: []string{"  realtime ,openai-organization.org_1,  openai-project.proj_1  "},
			want:      []string{"realtime", "openai-organization.org_1", "openai-project.proj_1"},
		},
		{
			name:      "multiple header lines keep order",
			protocols: []string{"realtime", "openai-organization.org_1, openai-project.proj_1"},
			want:      []string{"realtime", "openai-organization.org_1", "openai-project.proj_1"},
		},
		{
			name:      "empty tokens are skipped",
			protocols: []string{"realtime,,", "", " , "},
			want:      []string{"realtime"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
			for _, line := range tt.protocols {
				req.Header.Add("Sec-WebSocket-Protocol", line)
			}

			require.Equal(t, tt.want, RealtimeSubprotocols(req))
		})
	}
}

func TestRealtimeSubprotocols_AfterMiddlewareExcludesKey(t *testing.T) {
	const key = "sk-test-subprotocol-key"

	req := newUpgradeRequest(t)
	req.Header.Add("Sec-WebSocket-Protocol", "realtime, "+RealtimeInsecureAPIKeyPrefix+key)
	req.Header.Add("Sec-WebSocket-Protocol", RealtimeBetaSubprotocol)

	var (
		tokens []string
		beta   bool
	)

	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(WithRealtimeSubprotocolAuth())
	router.GET("/v1/realtime", func(c *gin.Context) {
		tokens = RealtimeSubprotocols(c.Request)
		beta = IsRealtimeBetaRequest(c.Request)
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"realtime", RealtimeBetaSubprotocol}, tokens)
	require.True(t, beta)
}
