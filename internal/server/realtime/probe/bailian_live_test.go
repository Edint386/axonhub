// Package probe holds live probes against real upstreams. They are skipped unless the
// corresponding environment variables are set and never run in CI.
//
// Bailian probe:
//
//	TEST_BAILIAN_API_KEY=sk-...  [TEST_BAILIAN_MODEL=qwen3.8-omni-flash-realtime]
//	[TEST_BAILIAN_WS_URL=wss://<WorkspaceId>.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime]
//	[AXONHUB_PROBE_OUTPUT=/tmp/findings.json]
//	go test ./internal/server/realtime/probe/ -run TestBailianLive -count=1 -v
//
// The probe records what the upstream actually accepts (24 kHz input, text and audio items,
// response overrides, VAD fields, partial updates, retrieve/delete, ping/pong) so the
// capability table in dialect/bailian can be adjusted. It uses the model's own speech output
// as the microphone input, because the server VAD ignores synthetic noise. Findings are printed
// with t.Log and, when AXONHUB_PROBE_OUTPUT is set, written there as JSON.
package probe

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/realtime"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/internal/server/realtime/dialect/bailian"
)

const (
	// logFrameLimit bounds a logged frame.
	logFrameLimit = 1200
	// speechRate is the PCM rate the model speaks at and the GA client rate.
	speechRate = 24000
)

// probeConn wraps one upstream socket with a background reader, so a recv timeout never
// poisons the connection (gorilla makes read errors permanent).
type probeConn struct {
	t    *testing.T
	conn *websocket.Conn

	mu  sync.Mutex
	log []string

	frames chan string
	done   chan struct{}
}

func probeModel() string {
	if model := os.Getenv("TEST_BAILIAN_MODEL"); model != "" {
		return model
	}

	return "qwen3.8-omni-flash-realtime"
}

func dialBailian(t *testing.T, model string) *probeConn {
	t.Helper()

	key := os.Getenv("TEST_BAILIAN_API_KEY")
	if key == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}

	wsURL := os.Getenv("TEST_BAILIAN_WS_URL")
	if wsURL == "" {
		wsURL = "wss://dashscope.aliyuncs.com/api-ws/v1/realtime"
	}

	if strings.Contains(wsURL, "?") {
		wsURL += "&model=" + model
	} else {
		wsURL += "?model=" + model
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+key)
	headers.Set("User-Agent", "AxonHub-Realtime-Probe/1.0")

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	conn, _, err := realtime.DialUpstream(ctx, nil, wsURL, headers, 15*time.Second)
	require.NoError(t, err, "dial %s", wsURL)

	p := &probeConn{t: t, conn: conn, frames: make(chan string, 1<<14), done: make(chan struct{})}

	go p.reader()

	t.Cleanup(func() {
		_ = conn.Close()
		<-p.done
	})

	return p
}

func (p *probeConn) reader() {
	defer close(p.done)

	for {
		_, data, err := p.conn.ReadMessage()
		if err != nil {
			p.note("<- read end: " + err.Error())
			return
		}

		frame := string(data)
		p.note("<- " + describeFrame(frame))

		select {
		case p.frames <- frame:
		default:
			p.note("!! frame dropped (probe buffer full)")
		}
	}
}

func (p *probeConn) note(line string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.log = append(p.log, line)
}

func (p *probeConn) transcript() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return strings.Join(p.log, "\n")
}

func (p *probeConn) send(frame string) {
	p.t.Helper()
	require.NoError(p.t, p.conn.WriteMessage(websocket.TextMessage, []byte(frame)))
	p.note("-> " + describeFrame(frame))
}

// recv waits for an event of one of the wanted types (or timeout); other events are consumed.
// It returns the matching frame or "".
func (p *probeConn) recv(timeout time.Duration, want ...string) string {
	frame, _ := p.recvUntil(timeout, nil, want...)

	return frame
}

// recvUntil is recv with a callback for every consumed frame; it also returns the number of
// frames consumed before the match.
func (p *probeConn) recvUntil(timeout time.Duration, each func(frame string), want ...string) (string, int) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	consumed := 0

	for {
		select {
		case frame := <-p.frames:
			consumed++

			if each != nil {
				each(frame)
			}

			typ := gjson.Get(frame, "type").String()
			for _, w := range want {
				if typ == w {
					return frame, consumed
				}
			}
		case <-p.done:
			// Drain what the reader left before giving up.
			select {
			case frame := <-p.frames:
				consumed++

				if each != nil {
					each(frame)
				}

				typ := gjson.Get(frame, "type").String()
				for _, w := range want {
					if typ == w {
						return frame, consumed
					}
				}
			default:
				return "", consumed
			}
		case <-timer.C:
			return "", consumed
		}
	}
}

