package realtime

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/lifecycle"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// passDialect forwards frames unchanged and reports response state transitions.
type passDialect struct {
	gated    bool
	startOut [][]byte
}

func (d *passDialect) Name() string { return "test" }

func (d *passDialect) Start() ([][]byte, bool) { return d.startOut, d.gated }

func (d *passDialect) ClientFrame(frame []byte) (dialect.Output, error) {
	return dialect.Output{ToUpstream: [][]byte{frame}}, nil
}

func (d *passDialect) UpstreamFrame(frame []byte) (dialect.Output, error) {
	out := dialect.Output{ToClient: [][]byte{frame}}

	switch dialect.EventType(frame) {
	case "response.created":
		v := true
		out.ResponseInProgress = &v
	case "response.done":
		v := false
		out.ResponseInProgress = &v
		out.Usage = []dialect.UsageRecord{{Kind: "response", ResponseID: gjson.GetBytes(frame, "response.id").String()}}
	case "session.updated":
		out.OpenGate = true
	}

	return out, nil
}

func (d *passDialect) CloseUpstream() [][]byte { return [][]byte{[]byte(`{"type":"session.finish"}`)} }

// recordingHooks collects hook calls.
type recordingHooks struct {
	mu      sync.Mutex
	usage   []dialect.UsageRecord
	summary *EndSummary
	ended   chan struct{}
	check   func() error
}

func newRecordingHooks() *recordingHooks { return &recordingHooks{ended: make(chan struct{})} }

func (h *recordingHooks) OnUsage(_ context.Context, rec dialect.UsageRecord) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.usage = append(h.usage, rec)
}

func (h *recordingHooks) OnCheck(context.Context) error {
	if h.check != nil {
		return h.check()
	}

	return nil
}

func (h *recordingHooks) OnFirstUpstreamFrame(time.Duration) {}

func (h *recordingHooks) OnEnd(_ context.Context, summary EndSummary) {
	h.mu.Lock()
	h.summary = &summary
	h.mu.Unlock()
	close(h.ended)
}

// fakeUpstream is a WebSocket server that records received frames and lets tests push frames.
type fakeUpstream struct {
	server   *httptest.Server
	mu       sync.Mutex
	received []string
	conn     *websocket.Conn
	ready    chan struct{}
	closed   chan *websocket.CloseError
	onConn   func(*websocket.Conn)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()

	f := &fakeUpstream{ready: make(chan struct{}), closed: make(chan *websocket.CloseError, 1)}
	upgrader := websocket.Upgrader{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		f.mu.Lock()
		f.conn = conn
		f.mu.Unlock()
		close(f.ready)

		if f.onConn != nil {
			f.onConn(conn)
		}

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				var ce *websocket.CloseError
				if errors.As(err, &ce) {
					f.closed <- ce
				} else {
					f.closed <- &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: err.Error()}
				}

				return
			}

			f.mu.Lock()
			f.received = append(f.received, string(data))
			f.mu.Unlock()
		}
	}))

	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeUpstream) send(t *testing.T, frame string) {
	t.Helper()
	<-f.ready

	f.mu.Lock()
	defer f.mu.Unlock()

	require.NoError(t, f.conn.WriteMessage(websocket.TextMessage, []byte(frame)))
}

func (f *fakeUpstream) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.received...)
}

func (f *fakeUpstream) waitFor(t *testing.T, n int) []string {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.got(); len(got) >= n {
			return got
		}

		time.Sleep(10 * time.Millisecond)
	}

	return f.got()
}

func wsURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// harness runs a Session between a test client and the fake upstream.
type harness struct {
	upstream *fakeUpstream
	hooks    *recordingHooks
	client   *websocket.Conn
	gateway  *httptest.Server
	session  *Session
	runDone  chan EndSummary
}

func newHarness(t *testing.T, cfg Config, d dialect.Dialect, hooks *recordingHooks) *harness {
	t.Helper()

	return newHarnessWith(t, t.Context(), cfg, d, hooks, false)
}

// newHarnessWith builds a harness with an explicit base context (e.g. one carrying a lifecycle
// manager) and the sideband flag.
func newHarnessWith(t *testing.T, ctx context.Context, cfg Config, d dialect.Dialect, hooks *recordingHooks, sideband bool) *harness {
	t.Helper()

	up := newFakeUpstream(t)
	cfg = cfg.withDefaults()

	upConn, _, err := DialUpstream(t.Context(), nil, wsURL(up.server), nil, 3*time.Second)
	require.NoError(t, err)

	h := &harness{upstream: up, hooks: hooks, runDone: make(chan EndSummary, 1)}
	h.session = NewSession(ctx, SessionParams{Config: cfg, Dialect: d, Upstream: upConn, Hooks: hooks, Name: "test", Sideband: sideband})

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	h.gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		h.runDone <- h.session.Run(conn)
	}))
	t.Cleanup(h.gateway.Close)

	client, _, err := websocket.DefaultDialer.Dial(wsURL(h.gateway), nil)
	require.NoError(t, err)

	h.client = client

	return h
}

