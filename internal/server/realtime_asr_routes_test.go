package server

import (
	"encoding/base64"
	"maps"
	"net/http"
	"net/http/httptest"
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
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// fakeBailianASR behaves like Bailian qwen3-asr-flash-realtime as probed on 2026-11-03: one
// session.update per session (a second one closes with 1007), beta-shaped events, cumulative
// usage on every completion, session.finished on session.finish.
type fakeBailianASR struct {
	server *httptest.Server

	mu       sync.Mutex
	urls     []string
	headers  []http.Header
	received [][]string
}

func newFakeBailianASR(t *testing.T) *fakeBailianASR {
	t.Helper()

	f := &fakeBailianASR{}
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

		send(`{"event_id":"event_c","type":"session.created","session":{"object":"realtime.session","id":"sess_fake","model":"` + r.URL.Query().Get("model") + `","modalities":["text"],"input_audio_format":"pcm","sample_rate":16000,"input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"turn_detection":{"type":"server_vad","threshold":0.2,"silence_duration_ms":800}}}`)

		updates, appends, utterances := 0, 0, 0

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
				updates++
				if updates > 1 {
					send(`{"event_id":"event_e","type":"error","error":{"code":"invalid_value","message":"Session update error: session already started or finished or failed."}}`)
					_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1007, "Session update error"), time.Now().Add(time.Second))

					return
				}

				send(`{"event_id":"event_u","type":"session.updated","session":{"object":"realtime.session","id":"sess_fake","input_audio_format":"pcm","sample_rate":16000}}`)
			case "input_audio_buffer.append":
				appends++

				// Every third append completes one utterance.
				if appends%3 != 0 {
					continue
				}

				utterances++
				item := "item_" + string(rune('A'+utterances-1))
				total := 50 * utterances

				send(`{"event_id":"e1","type":"input_audio_buffer.speech_started","audio_start_ms":0,"item_id":"` + item + `"}`)
				send(`{"event_id":"e2","type":"conversation.item.created","item":{"id":"` + item + `","object":"realtime.item","type":"message","status":"in_progress","role":"assistant","content":[{"type":"input_audio"}]}}`)
				send(`{"event_id":"e3","type":"conversation.item.input_audio_transcription.text","item_id":"` + item + `","content_index":0,"text":"","stash":"你好","language":"zh","emotion":"neutral"}`)
				send(`{"event_id":"e4","type":"conversation.item.input_audio_transcription.text","item_id":"` + item + `","content_index":0,"text":"","stash":"你好世界","language":"zh","emotion":"neutral"}`)
				send(`{"event_id":"e5","type":"input_audio_buffer.speech_stopped","audio_end_ms":900,"item_id":"` + item + `"}`)
				send(`{"event_id":"e6","type":"input_audio_buffer.committed","item_id":"` + item + `"}`)
				send(`{"event_id":"e7","type":"conversation.item.input_audio_transcription.completed","item_id":"` + item + `","content_index":0,"transcript":"你好世界。","language":"zh","emotion":"neutral","usage":{"duration":` + itoaTest(utterances) + `,"total_tokens":` + itoaTest(total) + `,"input_tokens":` + itoaTest(total-10*utterances) + `,"output_tokens":` + itoaTest(10*utterances) + `,"input_tokens_details":{"text_tokens":3,"audio_tokens":` + itoaTest(total-10*utterances-3) + `},"output_tokens_details":{"text_tokens":` + itoaTest(10*utterances) + `}}}`)
			case "session.finish":
				send(`{"event_id":"event_f","type":"session.finished"}`)
			}
		}
	}))

	t.Cleanup(f.server.Close)

	return f
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}

	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}

	return string(b)
}

func (f *fakeBailianASR) session(t *testing.T, idx int) (string, http.Header, []string) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	require.Greater(t, len(f.urls), idx)

	return f.urls[idx], f.headers[idx], append([]string(nil), f.received[idx]...)
}

type asrRouteHarness struct {
	*codexRouteHarness

	upstream *fakeBailianASR
	gateway  *httptest.Server
}

func newASRRouteHarness(t *testing.T) *asrRouteHarness {
	t.Helper()

	h := newCodexRouteHarness(t)
	upstream := newFakeBailianASR(t)

	h.db.Channel.Create().
		SetName("bailian-asr").
		SetType(channel.TypeBailian).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetEndpoints([]objects.ChannelEndpoint{{APIFormat: llm.APIFormatOpenAIRealtime.String(), BaseURL: upstream.server.URL + "/api-ws/v1", Path: "/realtime"}}).
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-bailian"}).
		SetSupportedModels([]string{"qwen3-asr-flash-realtime", "qwen3.8-omni-flash-realtime"}).
		SetDefaultTestModel("qwen3-asr-flash-realtime").
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
	orch := orchestrator.NewChatCompletionOrchestrator(channels, selector, h.requests, httpclient.NewHttpClient(), openai.NewInboundTransformer(), h.system, usage, nil, nil, nil, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil)
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: false})

	registerRealtimeRoutes(
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: 5 * time.Second}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	return &asrRouteHarness{codexRouteHarness: h, upstream: upstream, gateway: gateway}
}