// describeFrame shortens audio payloads so transcripts stay readable.
func describeFrame(frame string) string {
	typ := gjson.Get(frame, "type").String()

	switch typ {
	case "input_audio_buffer.append", "response.audio.delta", "response.output_audio.delta":
		n := len(gjson.Get(frame, "audio").String()) + len(gjson.Get(frame, "delta").String())
		return typ + " (" + itoaInt(n) + " base64 chars)"
	}

	return truncate(frame)
}

func truncate(s string) string {
	if len(s) > logFrameLimit {
		return s[:logFrameLimit] + "...(" + itoaInt(len(s)) + " bytes)"
	}

	return s
}

// findings is an ordered list so the report reads top to bottom.
type findings struct {
	Model string    `json:"model"`
	Items []finding `json:"items"`
}

type finding struct {
	Name   string `json:"name"`
	Result string `json:"result"`
}

func (f *findings) add(name, result string) {
	f.Items = append(f.Items, finding{Name: name, Result: result})
}

func (f *findings) flush(t *testing.T) {
	data, _ := json.MarshalIndent(f, "", "  ")
	t.Logf("FINDINGS:\n%s", data)

	if out := os.Getenv("AXONHUB_PROBE_OUTPUT"); out != "" {
		_ = os.WriteFile(out, data, 0o600)
	}
}

func raw(frame, path string) string {
	v := gjson.Get(frame, path)
	if !v.Exists() {
		return "<absent>"
	}

	return v.Raw
}

