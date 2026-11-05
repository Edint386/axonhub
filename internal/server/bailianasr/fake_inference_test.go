package bailianasr

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// fakeInference is a DashScope inference WebSocket for qwen-audio-3.1-asr-flash-message, shaped
// like the 2026-11-05 live probe: one empty partial and one final per sentence, usage cumulative
// per task on the finals, task-finished with an empty output.
type fakeInference struct {
	t      *testing.T
	server *httptest.Server

	mu sync.Mutex
	// sentences are the finals sent after finish-task, with their cumulative usage.
	sentences []fakeSentence
	// failAfterAudio sends task-failed once the first audio frame arrived.
	failAfterAudio [2]string
	// rejectHandshake answers the upgrade with this status and a DashScope error body.
	rejectHandshake int

	runTasks       []string
	authorizations []string
	relayTokens    []string
	frames         []int
	audio          []byte
	audioBeforeRun bool
	frameTimes     []time.Time
	// sessionsEnded counts WebSocket sessions whose read loop ended (the client closed or the
	// fake hung up), so a test can wait until no frame is still in flight.
	sessionsEnded int
}

type fakeSentence struct {
	text  string
	begin int
	end   int
	usage string
}

func newFakeInference(t *testing.T) *fakeInference {
	t.Helper()

	f := &fakeInference{
		t: t,
		sentences: []fakeSentence{
			{text: "今天天气不错，我们去公园散步。", begin: 160, end: 3200, usage: `{"duration":3,"input_tokens":44,"output_tokens":8,"total_tokens":52}`},
			{text: "明天下午三点开会，记得带电脑。", begin: 4400, end: 8016, usage: `{"duration":8,"input_tokens":90,"output_tokens":18,"total_tokens":108}`},
		},
	}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeInference) baseURL() string {
	return f.server.URL + "/compatible-mode/v1"
}

func (f *fakeInference) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api-ws/v1/inference" {
		http.Error(w, `{"code":"NotFound","message":"no route"}`, http.StatusNotFound)
		return
	}

	f.mu.Lock()
	f.authorizations = append(f.authorizations, r.Header.Get("Authorization"))
	f.relayTokens = append(f.relayTokens, r.Header.Get("X-Relay-Token"))
	reject := f.rejectHandshake
	f.mu.Unlock()

	if reject != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reject)
		_, _ = fmt.Fprint(w, `{"code":"InvalidApiKey","message":"Invalid API-key provided.","request_id":"req-ws"}`)

		return
	}

	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	f.session(conn)

	f.mu.Lock()
	f.sessionsEnded++
	f.mu.Unlock()
}

func (f *fakeInference) session(conn *websocket.Conn) {
	taskID := ""
	started := false

	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			return
		}

		if kind == websocket.BinaryMessage {
			f.mu.Lock()
			if !started {
				f.audioBeforeRun = true
			}

			f.frames = append(f.frames, len(data))
			f.audio = append(f.audio, data...)
			f.frameTimes = append(f.frameTimes, time.Now())
			failure, first := f.failAfterAudio, len(f.frames) == 1
			f.mu.Unlock()

			if failure[0] != "" && first {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
					`{"header":{"task_id":"%s","event":"task-failed","error_code":"%s","error_message":"%s","attributes":{}},"payload":{}}`,
					taskID, failure[0], failure[1])))

				return
			}

			continue
		}

		root := gjson.ParseBytes(data)

		switch root.Get("header.action").String() {
		case "run-task":
			taskID = root.Get("header.task_id").String()
			started = true

			f.mu.Lock()
			f.runTasks = append(f.runTasks, string(data))
			f.mu.Unlock()

			_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
				`{"header":{"task_id":"%s","event":"task-started","attributes":{}},"payload":{}}`, taskID)))
		case "finish-task":
			f.finish(conn, taskID)
			return
		}
	}
}

func (f *fakeInference) finish(conn *websocket.Conn, taskID string) {
	f.mu.Lock()
	sentences := append([]fakeSentence(nil), f.sentences...)
	f.mu.Unlock()

	for i, s := range sentences {
		frames := []string{
			fmt.Sprintf(`{"header":{"task_id":"%s","event":"result-generated","attributes":{}},"payload":{"output":{"sentence":`+
				`{"sentence_id":%d,"begin_time":%d,"end_time":null,"text":"","sentence_end":false,"sentence_begin":true,"words":[]}}}}`,
				taskID, i+1, s.begin),
			fmt.Sprintf(`{"header":{"task_id":"%s","event":"result-generated","attributes":{}},"payload":{"output":{"sentence":`+
				`{"sentence_id":%d,"begin_time":%d,"end_time":%d,"text":"%s","channel_id":0,"speaker_id":null,"sentence_end":true,`+
				`"words":[{"begin_time":%d,"end_time":%d,"text":"%s","punctuation":"","fixed":true}]}},"usage":%s}}`,
				taskID, i+1, s.begin, s.end, s.text, s.begin, s.end, s.text, s.usage),
		}

		for _, frame := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				return
			}
		}
	}

	_ = conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"header":{"task_id":"%s","event":"task-finished","attributes":{}},"payload":{"output":{}}}`, taskID)))
}

func (f *fakeInference) snapshot() fakeInference {
	f.mu.Lock()
	defer f.mu.Unlock()

	return fakeInference{
		runTasks:       append([]string(nil), f.runTasks...),
		authorizations: append([]string(nil), f.authorizations...),
		relayTokens:    append([]string(nil), f.relayTokens...),
		frames:         append([]int(nil), f.frames...),
		audio:          append([]byte(nil), f.audio...),
		audioBeforeRun: f.audioBeforeRun,
		frameTimes:     append([]time.Time(nil), f.frameTimes...),
		sessionsEnded:  f.sessionsEnded,
	}
}

func (f *fakeInference) set(fn func(*fakeInference)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	fn(f)
}

// toneWAV is a WAV of a 440 Hz tone with the given layout.
func toneWAV(rate, channels int, seconds float64) []byte {
	frames := int(float64(rate) * seconds)
	data := make([]byte, 44+frames*channels*2)

	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(36+frames*channels*2))
	copy(data[8:16], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(data[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(data[28:32], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(data[32:34], uint16(channels*2))
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(frames*channels*2))

	for i := range frames {
		v := int16(8000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
		for ch := range channels {
			binary.LittleEndian.PutUint16(data[44+(i*channels+ch)*2:], uint16(v))
		}
	}

	return data
}

// withListChunk inserts a LIST chunk between fmt and data, as many recorders write.
func withListChunk(wav []byte) []byte {
	list := append([]byte("LIST"), 0, 0, 0, 0)
	binary.LittleEndian.PutUint32(list[4:], 6)
	list = append(list, []byte("INFOab")...)

	out := append([]byte(nil), wav[:36]...)
	out = append(out, list...)
	out = append(out, wav[36:]...)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))

	return out
}

func joinTexts(sentences []fakeSentence) string {
	var b strings.Builder
	for _, s := range sentences {
		b.WriteString(s.text)
	}

	return b.String()
}