func (h *asrRouteHarness) dial(t *testing.T, path string, extra http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	header := http.Header{}
	header.Set("Authorization", "Bearer "+h.key.Key)

	maps.Copy(header, extra)

	return websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.gateway.URL, "http")+path, header)
}

// readUntil reads client frames until one of type typ arrives and returns all of them.
func readUntil(t *testing.T, conn *websocket.Conn, typ string) []gjson.Result {
	t.Helper()

	var frames []gjson.Result

	for {
		frame := gjson.Parse(readWS(t, conn))
		frames = append(frames, frame)

		if frame.Get("type").String() == typ {
			return frames
		}
	}
}

func (h *asrRouteHarness) usageLogs(t *testing.T, n int) []*ent.UsageLog {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for {
		logs, err := h.db.UsageLog.Query().Order(ent.Asc(usagelog.FieldID)).All(h.ctx)
		require.NoError(t, err)

		if len(logs) >= n || time.Now().After(deadline) {
			return logs
		}

		time.Sleep(20 * time.Millisecond)
	}
}

func pcm24(ms int) string {
	return base64.StdEncoding.EncodeToString(make([]byte, 24000*2*ms/1000))
}

func TestRealtimeASRRoutes_GATranscriptionSessionOnBailianASR(t *testing.T) {
	h := newASRRouteHarness(t)

	conn, resp, err := h.dial(t, "/v1/realtime?intent=transcription&model=qwen3-asr-flash-realtime", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	created := gjson.Parse(readWS(t, conn))
	require.Equal(t, "session.created", created.Get("type").String())
	require.Equal(t, "transcription", created.Get("session.type").String())
	require.EqualValues(t, 24000, created.Get("session.audio.input.format.rate").Int())

	// Two client updates are answered by the gateway; Bailian would close on the second one.
	for _, update := range []string{
		`{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"gpt-4o-transcribe","language":"zh"}}}}}`,
		`{"event_id":"u2","type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":{"type":"server_vad","silence_duration_ms":600}}}}}`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(update)))
		require.Equal(t, "session.updated", gjson.Get(readWS(t, conn), "type").String())
	}

	// Two utterances (three appends each in the fake).
	for range 6 {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"`+pcm24(100)+`"}`)))
	}

	first := readUntil(t, conn, "conversation.item.done")
	second := readUntil(t, conn, "conversation.item.done")

	for _, frames := range [][]gjson.Result{first, second} {
		var transcript strings.Builder

		for _, f := range frames {
			require.NotEqual(t, "error", f.Get("type").String(), f.Raw)

			if f.Get("type").String() == "conversation.item.input_audio_transcription.delta" {
				transcript.WriteString(f.Get("delta").String())
			}

			if f.Get("type").String() == "conversation.item.input_audio_transcription.completed" {
				require.EqualValues(t, 50, f.Get("usage.total_tokens").Int(), "per-utterance usage, not Bailian's running total")
			}
		}

		require.Equal(t, "你好世界。", transcript.String())
	}

	require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
	_ = conn.Close()

	// Upstream side: the model in the URL, no intent, one merged update at 16 kHz, resampled
	// audio, and session.finish at the end.
	logs := h.usageLogs(t, 2)

	url, headers, received := h.upstream.session(t, 0)
	require.Contains(t, url, "model=qwen3-asr-flash-realtime")
	require.NotContains(t, url, "intent")
	require.Equal(t, "Bearer sk-bailian", headers.Get("Authorization"))
	require.Empty(t, headers.Get("Openai-Beta"))

	var updates, appends int

	for _, frame := range received {
		switch gjson.Get(frame, "type").String() {
		case "session.update":
			updates++

			require.EqualValues(t, 16000, gjson.Get(frame, "session.sample_rate").Int())
			require.EqualValues(t, 600, gjson.Get(frame, "session.turn_detection.silence_duration_ms").Int())
			require.Equal(t, "zh", gjson.Get(frame, "session.input_audio_transcription.language").String())
		case "input_audio_buffer.append":
			appends++

			pcm, err := base64.StdEncoding.DecodeString(gjson.Get(frame, "audio").String())
			require.NoError(t, err)
			require.InDelta(t, 3200, len(pcm), 64)
		}
	}

	require.Equal(t, 1, updates)
	require.Equal(t, 6, appends)
	require.Equal(t, "session.update", gjson.Get(received[0], "type").String(), "the config precedes the first audio")

	// Accounting: one usage row per utterance, billed under the ASR model, with deltas.
	require.Len(t, logs, 2)

	for _, l := range logs {
		require.Equal(t, "qwen3-asr-flash-realtime", l.ModelID)
		require.EqualValues(t, 50, l.TotalTokens)
		require.EqualValues(t, 40, l.PromptTokens)
		require.EqualValues(t, 10, l.CompletionTokens)
	}
}

func TestRealtimeASRRoutes_LegacyClientIsRelayedUnchanged(t *testing.T) {
	h := newASRRouteHarness(t)

	conn, resp, err := h.dial(t, "/v1/realtime?model=qwen3-asr-flash-realtime", http.Header{"Openai-Beta": []string{"realtime=v1"}})
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

	created := gjson.Parse(readWS(t, conn))
	require.Equal(t, "realtime.session", created.Get("session.object").String(), "Bailian's own session object")

	update := `{"event_id":"u1","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":null}}`
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(update)))
	require.Equal(t, "session.updated", gjson.Get(readWS(t, conn), "type").String())

	audio16 := base64.StdEncoding.EncodeToString(make([]byte, 3200))
	for range 6 {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"`+audio16+`"}`)))
	}

	first := readUntil(t, conn, "conversation.item.input_audio_transcription.completed")
	require.Contains(t, first[len(first)-1].Raw, `"total_tokens":50`)

	second := readUntil(t, conn, "conversation.item.input_audio_transcription.completed")
	require.Contains(t, second[len(second)-1].Raw, `"total_tokens":100`, "the beta client sees Bailian's running total")

	hasText := false

	for _, f := range first {
		if f.Get("type").String() == "conversation.item.input_audio_transcription.text" {
			hasText = true
		}
	}

	require.True(t, hasText, "Bailian's own event names reach the client")

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.finish"}`)))
	require.Equal(t, "session.finished", gjson.Get(readWS(t, conn), "type").String())

	require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
	_ = conn.Close()

	logs := h.usageLogs(t, 2)
	require.Len(t, logs, 2)
	require.EqualValues(t, 50, logs[0].TotalTokens)
	require.EqualValues(t, 50, logs[1].TotalTokens, "billed per utterance")

	_, _, received := h.upstream.session(t, 0)
	require.Equal(t, update, received[0], "client frames are forwarded byte for byte")

	finishes := 0

	for _, frame := range received {
		if gjson.Get(frame, "type").String() == "session.finish" {
			finishes++
		}
	}

	require.Equal(t, 1, finishes, "the gateway does not finish a session the client already finished")

	rows, err := h.db.Request.Query().Where(request.ModelIDEQ("qwen3-asr-flash-realtime")).All(h.ctx)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
}

