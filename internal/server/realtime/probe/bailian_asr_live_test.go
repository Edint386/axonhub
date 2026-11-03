package probe

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/server/realtime"
	"github.com/looplj/axonhub/internal/server/realtime/audio"
	"github.com/looplj/axonhub/llm/httpclient"
)

// TestBailianLive_ASR probes the Qwen-ASR realtime endpoint (qwen3-asr-flash-realtime) and
// records exactly what it sends: the session.created defaults, which session.update shapes it
// accepts, every event of a server_vad turn and a manual commit turn, session.finish, ping/pong
// and the handshake result for an unknown model name.
//
//	TEST_BAILIAN_API_KEY=sk-...  [TEST_BAILIAN_ASR_MODEL=qwen3-asr-flash-realtime]
//	go test ./internal/server/realtime/probe/ -run TestBailianLive_ASR -count=1 -v -timeout 600s
//
// The probe audio is real Chinese speech spoken by qwen3.8-omni-flash-realtime (24 kHz), which
// is fetched once and resampled to 16 kHz in memory; the model's own transcript of what it
// said is recorded so the ASR transcript can be compared against it.
func TestBailianLive_ASR(t *testing.T) {
	if os.Getenv("TEST_BAILIAN_API_KEY") == "" {
		t.Skip("TEST_BAILIAN_API_KEY not set")
	}

	model := asrModel()
	f := &findings{Model: model}

	defer f.flush(t)

	// --- 1. session.created defaults.
	p0 := asrDial(t, model, nil)
	defer func() { t.Logf("transcript ASR-1 (defaults):\n%s", p0.transcript()) }()

	created := p0.recv(10*time.Second, "session.created", "error")
	f.add("1 session.created (full)", created)
	f.add("1 session keys", "model="+raw(created, "session.model")+" modalities="+raw(created, "session.modalities")+
		" input_audio_format="+raw(created, "session.input_audio_format")+" sample_rate="+raw(created, "session.sample_rate")+
		" audio="+raw(created, "session.audio")+" turn_detection="+raw(created, "session.turn_detection")+
		" input_audio_transcription="+raw(created, "session.input_audio_transcription")+" voice="+raw(created, "session.voice")+
		" output_audio_format="+raw(created, "session.output_audio_format")+" instructions="+raw(created, "session.instructions"))

	// Anything else the server volunteers in the first seconds.
	var unsolicited []string

	_, extra := p0.recvUntil(2*time.Second, func(frame string) { unsolicited = append(unsolicited, shortenAudio(frame)) }, "__never__")
	f.add("1 unsolicited frames after session.created (2 s)", itoaInt(extra)+" "+strings.Join(unsolicited, " || "))

	_ = p0.conn.Close()

	// --- 2. session.update variants, one fresh connection each, with and without OpenAI-Beta.
	results := runASRVariants(t, model, asrVariants())

	for _, r := range results {
		f.add("2 "+r.name, r.result)
	}

	accepted16 := pickAccepted(results, "c", "a", "h", "e")
	accepted24 := variantAccepted(results, "g", "24000")

	f.add("2 chosen 16k update for streaming", accepted16.name+" -> "+accepted16.update)
	f.add("2 24000 accepted by variant g?", boolStr(accepted24))

	// --- 3. Real speech from the omni model (once), resampled to 16 kHz.
	speech24, omniTranscript := fetchOmniSpeech(t, f)

	rs, err := audio.NewResampler(speechRate, 16000)
	require.NoError(t, err)

	speech16 := rs.Process(speech24)

	f.add("3 omni model transcript of the speech", omniTranscript)
	f.add("3 speech bytes", "24k="+itoaInt(len(speech24))+" ("+itoaInt(int(audio.ByteLenToSeconds(len(speech24), speechRate)*1000))+" ms) 16k="+
		itoaInt(len(speech16))+" ("+itoaInt(int(audio.ByteLenToSeconds(len(speech16), 16000)*1000))+" ms)")

	if len(speech16) < 16000*2 {
		f.add("3 note", "less than 1 s of speech; VAD and transcription results below are inconclusive")
	}

	// --- 4. server_vad streaming at 16 kHz (and 24 kHz when accepted).
	vad16 := streamASR(t, model, "server_vad@16k", accepted16.update, 16000, speech16, true)
	f.add("4 server_vad@16k events", vad16.render())
	f.add("4 server_vad@16k event types", vad16.types())
	f.add("4 server_vad@16k summary", vad16.summary())
	f.add("4 server_vad@16k ping/pong", vad16.pong)
	f.add("4 server_vad@16k session.finish", vad16.finish)

	if accepted24 {
		update24 := variantByName(results, "g").update
		if accepted16.name == "c" {
			update24 = `{"input_audio_format":"pcm","sample_rate":24000,"turn_detection":{"type":"server_vad","threshold":0.3,"silence_duration_ms":600,"prefix_padding_ms":300}}`
		}

		vad24 := streamASR(t, model, "server_vad@24k", update24, speechRate, speech24, true)
		f.add("4 server_vad@24k events", vad24.render())
		f.add("4 server_vad@24k summary", vad24.summary())
	} else {
		f.add("4 server_vad@24k", "skipped: 24000 not accepted")
	}

	// --- 5. Manual mode: turn_detection null, append everything, commit.
	manualUpdate := accepted16.update
	if manualUpdate == "" {
		manualUpdate = `{"turn_detection":null}`
	} else {
		manualUpdate, _ = sjson.SetRaw(manualUpdate, "turn_detection", "null")
	}

	manual := streamASR(t, model, "manual@16k", manualUpdate, 16000, speech16, false)
	f.add("5 manual@16k update", manualUpdate)
	f.add("5 manual@16k events", manual.render())
	f.add("5 manual@16k event types", manual.types())
	f.add("5 manual@16k summary", manual.summary())
	f.add("5 manual@16k ping/pong", manual.pong)
	f.add("5 manual@16k session.finish", manual.finish)

	// --- 6. session.finish on a raw connection so an oversized close reason is captured.
	f.add("6 session.finish raw frames", rawFinish(t, model))

	// --- 7. Unknown model name on the same URL.
	f.add("7 unknown model qwen3-asr-flash", rawHandshake(t, "qwen3-asr-flash"))
}

