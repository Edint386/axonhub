package server

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent/apikey"
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

// fakeRealtimeUpstream emulates an OpenAI-compatible realtime upstream: a WebSocket session
// endpoint, the WebRTC call-create endpoint (OpenAI and Codex backend shapes), a sideband
// endpoint and hangup.
type fakeRealtimeUpstream struct {
	server *httptest.Server

	mu          sync.Mutex
	sessionReqs []*http.Request
	callReqs    []capturedRequest
	hangups     int
}

type capturedRequest struct {
	req  *http.Request
	body []byte
}

func newFakeRealtimeUpstream(t *testing.T) *fakeRealtimeUpstream {
	t.Helper()

	f := &fakeRealtimeUpstream{}
	upgrader := websocket.Upgrader{}

	mux := http.NewServeMux()

	serveSession := func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessionReqs = append(f.sessionReqs, r.Clone(context.WithoutCancel(r.Context())))
		f.mu.Unlock()

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		model := r.URL.Query().Get("model")
		if model == "" {
			model = "gpt-realtime-attached"
		}

		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.created","event_id":"evt_1","session":{"type":"realtime","model":"`+model+`","object":"realtime.session"}}`))

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}

			switch gjson.GetBytes(data, "type").String() {
			case "session.update":
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.updated","event_id":"evt_2","session":{"type":"realtime","model":"`+model+`"}}`))
			case "response.create":
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","event_id":"evt_3","response":{"id":"resp_1","status":"in_progress"}}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.output_audio.delta","event_id":"evt_4","response_id":"resp_1","delta":"AAAA"}`))
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.done","event_id":"evt_5","response":{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_audio","audio":"AAAA","transcript":"hi"}]}],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30,"input_token_details":{"text_tokens":4,"audio_tokens":6,"cached_tokens":0},"output_token_details":{"text_tokens":5,"audio_tokens":15}}}}`))
			}
		}
	}

	serveCall := func(location string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)

			f.mu.Lock()
			f.callReqs = append(f.callReqs, capturedRequest{req: r.Clone(context.WithoutCancel(r.Context())), body: body})
			f.mu.Unlock()

			w.Header().Set("Content-Type", "application/sdp")
			w.Header().Set("Location", location)
			w.Header().Set("X-Request-ID", "req_fake")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\n"))
		}
	}

	mux.HandleFunc("GET /v1/realtime", serveSession)
	mux.HandleFunc("POST /v1/realtime/calls", serveCall("/v1/realtime/calls/rtc_test1"))
	mux.HandleFunc("POST /v1/realtime/calls/{id}/hangup", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hangups++
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /backend-api/codex/realtime/calls", serveCall("/v1/live/rtc_codex1"))

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeRealtimeUpstream) lastSessionRequest(t *testing.T) *http.Request {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.sessionReqs)
		f.mu.Unlock()

		if n > 0 {
			f.mu.Lock()
			defer f.mu.Unlock()

			return f.sessionReqs[n-1]
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("upstream never received a session request")

	return nil
}

func (f *fakeRealtimeUpstream) lastCall(t *testing.T) capturedRequest {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.NotEmpty(t, f.callReqs)

	return f.callReqs[len(f.callReqs)-1]
}

type realtimeRouteHarness struct {
	*codexRouteHarness

	upstream *fakeRealtimeUpstream
	gateway  *httptest.Server
	otherKey string
}

