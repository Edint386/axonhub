package probe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/internal/server/realtime/dialect/bailian"
)

// TestBailianLive_ParaformerAccess is digest step-1: dial inference, run-task, finish-task with
// ZERO audio. task-started means the account can use Paraformer. finish-task with no audio may
// return task-finished or task-failed/NO_VALID_AUDIO_ERROR — both count as access OK.
//
//	export TEST_BAILIAN_API_KEY="$(tr -d '\r\n' < /path/to/key)"
//	go test ./internal/server/realtime/probe/ -run 'TestBailianLive_ParaformerAccess$' -count=1 -v -timeout 60s
func TestBailianLive_ParaformerAccess(t *testing.T) {
	if os.Getenv("TEST_BAILIAN_API_KEY") == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}

	model := paraformerModel()
	conn := dialParaformer(t)
	defer func() { _ = conn.Close() }()

	taskID := uuid.NewString()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, paraformerRunTask(taskID, model, 16000, nil)))

	started := readParaformerEvent(t, conn, 15*time.Second)
	ev := paraformerEvent(started)
	t.Logf("after run-task: event=%s raw=%s", ev, truncate(string(started)))
	if ev != "task-started" {
		code := gjson.GetBytes(started, "header.error_code").String()
		msg := gjson.GetBytes(started, "header.error_message").String()
		t.Logf("AUDIO_SPENT_SECONDS=0 access=denied error_code=%s message=%s", code, msg)
		if code == "Arrearage" {
			t.Skip("Bailian account Arrearage: handshake OK but Paraformer run-task denied; clear billing before speech probes")
		}
		require.Equal(t, "task-started", ev, "account/access check failed: %s", truncate(string(started)))
	}

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, paraformerFinishTask(taskID)))

	var finishedEv, finishedCode string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		frame := readParaformerEvent(t, conn, time.Until(deadline))
		ev := paraformerEvent(frame)
		code := gjson.GetBytes(frame, "header.error_code").String()
		t.Logf("after finish-task: event=%s error_code=%s raw=%s", ev, code, truncate(string(frame)))
		if ev == "task-finished" || ev == "task-failed" {
			finishedEv, finishedCode = ev, code
			break
		}
	}
	// Digest C2: task-finished OR a clear task-failed after task-started confirms access.
	switch {
	case finishedEv == "task-finished":
		// ideal
	case finishedEv == "task-failed" && finishedCode == "NO_VALID_AUDIO_ERROR":
		// expected when finishing with zero audio
	default:
		require.Failf(t, "unexpected finish outcome", "event=%s error_code=%s", finishedEv, finishedCode)
	}
	t.Logf("AUDIO_SPENT_SECONDS=0 model=%s access=ok finish=%s code=%s", model, finishedEv, finishedCode)
}