func asrModel() string {
	if m := os.Getenv("TEST_BAILIAN_ASR_MODEL"); m != "" {
		return m
	}

	return "qwen3-asr-flash-realtime"
}

func boolStr(b bool) string {
	if b {
		return "true"
	}

	return "false"
}

// asrConnect dials like dialBailian but takes extra headers and leaves the reader to the caller.
func asrConnect(t *testing.T, model string, extra http.Header) (*websocket.Conn, http.Header, error) {
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

	for k, vs := range extra {
		for _, v := range vs {
			headers.Add(k, v)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	return realtime.DialUpstream(ctx, nil, wsURL, headers, 15*time.Second)
}

// asrDial is dialBailian with an extra-header option (background reader, probeConn helpers).
func asrDial(t *testing.T, model string, extra http.Header) *probeConn {
	t.Helper()

	conn, _, err := asrConnect(t, model, extra)
	require.NoError(t, err, "dial model %s", model)

	p := &probeConn{t: t, conn: conn, frames: make(chan string, 1<<14), done: make(chan struct{})}

	go p.reader()

	t.Cleanup(func() {
		_ = conn.Close()
		<-p.done
	})

	return p
}

type asrVariant struct {
	name   string
	update string // The session object.
}

func asrVariants() []asrVariant {
	return []asrVariant{
		{"a", `{"input_audio_format":"pcm","sample_rate":16000}`},
		{"b", `{"input_audio_format":"pcm16"}`},
		{"c", `{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":{"type":"server_vad","threshold":0.3,"silence_duration_ms":600,"prefix_padding_ms":300}}`},
		{"d", `{"turn_detection":null}`},
		{"e", `{"audio":{"input":{"format":{"type":"pcm","sample_rate":16000}}}}`},
		{"f", `{"input_audio_transcription":{"language":"zh"}}`},
		{"g", `{"input_audio_format":"pcm","sample_rate":24000}`},
		{"h", `{"modalities":["text"],"input_audio_format":"pcm","sample_rate":16000,"input_audio_transcription":{"language":"zh"},"turn_detection":{"type":"server_vad","threshold":0.0,"prefix_padding_ms":300,"silence_duration_ms":800}}`},
		{"i", `{"language":"zh"}`},
		{"j", `{"input_audio_transcription":{"language":"zh","vocabulary":"阳光明媚"}}`},
		{"k", `{"asr_options":{"language":"zh","enable_itn":true}}`},
		{"l", `{"input_audio_format":"pcm","sample_rate":8000}`},
		{"m", `{"turn_detection":{"type":"semantic_vad"}}`},
		{"n", `{"modalities":["text","audio"]}`},
	}
}

type variantResult struct {
	name   string // "<letter>" or "<letter>+beta".
	letter string
	beta   bool
	update string
	result string
	frames []string
}

// runASRVariants sends one session.update per fresh connection (with and without the
// OpenAI-Beta header) and records the raw frames that come back within 5 s.
func runASRVariants(t *testing.T, model string, variants []asrVariant) []variantResult {
	t.Helper()

	jobs := make([]variantResult, 0, 2*len(variants))

	for _, beta := range []bool{false, true} {
		for _, v := range variants {
			name := v.name
			if beta {
				name += "+beta"
			}

			jobs = append(jobs, variantResult{name: name, letter: v.name, beta: beta, update: v.update})
		}
	}

	var wg sync.WaitGroup

	sem := make(chan struct{}, 3)

	for i := range jobs {
		wg.Add(1)

		go func(r *variantResult) {
			defer wg.Done()

			sem <- struct{}{}
			defer func() { <-sem }()

			var extra http.Header
			if r.beta {
				extra = http.Header{"OpenAI-Beta": []string{"realtime=v1"}}
			}

			conn, _, err := asrConnect(t, model, extra)
			if err != nil {
				r.result = "dial error: " + dialErrorText(err)
				return
			}

			defer func() { _ = conn.Close() }()

			_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

			_, first, err := conn.ReadMessage()
			if err != nil {
				r.result = "no session.created: " + err.Error()
				return
			}

			if gjson.GetBytes(first, "type").String() != "session.created" {
				r.result = "first frame not session.created: " + truncate(string(first))
				return
			}

			frame := `{"event_id":"asr_` + r.name + `","type":"session.update","session":` + r.update + `}`
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				r.result = "write error: " + err.Error()
				return
			}

			r.frames = readRawFrames(t, conn.UnderlyingConn(), 5*time.Second)
			r.result = classifyVariant(r.frames)
		}(&jobs[i])
	}

	wg.Wait()

	for _, r := range jobs {
		t.Logf("variant %s -> %s\n%s", r.name, r.update, strings.Join(r.frames, "\n"))
	}

	return jobs
}

// classifyVariant summarises the raw frames of one variant: the reply type, the echoed session
// (full), or "nothing within 5 s".
func classifyVariant(frames []string) string {
	var parts []string

	for _, line := range frames {
		switch {
		case strings.HasPrefix(line, "<- CLOSE"):
			parts = append(parts, line)
		case strings.HasPrefix(line, "<- read end:"):
			if strings.Contains(line, "i/o timeout") {
				if len(parts) == 0 {
					parts = append(parts, "nothing within 5 s")
				}
			} else {
				parts = append(parts, line)
			}
		case strings.HasPrefix(line, "<- "):
			body := strings.TrimPrefix(line, "<- ")
			typ := gjson.Get(body, "type").String()

			switch typ {
			case "session.updated":
				parts = append(parts, "session.updated session="+raw(body, "session"))
			case "error":
				parts = append(parts, "error="+raw(body, "error"))
			default:
				parts = append(parts, line)
			}
		default:
			parts = append(parts, line)
		}
	}

	return strings.Join(parts, " || ")
}

func variantByName(results []variantResult, name string) variantResult {
	for _, r := range results {
		if r.name == name {
			return r
		}
	}

	return variantResult{}
}

// variantAccepted reports whether the named (no-header) variant got a session.updated whose
// session echoes want.
func variantAccepted(results []variantResult, name, want string) bool {
	r := variantByName(results, name)

	return strings.HasPrefix(r.result, "session.updated") && strings.Contains(r.result, want)
}

// pickAccepted returns the first of the named no-header variants that got session.updated.
func pickAccepted(results []variantResult, names ...string) asrVariant {
	for _, n := range names {
		r := variantByName(results, n)
		if strings.HasPrefix(r.result, "session.updated") {
			return asrVariant{name: n, update: r.update}
		}
	}

	return asrVariant{name: "none (defaults)", update: ""}
}

func dialErrorText(err error) string {
	if he, ok := errors.AsType[*httpclient.Error](err); ok {
		return he.Error() + " body=" + truncate(string(he.Body)) + " headers=" + headerText(he.Headers)
	}

	return err.Error()
}

func headerText(h http.Header) string {
	var parts []string

	for k, vs := range h {
		if strings.EqualFold(k, "Authorization") {
			continue
		}

		parts = append(parts, k+"="+strings.Join(vs, ","))
	}

	return strings.Join(parts, "; ")
}

// fetchOmniSpeech asks the omni model to read a Chinese sentence aloud and returns its 24 kHz
// PCM plus the model's own transcript of what it said.
func fetchOmniSpeech(t *testing.T, f *findings) ([]byte, string) {
	t.Helper()

	omni := probeModel()

	p := dialBailian(t, omni)
	defer func() { t.Logf("transcript omni speech:\n%s", p.transcript()) }()

	created := p.recv(10*time.Second, "session.created", "error")
	require.Contains(t, created, "session.created", "omni: no session.created")

	p.send(`{"event_id":"omni_init","type":"session.update","session":{"modalities":["text","audio"],"voice":"Tina","instructions":"You are a terse assistant. Reply in one short sentence.","turn_detection":null,"audio":{"input":{"format":{"type":"pcm","sample_rate":24000}},"output":{"format":{"type":"pcm","sample_rate":24000}}}}}`)

	ack := p.recv(10*time.Second, "session.updated", "error")
	f.add("3 omni init update", truncate(ack))

	p.send(`{"event_id":"omni_text","type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"请用普通话清晰地朗读这句话：今天天气不错，阳光明媚，我们一起去公园散步吧。"}]}}`)
	p.recv(8*time.Second, "conversation.item.created", "error")

	p.send(`{"event_id":"omni_resp","type":"response.create","response":{"modalities":["text","audio"],"instructions":"只朗读引号里的句子，不要添加别的内容。"}}`)

	var (
		speech     []byte
		transcript strings.Builder
		seen       = map[string]int{}
	)

	done, _ := p.recvUntil(60*time.Second, func(frame string) {
		typ := gjson.Get(frame, "type").String()
		seen[typ]++

		switch typ {
		case "response.audio.delta":
			if chunk, err := base64.StdEncoding.DecodeString(gjson.Get(frame, "delta").String()); err == nil {
				speech = append(speech, chunk...)
			}
		case "response.audio_transcript.delta":
			transcript.WriteString(gjson.Get(frame, "delta").String())
		}
	}, "response.done", "error")

	types := make([]string, 0, len(seen))
	for typ, n := range seen {
		types = append(types, typ+"×"+itoaInt(n))
	}

	f.add("3 omni response events", strings.Join(types, ", "))
	f.add("3 omni response.done status/usage", "status="+raw(done, "response.status")+" usage="+raw(done, "response.usage"))

	tr := transcript.String()
	if tr == "" {
		tr = gjson.Get(done, "response.output.0.content.0.transcript").String()
	}

	if outTr := gjson.Get(done, "response.output.#.content.#.transcript").Raw; outTr != "" {
		tr += " | response.done transcript=" + outTr
	}

	_ = p.conn.Close()

	return speech, tr
}

// asrRun is everything recorded during one streaming session against the ASR model.
type asrRun struct {
	label  string
	start  time.Time
	mu     sync.Mutex
	lines  []string
	events map[string]int
	last   time.Time

	speechStarted string
	speechStopped string
	committed     string
	itemCreated   string
	deltas        []string
	completed     string
	failed        string
	errs          []string
	closed        string

	pong   string
	finish string
}

func (r *asrRun) at() string {
	ms := time.Since(r.start).Milliseconds()

	return "[" + itoaInt(int(ms)) + "ms]"
}

func (r *asrRun) mark(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.lines = append(r.lines, r.at()+" "+line)
}

func (r *asrRun) record(frame string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.last = time.Now()

	typ := gjson.Get(frame, "type").String()
	r.events[typ]++
	r.lines = append(r.lines, r.at()+" <- "+shortenAudio(frame))

	switch typ {
	case "input_audio_buffer.speech_started":
		if r.speechStarted == "" {
			r.speechStarted = frame
		}
	case "input_audio_buffer.speech_stopped":
		if r.speechStopped == "" {
			r.speechStopped = frame
		}
	case "input_audio_buffer.committed":
		if r.committed == "" {
			r.committed = frame
		}
	case "conversation.item.created":
		if r.itemCreated == "" {
			r.itemCreated = frame
		}
	case "conversation.item.input_audio_transcription.delta":
		r.deltas = append(r.deltas, frame)
	case "conversation.item.input_audio_transcription.completed":
		r.completed = frame
	case "conversation.item.input_audio_transcription.failed":
		r.failed = frame
	case "error":
		r.errs = append(r.errs, frame)
	}
}

func (r *asrRun) render() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return "\n" + strings.Join(r.lines, "\n")
}

