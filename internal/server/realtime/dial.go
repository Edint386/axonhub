package realtime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/looplj/axonhub/llm/httpclient"
)

var (
	// ErrDialTimeout is returned when the upstream handshake did not complete in time.
	ErrDialTimeout = errors.New("realtime: upstream websocket handshake timed out")
	// ErrDialNetwork is returned when the upstream could not be reached at all.
	ErrDialNetwork = errors.New("realtime: upstream websocket dial failed")
	// ErrUnsupportedProxy is returned when the channel proxy cannot carry a WebSocket dial.
	ErrUnsupportedProxy = errors.New("realtime: channel proxy scheme is not supported for websocket dials")
)

// maxHandshakeErrorBody caps the rejected-handshake body kept for error reporting.
const maxHandshakeErrorBody = 64 << 10

// hopByHopHeaders must never be forwarded on a WebSocket handshake; gorilla sets its own.
var hopByHopHeaders = []string{
	"Connection", "Upgrade", "Keep-Alive", "Proxy-Connection", "Transfer-Encoding", "Te", "Trailer",
	"Host", "Content-Length", "Content-Type",
	"Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol", "Sec-Websocket-Accept",
}

// DialUpstream opens a WebSocket connection to wsURL reusing the channel HTTP client's proxy and
// TLS settings. A rejected handshake is returned as *httpclient.Error carrying the upstream
// status, headers and body; timeouts wrap ErrDialTimeout and other failures wrap ErrDialNetwork.
func DialUpstream(ctx context.Context, hc *httpclient.HttpClient, wsURL string, headers http.Header, handshakeTimeout time.Duration) (*websocket.Conn, http.Header, error) {
	dialer, err := newDialer(hc, wsURL, handshakeTimeout)
	if err != nil {
		return nil, nil, err
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, sanitizeDialHeaders(headers))
	if err != nil {
		// gorilla also hands back the response for a 101 whose Upgrade, Connection or
		// Sec-WebSocket-Accept header is wrong (a misbehaving proxy, say); only a real
		// rejection (3xx and above) is an upstream answer worth reporting with its status.
		if resp != nil && resp.StatusCode >= http.StatusMultipleChoices {
			return nil, resp.Header, handshakeError(wsURL, resp)
		}

		if resp != nil {
			_ = resp.Body.Close()

			return nil, resp.Header, fmt.Errorf("%w: %w (upstream answered %s without completing the websocket handshake)", ErrDialNetwork, err, resp.Status)
		}

		if isTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, fmt.Errorf("%w: %w", ErrDialTimeout, err)
		}

		return nil, nil, fmt.Errorf("%w: %w", ErrDialNetwork, err)
	}

	var respHeader http.Header
	if resp != nil {
		respHeader = resp.Header
	}

	return conn, respHeader, nil
}

// newDialer builds a gorilla Dialer from the channel HTTP client: proxy function, the
// transport's DialContext (so SOCKS proxies configured on the channel are honoured) and a TLS
// config pinned to HTTP/1.1 (gorilla cannot speak h2). Without a client the environment proxy
// applies; either way the proxy scheme is checked before dialing.
func newDialer(hc *httpclient.HttpClient, wsURL string, handshakeTimeout time.Duration) (*websocket.Dialer, error) {
	dialer := &websocket.Dialer{
		HandshakeTimeout: handshakeTimeout,
	}

	proxyFunc := http.ProxyFromEnvironment
	if hc != nil {
		if channelProxy := hc.ProxyFunc(); channelProxy != nil {
			proxyFunc = channelProxy
		}
	}

	dialer.Proxy = websocketProxyFunc(proxyFunc)

	proxyURL, err := proxyForURL(dialer.Proxy, wsURL)
	if err != nil {
		return nil, err
	}

	if hc != nil {
		if native := hc.GetNativeClient(); native != nil {
			if transport, ok := native.Transport.(*http.Transport); ok && transport != nil {
				if transport.DialContext != nil {
					dialer.NetDialContext = transport.DialContext
				}

				if transport.TLSClientConfig != nil {
					tlsConfig := transport.TLSClientConfig.Clone()
					tlsConfig.NextProtos = []string{"http/1.1"}
					dialer.TLSClientConfig = tlsConfig
				}
			}
		}
	}

	if dialer.TLSClientConfig == nil {
		dialer.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	}

	// Direct dials keep a tail of the stream so oversized close reasons survive (see tailconn.go).
	// Through a proxy gorilla owns the connection setup, so the tail is skipped there.
	if proxyURL == nil {
		installTailDial(dialer)
	}

	return dialer, nil
}

// websocketProxyFunc adapts a proxy function to what gorilla can dial: gorilla's proxy
// registry only knows "http" and "socks5", so "socks5h" (same SOCKS5 dial, hostname resolved by
// the proxy, which gorilla's SOCKS5 dialer does anyway) is rewritten to "socks5" and every other
// scheme fails with ErrUnsupportedProxy. A nil proxy function is passed through unchanged.
func websocketProxyFunc(proxyFunc func(*http.Request) (*url.URL, error)) func(*http.Request) (*url.URL, error) {
	if proxyFunc == nil {
		return nil
	}

	return func(r *http.Request) (*url.URL, error) {
		proxyURL, err := proxyFunc(r)
		if err != nil || proxyURL == nil {
			return proxyURL, err
		}

		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "socks5":
			return proxyURL, nil
		case "socks5h":
			rewritten := *proxyURL
			rewritten.Scheme = "socks5"

			return &rewritten, nil
		default:
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedProxy, proxyURL.Scheme)
		}
	}
}

// proxyForURL evaluates the proxy function for the https form of the websocket URL. A nil
// proxy function yields no proxy.
func proxyForURL(proxyFunc func(*http.Request) (*url.URL, error), wsURL string) (*url.URL, error) {
	if proxyFunc == nil {
		return nil, nil
	}

	target, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("parse websocket url: %w", err)
	}

	switch target.Scheme {
	case "wss":
		target.Scheme = "https"
	case "ws":
		target.Scheme = "http"
	}

	req := &http.Request{Method: http.MethodGet, URL: target, Header: http.Header{}, Host: target.Host}

	return proxyFunc(req)
}

// sanitizeDialHeaders drops hop-by-hop and WebSocket handshake headers from a header set that
// is about to be sent on an upstream dial.
func sanitizeDialHeaders(headers http.Header) http.Header {
	out := http.Header{}

	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		if isHopByHop(canonical) {
			continue
		}

		for _, v := range values {
			out.Add(canonical, v)
		}
	}

	return out
}

func isHopByHop(canonical string) bool {
	return slices.Contains(hopByHopHeaders, canonical)
}

// handshakeError converts a rejected handshake response into *httpclient.Error.
func handshakeError(wsURL string, resp *http.Response) error {
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHandshakeErrorBody))

	return &httpclient.Error{
		Method:     http.MethodGet,
		URL:        wsURL,
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       body,
		Headers:    resp.Header,
	}
}

// ToWebSocketURL converts an http(s) URL into its ws(s) form; ws(s) URLs are returned unchanged.
func ToWebSocketURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("invalid websocket url: %w", err)
	}

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
	default:
		return "", fmt.Errorf("unsupported websocket url scheme %q", u.Scheme)
	}

	return u.String(), nil
}
