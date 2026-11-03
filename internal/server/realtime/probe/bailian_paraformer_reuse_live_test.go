package probe

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime"
)

// TestBailianLive_ParaformerTaskReuse checks the one fact the gateway's commit handling relies
// on: after finish-task / task-finished, a second run-task works on the same connection. It
// sends about one second of speech per task (about 2 s of billed audio in total).
func TestBailianLive_ParaformerTaskReuse(t *testing.T) {
	key := os.Getenv("TEST_BAILIAN_API_KEY")
	if key == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}

	pcm, err := os.ReadFile("testdata/zh_24000.pcm")
	require.NoError(t, err)

	// One second of the 24 kHz clip, resampled down by taking every third sample pair
	// would distort; send it as 24 kHz instead (Paraformer v2 accepts any rate it is told).
	second := pcm[:24000*2]

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	conn, _, err := realtime.DialUpstream(ctx, nil, "wss://dashscope.aliyuncs.com/api-ws/v1/inference", headers, 15*time.Second)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	read := func(want string) string {
		var seen []string

		for {
			_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))

			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("waiting for %s: %v (seen %s)", want, err, strings.Join(seen, ", "))
			}

			event := gjson.GetBytes(data, "header.event").String()
			seen = append(seen, event)

			if event == "result-generated" && gjson.GetBytes(data, "payload.output.sentence.sentence_end").Bool() {
				t.Logf("final: %s usage=%s", gjson.GetBytes(data, "payload.output.sentence.text").String(), gjson.GetBytes(data, "payload.usage").Raw)
			}

			if event == "task-failed" {
				t.Fatalf("task-failed: %s", string(data))
			}

			if event == want {
				t.Logf("%s after %s", want, strings.Join(seen, ", "))
				return string(data)
			}
		}
	}

	for round := 1; round <= 2; round++ {
		taskID := uuid.NewString()

		run, _ := json.Marshal(map[string]any{
			"header": map[string]any{"action": "run-task", "task_id": taskID, "streaming": "duplex"},
			"payload": map[string]any{
				"task_group": "audio", "task": "asr", "function": "recognition", "model": "paraformer-realtime-v2",
				"parameters": map[string]any{"format": "pcm", "sample_rate": 24000},
				"input":      map[string]any{},
			},
		})
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, run))
		read("task-started")

		for off := 0; off < len(second); off += 4800 {
			require.NoError(t, conn.WriteMessage(websocket.BinaryMessage, second[off:min(off+4800, len(second))]))
		}

		finish, _ := json.Marshal(map[string]any{
			"header":  map[string]any{"action": "finish-task", "task_id": taskID, "streaming": "duplex"},
			"payload": map[string]any{"input": map[string]any{}},
		})
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, finish))
		read("task-finished")

		t.Logf("round %d ok on the same connection", round)
	}
}