// TestBailianLive_ParaformerSpeechDialect drives the GA↔Paraformer dialect against the live
// inference endpoint with at most ~4 s of Mandarin PCM (testdata/zh_24000.pcm). Opt-in only.
//
//	TEST_BAILIAN_PARAFORMER_SPEECH=1 go test ... -run 'TestBailianLive_ParaformerSpeechDialect$'
func TestBailianLive_ParaformerSpeechDialect(t *testing.T) {
	if os.Getenv("TEST_BAILIAN_API_KEY") == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}
	if os.Getenv("TEST_BAILIAN_PARAFORMER_SPEECH") == "" {
		t.Skip("TEST_BAILIAN_PARAFORMER_SPEECH not set (refusing to spend audio without explicit opt-in)")
	}

	model := paraformerModel()
	require.Equal(t, "paraformer-realtime-v2", model, "this probe uses only the free-quota v2 model")

	pcm24 := mustReadTestdata(t, "zh_24000.pcm")
	// Cap client-side 24 kHz PCM at 4 s before dialect resampling.
	max24 := 24000 * 2 * 4
	if len(pcm24) > max24 {
		pcm24 = pcm24[:max24]
	}
	spent := audio.ByteLenToSeconds(len(pcm24), 24000)
	t.Logf("client audio budget: %.2f s @24kHz (%d bytes) model=%s", spent, len(pcm24), model)
	require.LessOrEqual(t, spent, 4.01)

	d, err := bailian.NewParaformer(bailian.ParaformerOptions{
		RequestedModel: "gpt-4o-transcribe",
		ActualModel:    model,
		IDs:            &dialect.EventIDs{},
	})
	require.NoError(t, err)

	created := d.Greeting()
	require.NotEmpty(t, created)
	require.Equal(t, "session.created", gjson.GetBytes(created[0], "type").String())

	out, err := d.ClientFrame([]byte(`{"event_id":"u1","type":"session.update","session":{"type":"transcription","audio":{"input":{"transcription":{"language":"zh"},"turn_detection":{"type":"server_vad","silence_duration_ms":800}}}}}`))
	require.NoError(t, err)
	require.Equal(t, "session.updated", gjson.GetBytes(out.ToClient[0], "type").String())

	conn := dialParaformer(t)
	defer func() { _ = conn.Close() }()

	chunks := audio.SplitPCM(pcm24, 24000, 100*time.Millisecond)
	require.NotEmpty(t, chunks)

	// First GA append → run-task + gate.
	firstAppend := gaAppendFrame(chunks[0])
	out, err = d.ClientFrame([]byte(firstAppend))
	require.NoError(t, err)
	require.True(t, out.CloseGate)
	require.Len(t, out.ToUpstream, 1)
	require.False(t, out.BinaryUpstream)
	require.Equal(t, "run-task", gjson.GetBytes(out.ToUpstream[0], "header.action").String())
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, out.ToUpstream[0]))

	started := readParaformerEvent(t, conn, 15*time.Second)
	t.Logf("after run-task: event=%s raw=%s", paraformerEvent(started), truncate(string(started)))
	if paraformerEvent(started) != "task-started" {
		code := gjson.GetBytes(started, "header.error_code").String()
		t.Logf("AUDIO_SPENT_SECONDS=0 speech_aborted error_code=%s", code)
		if code == "Arrearage" {
			t.Skip("Bailian account Arrearage; cannot spend speech quota until billing is cleared")
		}
		require.Equal(t, "task-started", paraformerEvent(started), truncate(string(started)))
	}

	out, err = d.UpstreamFrame(started)
	require.NoError(t, err)
	require.True(t, out.OpenGate)
	require.True(t, out.BinaryUpstream)
	for _, frame := range out.ToUpstream {
		require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, frame))
	}

	// Remaining GA appends → binary PCM via dialect.
	for i, chunk := range chunks[1:] {
		out, err = d.ClientFrame([]byte(gaAppendFrame(chunk)))
		require.NoError(t, err, "chunk %d", i+1)
		require.True(t, out.BinaryUpstream)
		for _, frame := range out.ToUpstream {
			require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, frame))
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, bye := range d.CloseUpstream() {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, bye))
	}

	deadline := time.Now().Add(20 * time.Second)
	var finals, partials int
	var lastText, lastCompleted string
	var lastUsage string
	var gaTypes []string
	for time.Now().Before(deadline) {
		frame := readParaformerEvent(t, conn, time.Until(deadline))
		ev := paraformerEvent(frame)
		t.Logf("upstream event=%s raw=%s", ev, truncate(string(frame)))

		out, err = d.UpstreamFrame(frame)
		require.NoError(t, err)
		for _, down := range out.ToClient {
			typ := gjson.GetBytes(down, "type").String()
			gaTypes = append(gaTypes, typ)
			if typ == "conversation.item.input_audio_transcription.completed" {
				lastCompleted = gjson.GetBytes(down, "transcript").String()
				lastUsage = gjson.GetBytes(down, "usage").Raw
			}
			if typ == "error" {
				t.Fatalf("dialect error: %s", truncate(string(down)))
			}
		}
		for _, rec := range out.Usage {
			t.Logf("usage kind=%s model=%s duration_seconds=%.3f", rec.Kind, rec.Model, rec.DurationSeconds)
		}

		switch ev {
		case "result-generated":
			sent := gjson.GetBytes(frame, "payload.output.sentence")
			if sent.Get("heartbeat").Bool() {
				continue
			}
			lastText = sent.Get("text").String()
			if sent.Get("sentence_end").Bool() {
				finals++
			} else {
				partials++
			}
		case "task-finished":
			t.Logf("AUDIO_SPENT_SECONDS=%.2f partials=%d finals=%d upstream_text=%q ga_completed=%q usage=%s ga_types=%v",
				spent, partials, finals, lastText, lastCompleted, lastUsage, gaTypes)
			require.NotEmpty(t, lastCompleted, "dialect should emit a completed transcript")
			return
		case "task-failed":
			t.Fatalf("task-failed: %s", truncate(string(frame)))
		}
	}
	t.Fatalf("timed out; partials=%d finals=%d upstream_text=%q ga_completed=%q ga_types=%v",
		partials, finals, lastText, lastCompleted, gaTypes)
}

func gaAppendFrame(pcm []byte) string {
	return `{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(pcm) + `"}`
}

func paraformerModel() string {
	if m := os.Getenv("TEST_BAILIAN_PARAFORMER_MODEL"); m != "" {
		return m
	}
	return "paraformer-realtime-v2"
}

func dialParaformer(t *testing.T) *websocket.Conn {
	t.Helper()
	key := os.Getenv("TEST_BAILIAN_API_KEY")
	require.NotEmpty(t, key)

	wsURL := os.Getenv("TEST_BAILIAN_INFERENCE_WS_URL")
	if wsURL == "" {
		wsURL = "wss://dashscope.aliyuncs.com/api-ws/v1/inference"
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("User-Agent", "AxonHub-Realtime-Probe/1.0")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, _, err := realtime.DialUpstream(ctx, nil, wsURL, headers, 15*time.Second)
	require.NoError(t, err, "dial %s", wsURL)
	return conn
}

func paraformerRunTask(taskID, model string, sampleRate int, languages []string) []byte {
	params := map[string]any{
		"format":      "pcm",
		"sample_rate": sampleRate,
	}
	if len(languages) > 0 {
		params["language_hints"] = languages
	}
	b, _ := json.Marshal(map[string]any{
		"header": map[string]any{
			"action":    "run-task",
			"task_id":   taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{
			"task_group": "audio",
			"task":       "asr",
			"function":   "recognition",
			"model":      model,
			"parameters": params,
			"input":      map[string]any{},
		},
	})
	return b
}

func paraformerFinishTask(taskID string) []byte {
	b, _ := json.Marshal(map[string]any{
		"header": map[string]any{
			"action":    "finish-task",
			"task_id":   taskID,
			"streaming": "duplex",
		},
		"payload": map[string]any{"input": map[string]any{}},
	})
	return b
}

func paraformerEvent(frame []byte) string {
	return gjson.GetBytes(frame, "header.event").String()
}

func readParaformerEvent(t *testing.T, conn *websocket.Conn, timeout time.Duration) []byte {
	t.Helper()
	if timeout <= 0 {
		timeout = time.Second
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	msgType, data, err := conn.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, websocket.TextMessage, msgType, "unexpected binary from paraformer control path")
	return data
}

func mustReadTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	require.NotEmpty(t, b)
	return b
}