func (r *asrRun) types() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	parts := make([]string, 0, len(r.events))
	for typ, n := range r.events {
		parts = append(parts, typ+"×"+itoaInt(n))
	}

	return strings.Join(parts, ", ")
}

func (r *asrRun) summary() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	span := "<n/a>"
	if r.speechStarted != "" && r.speechStopped != "" {
		span = itoaInt(int(gjson.Get(r.speechStopped, "audio_end_ms").Int()-gjson.Get(r.speechStarted, "audio_start_ms").Int())) + " ms"
	}

	deltaFields := map[string]int{}

	var deltaText strings.Builder

	for _, d := range r.deltas {
		gjson.Parse(d).ForEach(func(k, _ gjson.Result) bool {
			deltaFields[k.String()]++
			return true
		})
		deltaText.WriteString(gjson.Get(d, "delta").String())
	}

	fields := make([]string, 0, len(deltaFields))
	for k := range deltaFields {
		fields = append(fields, k)
	}

	completedFields := "<absent>"
	if r.completed != "" {
		var ks []string

		gjson.Parse(r.completed).ForEach(func(k, _ gjson.Result) bool {
			ks = append(ks, k.String())
			return true
		})
		completedFields = strings.Join(ks, ",")
	}

	return "speech_started=" + raw(r.speechStarted, "audio_start_ms") + " speech_stopped=" + raw(r.speechStopped, "audio_end_ms") + " span=" + span +
		" committed=" + truncate(r.committed) + " item.created=" + truncate(r.itemCreated) +
		" delta_count=" + itoaInt(len(r.deltas)) + " delta_fields=" + strings.Join(fields, ",") + " delta_concat=" + deltaText.String() +
		" completed_fields=" + completedFields + " completed_transcript=" + raw(r.completed, "transcript") + " completed_usage=" + raw(r.completed, "usage") +
		" completed=" + truncate(r.completed) + " failed=" + truncate(r.failed) + " errors=" + strings.Join(r.errs, " | ") + " closed=" + r.closed
}