func (h *harness) wait(t *testing.T) EndSummary {
	t.Helper()

	select {
	case s := <-h.runDone:
		return s
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end")
		return EndSummary{}
	}
}

func readFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) (string, error) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(timeout)))

	_, data, err := conn.ReadMessage()

	return string(data), err
}

func testConfig() Config {
	return Config{
		PingInterval:       50 * time.Millisecond,
		PongWait:           2 * time.Second,
		IdleTimeout:        5 * time.Second,
		WriteTimeout:       time.Second,
		CloseGrace:         500 * time.Millisecond,
		ResponseDrain:      500 * time.Millisecond,
		GateTimeout:        time.Second,
		QuotaCheckInterval: time.Hour,
		MaxSessionDuration: time.Minute,
	}.withDefaults()
}

func TestSession_RelaysBothDirectionsAndRecordsUsage(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update","session":{}}`)))
	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`)))

	got := h.upstream.waitFor(t, 2)
	require.Equal(t, []string{`{"type":"session.update","session":{}}`, `{"type":"input_audio_buffer.append","audio":"AAAA"}`}, got)

	h.upstream.send(t, `{"type":"response.created","response":{"id":"resp_1"}}`)
	h.upstream.send(t, `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":1}}}`)

	first, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.Get(first, "type").String())

	second, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.done", gjson.Get(second, "type").String())

	require.NoError(t, h.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second)))

	summary := h.wait(t)
	require.Equal(t, CauseClientClosed, summary.Cause)
	require.False(t, summary.Failed())
	require.Equal(t, int64(2), summary.Events)
	require.Equal(t, int64(1), summary.Responses)

	<-hooks.ended
	require.Len(t, hooks.usage, 1)
	require.Equal(t, "resp_1", hooks.usage[0].ResponseID)

	// The upstream received the dialect goodbye frame and then a normal close.
	frames := h.upstream.waitFor(t, 3)
	require.Equal(t, `{"type":"session.finish"}`, frames[len(frames)-1])

	select {
	case ce := <-h.upstream.closed:
		require.Equal(t, websocket.CloseNormalClosure, ce.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive a close frame")
	}
}

func TestSession_UpstreamCloseIsMappedToClient(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	<-h.upstream.ready
	h.upstream.mu.Lock()
	require.NoError(t, h.upstream.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1006, "x"), time.Now().Add(time.Second)))
	h.upstream.mu.Unlock()

	_, err := readFrame(t, h.client, 3*time.Second)
	require.Error(t, err)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	// 1006 is not a valid wire close code: gorilla treats the upstream frame as a protocol
	// error, which the session reports as an upstream error and maps to 1011 for the client.
	require.Equal(t, websocket.CloseInternalServerErr, ce.Code)

	summary := h.wait(t)
	require.Equal(t, CauseUpstreamError, summary.Cause)
	require.True(t, summary.Failed())
}

func TestSession_UpstreamNormalCloseForwarded(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	<-h.upstream.ready
	h.upstream.mu.Lock()
	require.NoError(t, h.upstream.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"), time.Now().Add(time.Second)))
	h.upstream.mu.Unlock()

	_, err := readFrame(t, h.client, 3*time.Second)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, websocket.CloseNormalClosure, ce.Code)
	require.Equal(t, "done", ce.Text)

	summary := h.wait(t)
	require.Equal(t, CauseUpstreamClosed, summary.Cause)
	require.False(t, summary.Failed())
}

func TestSession_PolicyCloseSendsErrorBeforeClose(t *testing.T) {
	hooks := newRecordingHooks()
	hooks.check = func() error { return errors.New("quota exceeded") }

	cfg := testConfig()
	cfg.QuotaCheckInterval = 100 * time.Millisecond

	h := newHarness(t, cfg, &passDialect{}, hooks)

	frame, err := readFrame(t, h.client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "error", gjson.Get(frame, "type").String())
	require.Equal(t, "quota exceeded", gjson.Get(frame, "error.message").String())

	_, err = readFrame(t, h.client, 3*time.Second)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, websocket.ClosePolicyViolation, ce.Code)

	summary := h.wait(t)
	require.Equal(t, CausePolicy, summary.Cause)
}