func TestBailianLive_Protocol(t *testing.T) {
	model := probeModel()
	f := &findings{Model: model}

	defer f.flush(t)

	// --- A. Defaults and the dialect-style init update.
	p := dialBailian(t, model)
	defer func() { t.Logf("transcript A:\n%s", p.transcript()) }()

	created := p.recv(10*time.Second, "session.created", "error")
	require.Contains(t, created, "session.created", "no session.created")

	f.add("A1 session.created", truncate(created))
	f.add("A1 defaults", "voice="+raw(created, "session.voice")+" input_audio_format="+raw(created, "session.input_audio_format")+
		" output_audio_format="+raw(created, "session.output_audio_format")+" transcription="+raw(created, "session.input_audio_transcription")+
		" audio="+raw(created, "session.audio")+" turn_detection="+raw(created, "session.turn_detection"))

	p.send(`{"event_id":"probe_init","type":"session.update","session":{"modalities":["text","audio"],"voice":"Tina","instructions":"You are a terse assistant. Reply in one short sentence.","input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"turn_detection":null,"audio":{"input":{"format":{"type":"pcm","sample_rate":24000}},"output":{"format":{"type":"pcm","sample_rate":24000}}}}}`)

	ack := p.recv(10*time.Second, "session.updated", "error")
	f.add("A2 init update (nested 24k in+out, Tina, asr model, turn_detection null)", truncate(ack))

	if gjson.Get(ack, "type").String() == "session.updated" {
		f.add("A2 echoed", "audio="+raw(ack, "session.audio")+" input_audio_format="+raw(ack, "session.input_audio_format")+
			" output_audio_format="+raw(ack, "session.output_audio_format")+" transcription="+raw(ack, "session.input_audio_transcription")+
			" turn_detection="+raw(ack, "session.turn_detection")+" voice="+raw(ack, "session.voice"))
	}

	// --- B. Text item, response.create overrides, and the model's speech as probe audio.
	p.send(`{"event_id":"probe_text","type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"请用普通话清晰地朗读这句话：今天天气不错，阳光明媚。"}]}}`)

	textItem := p.recv(8*time.Second, "conversation.item.created", "error")
	f.add("B1 input_text item", truncate(textItem))

	textItemID := gjson.Get(textItem, "item.id").String()

	p.send(`{"event_id":"probe_resp","type":"response.create","response":{"modalities":["text","audio"],"instructions":"只朗读引号里的句子，不要添加别的内容。"}}`)

	var (
		speech   []byte
		seen     = map[string]int{}
		respMade string
	)

	done, _ := p.recvUntil(60*time.Second, func(frame string) {
		typ := gjson.Get(frame, "type").String()
		seen[typ]++

		switch typ {
		case "response.created":
			respMade = frame
		case "response.audio.delta":
			if chunk, err := base64.StdEncoding.DecodeString(gjson.Get(frame, "delta").String()); err == nil {
				speech = append(speech, chunk...)
			}
		}
	}, "response.done", "error")

	types := make([]string, 0, len(seen))
	for typ, n := range seen {
		types = append(types, typ+"×"+itoaInt(n))
	}

	f.add("B2 response events", strings.Join(types, ", "))
	f.add("B2 response.created", truncate(respMade))
	f.add("B2 response.done", truncate(done))
	f.add("B2 usage", raw(done, "response.usage"))
	f.add("B2 speech bytes @24k", itoaInt(len(speech))+" ("+itoaInt(len(speech)/(speechRate*2))+" s)")

	if len(speech) < speechRate*2 {
		f.add("B2 note", "less than 1 s of speech; VAD probes below will be inconclusive")
	}

	speechMs := int64(len(speech)) * 1000 / (speechRate * 2)

	// --- C. Manual-mode audio input at 24 kHz: append + commit, then transcription + response.
	for _, chunk := range audio.SplitPCM(speech, speechRate, 100*time.Millisecond) {
		p.send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
	}

	p.send(`{"event_id":"probe_commit","type":"input_audio_buffer.commit"}`)

	committed := p.recv(8*time.Second, "input_audio_buffer.committed", "error")
	f.add("C1 commit", truncate(committed))

	var deltas []string

	transcript, _ := p.recvUntil(20*time.Second, func(frame string) {
		if gjson.Get(frame, "type").String() == "conversation.item.input_audio_transcription.delta" {
			deltas = append(deltas, "text="+raw(frame, "text")+" stash="+raw(frame, "stash")+" delta="+raw(frame, "delta"))
		}
	}, "conversation.item.input_audio_transcription.completed", "conversation.item.input_audio_transcription.failed", "error")

	f.add("C2 transcription deltas", strings.Join(deltas, " | "))
	f.add("C2 transcription completed (24k manual)", truncate(transcript))

	p.send(`{"event_id":"probe_resp2","type":"response.create","response":{"modalities":["text"],"instructions":"用一句话复述用户刚才说的话。"}}`)
	done2 := p.recv(45*time.Second, "response.done", "error")
	f.add("C3 text-only response after audio", "status="+raw(done2, "response.status")+" output="+raw(done2, "response.output")+" usage="+raw(done2, "response.usage"))

	// --- D. Other item kinds.
	oneSecond := speech
	if len(oneSecond) > speechRate*2 {
		oneSecond = oneSecond[:speechRate*2]
	}

	p.send(`{"event_id":"probe_audio_item","type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_audio","audio":"` + base64.StdEncoding.EncodeToString(oneSecond) + `"}]}}`)
	f.add("D1 input_audio item", truncate(p.recv(8*time.Second, "conversation.item.created", "error")))

	p.send(`{"event_id":"probe_assistant_item","type":"conversation.item.create","item":{"type":"message","role":"assistant","content":[{"type":"text","text":"好的，我记住了。"}]}}`)
	f.add("D2 assistant text item", truncate(p.recv(8*time.Second, "conversation.item.created", "error")))

	// --- E. Partial update semantics, retrieve, delete, ping/pong, finish.
	p.send(`{"event_id":"probe_partial","type":"session.update","session":{"instructions":"Still terse."}}`)
	partial := p.recv(8*time.Second, "session.updated", "error")
	f.add("E1 partial update keeps turn_detection null?", "turn_detection="+raw(partial, "session.turn_detection")+" audio="+raw(partial, "session.audio")+" voice="+raw(partial, "session.voice"))

	if textItemID != "" {
		p.send(`{"event_id":"probe_retrieve","type":"conversation.item.retrieve","item_id":"` + textItemID + `"}`)
		f.add("E2 retrieve", truncate(p.recv(8*time.Second, "conversation.item.retrieved", "error")))

		p.send(`{"event_id":"probe_delete","type":"conversation.item.delete","item_id":"` + textItemID + `"}`)
		f.add("E3 delete", truncate(p.recv(8*time.Second, "conversation.item.deleted", "error")))
	}

	pong := make(chan struct{}, 1)
	p.conn.SetPongHandler(func(string) error {
		select {
		case pong <- struct{}{}:
		default:
		}

		return nil
	})
	require.NoError(t, p.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(2*time.Second)))

	select {
	case <-pong:
		f.add("E4 pong", "pong received")
	case <-time.After(3 * time.Second):
		f.add("E4 pong", "no pong within 3s")
	}

	p.send(`{"type":"session.finish"}`)
	f.add("E5 after session.finish", truncate(p.recv(5*time.Second, "session.finished", "error")))

	select {
	case <-p.done:
		f.add("E5 socket after finish", "closed by server: "+lastLine(p))
	case <-time.After(3 * time.Second):
		f.add("E5 socket after finish", "still open 3s after session.finished")
	}

	// --- F. Server VAD at 24 kHz and 16 kHz with the same speech, plus semantic_vad.
	if speechMs > 500 {
		f.add("F0 true speech duration", itoaInt(int(speechMs))+" ms")

		span, tr, auto := vadRun(t, model, 24000, speech, p.t)
		f.add("F1 server_vad @24k: speech span", itoaInt(int(span))+" ms (≈true → 24k honoured; ≈1.5× → read as 16k)")
		f.add("F1 server_vad @24k: transcript", tr)
		f.add("F1 server_vad @24k: auto response", auto)

		rs, err := audio.NewResampler(24000, 16000)
		require.NoError(t, err)

		speech16 := rs.Process(speech)

		span16, tr16, auto16 := vadRun(t, model, 16000, speech16, p.t)
		f.add("F2 server_vad @16k (resampled): speech span", itoaInt(int(span16))+" ms")
		f.add("F2 server_vad @16k (resampled): transcript", tr16)
		f.add("F2 server_vad @16k (resampled): auto response", auto16)
	}

	ps := dialBailian(t, model)
	ps.recv(10*time.Second, "session.created")
	ps.send(`{"event_id":"probe_semantic","type":"session.update","session":{"turn_detection":{"type":"semantic_vad","eagerness":"auto","create_response":true,"interrupt_response":true}}}`)
	f.add("F3 semantic_vad", truncate(ps.recv(8*time.Second, "session.updated", "error")))
	ps.send(`{"event_id":"probe_idle","type":"session.update","session":{"turn_detection":{"type":"server_vad","idle_timeout_ms":5000,"silence_duration_ms":500}}}`)
	f.add("F4 server_vad idle_timeout_ms", truncate(ps.recv(8*time.Second, "session.updated", "error")))
	ps.send(`{"event_id":"probe_voice_bad","type":"session.update","session":{"voice":"alloy"}}`)
	f.add("F5 unknown voice alloy", truncate(ps.recv(8*time.Second, "session.updated", "error")))
	ps.send(`{"event_id":"probe_prompt","type":"session.update","session":{"prompt":{"id":"pmpt_x"}}}`)
	f.add("F6 unknown field prompt", truncate(ps.recv(8*time.Second, "session.updated", "error")))
	ps.send(`{"type":"session.finish"}`)
}