// shortenAudio replaces long base64 audio values with their length.
func shortenAudio(frame string) string {
	for _, key := range []string{"audio", "delta", "item.content.0.audio"} {
		v := gjson.Get(frame, key)
		if v.Type == gjson.String && len(v.String()) > 120 && looksBase64(v.String()) {
			frame, _ = sjson.Set(frame, key, "<"+itoaInt(len(v.String()))+" base64 chars>")
		}
	}

	return frame
}

func looksBase64(s string) bool {
	for _, c := range s[:min(len(s), 64)] {
		if (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '+' && c != '/' && c != '=' {
			return false
		}
	}

	return true
}

// streamASR opens a fresh ASR session, applies update (the session object; "" skips the
// update), streams speech at rate in paced 100 ms chunks followed by 1.5 s of silence (vad) or
// appends everything and commits (manual), then records every event until 10 s after the last
// one. It then pings and sends session.finish on the same socket.
func streamASR(t *testing.T, model, label, update string, rate int, speech []byte, vad bool) *asrRun {
	t.Helper()

	p := asrDial(t, model, nil)
	defer func() { t.Logf("transcript %s:\n%s", label, p.transcript()) }()

	run := &asrRun{label: label, start: time.Now(), events: map[string]int{}, last: time.Now()}

	created := p.recv(10*time.Second, "session.created", "error")
	run.mark("session.created: " + created)

	if update != "" {
		p.send(`{"event_id":"asr_stream_init","type":"session.update","session":` + update + `}`)

		ack := p.recv(8*time.Second, "session.updated", "error")
		run.mark("session.update " + update + " -> " + ack)
	}

	// Background collector so event timestamps are real.
	stop := make(chan struct{})
	collectorDone := make(chan struct{})

	go func() {
		defer close(collectorDone)

		for {
			select {
			case frame := <-p.frames:
				run.record(frame)
			case <-p.done:
				for {
					select {
					case frame := <-p.frames:
						run.record(frame)
					default:
						run.mu.Lock()
						run.closed = lastLine(p)
						run.lines = append(run.lines, run.at()+" socket closed: "+run.closed)
						run.mu.Unlock()

						return
					}
				}
			case <-stop:
				return
			}
		}
	}()

	chunks := audio.SplitPCM(speech, rate, 100*time.Millisecond)

	run.mark("streaming " + itoaInt(len(chunks)) + " speech chunks @" + itoaInt(rate) + " (" + itoaInt(int(audio.ByteLenToSeconds(len(speech), rate)*1000)) + " ms)")

	if vad {
		for _, chunk := range chunks {
			p.send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
			time.Sleep(100 * time.Millisecond)
		}

		run.mark("speech sent; streaming 1500 ms silence")

		for _, chunk := range audio.SplitPCM(audio.Silence(rate, 1500*time.Millisecond), rate, 100*time.Millisecond) {
			p.send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
			time.Sleep(100 * time.Millisecond)
		}

		run.mark("silence sent")
	} else {
		for _, chunk := range chunks {
			p.send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)
		}

		run.mark("all chunks appended; sending input_audio_buffer.commit")
		p.send(`{"event_id":"asr_commit","type":"input_audio_buffer.commit"}`)
	}

	// Wait until 10 s after the last event (or 40 s overall, or the socket closes).
	run.mu.Lock()
	run.last = time.Now()
	run.mu.Unlock()

	deadline := time.Now().Add(40 * time.Second)

wait:
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			break wait
		case <-time.After(200 * time.Millisecond):
		}

		run.mu.Lock()
		quiet := time.Since(run.last)
		run.mu.Unlock()

		if quiet >= 10*time.Second {
			break
		}
	}

	run.mark("quiet for 10 s (or socket closed / 40 s cap); probing ping/pong then session.finish")

	// Ping / pong.
	pong := make(chan struct{}, 1)
	p.conn.SetPongHandler(func(string) error {
		select {
		case pong <- struct{}{}:
		default:
		}

		return nil
	})

	select {
	case <-p.done:
		run.pong = "socket already closed"
	default:
		if err := p.conn.WriteControl(websocket.PingMessage, []byte("probe"), time.Now().Add(2*time.Second)); err != nil {
			run.pong = "ping write error: " + err.Error()
		} else {
			select {
			case <-pong:
				run.pong = "pong received"
			case <-time.After(3 * time.Second):
				run.pong = "no pong within 3 s"
			}
		}
	}

	run.mark("ping/pong: " + run.pong)

	// session.finish on this socket (gorilla reports a close with a short reason verbatim).
	select {
	case <-p.done:
		run.finish = "socket already closed before session.finish: " + lastLine(p)
	default:
		if err := p.conn.WriteMessage(websocket.TextMessage, []byte(`{"event_id":"asr_finish","type":"session.finish"}`)); err != nil {
			run.finish = "write error: " + err.Error()
		} else {
			run.mark("-> session.finish")

			var after []string

			finDeadline := time.Now().Add(6 * time.Second)

		fin:
			for time.Now().Before(finDeadline) {
				select {
				case frame := <-p.frames:
					run.record(frame)
					after = append(after, shortenAudio(frame))
				case <-p.done:
					for {
						select {
						case frame := <-p.frames:
							run.record(frame)
							after = append(after, shortenAudio(frame))
						default:
							after = append(after, "socket closed: "+lastLine(p))

							break fin
						}
					}
				case <-time.After(200 * time.Millisecond):
				}
			}

			if len(after) == 0 {
				after = append(after, "nothing within 6 s, socket still open")
			}

			run.finish = strings.Join(after, " || ")
		}
	}

	run.mark("after session.finish: " + run.finish)

	close(stop)
	<-collectorDone

	_ = p.conn.Close()

	return run
}