func TestRealtimeASRRoutes_LegacyClientForANonASRModelIsRejected(t *testing.T) {
	h := newASRRouteHarness(t)

	// The omni model exists on the channel, but only ASR models take the beta protocol.
	_, resp, err := h.dial(t, "/v1/realtime?model=qwen3.8-omni-flash-realtime", http.Header{"Openai-Beta": []string{"realtime=v1"}})
	require.Error(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readErrorBody(t, resp), "Qwen-ASR")

	// A model no channel serves is "model not found", whatever the protocol.
	_, resp, err = h.dial(t, "/v1/realtime?model=no-such-model", http.Header{"Openai-Beta": []string{"realtime=v1"}})
	require.Error(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, resp.StatusCode)
}

func TestRealtimeASRRoutes_LegacyClientWithoutModelAccessGetsTheAccessAnswer(t *testing.T) {
	h := newASRRouteHarness(t)

	restricted := h.db.APIKey.Create().
		SetName("restricted").
		SetKey("ah-restricted-key").
		SetUserID(h.key.UserID).
		SetProjectID(h.key.ProjectID).
		SetType(h.key.Type).
		SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "p", Profiles: []objects.APIKeyProfile{{Name: "p", ModelIDs: []string{"gpt-4o"}}}}).
		SaveX(h.ctx)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+restricted.Key)
	header.Set("Openai-Beta", "realtime=v1")

	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(h.gateway.URL, "http")+"/v1/realtime?model=qwen3-asr-flash-realtime", header)
	require.Error(t, err)
	require.NotEqual(t, http.StatusSwitchingProtocols, resp.StatusCode)
	require.NotContains(t, readErrorBody(t, resp), "beta protocol", "a model-access denial is not a protocol problem")
}
