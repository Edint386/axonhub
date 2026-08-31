package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestCodexHandlers_StartOAuth_InvalidJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  httpclient.NewHttpClient(),
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(contexts.WithProjectID(c.Request.Context(), 123))
		c.Next()
	})
	router.POST("/admin/codex/oauth/start", h.StartOAuth)

	req := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/start", bytes.NewBufferString("{"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "invalid request format")
}

func TestCodexHandlers_StartOAuth_DoesNotIncludeOriginatorParam(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  httpclient.NewHttpClient(),
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(contexts.WithProjectID(c.Request.Context(), 123))
		c.Next()
	})
	router.POST("/admin/codex/oauth/start", h.StartOAuth)

	req := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/start", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.SessionID)

	parsed, err := url.Parse(resp.AuthURL)
	require.NoError(t, err)
	query := parsed.Query()
	require.Empty(t, query.Get("originator"))
	require.Equal(t, codex.ClientID, query.Get("client_id"))
	require.Equal(t, codex.RedirectURI, query.Get("redirect_uri"))
	require.Equal(t, "true", query.Get("codex_cli_simplified_flow"))
	require.Equal(t, resp.SessionID, query.Get("state"))
}

func TestCodexHandlers_HTTPClientForOAuthExchange_UsesProvidedProxyMode(t *testing.T) {
	baseClient := httpclient.NewHttpClientWithProxy(&httpclient.ProxyConfig{
		Type: httpclient.ProxyTypeURL,
		URL:  "http://base-proxy.example:8080",
	})
	h := &CodexHandlers{httpClient: baseClient}

	require.Same(t, baseClient, h.httpClientForOAuthExchange(nil))

	request := httptest.NewRequest(http.MethodPost, "https://oauth.example/token", nil)

	tests := []struct {
		name   string
		proxy  *httpclient.ProxyConfig
		assert func(t *testing.T, selected *httpclient.HttpClient)
	}{
		{
			name:  "disabled",
			proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
			assert: func(t *testing.T, selected *httpclient.HttpClient) {
				proxyURL, err := selected.ProxyFunc()(request)
				require.NoError(t, err)
				require.Nil(t, proxyURL)
			},
		},
		{
			name:  "environment",
			proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeEnvironment},
			assert: func(t *testing.T, selected *httpclient.HttpClient) {
				require.Equal(
					t,
					reflect.ValueOf(http.ProxyFromEnvironment).Pointer(),
					reflect.ValueOf(selected.ProxyFunc()).Pointer(),
				)
			},
		},
		{
			name: "url",
			proxy: &httpclient.ProxyConfig{
				Type: httpclient.ProxyTypeURL,
				URL:  "http://oauth-proxy.example:8080",
			},
			assert: func(t *testing.T, selected *httpclient.HttpClient) {
				proxyURL, err := selected.ProxyFunc()(request)
				require.NoError(t, err)
				require.Equal(t, "http://oauth-proxy.example:8080", proxyURL.String())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			selected := h.httpClientForOAuthExchange(tt.proxy)
			require.NotSame(t, baseClient, selected)
			tt.assert(t, selected)
		})
	}
}

func TestValidateCodexOAuthProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   *httpclient.ProxyConfig
		wantErr string
	}{
		{name: "nil", proxy: nil},
		{name: "disabled", proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled}},
		{name: "environment", proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeEnvironment}},
		{name: "url", proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "http://proxy.example:8080"}},
		{name: "empty url", proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "  "}, wantErr: "proxy URL is required"},
		{name: "unknown", proxy: &httpclient.ProxyConfig{Type: "unknown"}, wantErr: "unsupported proxy type"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCodexOAuthProxy(tt.proxy)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestCodexHandlers_Exchange_InvalidProxyDoesNotConsumeState(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  httpclient.NewHttpClient(),
	})

	router := gin.New()
	router.POST("/admin/codex/oauth/start", h.StartOAuth)
	router.POST("/admin/codex/oauth/exchange", h.Exchange)

	startReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/start", bytes.NewBufferString("{}"))
	startReq.Header.Set("Content-Type", "application/json")
	startW := httptest.NewRecorder()
	router.ServeHTTP(startW, startReq)
	require.Equal(t, http.StatusOK, startW.Code)

	var startResp StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &startResp))

	invalidProxyBody, err := json.Marshal(ExchangeCodexOAuthRequest{
		SessionID:   startResp.SessionID,
		CallbackURL: "http://localhost:1455/auth/callback?code=test-code&state=" + startResp.SessionID,
		Proxy:       &httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "  "},
	})
	require.NoError(t, err)

	invalidProxyReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(invalidProxyBody))
	invalidProxyReq.Header.Set("Content-Type", "application/json")
	invalidProxyW := httptest.NewRecorder()
	router.ServeHTTP(invalidProxyW, invalidProxyReq)
	require.Equal(t, http.StatusBadRequest, invalidProxyW.Code)
	require.Contains(t, invalidProxyW.Body.String(), "proxy URL is required")

	retryBody, err := json.Marshal(ExchangeCodexOAuthRequest{
		SessionID:   startResp.SessionID,
		CallbackURL: "http://localhost:1455/auth/callback?code=test-code&state=mismatch",
		Proxy:       &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled},
	})
	require.NoError(t, err)

	retryReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(retryBody))
	retryReq.Header.Set("Content-Type", "application/json")
	retryW := httptest.NewRecorder()
	router.ServeHTTP(retryW, retryReq)
	require.Equal(t, http.StatusBadRequest, retryW.Code)
	require.Contains(t, retryW.Body.String(), "oauth state mismatch")
}

