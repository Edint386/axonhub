package probe

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/realtime/audio"
)

// TestBailianLive_ASREdges probes the Bailian ASR behaviours the adapter depends on and the
// first probe left open: several utterances on one socket, commit and clear in server_vad
// mode, clear in manual mode, session.finish while speech is still in progress, and whether
// the server echoes a client close frame.
func TestBailianLive_ASREdges(t *testing.T) {
	model := asrModel()
	f := &findings{Model: model}

	defer f.flush(t)

	speech24, omniTranscript := fetchOmniSpeech(t, f)

	rs, err := audio.NewResampler(speechRate, 16000)
	require.NoError(t, err)

	speech := rs.Process(speech24)
	f.add("0 speech", omniTranscript+" / 16k bytes="+itoaInt(len(speech)))

	const vad16 = `{"event_id":"e_vad","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":{"type":"server_vad","silence_duration_ms":600}}}`

	const manual16 = `{"event_id":"e_manual","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":null}}`

	silence := audio.Silence(16000, 1500*time.Millisecond)

	// --- A. Two utterances on one server_vad socket.
	a := asrEdgeSession(t, model, vad16)
	a.stream(speech, true)
	a.stream(silence, true)
	a.stream(speech, true)
	a.stream(silence, true)
	f.add("A two utterances", a.drain(8*time.Second))

	// --- B. input_audio_buffer.commit in server_vad mode, mid-utterance.
	b := asrEdgeSession(t, model, vad16)
	b.stream(speech[:len(speech)/2], true)
	b.p.send(`{"event_id":"e_commit","type":"input_audio_buffer.commit"}`)
	f.add("B commit in server_vad (mid speech)", b.drain(6*time.Second))
	b.stream(speech[len(speech)/2:], true)
	b.stream(silence, true)
	f.add("B after commit: rest of speech + silence", b.drain(6*time.Second))

	// --- C. input_audio_buffer.clear in server_vad mode, mid-utterance, then a fresh utterance.
	c := asrEdgeSession(t, model, vad16)
	c.stream(speech[:len(speech)/2], true)
	c.p.send(`{"event_id":"e_clear","type":"input_audio_buffer.clear"}`)
	f.add("C clear in server_vad (mid speech)", c.drain(4*time.Second))
	c.stream(speech, true)
	c.stream(silence, true)
	f.add("C after clear: full speech + silence", c.drain(6*time.Second))

	// --- D. input_audio_buffer.clear in manual mode, then append + commit.
	d := asrEdgeSession(t, model, manual16)
	d.stream(speech[:len(speech)/2], false)
	d.p.send(`{"event_id":"e_clear_m","type":"input_audio_buffer.clear"}`)
	f.add("D clear in manual mode", d.drain(4*time.Second))
	d.stream(speech, false)
	d.p.send(`{"event_id":"e_commit_m","type":"input_audio_buffer.commit"}`)
	f.add("D manual after clear: append + commit", d.drain(8*time.Second))

	// --- E. session.finish while speech is still in progress (no trailing silence).
	e := asrEdgeSession(t, model, vad16)
	e.stream(speech, true)
	e.p.send(`{"event_id":"e_finish","type":"session.finish"}`)
	f.add("E session.finish mid speech (server_vad)", e.drain(8*time.Second))

	// --- F. session.finish in manual mode with uncommitted audio.
	m := asrEdgeSession(t, model, manual16)
	m.stream(speech, false)
	m.p.send(`{"event_id":"e_finish_m","type":"session.finish"}`)
	f.add("F session.finish with uncommitted manual audio", m.drain(8*time.Second))

	// --- G. Client close frame: does the server echo it?
	g := asrEdgeSession(t, model, vad16)
	g.p.send(`{"type":"session.finish"}`)
	g.drain(2 * time.Second)

	require.NoError(t, g.p.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"), time.Now().Add(time.Second)))
	f.add("G after client close frame", g.drain(4*time.Second))

	// --- H. Mid-session session.update after audio started (language change).
	h := asrEdgeSession(t, model, vad16)
	h.stream(speech[:len(speech)/3], true)
	h.p.send(`{"event_id":"e_lang","type":"session.update","session":{"input_audio_transcription":{"language":"zh"}}}`)
	f.add("H session.update after audio started", h.drain(3*time.Second))

	// --- I. A bad session.update value: does it close the socket?
	i := asrEdgeSession(t, model, vad16)
	i.p.send(`{"event_id":"e_bad","type":"session.update","session":{"input_audio_transcription":{"language":"xx-invalid"}}}`)
	f.add("I unsupported language code", i.drain(4*time.Second))
}

// asrEdge is one probe connection to the ASR model, configured with one session.update.
type asrEdge struct {
	t *testing.T
	p *probeConn
}

func asrEdgeSession(t *testing.T, model, update string) *asrEdge {
	t.Helper()

	p := asrDial(t, model, nil)
	p.recv(10*time.Second, "session.created", "error")
	p.send(update)
	p.recv(5*time.Second, "session.updated", "error")

	return &asrEdge{t: t, p: p}
}

// stream sends pcm in 100 ms appends, paced like a microphone when paced is set.
func (e *asrEdge) stream(pcm []byte, paced bool) {
	for _, chunk := range audio.SplitPCM(pcm, 16000, 100*time.Millisecond) {
		e.p.send(`{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(chunk) + `"}`)

		if paced {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// drain collects every frame until d of silence, compacting transcription hypotheses.
func (e *asrEdge) drain(d time.Duration) string {
	var lines []string

	for {
		frame, n := e.p.recvUntil(d, func(frame string) {
			lines = append(lines, shortenAudio(frame))
		}, "__never__")

		if frame == "" && n == 0 {
			break
		}
	}

	select {
	case <-e.p.done:
		lines = append(lines, "SOCKET CLOSED: "+lastLine(e.p))
	default:
	}

	return strings.Join(lines, "\n")
}

// TestBailianLive_ASRUpdates checks whether a second session.update is accepted before any
// audio was sent.
func TestBailianLive_ASRUpdates(t *testing.T) {
	model := asrModel()
	f := &findings{Model: model}

	defer f.flush(t)

	steps := []struct{ name, first, second string }{
		{"identical updates", `{"event_id":"u1","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000}}`, `{"event_id":"u2","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000}}`},
		{"vad then language", `{"event_id":"u1","type":"session.update","session":{"turn_detection":{"type":"server_vad","silence_duration_ms":600}}}`, `{"event_id":"u2","type":"session.update","session":{"input_audio_transcription":{"language":"zh"}}}`},
	}

	for _, s := range steps {
		p := asrDial(t, model, nil)
		p.recv(10*time.Second, "session.created", "error")

		e := &asrEdge{t: t, p: p}

		p.send(s.first)
		first := e.drain(3 * time.Second)

		p.send(s.second)
		f.add(s.name, "first -> "+first+"\nsecond -> "+e.drain(4*time.Second))
	}

	// A session with no session.update at all: does default-config streaming work?
	speech24, _ := fetchOmniSpeech(t, f)

	rs, err := audio.NewResampler(speechRate, 16000)
	require.NoError(t, err)

	p := asrDial(t, model, nil)
	p.recv(10*time.Second, "session.created", "error")

	e := &asrEdge{t: t, p: p}
	e.stream(rs.Process(speech24), true)
	e.stream(audio.Silence(16000, 1500*time.Millisecond), true)
	f.add("no session.update at all", e.drain(5*time.Second))
}

// TestBailianLive_ASRUpdateThenImmediateAudio checks whether audio may follow the
// session.update without waiting for session.updated.
func TestBailianLive_ASRUpdateThenImmediateAudio(t *testing.T) {
	model := asrModel()
	f := &findings{Model: model}

	defer f.flush(t)

	variants := map[string]string{
		"vad update":    `{"event_id":"axh_asr_init","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":{"type":"server_vad","silence_duration_ms":600},"input_audio_transcription":{"language":"zh"}}}`,
		"minimal":       `{"event_id":"axh_asr_init","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000}}`,
		"manual update": `{"event_id":"axh_asr_init","type":"session.update","session":{"input_audio_format":"pcm","sample_rate":16000,"turn_detection":null}}`,
	}

	for name, update := range variants {
		p := asrDial(t, model, nil)
		p.recv(10*time.Second, "session.created", "error")

		e := &asrEdge{t: t, p: p}
		p.send(update)
		e.stream(make([]byte, 16000*2/2), false) // 500 ms of silence right behind the update

		f.add(name+" (no wait)", e.drain(4*time.Second))
	}

	// The same with the update acknowledged first.
	p := asrDial(t, model, nil)
	p.recv(10*time.Second, "session.created", "error")

	e := &asrEdge{t: t, p: p}
	p.send(variants["vad update"])
	ack := p.recv(5*time.Second, "session.updated", "error")
	e.stream(make([]byte, 16000*2/2), false)
	f.add("vad update (waited)", "ack="+truncate(ack)+"\n"+e.drain(4*time.Second))
}
