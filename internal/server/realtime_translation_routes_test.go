package server

import (
	"bytes"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

const liveTranslateModel = "qwen3.8-livetranslate-flash-realtime"

// fakeBailianLiveTranslate behaves like Bailian qwen3.8-livetranslate-flash-realtime as probed on
// 2026-11-05: one session.update per session (a second one closes with 1007), a missing voice
// fails the first response with "Voice 'Chelsie' is not supported" and 1007, one response cycle
// per spoken sentence with per-response usage, session.finished on session.finish. It replays
// the dialect's two-sentence fixture: sentence one after the third append, sentence two after
// the sixth.
type fakeBailianLiveTranslate struct {
	server *httptest.Server

	mu       sync.Mutex
	urls     []string
	headers  []http.Header
	received [][]string
}

func newFakeBailianLiveTranslate(t *testing.T) *fakeBailianLiveTranslate {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("realtime", "dialect", "bailian", "testdata", "livetranslate_two_sentences.jsonl"))
	require.NoError(t, err)

	frames := strings.Split(string(bytes.TrimSpace(data)), "\n")

	firstDone := 0
	for i, frame := range frames {
		if gjson.Get(frame, "type").String() == "response.done" {
			firstDone = i
			break
		}
	}

	require.Positive(t, firstDone)

	created, updated, finished := frames[0], frames[1], frames[len(frames)-1]
	sentences := [][]string{frames[2 : firstDone+1], frames[firstDone+1 : len(frames)-1]}

	f := &fakeBailianLiveTranslate{}
	upgrader := websocket.Upgrader{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		f.mu.Lock()
		f.urls = append(f.urls, r.URL.String())
		f.headers = append(f.headers, r.Header.Clone())
		f.received = append(f.received, nil)
		idx := len(f.received) - 1
		f.mu.Unlock()

		send := func(frame string) { _ = conn.WriteMessage(websocket.TextMessage, []byte(frame)) }
		fail := func(frame, reason string) {
			send(frame)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1007, reason), time.Now().Add(time.Second))
		}

		send(created)

		updates, appends, voice := 0, 0, ""

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}

			frame := string(data)

			f.mu.Lock()
			f.received[idx] = append(f.received[idx], frame)
			f.mu.Unlock()

			switch gjson.Get(frame, "type").String() {
			case "session.update":
				if updates++; updates > 1 {
					fail(`{"event_id":"event_e1","type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Session update error: session already started or finished or failed."}}`, "Session update error")
					return
				}

				voice = gjson.Get(frame, "session.voice").String()

				send(updated)
			case "input_audio_buffer.append":
				appends++

				if appends%3 != 0 || appends/3 > len(sentences) {
					continue
				}

				if voice == "" || voice == "Chelsie" {
					fail(`{"event_id":"event_e2","type":"error","error":{"code":"COMMON_ERROR","message":"<400> InternalError.Algo.InvalidParameter: Voice 'Chelsie' is not supported."}}`, "InternalError.Algo.InvalidParameter")
					return
				}

				for _, out := range sentences[appends/3-1] {
					send(out)
				}
			case "session.finish":
				send(finished)
			}
		}
	}))

	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeBailianLiveTranslate) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.urls)
}

func (f *fakeBailianLiveTranslate) session(t *testing.T, idx int) (string, http.Header, []string) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Greater(t, len(f.urls), idx)

	return f.urls[idx], f.headers[idx], append([]string(nil), f.received[idx]...)
}

type translationRouteHarness struct {
	*asrRouteHarness

	fake *fakeBailianLiveTranslate
}

func newTranslationRouteHarness(t *testing.T) *translationRouteHarness {
	t.Helper()

	h := newCodexRouteHarness(t)
	fake := newFakeBailianLiveTranslate(t)

	h.db.Channel.Create().
		SetName("bailian-livetranslate").
		SetType(channel.TypeBailian).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetEndpoints([]objects.ChannelEndpoint{{APIFormat: llm.APIFormatOpenAIRealtime.String(), BaseURL: fake.server.URL + "/api-ws/v1", Path: "/realtime"}}).
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-bailian"}).
		SetSupportedModels([]string{liveTranslateModel, "qwen3.8-omni-flash-realtime"}).
		SetDefaultTestModel("qwen3.8-omni-flash-realtime").
		SaveX(h.ctx)

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
	orch := orchestrator.NewChatCompletionOrchestrator(channels, selector, h.requests, httpclient.NewHttpClient(), openai.NewInboundTransformer(), h.system, usage, nil, nil, nil, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil, orchestrator.PluginIntegration{})
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: false})

	registerRealtimeRoutes(
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: 5 * time.Second}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	return &translationRouteHarness{asrRouteHarness: &asrRouteHarness{codexRouteHarness: h, gateway: gateway}, fake: fake}
}

