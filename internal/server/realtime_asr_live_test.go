package server

import (
	"encoding/base64"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

// TestRealtimeASRLive drives the whole gateway against the real Bailian service: it speaks a
// sentence through the gateway's Qwen-Omni path, then transcribes that audio through the
// gateway's Qwen-ASR paths (GA server_vad, GA manual, beta passthrough). It is skipped without
// TEST_BAILIAN_API_KEY and never runs in CI.
func TestRealtimeASRLive(t *testing.T) {
	key := os.Getenv("TEST_BAILIAN_API_KEY")
	if key == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}

	h := newCodexRouteHarness(t)

	h.db.Channel.Create().
		SetName("bailian").
		SetType(channel.TypeBailian).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://dashscope.aliyuncs.com/compatible-mode/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: key}).
		SetSupportedModels([]string{"qwen3.8-omni-flash-realtime", "qwen3-asr-flash-realtime"}).
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
	orch := orchestrator.NewChatCompletionOrchestrator(channels, selector, h.requests, httpclient.NewHttpClient(), openai.NewInboundTransformer(), h.system, usage, nil, nil, nil, biz.NewLiveStreamRegistry(), orchestrator.NewChannelLimiterManager(), nil, orchestrator.PluginIntegration{})
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: false})

	registerRealtimeRoutes(
		&Server{Engine: h.router, Config: Config{LLMRequestTimeout: 30 * time.Second}},
		Handlers{OpenAI: &api.OpenAIHandlers{ChatCompletionHandlers: api.NewChatCompletionHandlers(orch)}},
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
	)

	gateway := httptest.NewServer(h.router)
	t.Cleanup(gateway.Close)

	dial := func(path string, extra http.Header) *websocket.Conn {
		header := http.Header{}
		header.Set("Authorization", "Bearer "+h.key.Key)

		maps.Copy(header, extra)

		conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(gateway.URL, "http")+path, header)
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("dial %s: %d %s", path, resp.StatusCode, readErrorBody(t, resp))
		}

		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		return conn
	}

	read := func(conn *websocket.Conn, timeout time.Duration) (gjson.Result, bool) {
		_ = conn.SetReadDeadline(time.Now().Add(timeout))

		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Logf("read end: %v", err)
			return gjson.Result{}, false
		}

		return gjson.ParseBytes(data), true
	}

	send := func(conn *websocket.Conn, frame string) {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(frame)))
	}

	// --- 1. Speech from Qwen-Omni through the gateway (GA protocol).
	omni := dial("/v1/realtime?model=qwen3.8-omni-flash-realtime", nil)

	ev, ok := read(omni, 15*time.Second)
	require.True(t, ok)
	require.Equal(t, "session.created", ev.Get("type").String())

	send(omni, `{"type":"session.update","session":{"type":"realtime","output_modalities":["audio"],"audio":{"input":{"turn_detection":null}}}}`)
	send(omni, `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"请用普通话清晰地朗读这句话，只读这句话：今天天气不错，阳光明媚，我们一起去公园散步吧。"}]}}`)
	send(omni, `{"type":"response.create"}`)

	var speech []byte

	for {
		ev, ok := read(omni, 60*time.Second)
		require.True(t, ok, "omni: no response.done")

		if ev.Get("type").String() == "response.output_audio.delta" {
			chunk, err := base64.StdEncoding.DecodeString(ev.Get("delta").String())
			require.NoError(t, err)

			speech = append(speech, chunk...)
		}

		require.NotEqual(t, "error", ev.Get("type").String(), ev.Raw)

		if ev.Get("type").String() == "response.done" {
			t.Logf("omni transcript: %s", ev.Get("response.output.0.content.0.transcript").String())
			break
		}
	}

	_ = omni.Close()

	require.Greater(t, len(speech), 24000*2, "less than a second of speech")
	t.Logf("speech: %d ms at 24 kHz", len(speech)*1000/(24000*2))

	stream := func(conn *websocket.Conn, pcm []byte, rate int, paced bool) {
		for _, chunk := range audio.SplitPCM(pcm, rate, 100*time.Millisecond) {
			send(conn, `{"type":"input_audio_buffer.append","audio":"`+base64.StdEncoding.EncodeToString(chunk)+`"}`)

			if paced {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	// collect reads until a frame of type until (or the timeout) and logs the event types.
	collect := func(name string, conn *websocket.Conn, until string, timeout time.Duration) []gjson.Result {
		var (
			frames []gjson.Result
			types  []string
		)

		deadline := time.Now().Add(timeout)

		for time.Now().Before(deadline) {
			ev, ok := read(conn, time.Until(deadline))
			if !ok {
				break
			}

			frames = append(frames, ev)
			types = append(types, ev.Get("type").String())

			if ev.Get("type").String() == until {
				break
			}
		}

		t.Logf("%s events: %s", name, strings.Join(types, ", "))

		return frames
	}

	checkGA := func(name string, frames []gjson.Result) {
		var (
			deltas    strings.Builder
			completed gjson.Result
		)

		for _, f := range frames {
			require.NotEqual(t, "error", f.Get("type").String(), "%s: %s", name, f.Raw)

			switch f.Get("type").String() {
			case "conversation.item.input_audio_transcription.delta":
				deltas.WriteString(f.Get("delta").String())
			case "conversation.item.input_audio_transcription.completed":
				completed = f
			}
		}

		require.True(t, completed.Exists(), "%s: no completed", name)

		transcript := completed.Get("transcript").String()
		t.Logf("%s transcript: %s | deltas: %s | usage: %s", name, transcript, deltas.String(), completed.Get("usage").Raw)

		require.Contains(t, transcript, "公园")
		require.Equal(t, transcript, deltas.String(), "%s: deltas must spell the transcript", name)
		require.Equal(t, "tokens", completed.Get("usage.type").String())
		require.Positive(t, completed.Get("usage.input_token_details.audio_tokens").Int())
	}

	// --- 2. GA transcription session, server_vad, 24 kHz client audio.
	vad := dial("/v1/realtime?intent=transcription&model=qwen3-asr-flash-realtime", nil)

	ev, ok = read(vad, 15*time.Second)
	require.True(t, ok)
	require.Equal(t, "transcription", ev.Get("session.type").String(), ev.Raw)

	send(vad, `{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"gpt-4o-transcribe","language":"zh"}}}}}`)
	send(vad, `{"event_id":"u2","type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":{"type":"server_vad","silence_duration_ms":600}}}}}`)

	for range 2 {
		ev, ok = read(vad, 5*time.Second)
		require.True(t, ok)
		require.Equal(t, "session.updated", ev.Get("type").String(), ev.Raw)
	}

	stream(vad, speech, 24000, true)
	stream(vad, make([]byte, 24000*2*3/2), 24000, true)

	checkGA("ga server_vad", collect("ga server_vad", vad, "conversation.item.done", 20*time.Second))

	require.NoError(t, vad.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
	_ = vad.Close()

	// --- 3. GA manual mode: append everything, commit.
	manual := dial("/v1/realtime?intent=transcription&model=qwen3-asr-flash-realtime", nil)

	_, ok = read(manual, 15*time.Second)
	require.True(t, ok)

	send(manual, `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"turn_detection":null}}}}`)

	_, ok = read(manual, 5*time.Second)
	require.True(t, ok)

	stream(manual, speech, 24000, false)
	send(manual, `{"type":"input_audio_buffer.commit"}`)

	checkGA("ga manual", collect("ga manual", manual, "conversation.item.done", 20*time.Second))

	_ = manual.Close()

	// --- 4. Beta-protocol client relayed unchanged (16 kHz, Bailian's own events).
	rs, err := audio.NewResampler(24000, 16000)
	require.NoError(t, err)

	speech16 := rs.Process(speech)

	legacy := dial("/v1/realtime?model=qwen3-asr-flash-realtime", http.Header{"Openai-Beta": []string{"realtime=v1"}})

	ev, ok = read(legacy, 15*time.Second)
	require.True(t, ok)
	require.Equal(t, "realtime.session", ev.Get("session.object").String(), ev.Raw)

	send(legacy, `{"type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":{"type":"server_vad","silence_duration_ms":600}}}`)

	ev, ok = read(legacy, 5*time.Second)
	require.True(t, ok)
	require.Equal(t, "session.updated", ev.Get("type").String(), ev.Raw)

	stream(legacy, speech16, 16000, true)
	stream(legacy, make([]byte, 16000*2*3/2), 16000, true)

	frames := collect("legacy", legacy, "conversation.item.input_audio_transcription.completed", 20*time.Second)
	require.NotEmpty(t, frames)

	last := frames[len(frames)-1]
	require.Equal(t, "conversation.item.input_audio_transcription.completed", last.Get("type").String())
	require.Contains(t, last.Get("transcript").String(), "公园")

	send(legacy, `{"type":"session.finish"}`)
	collect("legacy finish", legacy, "session.finished", 5*time.Second)

	_ = legacy.Close()

	// --- 5. Accounting: ASR usage rows billed under the ASR model.
	deadline := time.Now().Add(10 * time.Second)

	for {
		logs, err := h.db.UsageLog.Query().All(h.ctx)
		require.NoError(t, err)

		asr := 0

		for _, l := range logs {
			if l.ModelID == "qwen3-asr-flash-realtime" {
				asr++

				t.Logf("usage row: model=%s prompt=%d (audio %d) completion=%d total=%d", l.ModelID, l.PromptTokens, l.PromptAudioTokens, l.CompletionTokens, l.TotalTokens)
			}
		}

		if asr >= 3 || time.Now().After(deadline) {
			require.GreaterOrEqual(t, asr, 3, "one row per transcribed utterance in the three ASR sessions")
			break
		}

		time.Sleep(200 * time.Millisecond)
	}
}