func TestCodexHandlers_Exchange_StateDeletedOnTokenExchangeFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var tokenCalls int

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"bad_gateway"}`))
	}))
	t.Cleanup(tokenServer.Close)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codex.TokenURL {
			proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, tokenServer.URL, req.Body)
			if err != nil {
				return nil, err
			}

			proxyReq.Header = req.Header.Clone()

			return http.DefaultTransport.RoundTrip(proxyReq)
		}

		return http.DefaultTransport.RoundTrip(req)
	})

	hc := httpclient.NewHttpClientWithClient(&http.Client{Transport: transport})

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  hc,
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(contexts.WithProjectID(c.Request.Context(), 123))
		c.Next()
	})
	router.POST("/admin/codex/oauth/start", h.StartOAuth)
	router.POST("/admin/codex/oauth/exchange", h.Exchange)

	startReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/start", bytes.NewBufferString("{}"))
	startReq.Header.Set("Content-Type", "application/json")

	startW := httptest.NewRecorder()
	router.ServeHTTP(startW, startReq)
	require.Equal(t, http.StatusOK, startW.Code)

	var startResp StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &startResp))
	require.NotEmpty(t, startResp.SessionID)

	exchangeBody, err := json.Marshal(ExchangeCodexOAuthRequest{
		SessionID:   startResp.SessionID,
		CallbackURL: "http://localhost:1455/auth/callback?code=test-code&state=" + startResp.SessionID,
	})
	require.NoError(t, err)

	exchangeReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(exchangeBody))
	exchangeReq.Header.Set("Content-Type", "application/json")

	exchangeW := httptest.NewRecorder()
	router.ServeHTTP(exchangeW, exchangeReq)
	require.Equal(t, http.StatusBadGateway, exchangeW.Code)
	require.Equal(t, 1, tokenCalls)

	exchangeReq2 := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(exchangeBody))
	exchangeReq2.Header.Set("Content-Type", "application/json")

	exchangeW2 := httptest.NewRecorder()
	router.ServeHTTP(exchangeW2, exchangeReq2)
	require.Equal(t, http.StatusBadRequest, exchangeW2.Code)
	require.Contains(t, exchangeW2.Body.String(), "invalid or expired oauth session")
}

func TestCodexHandlers_Exchange_RejectsStateMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600,"token_type":"bearer"}`))
	}))
	t.Cleanup(tokenServer.Close)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codex.TokenURL {
			body, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()

			proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, tokenServer.URL, bytes.NewBuffer(body))
			if err != nil {
				return nil, err
			}

			proxyReq.Header = req.Header.Clone()

			return http.DefaultTransport.RoundTrip(proxyReq)
		}

		return http.DefaultTransport.RoundTrip(req)
	})

	hc := httpclient.NewHttpClientWithClient(&http.Client{Transport: transport})

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  hc,
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(contexts.WithProjectID(c.Request.Context(), 123))
		c.Next()
	})
	router.POST("/admin/codex/oauth/start", h.StartOAuth)
	router.POST("/admin/codex/oauth/exchange", h.Exchange)

	startReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/start", bytes.NewBufferString("{}"))
	startReq.Header.Set("Content-Type", "application/json")

	startW := httptest.NewRecorder()
	router.ServeHTTP(startW, startReq)
	require.Equal(t, http.StatusOK, startW.Code)

	var startResp StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &startResp))
	require.NotEmpty(t, startResp.SessionID)

	exchangeBody, err := json.Marshal(ExchangeCodexOAuthRequest{
		SessionID:   startResp.SessionID,
		CallbackURL: "http://localhost:1455/auth/callback?code=test-code&state=mismatch",
	})
	require.NoError(t, err)

	exchangeReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(exchangeBody))
	exchangeReq.Header.Set("Content-Type", "application/json")

	exchangeW := httptest.NewRecorder()
	router.ServeHTTP(exchangeW, exchangeReq)
	require.Equal(t, http.StatusBadRequest, exchangeW.Code)
	require.Contains(t, exchangeW.Body.String(), "oauth state mismatch")

	exchangeBody2, err := json.Marshal(ExchangeCodexOAuthRequest{
		SessionID:   startResp.SessionID,
		CallbackURL: "http://localhost:1455/auth/callback?code=test-code&state=" + startResp.SessionID,
	})
	require.NoError(t, err)

	exchangeReq2 := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(exchangeBody2))
	exchangeReq2.Header.Set("Content-Type", "application/json")

	exchangeW2 := httptest.NewRecorder()
	router.ServeHTTP(exchangeW2, exchangeReq2)
	require.Equal(t, http.StatusBadRequest, exchangeW2.Code)
	require.Contains(t, exchangeW2.Body.String(), "invalid or expired oauth session")
}