// requestRows waits until n request rows exist and none is still processing.
func (h *translationRouteHarness) requestRows(t *testing.T, n int) []*ent.Request {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		rows, err := h.db.Request.Query().Order(ent.Asc(request.FieldID)).All(h.ctx)
		require.NoError(t, err)

		settled := len(rows) >= n
		for _, row := range rows {
			settled = settled && row.Status != request.StatusProcessing && row.Status != request.StatusPending
		}

		if settled || time.Now().After(deadline) {
			return rows
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func TestRealtimeTranslationRoutes_TwoSentenceSessionOnBailianLiveTranslate(t *testing.T) {
	h := newTranslationRouteHarness(t)

	conn, resp, err := h.dial(t, "/v1/realtime/translations?model="+liveTranslateModel, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	created := gjson.Parse(readWS(t, conn))
	require.Equal(t, "session.created", created.Get("type").String())
	require.Equal(t, "translation", created.Get("session.type").String())
	require.Equal(t, liveTranslateModel, created.Get("session.model").String())
	require.Equal(t, "en", created.Get("session.audio.output.language").String())

	// Two client updates are answered by the gateway; Bailian would close on the second one.
	for _, update := range []string{
		`{"event_id":"u1","type":"session.update","session":{"audio":{"output":{"language":"en"},"input":{"transcription":{"model":"gpt-realtime-whisper"}}}}}`,
		`{"event_id":"u2","type":"session.update","session":{"audio":{"input":{"noise_reduction":{"type":"near_field"}}}}}`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(update)))

		updated := gjson.Parse(readWS(t, conn))
		require.Equal(t, "session.updated", updated.Get("type").String(), updated.Raw)
	}

	// Two sentences (three appends each in the fake), then session.close.
	for range 6 {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.input_audio_buffer.append","audio":"`+pcm24(100)+`"}`)))
	}

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"event_id":"c1","type":"session.close"}`)))

	var (
		input, output strings.Builder
		audioDeltas   int
		types         []string
	)

	for {
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))

		_, data, err := conn.ReadMessage()
		if err != nil {
			ce, ok := errors.AsType[*websocket.CloseError](err)
			require.True(t, ok, "the session ends with a close frame: %v", err)
			require.Equal(t, websocket.CloseNormalClosure, ce.Code)

			break
		}

		ev := gjson.ParseBytes(data)
		types = append(types, ev.Get("type").String())

		switch ev.Get("type").String() {
		case "session.input_transcript.delta":
			input.WriteString(ev.Get("delta").String())
		case "session.output_transcript.delta":
			output.WriteString(ev.Get("delta").String())
		case "session.output_audio.delta":
			audioDeltas++

			require.EqualValues(t, 24000, ev.Get("sample_rate").Int())
		default:
			require.Equal(t, "session.closed", ev.Get("type").String(), "unexpected event %s", ev.Raw)
		}
	}

	require.Equal(t, "session.closed", types[len(types)-1], "session.closed precedes the close frame")
	require.Equal(t, "Today, the weather is nice. Let's go for a walk in the park. Tomorrow at 3 p.m. we have a meeting, remember to bring your computer. ", output.String())
	require.Equal(t, "今天 天气不错 我们去公园散步 明天 下午三点开会 记得带电脑 ", input.String())
	require.Equal(t, 4, audioDeltas)

	// Upstream side: the model in the URL, one merged update with a usable voice before the
	// first audio, 16 kHz audio and exactly one session.finish.
	url, headers, received := h.fake.session(t, 0)
	require.Contains(t, url, "model="+liveTranslateModel)
	require.Equal(t, "Bearer sk-bailian", headers.Get("Authorization"))

	require.Equal(t, "session.update", gjson.Get(received[0], "type").String(), "the config precedes the first audio")
	require.JSONEq(t, `{"output_modalities":["text","audio"],"voice":"Tina","audio":{"input":{"format":{"type":"pcm","sample_rate":16000}}},"translation":{"language":"en"}}`,
		gjson.Get(received[0], "session").Raw)

	var updates, appends, finishes int

	for _, frame := range received {
		switch gjson.Get(frame, "type").String() {
		case "session.update":
			updates++
		case "input_audio_buffer.append":
			appends++

			pcm, err := base64.StdEncoding.DecodeString(gjson.Get(frame, "audio").String())
			require.NoError(t, err)
			require.InDelta(t, 3200, len(pcm), 64)
		case "session.finish":
			finishes++
		}
	}

	require.Equal(t, 1, updates)
	require.Equal(t, 6, appends)
	require.Equal(t, 1, finishes)
	require.Equal(t, "session.finish", gjson.Get(received[len(received)-1], "type").String())

	// Accounting: one usage row per response.done under the livetranslate model, plus the
	// session row, all completed (a client session.close is not a failure).
	logs := h.usageLogs(t, 2)
	require.Len(t, logs, 2)
	require.EqualValues(t, 154, logs[0].TotalTokens)
	require.EqualValues(t, 75, logs[0].PromptTokens)
	require.EqualValues(t, 79, logs[0].CompletionTokens)
	require.EqualValues(t, 269, logs[1].TotalTokens)
	require.EqualValues(t, 110, logs[1].PromptTokens)

	for _, l := range logs {
		require.Equal(t, liveTranslateModel, l.ModelID)
	}

	rows := h.requestRows(t, 3)
	require.Len(t, rows, 3)

	for _, row := range rows {
		require.Equal(t, request.StatusCompleted, row.Status, string(row.ResponseBody))
		require.Equal(t, liveTranslateModel, row.ModelID)
		require.Equal(t, "openai/realtime", row.Format)
	}

	require.Contains(t, string(rows[2].ResponseBody), `"dialect":"bailian_livetranslate"`)
}

func TestRealtimeTranslationRoutes_RejectionsNeverDialUpstream(t *testing.T) {
	h := newTranslationRouteHarness(t)

	for _, path := range []string{"/v1/realtime/translations/calls", "/v1/realtime/translations/client_secrets"} {
		req, err := http.NewRequest(http.MethodPost, h.gateway.URL+path+"?model="+liveTranslateModel, strings.NewReader("{}"))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+h.key.Key)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode, path)
		require.Contains(t, readErrorBody(t, resp), "is not supported by this gateway", path)
	}

	for _, tc := range []struct {
		path   string
		header http.Header
		status int
		body   string
	}{
		{"/v1/realtime/translations?model=" + liveTranslateModel, http.Header{"Openai-Beta": []string{"realtime=v1"}}, http.StatusBadRequest, "beta protocol"},
		{"/v1/realtime/translations?intent=transcription&model=" + liveTranslateModel, nil, http.StatusBadRequest, "intent"},
		{"/v1/realtime/translations?call_id=rtc_1&model=" + liveTranslateModel, nil, http.StatusBadRequest, "call_id"},
		{"/v1/realtime/translations", nil, http.StatusBadRequest, "model"},
		// The omni model exists, but only livetranslate models serve translation sessions.
		{"/v1/realtime/translations?model=qwen3.8-omni-flash-realtime", nil, http.StatusBadRequest, "Qwen-LiveTranslate"},
		// And livetranslate models serve nothing else.
		{"/v1/realtime?model=" + liveTranslateModel, nil, http.StatusBadRequest, "/v1/realtime/translations"},
		{"/v1/realtime?intent=transcription&model=" + liveTranslateModel, nil, http.StatusBadRequest, "/v1/realtime/translations"},
		// A model no channel serves is "model not found", like on the chat path.
		{"/v1/realtime/translations?model=no-such-model", nil, http.StatusUnprocessableEntity, "no-such-model"},
	} {
		_, resp, err := h.dial(t, tc.path, tc.header)
		require.Error(t, err, tc.path)
		require.Equal(t, tc.status, resp.StatusCode, tc.path)
		require.Contains(t, readErrorBody(t, resp), tc.body, tc.path)
	}

	require.Zero(t, h.fake.sessions(), "no rejected request reaches the upstream")
}