func lastLine(p *probeConn) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.log) == 0 {
		return ""
	}

	return p.log[len(p.log)-1]
}

// vadRun streams speech (PCM at rate) into a fresh server_vad session paced like a
// microphone, then returns the VAD span, the transcription frame and the auto response summary.
func vadRun(t *testing.T, model string, rate int, speech []byte, _ *testing.T) (span int64, transcript, auto string) {
	t.Helper()

	p := dialBailian(t, model)
	defer func() { t.Logf("transcript VAD@%d:\n%s", rate, p.transcript()) }()

	p.recv(10*time.Second, "session.created")

	format := `{"type":"pcm","sample_rate":` + itoaInt(rate) + `}`
	p.send(`{"event_id":"probe_vad","type":"session.update","session":{"modalities":["text","audio"],"input_audio_transcription":{"model":"qwen3-asr-flash-realtime"},"turn_detection":{"type":"server_vad","threshold":0.3,"silence_duration_ms":600,"prefix_padding_ms":300,"create_response":true,"interrupt_response":true},"audio":{"input":{"format":` + format + `}}}}`)

	vack := p.recv(8*time.Second, "session.updated", "error")
	if gjson.Get(vack, "type").String() != "session.updated" {
		return 0, "session.update rejected: " + truncate(vack), ""
	}

	chunks := audio.SplitPCM(speech, rate, 100*time.Millisecond)
	chunks = append(chunks, audio.SplitPCM(audio.Silence(rate, 1500*time.Millisecond), rate, 100*time.Millisecond)...)

	for _, chunk := range chunks {
		p.send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
		time.Sleep(100 * time.Millisecond)
	}

	var started, stopped, completed, respDone string

	_, _ = p.recvUntil(30*time.Second, func(frame string) {
		switch gjson.Get(frame, "type").String() {
		case "input_audio_buffer.speech_started":
			if started == "" {
				started = frame
			}
		case "input_audio_buffer.speech_stopped":
			if stopped == "" {
				stopped = frame
			}
		case "conversation.item.input_audio_transcription.completed":
			completed = frame
		case "response.done":
			respDone = frame
		}
	}, "response.done", "error")

	if started != "" && stopped != "" {
		span = gjson.Get(stopped, "audio_end_ms").Int() - gjson.Get(started, "audio_start_ms").Int()
	}

	transcript = "started=" + raw(started, "audio_start_ms") + " stopped=" + raw(stopped, "audio_end_ms") + " transcript=" + raw(completed, "transcript")
	auto = "status=" + raw(respDone, "response.status") + " output_types=" + raw(respDone, "response.output.#.content.#.type") + " usage=" + raw(respDone, "response.usage")

	p.send(`{"type":"session.finish"}`)

	return span, transcript, auto
}

