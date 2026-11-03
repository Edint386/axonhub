package server

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// TestRealtimeParaformerGatewayLive is the full in-process /v1/realtime E2E against real
// Bailian Paraformer: httptest gateway + Bailian channel + GA transcription client + one
// ≤4 s Mandarin clip. Opt-in only; never runs in CI.
//
//	export TEST_BAILIAN_API_KEY="$(tr -d '\r\n' < /path/to/key)"
//	export TEST_BAILIAN_PARAFORMER_GATEWAY=1
//	go test ./internal/server/ -run 'TestRealtimeParaformerGatewayLive$' -count=1 -v -timeout 120s
func TestRealtimeParaformerGatewayLive(t *testing.T) {
	key := os.Getenv("TEST_BAILIAN_API_KEY")
	if key == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}
	if os.Getenv("TEST_BAILIAN_PARAFORMER_GATEWAY") == "" {
		t.Skip("TEST_BAILIAN_PARAFORMER_GATEWAY not set (refusing to spend audio without explicit opt-in)")
	}

	const model = "paraformer-realtime-v2"

	h := newCodexRouteHarness(t)

	h.db.Channel.Create().
		SetName("bailian-paraformer").
		SetType(channel.TypeBailian).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: key}).
		SetSupportedModels([]string{model}).
		SetDefaultTestModel(model).
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
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: 30 * time.Second}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	pcmPath := filepath.Join("realtime", "probe", "testdata", "zh_24000.pcm")
	pcm24, err := os.ReadFile(pcmPath)
	require.NoError(t, err, "testdata %s", pcmPath)
	// Keep speech short enough that speech + VAD trailing silence stay ≤5 s billed audio.
	maxSpeech := 24000 * 2 * 35 / 10 // 3.5 s
	if len(pcm24) > maxSpeech {
		pcm24 = pcm24[:maxSpeech]
	}
	silence := make([]byte, 24000*2) // 1.0 s of zeros for server_vad to end the sentence
	spent := audio.ByteLenToSeconds(len(pcm24)+len(silence), 24000)
	t.Logf("AUDIO_SPENT_SECONDS=%.2f speech=%.2fs silence=1.00s model=%s", spent, audio.ByteLenToSeconds(len(pcm24), 24000), model)
	require.LessOrEqual(t, spent, 5.0)

	header := http.Header{}
	header.Set("Authorization", "Bearer "+h.key.Key)
	wsURL := "ws" + strings.TrimPrefix(gateway.URL, "http") + "/v1/realtime?intent=transcription&model=" + model
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, header)
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("dial: %d %s", resp.StatusCode, readErrorBody(t, resp))
	}
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	read := func(timeout time.Duration) (gjson.Result, bool) {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Logf("read end: %v", err)
			return gjson.Result{}, false
		}
		return gjson.ParseBytes(data), true
	}
	send := func(frame string) {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
	}

	ev, ok := read(15 * time.Second)
	require.True(t, ok)
	require.Equal(t, "session.created", ev.Get("type").String(), ev.Raw)
	require.Equal(t, "transcription", ev.Get("session.type").String())
	require.Equal(t, "realtime.transcription_session", ev.Get("session.object").String())

	send(`{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"gpt-4o-transcribe","language":"zh"},"turn_detection":{"type":"server_vad","silence_duration_ms":800}}}}}`)
	ev, ok = read(5 * time.Second)
	require.True(t, ok)
	require.Equal(t, "session.updated", ev.Get("type").String(), ev.Raw)

	for _, chunk := range audio.SplitPCM(pcm24, 24000, 100*time.Millisecond) {
		send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
		time.Sleep(100 * time.Millisecond)
	}
	for _, chunk := range audio.SplitPCM(silence, 24000, 100*time.Millisecond) {
		send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
		time.Sleep(100 * time.Millisecond)
	}

	var (
		completed gjson.Result
		types     []string
		deltas    strings.Builder
	)
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && !completed.Exists() {
		ev, ok = read(time.Until(deadline))
		if !ok {
			break
		}
		typ := ev.Get("type").String()
		types = append(types, typ)
		require.NotEqual(t, "error", typ, ev.Raw)
		switch typ {
		case "conversation.item.input_audio_transcription.delta":
			deltas.WriteString(ev.Get("delta").String())
		case "conversation.item.input_audio_transcription.completed":
			completed = ev
		}
	}
	t.Logf("events: %s", strings.Join(types, ", "))
	require.True(t, completed.Exists(), "no completed transcription")

	transcript := completed.Get("transcript").String()
	t.Logf("transcript=%q deltas=%q usage=%s", transcript, deltas.String(), completed.Get("usage").Raw)
	require.Contains(t, transcript, "公园")
	require.Equal(t, "duration", completed.Get("usage.type").String())
	require.Greater(t, completed.Get("usage.seconds").Float(), 0.0)

	_ = conn.Close()

	// Accounting: one usage log (and request summary with duration) under the Paraformer model.
	var (
		gotLog bool
		gotSec bool
	)
	accDeadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(accDeadline) {
		logs, err := h.db.UsageLog.Query().Where(usagelog.ModelIDEQ(model)).All(h.ctx)
		require.NoError(t, err)
		for _, l := range logs {
			gotLog = true
			t.Logf("usage_log: model=%s prompt=%d completion=%d total=%d", l.ModelID, l.PromptTokens, l.CompletionTokens, l.TotalTokens)
		}

		reqs, err := h.db.Request.Query().Where(request.ModelIDEQ(model)).All(h.ctx)
		require.NoError(t, err)
		for _, r := range reqs {
			body := string(r.RequestBody)
			t.Logf("request model=%s status=%s body=%s", r.ModelID, r.Status, truncateTest(body, 300))
			if strings.Contains(body, "duration_seconds") || strings.Contains(body, `"seconds"`) {
				gotSec = true
			}
			if len(r.ResponseBody) > 0 {
				rb := string(r.ResponseBody)
				if strings.Contains(rb, "duration_seconds") || strings.Contains(rb, `"seconds"`) {
					gotSec = true
				}
			}
		}

		if gotLog && gotSec {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.True(t, gotLog, "expected a UsageLog row for %s", model)
	require.True(t, gotSec, "expected request summary to carry duration seconds for %s", model)
	t.Logf("AUDIO_SPENT_SECONDS=%.2f transcript=%q usage_seconds=%v accounting=ok",
		spent, transcript, completed.Get("usage.seconds").Float())
}

func truncateTest(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
