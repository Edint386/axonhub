package server

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// callsUpstreamMode selects how the fake call-create endpoint answers.
type callsUpstreamMode int32

const (
	// callsUpstreamNormal answers 201 with a Location.
	callsUpstreamNormal callsUpstreamMode = iota
	// callsUpstreamNoLocation answers 201 without any Location (the call cannot be addressed).
	callsUpstreamNoLocation
	// callsUpstreamSlow never answers before the gateway request deadline.
	callsUpstreamSlow
)

// fakeCallsUpstream emulates the OpenAI call-create, sideband and hangup endpoints with
// test-controlled behaviour. A sideband streams one metered response and then stays open
// until the test ends the call (endCall), when it says goodbye with close code 1000; once
// ended, further attaches are refused with 404 like the real upstream does.
type fakeCallsUpstream struct {
	server *httptest.Server

	mode    atomic.Int32
	ended   atomic.Bool
	endCall chan struct{}

	mu        sync.Mutex
	calls     []*http.Request
	sidebands []*http.Request
	hangups   []*http.Request
}

func newFakeCallsUpstream(t *testing.T) *fakeCallsUpstream {
	t.Helper()

	f := &fakeCallsUpstream{endCall: make(chan struct{})}
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/realtime/calls", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		f.record(&f.calls, r)

		switch callsUpstreamMode(f.mode.Load()) {
		case callsUpstreamSlow:
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}

			return
		case callsUpstreamNoLocation:
			w.Header().Set("Content-Type", "application/sdp")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("v=0\r\n"))

			return
		case callsUpstreamNormal:
		}

		w.Header().Set("Content-Type", "application/sdp")
		w.Header().Set("Location", "/v1/realtime/calls/rtc_mon1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\n"))
	})

	mux.HandleFunc("GET /v1/realtime", func(w http.ResponseWriter, r *http.Request) {
		f.record(&f.sidebands, r)

		if f.ended.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"No session found for the provided call_id","type":"invalid_request_error","code":"call_id_not_found"}}`))

			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","event_id":"evt_1","session":{"type":"realtime","model":"gpt-realtime","object":"realtime.session"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","event_id":"evt_2","response":{"id":"resp_1","status":"in_progress"}}`))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","event_id":"evt_3","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_token_details":{"text_tokens":4,"audio_tokens":6,"cached_tokens":0},"output_token_details":{"text_tokens":5,"audio_tokens":15}}}}`))

		// Keep reading so pings are answered and a peer close is noticed.
		peerGone := make(chan struct{})

		go func() {
			defer close(peerGone)

			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}()

		select {
		case <-peerGone:
			return
		case <-f.endCall:
		}

		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "call ended"), time.Now().Add(time.Second))

		select {
		case <-peerGone:
		case <-time.After(2 * time.Second):
		}
	})

	mux.HandleFunc("POST /v1/realtime/calls/{id}/hangup", func(w http.ResponseWriter, r *http.Request) {
		f.record(&f.hangups, r)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeCallsUpstream) record(list *[]*http.Request, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	*list = append(*list, r.Clone(context.WithoutCancel(r.Context())))
}

func (f *fakeCallsUpstream) count(list *[]*http.Request) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(*list)
}

// wait blocks until at least n requests were captured in list and returns them.
func (f *fakeCallsUpstream) wait(t *testing.T, list *[]*http.Request, n int) []*http.Request {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		f.mu.Lock()
		got := append([]*http.Request(nil), (*list)...)
		f.mu.Unlock()

		if len(got) >= n {
			return got
		}

		if time.Now().After(deadline) {
			t.Fatalf("expected %d upstream requests, got %d", n, len(got))
		}

		time.Sleep(10 * time.Millisecond)
	}
}

type callsRouteHarness struct {
	*codexRouteHarness

	upstream *fakeCallsUpstream
	gateway  *httptest.Server
}