func newRealtimeRouteHarness(t *testing.T) *realtimeRouteHarness {
	t.Helper()

	h := newCodexRouteHarness(t)
	upstream := newFakeRealtimeUpstream(t)

	h.db.Channel.Create().
		SetName("openai-realtime").
		SetType(channel.TypeOpenai).
		SetStatus(channel.StatusEnabled).
		SetBaseURL(upstream.server.URL + "/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-upstream"}).
		SetSupportedModels([]string{"gpt-realtime"}).
		SetDefaultTestModel("gpt-realtime").
		SaveX(h.ctx)

	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "acct_test"},
		"exp":                         time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("secret"))
	require.NoError(t, err)

	h.db.Channel.Create().
		SetName("codex-voice").
		SetType(channel.TypeCodex).
		SetStatus(channel.StatusEnabled).
		SetBaseURL(upstream.server.URL + "/backend-api/codex#").
		SetCredentials(objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{AccessToken: token, RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour)}}).
		SetSupportedModels([]string{"gpt-live-1-codex"}).
		SetDefaultTestModel("gpt-live-1-codex").
		SaveX(h.ctx)

	// The codex harness pins an empty channel list; build a real channel service (it loads the
	// enabled channels at construction) and an orchestrator on top of it for the realtime routes.
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

	other := h.db.APIKey.Create().SetName("other").SetKey("ah-other-key").SetUserID(h.key.UserID).SetProjectID(h.key.ProjectID).SetType(apikey.TypeUser).SaveX(h.ctx)

	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: false})

	registerRealtimeRoutes(
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: 5 * time.Second}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	return &realtimeRouteHarness{codexRouteHarness: h, upstream: upstream, gateway: gateway, otherKey: other.Key}
}

func (h *realtimeRouteHarness) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(h.gateway.URL, "http") + path
}

func (h *realtimeRouteHarness) dial(t *testing.T, path, key string, extra http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	header := http.Header{}
	if key != "" {
		header.Set("Authorization", "Bearer "+key)
	}

	maps.Copy(header, extra)

	return websocket.DefaultDialer.Dial(h.wsURL(path), header)
}

func readWS(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

	_, data, err := conn.ReadMessage()
	require.NoError(t, err)

	return string(data)
}

func (h *realtimeRouteHarness) waitRequests(t *testing.T, n int) []*request.Status {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count, err := h.db.Request.Query().Count(h.ctx)
		require.NoError(t, err)

		if count >= n {
			rows, err := h.db.Request.Query().All(h.ctx)
			require.NoError(t, err)

			statuses := make([]*request.Status, 0, len(rows))
			for _, row := range rows {
				status := row.Status
				statuses = append(statuses, &status)
			}

			return statuses
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("expected at least %d request rows", n)

	return nil
}

func TestRealtimeRoutes_SessionRelayAndAccounting(t *testing.T) {
	h := newRealtimeRouteHarness(t)

	conn, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime", h.key.Key, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	defer conn.Close()

	created := readWS(t, conn)
	require.Equal(t, "session.created", gjson.Get(created, "type").String())
	require.Equal(t, "gpt-realtime", gjson.Get(created, "session.model").String())

	upstreamReq := h.upstream.lastSessionRequest(t)
	require.Equal(t, "Bearer sk-upstream", upstreamReq.Header.Get("Authorization"))
	require.Equal(t, "gpt-realtime", upstreamReq.URL.Query().Get("model"))
	require.Empty(t, upstreamReq.Header.Get("Sec-WebSocket-Protocol"))

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)))

	require.Equal(t, "response.created", gjson.Get(readWS(t, conn), "type").String())
	require.Equal(t, "response.output_audio.delta", gjson.Get(readWS(t, conn), "type").String())

	done := readWS(t, conn)
	require.Equal(t, "response.done", gjson.Get(done, "type").String())
	require.Equal(t, int64(30), gjson.Get(done, "response.usage.total_tokens").Int())

	require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second)))

	// One turn row plus one session summary row.
	statuses := h.waitRequests(t, 2)
	for _, status := range statuses {
		require.Equal(t, request.StatusCompleted, *status)
	}

	logs, err := h.db.UsageLog.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, logs, 1)
	require.EqualValues(t, 10, logs[0].PromptTokens)
	require.EqualValues(t, 20, logs[0].CompletionTokens)
	require.EqualValues(t, 6, logs[0].PromptAudioTokens)
	require.EqualValues(t, 15, logs[0].CompletionAudioTokens)
	require.Equal(t, "openai/realtime", logs[0].Format)

	rows, err := h.db.Request.Query().All(h.ctx)
	require.NoError(t, err)

	for _, row := range rows {
		require.Equal(t, "gpt-realtime", row.ModelID)
		require.Equal(t, "openai/realtime", row.Format)
	}
}

