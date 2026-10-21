package realtime

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm/httpclient"
)

// oversizedCloseFrame builds a server close frame whose reason exceeds the control-frame limit.
func oversizedCloseFrame(code int, reason string) []byte {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, uint16(code)) //nolint:gosec // G115: close codes fit in 16 bits
	copy(payload[2:], reason)

	frame := []byte{0x88, 0x7e, 0, 0}
	binary.BigEndian.PutUint16(frame[2:], uint16(len(payload))) //nolint:gosec // G115: bounded by the test string

	return append(frame, payload...)
}

func TestTailConn_OversizedCloseScan(t *testing.T) {
	reason := strings.Repeat("quota exhausted; ", 11) // 187 bytes
	c := &tailConn{}

	// Older bytes (a text frame) followed by the close frame, split across reads.
	frame := append([]byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'}, oversizedCloseFrame(1007, reason)...)
	c.tail = append(c.tail, frame[:10]...)
	c.tail = append(c.tail, frame[10:]...)

	code, got, ok := c.OversizedClose()
	require.True(t, ok)
	require.Equal(t, 1007, code)
	require.Equal(t, reason, got)

	// A normal-sized close (0x7e not used) is not an oversized close.
	c.tail = websocket.FormatCloseMessage(1000, "bye")
	_, _, ok = c.OversizedClose()
	require.False(t, ok)

	// Invalid status codes are skipped.
	c.tail = oversizedCloseFrame(7, strings.Repeat("x", 130))
	_, _, ok = c.OversizedClose()
	require.False(t, ok)
}

func TestDialUpstream_InstallsTailOnDirectDials(t *testing.T) {
	upgrader := websocket.Upgrader{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created"}`))
		_, _, _ = conn.ReadMessage()
	})

	t.Run("ws", func(t *testing.T) {
		server := httptest.NewServer(handler)
		t.Cleanup(server.Close)

		conn, _, err := DialUpstream(t.Context(), nil, wsURL(server), nil, 3*time.Second)
		require.NoError(t, err)

		t.Cleanup(func() { _ = conn.Close() })

		_, isTail := conn.UnderlyingConn().(*tailConn)
		require.True(t, isTail)

		_, data, err := conn.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, "session.created", gjson.GetBytes(data, "type").String())
	})

	t.Run("wss through the channel client", func(t *testing.T) {
		server := httptest.NewTLSServer(handler)
		t.Cleanup(server.Close)

		hc := httpclient.NewHttpClient(httpclient.WithInsecureSkipVerify(true))

		conn, _, err := DialUpstream(t.Context(), hc, "wss"+strings.TrimPrefix(server.URL, "https"), nil, 3*time.Second)
		require.NoError(t, err)

		t.Cleanup(func() { _ = conn.Close() })

		tc, isTail := conn.UnderlyingConn().(*tailConn)
		require.True(t, isTail)

		_, isTLS := tc.Conn.(interface{ ConnectionState() any })
		_ = isTLS // the wrapped conn is a *tls.Conn; its concrete type is not asserted here

		_, data, err := conn.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, "session.created", gjson.GetBytes(data, "type").String())
	})

	t.Run("proxied dials keep gorilla's own setup", func(t *testing.T) {
		dialer, err := newDialer(httpclient.NewHttpClientWithProxy(&httpclient.ProxyConfig{Type: httpclient.ProxyTypeURL, URL: "http://127.0.0.1:9"}), "wss://example.invalid/v1/realtime", time.Second)
		require.NoError(t, err)
		require.Nil(t, dialer.NetDialTLSContext)
	})
}

func TestSession_OversizedUpstreamCloseReasonReachesClient(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	reason := `The free tier of the model has been exhausted. If you wish to continue access the model on a paid basis, please disable the "use free tier only" mode in the management console.`
	require.Greater(t, len(reason), 125)

	<-h.upstream.ready
	h.upstream.mu.Lock()
	raw, isConn := h.upstream.conn.UnderlyingConn().(net.Conn)
	require.True(t, isConn)
	_, err := raw.Write(oversizedCloseFrame(1007, reason))
	h.upstream.mu.Unlock()
	require.NoError(t, err)

	// The client first gets a GA error event carrying the whole reason...
	frame, err := readFrame(t, h.client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "error", gjson.Get(frame, "type").String())
	require.Equal(t, "upstream_closed", gjson.Get(frame, "error.code").String())
	require.Equal(t, reason, gjson.Get(frame, "error.message").String())

	// ...then the real close code with the reason cut to what a close frame can carry.
	_, err = readFrame(t, h.client, 3*time.Second)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, 1007, ce.Code)
	require.LessOrEqual(t, len(ce.Text), 123)
	require.True(t, strings.HasPrefix(reason, ce.Text))

	summary := h.wait(t)
	require.Equal(t, CauseUpstreamClosed, summary.Cause)
	require.Equal(t, "1007", summary.Code)
	require.Equal(t, reason, summary.Reason)
	require.True(t, summary.Failed())
}