// rawFinish opens a fresh ASR session, sends session.finish and reads the raw frames so a close
// frame with an oversized reason is captured verbatim.
func rawFinish(t *testing.T, model string) string {
	t.Helper()

	conn, _, err := asrConnect(t, model, nil)
	if err != nil {
		return "dial error: " + dialErrorText(err)
	}

	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	_, first, err := conn.ReadMessage()
	if err != nil {
		return "no session.created: " + err.Error()
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"event_id":"asr_raw_finish","type":"session.finish"}`)); err != nil {
		return "write error: " + err.Error()
	}

	frames := readRawFrames(t, conn.UnderlyingConn(), 6*time.Second)

	return "first=" + gjson.GetBytes(first, "type").String() + " || " + strings.Join(frames, " || ")
}

// rawHandshake dials an arbitrary model name and records the handshake result and the first raw
// frames (or the rejection body).
func rawHandshake(t *testing.T, model string) string {
	t.Helper()

	conn, respHeader, err := asrConnect(t, model, nil)
	if err != nil {
		return "handshake failed: " + dialErrorText(err)
	}

	defer func() { _ = conn.Close() }()

	frames := readRawFrames(t, conn.UnderlyingConn(), 6*time.Second)

	return "handshake OK (101) headers=" + headerText(respHeader) + " || " + strings.Join(frames, " || ")
}