func TestRealtimeRoutes_RejectsBetaUnknownModelAndMissingKey(t *testing.T) {
	h := newRealtimeRouteHarness(t)

	_, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime", h.key.Key, http.Header{"Openai-Beta": []string{"realtime=v1"}})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	// Unknown models are answered exactly like the chat path (422 invalid_model_error).
	_, resp, err = h.dial(t, "/v1/realtime?model=no-such-model", h.key.Key, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)

	_, resp, err = h.dial(t, "/v1/realtime?model=gpt-realtime", "", nil)
	require.Error(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// The beta and auth rejections stop in the handler and middleware and leave no row. The
	// unknown model fails candidate selection, which is recorded like a chat request that
	// finds no channel: one failed row naming the model and the reason.
	rows, err := h.db.Request.Query().All(h.ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, request.StatusFailed, rows[0].Status)
	require.Equal(t, "no-such-model", rows[0].ModelID)
	require.Equal(t, "openai/realtime", rows[0].Format)
	require.Contains(t, string(rows[0].ResponseBody), "no-such-model")
}

func TestRealtimeRoutes_SubprotocolKeyIsLiftedAndEchoed(t *testing.T) {
	h := newRealtimeRouteHarness(t)

	conn, resp, err := h.dial(t, "/v1/realtime?model=gpt-realtime", "", http.Header{
		"Sec-WebSocket-Protocol": []string{"realtime, openai-insecure-api-key." + h.key.Key},
	})
	require.NoError(t, err)
	require.Equal(t, "realtime", resp.Header.Get("Sec-WebSocket-Protocol"))

	defer conn.Close()

	require.Equal(t, "session.created", gjson.Get(readWS(t, conn), "type").String())

	upstreamReq := h.upstream.lastSessionRequest(t)
	require.NotContains(t, upstreamReq.Header.Get("Sec-WebSocket-Protocol"), "openai-insecure-api-key")
	require.Equal(t, "Bearer sk-upstream", upstreamReq.Header.Get("Authorization"))
}

func TestRealtimeRoutes_CallCreateSidebandAndHangup(t *testing.T) {
	h := newRealtimeRouteHarness(t)

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/realtime/calls?model=gpt-realtime", strings.NewReader("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key.Key)
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	require.Equal(t, "/v1/realtime/calls/rtc_test1", resp.Header.Get("Location"))
	require.Equal(t, "Location", resp.Header.Get("Access-Control-Expose-Headers"))
	require.Contains(t, resp.Header.Get("Content-Type"), "application/sdp")
	require.True(t, strings.HasPrefix(string(body), "v=0"))

	upstreamCall := h.upstream.lastCall(t)
	require.Equal(t, "Bearer sk-upstream", upstreamCall.req.Header.Get("Authorization"))
	require.Equal(t, "application/sdp", upstreamCall.req.Header.Get("Accept"))
	require.Empty(t, upstreamCall.req.URL.Query().Get("model"), "the model query must not be forwarded")

	mediaType, params, err := mime.ParseMediaType(upstreamCall.req.Header.Get("Content-Type"))
	require.NoError(t, err)
	require.Equal(t, "multipart/form-data", mediaType)

	reader := multipart.NewReader(strings.NewReader(string(upstreamCall.body)), params["boundary"])
	parts := map[string]string{}

	for {
		part, err := reader.NextPart()
		if err != nil {
			break
		}

		data, _ := io.ReadAll(part)
		parts[part.FormName()] = string(data)
	}

	require.True(t, strings.HasPrefix(parts["sdp"], "v=0"))
	require.Equal(t, "gpt-realtime", gjson.Get(parts["session"], "model").String())
	require.Equal(t, "realtime", gjson.Get(parts["session"], "type").String())

	// The call is recorded as a request with a zero-token usage log.
	h.waitRequests(t, 1)

	logs, err := h.db.UsageLog.Query().All(h.ctx)
	require.NoError(t, err)
	require.NotEmpty(t, logs)

	// Sideband: the creating key may attach; another key gets 404.
	conn, wsResp, err := h.dial(t, "/v1/realtime?call_id=rtc_test1", h.key.Key, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, wsResp.StatusCode)
	require.Equal(t, "session.created", gjson.Get(readWS(t, conn), "type").String())
	require.NoError(t, conn.Close())

	_, wsResp, err = h.dial(t, "/v1/realtime?call_id=rtc_test1", h.otherKey, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, wsResp.StatusCode)

	_, wsResp, err = h.dial(t, "/v1/realtime?call_id=rtc_unknown", h.key.Key, nil)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, wsResp.StatusCode)

	// Hangup by the owner forwards upstream and forgets the binding.
	hangup, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/realtime/calls/rtc_test1/hangup", nil)
	require.NoError(t, err)
	hangup.Header.Set("Authorization", "Bearer "+h.key.Key)

	hresp, err := http.DefaultClient.Do(hangup)
	require.NoError(t, err)
	_ = hresp.Body.Close()
	require.Equal(t, http.StatusOK, hresp.StatusCode)

	hresp, err = http.DefaultClient.Do(hangup)
	require.NoError(t, err)
	_ = hresp.Body.Close()
	require.Equal(t, http.StatusNotFound, hresp.StatusCode)

	h.upstream.mu.Lock()
	require.Equal(t, 1, h.upstream.hangups)
	h.upstream.mu.Unlock()
}

