package realtime

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

func TestWebsocketProxyFunc(t *testing.T) {
	fixed := func(raw string) func(*http.Request) (*url.URL, error) {
		return func(*http.Request) (*url.URL, error) {
			if raw == "" {
				return nil, nil
			}

			return url.Parse(raw)
		}
	}

	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "api.example.com"}}

	tests := []struct {
		name    string
		proxy   string
		want    string
		wantErr error
	}{
		{name: "http proxies pass through", proxy: "http://user:pw@proxy.example.com:3128", want: "http://user:pw@proxy.example.com:3128"},
		{name: "socks5 proxies pass through", proxy: "socks5://proxy.example.com:1080", want: "socks5://proxy.example.com:1080"},
		{name: "socks5h is rewritten to the socks5 gorilla knows", proxy: "SOCKS5H://user:pw@proxy.example.com:1080", want: "socks5://user:pw@proxy.example.com:1080"},
		{name: "unknown schemes are rejected", proxy: "ftp://proxy.example.com:21", wantErr: ErrUnsupportedProxy},
		{name: "https proxies are rejected", proxy: "https://proxy.example.com:443", wantErr: ErrUnsupportedProxy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := websocketProxyFunc(fixed(tt.proxy))(req)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				require.Nil(t, got)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tt.want, got.String())
		})
	}

	t.Run("no proxy stays no proxy", func(t *testing.T) {
		got, err := websocketProxyFunc(fixed(""))(req)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("a nil proxy function stays nil", func(t *testing.T) {
		require.Nil(t, websocketProxyFunc(nil))
	})

	t.Run("proxy function errors propagate", func(t *testing.T) {
		boom := errors.New("boom")

		_, err := websocketProxyFunc(func(*http.Request) (*url.URL, error) { return nil, boom })(req)
		require.ErrorIs(t, err, boom)
	})
}

func TestNewDialer(t *testing.T) {
	t.Run("without a client the environment proxy applies and is checked", func(t *testing.T) {
		dialer, err := newDialer(nil, "wss://api.example.com/v1/realtime", time.Second)
		require.NoError(t, err)
		require.NotNil(t, dialer.Proxy, "the environment proxy function is installed")
		require.Equal(t, time.Second, dialer.HandshakeTimeout)
		require.NotNil(t, dialer.TLSClientConfig)
		require.Equal(t, []string{"http/1.1"}, dialer.TLSClientConfig.NextProtos)
	})

	t.Run("a channel http proxy is accepted", func(t *testing.T) {
		hc := httpclient.NewHttpClientWithProxy(&httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "http://proxy.example.com:3128"})

		dialer, err := newDialer(hc, "wss://api.example.com/v1/realtime", time.Second)
		require.NoError(t, err)

		proxyURL, err := proxyForURL(dialer.Proxy, "wss://api.example.com/v1/realtime")
		require.NoError(t, err)
		require.Equal(t, "http://proxy.example.com:3128", proxyURL.String())
	})

	t.Run("a disabled channel proxy dials directly", func(t *testing.T) {
		hc := httpclient.NewHttpClientWithProxy(&httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled})

		dialer, err := newDialer(hc, "wss://api.example.com/v1/realtime", time.Second)
		require.NoError(t, err)

		proxyURL, err := proxyForURL(dialer.Proxy, "wss://api.example.com/v1/realtime")
		require.NoError(t, err)
		require.Nil(t, proxyURL)
	})

	t.Run("an invalid websocket url is rejected", func(t *testing.T) {
		_, err := newDialer(nil, "wss://bad host/realtime", time.Second)
		require.Error(t, err)
	})
}

// handshakeTestServer serves the handshake outcomes DialUpstream must classify.
func handshakeTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	upgrader := websocket.Upgrader{}

	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		_ = conn.Close()
	})
	mux.HandleFunc("/unauthorized", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
	})
	mux.HandleFunc("/plain", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>not a websocket endpoint</html>"))
	})
	mux.HandleFunc("/bad-accept", func(w http.ResponseWriter, _ *http.Request) {
		// A 101 whose Sec-WebSocket-Accept does not match the challenge, as a broken proxy
		// would produce: gorilla returns the response together with ErrBadHandshake.
		hijacker, ok := w.(http.Hijacker)
		require.True(t, ok)

		conn, rw, err := hijacker.Hijack()
		require.NoError(t, err)

		defer func() { _ = conn.Close() }()

		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: bm90LXRoZS1yaWdodC1hY2NlcHQ=\r\n\r\n")
		_ = rw.Flush()
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}

		w.WriteHeader(http.StatusServiceUnavailable)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

func TestDialUpstream(t *testing.T) {
	server := handshakeTestServer(t)
	base := "ws" + strings.TrimPrefix(server.URL, "http")

	t.Run("a completed handshake returns the connection", func(t *testing.T) {
		conn, header, err := DialUpstream(t.Context(), nil, base+"/ok", http.Header{"Authorization": []string{"Bearer x"}}, 2*time.Second)
		require.NoError(t, err)
		require.NotNil(t, conn)
		require.Equal(t, "websocket", header.Get("Upgrade"))
		_ = conn.Close()
	})

	t.Run("a rejected handshake keeps the upstream status, headers and body", func(t *testing.T) {
		_, header, err := DialUpstream(t.Context(), nil, base+"/unauthorized", nil, 2*time.Second)
		require.Error(t, err)

		var herr *httpclient.Error
		require.ErrorAs(t, err, &herr)
		require.Equal(t, http.StatusUnauthorized, herr.StatusCode)
		require.Contains(t, string(herr.Body), "invalid_api_key")
		require.Equal(t, "7", herr.Headers.Get("Retry-After"))
		require.Equal(t, "7", header.Get("Retry-After"))
		require.NotErrorIs(t, err, ErrDialNetwork)
	})

	t.Run("a non-websocket 200 answer is a network failure not an upstream status", func(t *testing.T) {
		_, _, err := DialUpstream(t.Context(), nil, base+"/plain", nil, 2*time.Second)
		require.ErrorIs(t, err, ErrDialNetwork)
		require.ErrorIs(t, err, websocket.ErrBadHandshake)

		var herr *httpclient.Error
		require.False(t, errors.As(err, &herr), "a 2xx answer must not be reported as an upstream rejection")
	})

	t.Run("a 101 with a wrong accept key is a network failure", func(t *testing.T) {
		_, _, err := DialUpstream(t.Context(), nil, base+"/bad-accept", nil, 2*time.Second)
		require.ErrorIs(t, err, ErrDialNetwork)

		var herr *httpclient.Error
		require.False(t, errors.As(err, &herr), "a failed 101 must not be reported with status 101")
	})

	t.Run("a slow handshake times out", func(t *testing.T) {
		_, _, err := DialUpstream(t.Context(), nil, base+"/slow", nil, 100*time.Millisecond)
		require.ErrorIs(t, err, ErrDialTimeout)
	})

	t.Run("an unreachable upstream is a network failure", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		addr := closed.URL
		closed.Close()

		_, _, err := DialUpstream(t.Context(), nil, "ws"+strings.TrimPrefix(addr, "http")+"/realtime", nil, 2*time.Second)
		require.ErrorIs(t, err, ErrDialNetwork)
	})
}