// newCallsRouteHarness mirrors newRealtimeRouteHarness with the controllable upstream behind
// n OpenAI channels (several to exercise failover) carrying the given header overrides, and
// the given gateway request timeout. The relay config is read from the environment, so
// t.Setenv calls must precede it.
func newCallsRouteHarness(t *testing.T, n int, overrides []objects.OverrideOperation, timeout time.Duration) *callsRouteHarness {
	t.Helper()

	h := newCodexRouteHarness(t)
	upstream := newFakeCallsUpstream(t)

	for i := range n {
		create := h.db.Channel.Create().
			SetName(fmt.Sprintf("openai-calls-%d", i)).
			SetType(channel.TypeOpenai).
			SetStatus(channel.StatusEnabled).
			SetBaseURL(upstream.server.URL + "/v1").
			SetCredentials(objects.ChannelCredentials{APIKey: "sk-upstream"}).
			SetSupportedModels([]string{"gpt-realtime"}).
			SetDefaultTestModel("gpt-realtime")

		if len(overrides) > 0 {
			create.SetSettings(&objects.ChannelSettings{HeaderOverrideOperations: overrides})
		}

		create.SaveX(h.ctx)
	}

	channels := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		Ent:           h.db,
		SystemService: h.system,
		HttpClient:    httpclient.NewHttpClient(),
	})
	t.Cleanup(channels.Stop)

	models := biz.NewModelService(biz.ModelServiceParams{Ent: h.db, SystemService: h.system, ChannelService: channels})
	usage := biz.NewUsageLogService(h.db, h.system, channels)
	selector := orchestrator.NewDefaultSelector(channels, models, h.system)
	orch := orchestrator.NewChatCompletionOrchestrator(channels, selector, h.requests, httpclient.NewHttpClient(), openai.NewInboundTransformer(), h.system, usage, nil, nil, nil, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil)

	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: false})

	registerRealtimeRoutes(
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: timeout}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	return &callsRouteHarness{codexRouteHarness: h, upstream: upstream, gateway: gateway}
}

// createCall posts an SDP offer for gpt-realtime with the owner key plus extra headers.
func (h *callsRouteHarness) createCall(t *testing.T, extra http.Header) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/realtime/calls?model=gpt-realtime", strings.NewReader("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key.Key)
	req.Header.Set("Content-Type", "application/sdp")

	maps.Copy(req.Header, extra)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	return resp.StatusCode, resp.Header, string(body)
}

// hangup posts the hangup for callID with the owner key.
func (h *callsRouteHarness) hangup(t *testing.T, callID string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/realtime/calls/"+callID+"/hangup", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key.Key)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	return resp.StatusCode
}

// dialSideband attaches to the call's sideband through the gateway with the owner key.
func (h *callsRouteHarness) dialSideband(callID string) (*websocket.Conn, *http.Response, error) {
	header := http.Header{}
	header.Set("Authorization", "Bearer "+h.key.Key)

	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.gateway.URL, "http")+"/v1/realtime?call_id="+callID, header)
}

// waitUsage blocks until a usage log with the given prompt token count exists.
func (h *callsRouteHarness) waitUsage(t *testing.T, promptTokens int64) {
	t.Helper()

	require.Eventually(t, func() bool {
		logs, err := h.db.UsageLog.Query().All(h.ctx)
		require.NoError(t, err)

		for _, l := range logs {
			if l.PromptTokens == promptTokens {
				return true
			}
		}

		return false
	}, 5*time.Second, 20*time.Millisecond, "no usage log with %d prompt tokens", promptTokens)
}

// waitSettledRequests blocks until at least n request rows exist and none is still processing.
func (h *callsRouteHarness) waitSettledRequests(t *testing.T, n int) []*ent.Request {
	t.Helper()

	var rows []*ent.Request

	require.Eventually(t, func() bool {
		var err error

		rows, err = h.db.Request.Query().All(h.ctx)
		require.NoError(t, err)

		if len(rows) < n {
			return false
		}

		for _, row := range rows {
			if row.Status == request.StatusProcessing {
				return false
			}
		}

		return true
	}, 5*time.Second, 20*time.Millisecond, "expected %d settled request rows, have %d", n, len(rows))

	return rows
}