func TestRealtimeRoutes_CodexLiveCallCreate(t *testing.T) {
	h := newRealtimeRouteHarness(t)

	var buf strings.Builder

	writer := multipart.NewWriter(&buf)
	require.NoError(t, writer.SetBoundary("codex-realtime-call-boundary"))

	sdpPart, err := writer.CreatePart(map[string][]string{"Content-Disposition": {`form-data; name="sdp"`}, "Content-Type": {"application/sdp"}})
	require.NoError(t, err)
	_, _ = sdpPart.Write([]byte("v=0\r\no=- 7 7 IN IP4 127.0.0.1\r\n"))

	sessionPart, err := writer.CreatePart(map[string][]string{"Content-Disposition": {`form-data; name="session"`}, "Content-Type": {"application/json"}})
	require.NoError(t, err)
	_, _ = sessionPart.Write([]byte(`{"model":"gpt-live-1-codex","instructions":"hi","audio":{"output":{"voice":"cove"}},"delegation":{"type":"client"},"id":"sess_x"}`))
	require.NoError(t, writer.Close())

	req, err := http.NewRequest(http.MethodPost, h.gateway.URL+"/v1/live", strings.NewReader(buf.String()))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+h.key.Key)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Openai-Alpha", "quicksilver=v2")
	req.Header.Set("X-Session-Id", "sess-123")
	req.Header.Set("Originator", "Codex Desktop")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	require.Equal(t, "/v1/live/rtc_codex1", resp.Header.Get("Location"))

	upstreamCall := h.upstream.lastCall(t)
	require.Equal(t, "/backend-api/codex/realtime/calls", upstreamCall.req.URL.Path)
	require.Equal(t, "quicksilver", upstreamCall.req.URL.Query().Get("intent"))
	require.Equal(t, "avas", upstreamCall.req.URL.Query().Get("architecture"))
	require.True(t, strings.HasPrefix(upstreamCall.req.Header.Get("Authorization"), "Bearer "))
	require.Equal(t, "acct_test", upstreamCall.req.Header.Get("Chatgpt-Account-Id"))
	require.Equal(t, "quicksilver=v2", upstreamCall.req.Header.Get("Openai-Alpha"))
	require.Equal(t, "sess-123", upstreamCall.req.Header.Get("X-Session-Id"))
	require.Equal(t, "Codex Desktop", upstreamCall.req.Header.Get("Originator"))

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(upstreamCall.body, &payload))
	require.True(t, strings.HasPrefix(gjson.GetBytes(payload["sdp"], "@this").String(), "v=0"))
	require.Equal(t, "gpt-live-1-codex", gjson.GetBytes(payload["session"], "model").String())
	require.False(t, gjson.GetBytes(payload["session"], "id").Exists())
	require.False(t, gjson.GetBytes(payload["session"], "type").Exists(), "codex v3 sessions carry no type")
}