func TestSession_GateQueuesClientFramesUntilOpened(t *testing.T) {
	hooks := newRecordingHooks()
	d := &passDialect{gated: true, startOut: [][]byte{[]byte(`{"type":"session.update","event_id":"init"}`)}}
	h := newHarness(t, testConfig(), d, hooks)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"a"}`)))
	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"b"}`)))

	// Only the init frame reaches the upstream while the gate is closed.
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, []string{`{"type":"session.update","event_id":"init"}`}, h.upstream.got())

	h.upstream.send(t, `{"type":"session.updated"}`)

	got := h.upstream.waitFor(t, 3)
	require.Equal(t, []string{`{"type":"session.update","event_id":"init"}`, `{"type":"a"}`, `{"type":"b"}`}, got)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"c"}`)))
	got = h.upstream.waitFor(t, 4)
	require.Equal(t, `{"type":"c"}`, got[3])

	require.NoError(t, h.client.Close())
	summary := h.wait(t)
	require.Equal(t, CauseClientError, summary.Cause)
}

func TestSession_GateTimeoutClosesWith1011(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	cfg.GateTimeout = 300 * time.Millisecond

	h := newHarness(t, cfg, &passDialect{gated: true}, hooks)

	frame, err := readFrame(t, h.client, 3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "error", gjson.Get(frame, "type").String())

	_, err = readFrame(t, h.client, 3*time.Second)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, websocket.CloseInternalServerErr, ce.Code)

	summary := h.wait(t)
	require.Equal(t, CauseGateTimeout, summary.Cause)
	require.True(t, summary.Failed())
}

func TestSession_MaxDurationEndsNormally(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	cfg.MaxSessionDuration = 1200 * time.Millisecond

	h := newHarness(t, cfg, &passDialect{}, hooks)

	_, err := readFrame(t, h.client, 5*time.Second)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, websocket.CloseNormalClosure, ce.Code)

	summary := h.wait(t)
	require.Equal(t, CauseMaxDuration, summary.Cause)
	require.False(t, summary.Failed())
}

func TestSession_ClientVanishesMidResponseStillRecordsUsage(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	h.upstream.send(t, `{"type":"response.created","response":{"id":"resp_9"}}`)

	first, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.Get(first, "type").String())

	// The upstream answers response.cancel with response.done carrying usage.
	h.upstream.onConn = nil

	go func() {
		got := h.upstream.waitFor(t, 1)
		if len(got) > 0 && strings.Contains(got[0], "response.cancel") {
			h.upstream.send(t, `{"type":"response.done","response":{"id":"resp_9","usage":{"input_tokens":5}}}`)
		}
	}()

	// Client drops the TCP connection without a close frame.
	require.NoError(t, h.client.UnderlyingConn().Close())

	summary := h.wait(t)
	require.Equal(t, CauseClientError, summary.Cause)

	<-hooks.ended
	require.Len(t, hooks.usage, 1)
	require.Equal(t, "resp_9", hooks.usage[0].ResponseID)
}

func TestSession_BinaryClientFrameRejected(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	require.NoError(t, h.client.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3}))

	frame, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "error", gjson.Get(frame, "type").String())
	require.Equal(t, "invalid_frame", gjson.Get(frame, "error.code").String())

	require.NoError(t, h.client.Close())
	h.wait(t)
}

// replyToCancel makes the fake upstream answer response.cancel with the given frames.
func (f *fakeUpstream) replyToCancel(t *testing.T, frames ...string) {
	t.Helper()

	go func() {
		deadline := time.Now().Add(3 * time.Second)

		for time.Now().Before(deadline) {
			for _, got := range f.got() {
				if strings.Contains(got, "response.cancel") {
					for _, frame := range frames {
						f.send(t, frame)
					}

					return
				}
			}

			time.Sleep(5 * time.Millisecond)
		}
	}()
}

func TestSession_CleanClientCloseMidResponseDrainsUsageAfterDeltas(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	h.upstream.send(t, `{"type":"response.created","response":{"id":"resp_a"}}`)

	first, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "response.created", gjson.Get(first, "type").String())

	// After the cancel, the upstream still streams several frames before response.done; every
	// one of them hits a client writer that gorilla already closed (the close was echoed).
	h.upstream.replyToCancel(t,
		`{"type":"response.output_audio.delta","delta":"AAAA"}`,
		`{"type":"response.output_audio.done"}`,
		`{"type":"response.output_item.done"}`,
		`{"type":"response.done","response":{"id":"resp_a","usage":{"input_tokens":7}}}`,
	)

	require.NoError(t, h.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second)))

	start := time.Now()
	summary := h.wait(t)
	require.Equal(t, CauseClientClosed, summary.Cause)
	require.False(t, summary.Failed())
	require.False(t, summary.ClientFault())

	<-hooks.ended
	require.Len(t, hooks.usage, 1)
	require.Equal(t, "resp_a", hooks.usage[0].ResponseID)

	// The session did not sit out the whole ResponseDrain once response.done arrived, nor the
	// CloseGrace for a client writer that had nothing left to do.
	require.Less(t, time.Since(start), testConfig().ResponseDrain+testConfig().CloseGrace)
}

func TestSession_SecondResponseInFlightIsStillDrained(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), &passDialect{}, hooks)

	h.upstream.send(t, `{"type":"response.created","response":{"id":"resp_1"}}`)
	h.upstream.send(t, `{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":1}}}`)
	h.upstream.send(t, `{"type":"response.created","response":{"id":"resp_2"}}`)

	for range 3 {
		_, err := readFrame(t, h.client, 2*time.Second)
		require.NoError(t, err)
	}

	h.upstream.replyToCancel(t, `{"type":"response.done","response":{"id":"resp_2","usage":{"input_tokens":2}}}`)

	require.NoError(t, h.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))

	summary := h.wait(t)
	require.Equal(t, CauseClientClosed, summary.Cause)
	require.Equal(t, int64(2), summary.Responses)

	<-hooks.ended
	require.Len(t, hooks.usage, 2)
	require.Equal(t, "resp_2", hooks.usage[1].ResponseID)
}

