package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// fakeBailianInference behaves like the qwen-audio-3.1 ASR models on Bailian's inference
// WebSocket as probed live on 2026-11-05: run-task → task-started, binary PCM, and on
// finish-task the sentence's empty partial, (-streaming) a cumulative partial, the final and
// task-finished. Only -message reports usage, cumulative per task.
type fakeBailianInference struct {
	server *httptest.Server

	mu       sync.Mutex
	urls     []string
	runTasks []string
	audio    int // PCM bytes received
}

func newFakeBailianInference(t *testing.T) *fakeBailianInference {
	t.Helper()

	f := &fakeBailianInference{}
	upgrader := websocket.Upgrader{}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		f.mu.Lock()
		f.urls = append(f.urls, r.URL.String())
		f.mu.Unlock()

		send := func(v any) {
			b, _ := json.Marshal(v)
			_ = conn.WriteMessage(websocket.TextMessage, b)
		}

		var taskID, model string

		taskAudio := 0

		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}

			if typ == websocket.BinaryMessage {
				taskAudio += len(data)

				f.mu.Lock()
				f.audio += len(data)
				f.mu.Unlock()

				continue
			}

			header := func(event string) map[string]any {
				return map[string]any{"task_id": taskID, "event": event, "attributes": map[string]any{}}
			}

			switch gjson.GetBytes(data, "header.action").String() {
			case "run-task":
				taskID = gjson.GetBytes(data, "header.task_id").String()
				model = gjson.GetBytes(data, "payload.model").String()

				f.mu.Lock()
				f.runTasks = append(f.runTasks, string(data))
				f.mu.Unlock()

				send(map[string]any{"header": header("task-started"), "payload": map[string]any{}})
			case "finish-task":
				end := taskAudio/32 - 100 // 16 kHz PCM16: 32 bytes per ms
				result := func(text string, final bool, usage map[string]any) {
					sentence := map[string]any{
						"sentence_id": 1, "begin_time": 160, "end_time": nil, "text": text, "channel_id": 0,
						"speaker_id": nil, "sentence_end": final, "words": []any{},
					}
					if final {
						sentence["end_time"] = end
					}
					if text == "" {
						sentence["sentence_begin"] = true
					}

					output := map[string]any{"sentence": sentence, "text": text}
					output["output"] = map[string]any{"sentence": sentence, "text": text}

					payload := map[string]any{"output": output}
					if usage != nil {
						payload["usage"] = usage
					}

					send(map[string]any{"header": header("result-generated"), "payload": payload})
				}

				var usage map[string]any
				if strings.HasSuffix(model, "-message") {
					usage = map[string]any{"duration": (taskAudio/32 + 999) / 1000, "input_tokens": 44, "output_tokens": 8, "total_tokens": 52}
				} else {
					result("你好", false, nil) // -message sends no partial text
				}

				result("", false, nil)
				result("你好世界。", true, usage)

				finished := map[string]any{"output": map[string]any{}}
				if usage != nil {
					finished["usage"] = usage
				}

				send(map[string]any{"header": header("task-finished"), "payload": finished})

				taskAudio = 0
			}
		}
	}))

	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeBailianInference) snapshot() ([]string, []string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.urls...), append([]string(nil), f.runTasks...), f.audio
}

// newInferenceRouteHarness serves /v1/realtime with one Bailian channel whose realtime endpoint
// is the fake; the streaming model has a duration_seconds price of 0.001 per second.
func newInferenceRouteHarness(t *testing.T) (*asrRouteHarness, *fakeBailianInference) {
	t.Helper()

	h := newCodexRouteHarness(t)
	upstream := newFakeBailianInference(t)

	ch := h.db.Channel.Create().
		SetName("bailian-qwen-audio").
		SetType(channel.TypeBailian).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetEndpoints([]objects.ChannelEndpoint{{APIFormat: llm.APIFormatOpenAIRealtime.String(), BaseURL: upstream.server.URL + "/api-ws/v1", Path: "/realtime"}}).
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-bailian"}).
		SetSupportedModels([]string{"qwen-audio-3.1-asr-flash-streaming", "qwen-audio-3.1-asr-flash-message"}).
		SetDefaultTestModel("qwen-audio-3.1-asr-flash-streaming").
		SaveX(h.ctx)

	perSecond := decimal.RequireFromString("0.001")
	h.db.ChannelModelPrice.Create().
		SetChannelID(ch.ID).
		SetModelID("qwen-audio-3.1-asr-flash-streaming").
		SetReferenceID("qwen-audio-streaming-seconds").
		SetPrice(objects.ModelPrice{Items: []objects.ModelPriceItem{{
			ItemCode: objects.PriceItemCodeDurationSeconds,
			Pricing:  objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &perSecond},
		}}}).
		SaveX(h.ctx)

	modelCache := biz.NewModelCacheService(biz.ModelCacheServiceParams{Ent: h.db})
	t.Cleanup(modelCache.Stop)
	channels := biz.NewChannelService(biz.ChannelServiceParams{
		ModelCache:    modelCache,
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		Ent:           h.db,
		SystemService: h.system,
		HttpClient:    httpclient.NewHttpClient(),
	})
	t.Cleanup(channels.Stop)

	models := biz.NewModelService(biz.ModelServiceParams{ModelCache: modelCache, Ent: h.db, SystemService: h.system, ChannelService: channels})
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

	return &asrRouteHarness{codexRouteHarness: h, gateway: gateway}, upstream
}