// TestBailianLive_DialectRoundTrip pushes GA frames through the real bailian dialect against the
// live upstream and checks the translated session.created the client would receive.
func TestBailianLive_DialectRoundTrip(t *testing.T) {
	model := probeModel()

	p := dialBailian(t, model)
	defer func() { t.Logf("upstream log:\n%s", p.transcript()) }()

	d, err := bailian.New(bailian.Options{RequestedModel: "my-voice", ActualModel: model, IDs: &dialect.EventIDs{}})
	require.NoError(t, err)

	_, gated := d.Start()
	require.True(t, gated)

	var clientFrames []string

	opened := false
	deadline := time.Now().Add(15 * time.Second)

	for !opened && time.Now().Before(deadline) {
		frame, _ := p.recvUntil(time.Until(deadline), nil, "session.created", "session.updated", "error")
		require.NotEmpty(t, frame, "upstream went silent")

		out, err := d.UpstreamFrame([]byte(frame))
		require.NoError(t, err)

		for _, up := range out.ToUpstream {
			p.send(string(up))
		}

		for _, down := range out.ToClient {
			clientFrames = append(clientFrames, string(down))
		}

		require.Nil(t, out.Close, "dialect closed the session: %v", out.Close)

		opened = out.OpenGate
	}

	require.True(t, opened, "gate never opened")
	require.NotEmpty(t, clientFrames)

	created := clientFrames[0]
	require.Equal(t, "session.created", gjson.Get(created, "type").String())
	require.Equal(t, "my-voice", gjson.Get(created, "session.model").String())
	require.Equal(t, int64(24000), gjson.Get(created, "session.audio.input.format.rate").Int())

	t.Logf("GA session.created rendered by the dialect:\n%s", created)

	// A full GA turn through the dialect: text item, audio response, usage in GA shape.
	push := func(frame string) {
		out, err := d.ClientFrame([]byte(frame))
		require.NoError(t, err, frame)

		for _, up := range out.ToUpstream {
			p.send(string(up))
		}

		for _, down := range out.ToClient {
			clientFrames = append(clientFrames, string(down))
		}
	}

	push(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"请用一句话介绍你自己。"}]}}`)
	push(`{"type":"response.create","response":{"output_modalities":["audio"]}}`)

	seen := map[string]int{}

	var usage []dialect.UsageRecord

	gaDone := ""

	// Every upstream frame goes through the dialect, so read the channel directly.
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()

loop:
	for gaDone == "" {
		select {
		case frame := <-p.frames:
			out, err := d.UpstreamFrame([]byte(frame))
			require.NoError(t, err, frame)

			usage = append(usage, out.Usage...)

			for _, up := range out.ToUpstream {
				p.send(string(up))
			}

			for _, down := range out.ToClient {
				ga := string(down)
				typ := gjson.Get(ga, "type").String()
				seen[typ]++

				clientFrames = append(clientFrames, ga)

				if typ == "response.done" {
					gaDone = ga
				}

				require.NotEqual(t, "error", typ, "dialect forwarded an error: %s", ga)
			}
		case <-p.done:
			break loop
		case <-timer.C:
			break loop
		}
	}

	types := make([]string, 0, len(seen))
	for typ, n := range seen {
		types = append(types, typ+"×"+itoaInt(n))
	}

	t.Logf("GA events from the dialect: %s", strings.Join(types, ", "))
	require.NotEmpty(t, gaDone, "no GA response.done from the dialect")
	require.Positive(t, seen["response.output_audio.delta"], "no GA audio deltas")
	require.Equal(t, "completed", gjson.Get(gaDone, "response.status").String())
	require.True(t, gjson.Get(gaDone, "response.usage.input_token_details").Exists(), "usage not in GA shape: %s", gjson.Get(gaDone, "response.usage").Raw)
	require.Len(t, usage, 1)
	require.NotNil(t, usage[0].Usage)
	t.Logf("GA response.done: %s", truncate(gaDone))
	t.Logf("usage record: prompt=%d completion=%d", usage[0].Usage.PromptTokens, usage[0].Usage.CompletionTokens)

	for _, frame := range d.CloseUpstream() {
		p.send(string(frame))
	}
}