func TestUpstreamFailure(t *testing.T) {
	t.Run("nil stays nil", func(t *testing.T) {
		require.NoError(t, UpstreamFailure(nil, http.StatusBadGateway))
	})

	t.Run("a rejected handshake becomes a response error with the parsed detail", func(t *testing.T) {
		rejected := &httpclient.Error{
			Method: http.MethodGet, URL: "wss://api.example.com/v1/realtime", StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized",
			Body:    []byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`),
			Headers: http.Header{"Retry-After": []string{"3"}},
		}

		err := UpstreamFailure(rejected, http.StatusUnauthorized)
		require.True(t, pipeline.IsUpstreamError(err))

		var respErr *llm.ResponseError
		require.ErrorAs(t, err, &respErr)
		require.Equal(t, http.StatusUnauthorized, respErr.StatusCode)
		require.Equal(t, llm.ErrorDetail{Message: "Incorrect API key provided", Type: "invalid_request_error", Code: "invalid_api_key"}, respErr.Detail)

		var herr *httpclient.Error
		require.False(t, errors.As(err, &herr), "the raw answer must not be reachable through the error chain")

		answer, ok := UpstreamAnswer(err)
		require.True(t, ok)
		require.Same(t, rejected, answer)

		require.Equal(t, err, UpstreamFailure(err, http.StatusBadGateway), "marking is idempotent")
	})

	t.Run("a non-json body is the message", func(t *testing.T) {
		err := UpstreamFailure(&httpclient.Error{StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: []byte("<html>blocked</html>")}, 0)

		var respErr *llm.ResponseError
		require.ErrorAs(t, err, &respErr)
		require.Equal(t, http.StatusForbidden, respErr.StatusCode)
		require.Equal(t, llm.ErrorDetail{Message: "<html>blocked</html>", Type: "upstream_error"}, respErr.Detail)

		empty := UpstreamFailure(&httpclient.Error{StatusCode: http.StatusBadGateway, Status: "502 Bad Gateway"}, 0)
		require.ErrorAs(t, empty, &respErr)
		require.Equal(t, "502 Bad Gateway", respErr.Detail.Message)
	})

	t.Run("transport failures keep their status and sentinel", func(t *testing.T) {
		timeout := UpstreamFailure(fmt.Errorf("%w: dial tcp: i/o timeout", ErrDialTimeout), http.StatusGatewayTimeout)
		require.ErrorIs(t, timeout, ErrDialTimeout)

		var respErr *llm.ResponseError
		require.ErrorAs(t, timeout, &respErr)
		require.Equal(t, http.StatusGatewayTimeout, respErr.StatusCode)
		require.Equal(t, "upstream_error", respErr.Detail.Type)

		network := UpstreamFailure(fmt.Errorf("%w: connection refused", ErrDialNetwork), 0)
		require.ErrorIs(t, network, ErrDialNetwork)
		require.ErrorAs(t, network, &respErr)
		require.Equal(t, http.StatusBadGateway, respErr.StatusCode, "no status means 502")

		_, ok := UpstreamAnswer(network)
		require.False(t, ok)
	})
}

func TestIsGatewayError(t *testing.T) {
	require.False(t, IsGatewayError(nil))
	require.True(t, IsGatewayError(ErrTooManySessions))
	require.True(t, IsGatewayError(fmt.Errorf("%w: %w", ErrChannelBusy, ErrDialTimeout)))
	require.True(t, IsGatewayError(fmt.Errorf("%w: x", ErrUnsupportedModel)))
	require.True(t, IsGatewayError(notFoundCall("rtc_1").Err))
	require.False(t, IsGatewayError(ErrDialNetwork))
	require.False(t, IsGatewayError(&httpclient.Error{StatusCode: http.StatusNotFound, Body: []byte(`{"error":{"code":"other"}}`)}))
}