func TestSession_SidebandNeverCancelsTheCallResponse(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarnessWith(t, t.Context(), testConfig(), &passDialect{}, hooks, true)

	h.upstream.send(t, `{"type":"response.created","response":{"id":"resp_s"}}`)

	_, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)

	require.NoError(t, h.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))

	summary := h.wait(t)
	require.Equal(t, CauseClientClosed, summary.Cause)

	for _, frame := range h.upstream.got() {
		require.NotContains(t, frame, "response.cancel")
	}

	select {
	case ce := <-h.upstream.closed:
		require.Equal(t, websocket.CloseNormalClosure, ce.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive a close frame")
	}
}

func TestSession_LifecycleShutdownSends1012And1001(t *testing.T) {
	hooks := newRecordingHooks()
	lc := lifecycle.New()
	h := newHarnessWith(t, lc.Attach(t.Context()), testConfig(), &passDialect{}, hooks, false)

	// Make sure the session is fully running before the shutdown lands.
	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)))
	h.upstream.waitFor(t, 1)

	lc.Shutdown(nil)

	_, err := readFrame(t, h.client, 3*time.Second)

	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	require.Equal(t, websocket.CloseServiceRestart, ce.Code)

	select {
	case up := <-h.upstream.closed:
		require.Equal(t, websocket.CloseGoingAway, up.Code)
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive a close frame")
	}

	summary := h.wait(t)
	require.Equal(t, CauseShutdown, summary.Cause)
	require.False(t, summary.Failed())
}

func TestSession_UpstreamIgnoringCloseStillEndsWithinGrace(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	cfg.CloseGrace = 400 * time.Millisecond

	up := newFakeUpstream(t)
	up.onConn = func(conn *websocket.Conn) {
		// Swallow close frames: never echo them.
		conn.SetCloseHandler(func(int, string) error { return nil })
	}

	upConn, _, err := DialUpstream(t.Context(), nil, wsURL(up.server), nil, 3*time.Second)
	require.NoError(t, err)

	session := NewSession(t.Context(), SessionParams{Config: cfg, Dialect: &passDialect{}, Upstream: upConn, Hooks: hooks, Name: "test"})

	runDone := make(chan EndSummary, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		runDone <- session.Run(conn)
	}))
	t.Cleanup(gateway.Close)

	client, _, err := websocket.DefaultDialer.Dial(wsURL(gateway), nil)
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))

	select {
	case summary := <-runDone:
		require.Equal(t, CauseClientClosed, summary.Cause)
	case <-time.After(5 * time.Second):
		t.Fatal("session did not end")
	}

	require.Less(t, time.Since(start), cfg.CloseGrace+cfg.WriteTimeout+time.Second)
}

func TestSession_PeerInitiatedCloseDoesNotWaitForGrace(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	cfg.CloseGrace = 3 * time.Second

	h := newHarness(t, cfg, &passDialect{}, hooks)

	start := time.Now()
	require.NoError(t, h.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))

	summary := h.wait(t)
	require.Equal(t, CauseClientClosed, summary.Cause)
	require.Less(t, time.Since(start), 1500*time.Millisecond)
}

func TestEndSummary_FailedAndClientFault(t *testing.T) {
	require.False(t, EndSummary{Cause: CauseAborted}.Failed())
	require.True(t, EndSummary{Cause: CauseAborted}.ClientFault())
	require.False(t, EndSummary{Cause: CauseClientError}.Failed())
	require.True(t, EndSummary{Cause: CauseClientError}.ClientFault())
	require.False(t, EndSummary{Cause: CauseUpstreamClosed, Code: "1005"}.Failed())
	require.True(t, EndSummary{Cause: CauseUpstreamClosed, Code: "1011"}.Failed())
	require.True(t, EndSummary{Cause: CauseWriteFailed}.Failed())
	require.False(t, EndSummary{Cause: CauseWriteFailed}.ClientFault())
}