func TestRealtimeCallsRoutes_MonitorMetersOutsidePerKeyCapAndForgetsEndedCall(t *testing.T) {
	// One per-key slot: the monitor must not take it, or the owner's own sideband gets 429.
	t.Setenv("AXONHUB_REALTIME_MAX_SESSIONS_PER_KEY", "1")

	overrides := []objects.OverrideOperation{{Op: objects.OverrideOpSet, Path: "X-Relay-Auth", Value: "relay-secret"}}
	h := newCallsRouteHarness(t, 1, overrides, 5*time.Second)

	status, header, body := h.createCall(t, http.Header{"Openai-Safety-Identifier": {"user-42"}})
	require.Equal(t, http.StatusCreated, status, body)
	require.Equal(t, "/v1/realtime/calls/rtc_mon1", header.Get("Location"))

	// Call-create carries the channel header overrides and the end-user safety identifier.
	created := h.upstream.wait(t, &h.upstream.calls, 1)[0]
	require.Equal(t, "Bearer sk-upstream", created.Header.Get("Authorization"))
	require.Equal(t, "relay-secret", created.Header.Get("X-Relay-Auth"))
	require.Equal(t, "user-42", created.Header.Get("Openai-Safety-Identifier"))

	// The monitor attaches under the same identity and overrides...
	monitor := h.upstream.wait(t, &h.upstream.sidebands, 1)[0]
	require.Equal(t, "rtc_mon1", monitor.URL.Query().Get("call_id"))
	require.Equal(t, "Bearer sk-upstream", monitor.Header.Get("Authorization"))
	require.Equal(t, "relay-secret", monitor.Header.Get("X-Relay-Auth"))

	// ...and meters the response nobody else observed.
	h.waitUsage(t, 10)

	// The monitor holds a global slot only, so the owner's sideband still fits the per-key cap.
	conn, resp, err := h.dialSideband("rtc_mon1")
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.Equal(t, "session.created", gjson.Get(readWS(t, conn), "type").String())
	require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second)))
	require.NoError(t, conn.Close())

	// The upstream ends the call: the open monitor gets close 1000 and re-attaches get 404.
	// The monitor confirms with one re-dial, forgets the binding and ends without a failure.
	h.upstream.ended.Store(true)
	close(h.upstream.endCall)

	attempts := len(h.upstream.wait(t, &h.upstream.sidebands, 3))

	// call row, monitor turn, client sideband summary, monitor summary: none failed, and the
	// monitor summary is a completed upstream_closed/1000 ending.
	rows := h.waitSettledRequests(t, 4)
	monitorSummaries := 0

	for _, row := range rows {
		require.NotEqual(t, request.StatusFailed, row.Status, "request %d", row.ID)

		if gjson.GetBytes(row.RequestBody, "kind").String() != "monitor" || !gjson.GetBytes(row.RequestBody, "cause").Exists() {
			continue
		}

		monitorSummaries++
		require.Equal(t, request.StatusCompleted, row.Status, "monitor summary %d", row.ID)
		require.Equal(t, "upstream_closed", gjson.GetBytes(row.RequestBody, "cause").String())
		require.Equal(t, "1000", gjson.GetBytes(row.RequestBody, "close_code").String())
		require.EqualValues(t, 1, gjson.GetBytes(row.RequestBody, "responses").Int())
	}

	require.Equal(t, 1, monitorSummaries)

	// The binding is gone: the gateway rejects the attach itself, without dialing upstream.
	_, resp, err = h.dialSideband("rtc_mon1")
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Equal(t, attempts, h.upstream.count(&h.upstream.sidebands))
}

func TestRealtimeCallsRoutes_HangupAppliesHeaderOverrides(t *testing.T) {
	overrides := []objects.OverrideOperation{{Op: objects.OverrideOpSet, Path: "X-Relay-Auth", Value: "relay-secret"}}
	h := newCallsRouteHarness(t, 1, overrides, 5*time.Second)

	status, _, body := h.createCall(t, nil)
	require.Equal(t, http.StatusCreated, status, body)
	h.upstream.wait(t, &h.upstream.sidebands, 1)

	require.Equal(t, http.StatusOK, h.hangup(t, "rtc_mon1"))

	hangup := h.upstream.wait(t, &h.upstream.hangups, 1)[0]
	require.Equal(t, "/v1/realtime/calls/rtc_mon1/hangup", hangup.URL.Path)
	require.Equal(t, "Bearer sk-upstream", hangup.Header.Get("Authorization"))
	require.Equal(t, "relay-secret", hangup.Header.Get("X-Relay-Auth"))

	require.Equal(t, http.StatusNotFound, h.hangup(t, "rtc_mon1"))

	// Ending the call upstream lets the monitor finish; a hung-up call is not a failure.
	h.upstream.ended.Store(true)
	close(h.upstream.endCall)

	rows := h.waitSettledRequests(t, 3)
	for _, row := range rows {
		require.Equal(t, request.StatusCompleted, row.Status, "request %d", row.ID)
	}

	require.Equal(t, 2, h.upstream.count(&h.upstream.sidebands), "the monitor re-dials once after the close frame")
}

func TestRealtimeCallsRoutes_CallCreateWithoutLocationFailsWithoutFailover(t *testing.T) {
	h := newCallsRouteHarness(t, 2, nil, 5*time.Second)
	h.upstream.mode.Store(int32(callsUpstreamNoLocation))

	status, _, body := h.createCall(t, nil)
	require.Equal(t, http.StatusBadGateway, status, body)

	// The first channel created an unaddressable call; the same offer must not be posted to
	// the second channel, and no monitor is started.
	require.Equal(t, 1, h.upstream.count(&h.upstream.calls))
	require.Equal(t, 0, h.upstream.count(&h.upstream.sidebands))
}

func TestRealtimeCallsRoutes_CallCreateTimeoutIs504(t *testing.T) {
	h := newCallsRouteHarness(t, 1, nil, 300*time.Millisecond)
	h.upstream.mode.Store(int32(callsUpstreamSlow))

	status, _, body := h.createCall(t, nil)
	require.Equal(t, http.StatusGatewayTimeout, status, body)
}