func TestCodexHandlers_Exchange_DeletesStateOnSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"a","refresh_token":"r","expires_in":3600,"token_type":"bearer"}`))
	}))
	t.Cleanup(tokenServer.Close)

	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() == codex.TokenURL {
			body, _ := io.ReadAll(req.Body)
			_ = req.Body.Close()

			proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, tokenServer.URL, bytes.NewBuffer(body))
			if err != nil {
				return nil, err
			}

			proxyReq.Header = req.Header.Clone()

			return http.DefaultTransport.RoundTrip(proxyReq)
		}

		return http.DefaultTransport.RoundTrip(req)
	})

	hc := httpclient.NewHttpClientWithClient(&http.Client{Transport: transport})

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  hc,
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(contexts.WithProjectID(c.Request.Context(), 123))
		c.Next()
	})
	router.POST("/admin/codex/oauth/start", h.StartOAuth)
	router.POST("/admin/codex/oauth/exchange", h.Exchange)

	startReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/start", bytes.NewBufferString("{}"))
	startReq.Header.Set("Content-Type", "application/json")

	startW := httptest.NewRecorder()
	router.ServeHTTP(startW, startReq)
	require.Equal(t, http.StatusOK, startW.Code)

	var startResp StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(startW.Body.Bytes(), &startResp))

	exchangeBody, err := json.Marshal(ExchangeCodexOAuthRequest{
		SessionID:   startResp.SessionID,
		CallbackURL: "http://localhost:1455/auth/callback?code=test-code&state=" + startResp.SessionID,
	})
	require.NoError(t, err)

	exchangeReq := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(exchangeBody))
	exchangeReq.Header.Set("Content-Type", "application/json")

	exchangeW := httptest.NewRecorder()
	router.ServeHTTP(exchangeW, exchangeReq)
	require.Equal(t, http.StatusOK, exchangeW.Code)

	exchangeReq2 := httptest.NewRequest(http.MethodPost, "/admin/codex/oauth/exchange", bytes.NewBuffer(exchangeBody))
	exchangeReq2.Header.Set("Content-Type", "application/json")

	exchangeW2 := httptest.NewRecorder()
	router.ServeHTTP(exchangeW2, exchangeReq2)
	require.Equal(t, http.StatusBadRequest, exchangeW2.Code)
	require.Contains(t, exchangeW2.Body.String(), "invalid or expired oauth session")
}

func TestCodexHandlers_DecodeAuthJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	h := NewCodexHandlers(CodexHandlersParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		HttpClient:  httpclient.NewHttpClient(),
	})

	router := gin.New()
	router.POST("/admin/codex/auth/decode", h.DecodeAuthJSON)

	req := httptest.NewRequest(http.MethodPost, "/admin/codex/auth/decode", bytes.NewBufferString(`{
		"auth_json":"{\"auth_mode\":\"chatgpt\",\"last_refresh\":\"2026-04-17T08:58:36.389Z\",\"tokens\":{\"access_token\":\"access\",\"refresh_token\":\"refresh\",\"id_token\":\"id\"}}"
	}`))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var resp DecodeCodexAuthJSONResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Contains(t, resp.Credentials, `"client_id":"`+codex.ClientID+`"`)
	require.Contains(t, resp.Credentials, `"access_token":"access"`)
	require.Contains(t, resp.Credentials, `"token_type":"bearer"`)
}