func TestRealtimeQwenAudioRoutes_GATranscriptionOverTheInferenceProtocol(t *testing.T) {
	cases := []struct {
		model     string
		usageType string
	}{
		{"qwen-audio-3.1-asr-flash-streaming", "duration"},
		{"qwen-audio-3.1-asr-flash-message", "tokens"},
	}

	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			h, upstream := newInferenceRouteHarness(t)

			conn, resp, err := h.dial(t, "/v1/realtime?model="+c.model, nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)

			created := gjson.Parse(readWS(t, conn))
			require.Equal(t, "session.created", created.Get("type").String())
			require.Equal(t, "transcription", created.Get("session.type").String(), "a transcription session without intent")
			require.EqualValues(t, 800, created.Get("session.audio.input.turn_detection.silence_duration_ms").Int())

			update := `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"noise_reduction":{"type":"near_field"},"transcription":{"language":"zh"},"turn_detection":{"type":"server_vad","silence_duration_ms":500}}}}}`
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(update)))
			updated := gjson.Parse(readWS(t, conn))
			require.Equal(t, "session.updated", updated.Get("type").String())
			require.Equal(t, "near_field", updated.Get("session.audio.input.noise_reduction.type").String())

			for range 10 {
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.append","audio":"`+pcm24(100)+`"}`)))
			}
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.commit"}`)))

			frames := readUntil(t, conn, "conversation.item.done")

			var transcript strings.Builder

			for _, f := range frames {
				require.NotEqual(t, "error", f.Get("type").String(), f.Raw)

				switch f.Get("type").String() {
				case "conversation.item.input_audio_transcription.delta":
					require.NotEmpty(t, f.Get("delta").String())
					transcript.WriteString(f.Get("delta").String())
				case "conversation.item.input_audio_transcription.completed":
					require.Equal(t, "你好世界。", f.Get("transcript").String())
					require.Equal(t, c.usageType, f.Get("usage.type").String())
				}
			}

			require.Equal(t, "你好世界。", transcript.String())

			require.NoError(t, conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
			_ = conn.Close()

			logs := h.usageLogs(t, 1)

			// Upstream: the inference endpoint without the model query, one run-task with only
			// the probed parameters, then 1 s of 16 kHz PCM.
			urls, runTasks, audio := upstream.snapshot()
			require.Len(t, urls, 1)
			require.True(t, strings.HasPrefix(urls[0], "/api-ws/v1/inference"), urls[0])
			require.NotContains(t, urls[0], "model=")

			require.Len(t, runTasks, 1)
			run := gjson.Parse(runTasks[0])
			require.Equal(t, c.model, run.Get("payload.model").String())
			require.Len(t, run.Get("header.task_id").String(), 32)
			require.JSONEq(t, `{"format":"pcm","sample_rate":16000,"language_hints":["zh"],"max_sentence_silence":500,"vad_model":"near_meeting_16k","heartbeat":true}`,
				run.Get("payload.parameters").Raw)
			require.InDelta(t, 32000, audio, 640)

			// Accounting: one row billed under the ASR model.
			require.Len(t, logs, 1)
			require.Equal(t, c.model, logs[0].ModelID)

			if c.usageType == "tokens" {
				require.EqualValues(t, 44, logs[0].PromptTokens)
				require.EqualValues(t, 8, logs[0].CompletionTokens)
				require.EqualValues(t, 52, logs[0].TotalTokens)

				return
			}

			require.Zero(t, logs[0].TotalTokens, "Bailian reports no usage for -streaming")
			require.Len(t, logs[0].CostItems, 1)
			require.Equal(t, objects.PriceItemCodeDurationSeconds, logs[0].CostItems[0].ItemCode)
			require.EqualValues(t, 1, logs[0].CostItems[0].Quantity, "1 s of audio")
			require.NotNil(t, logs[0].TotalCost)
			require.InDelta(t, 0.001, *logs[0].TotalCost, 1e-9)
		})
	}
}
