package realtime

import (
	"encoding/base64"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/lifecycle"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/internal/server/realtime/dialect/bailian"
)

// billingDialect is goodbyeDialect for a dialect that bills the audio it forwards: every "audio"
// frame goes upstream and counts until the session hands it back (Output.Discarded).
type billingDialect struct {
	goodbyeDialect

	forwarded atomic.Int64
	discarded atomic.Int64
}

func (d *billingDialect) ClientFrame(frame []byte) (dialect.Output, error) {
	if string(frame) != "audio" {
		return d.goodbyeDialect.ClientFrame(frame)
	}

	d.forwarded.Add(1)

	return dialect.Output{
		ToUpstream: [][]byte{frame},
		Discarded:  func(sent int) { d.discarded.Add(int64(1 - sent)) },
	}, nil
}

func countFrames(frames []string, match func(string) bool) int {
	n := 0

	for _, f := range frames {
		if match(f) {
			n++
		}
	}

	return n
}

// Client frames the session no longer forwards once it is ending are handed back to the dialect,
// so audio that never reached the upstream is not billed; queued frames are not.
func TestSession_UpstreamFramesDroppedWhileEndingAreHandedBack(t *testing.T) {
	hooks := newRecordingHooks()
	d := &billingDialect{goodbyeDialect: goodbyeDialect{drain: 3 * time.Second}}
	h := newHarness(t, testConfig(), d, hooks)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte("audio")))
	h.upstream.waitFor(t, 1)

	// "bye" ends the session; teardown sends "finish" and drains while the client keeps sending.
	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte("bye")))

	for range 3 {
		require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte("audio")))
	}

	require.Eventually(t, func() bool { return d.discarded.Load() == 3 }, 3*time.Second, 10*time.Millisecond)
	h.upstream.send(t, "finished")
	h.wait(t)

	require.EqualValues(t, 4, d.forwarded.Load())
	require.EqualValues(t, 3, d.discarded.Load())
	require.Equal(t, 1, countFrames(h.upstream.got(), func(f string) bool { return f == "audio" }))
}

func pcm24Append(ms int) string {
	return `{"type":"input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(make([]byte, ms*48)) + `"}`
}

func newQwenAudioStreaming(t *testing.T) dialect.Dialect {
	t.Helper()

	d, err := bailian.NewParaformer(bailian.ParaformerOptions{RequestedModel: "gpt-4o-transcribe", ActualModel: "qwen-audio-3.1-asr-flash-streaming"})
	require.NoError(t, err)

	return d
}

func isAction(action string) func(string) bool {
	return func(f string) bool { return gjson.Get(f, "header.action").String() == action }
}

const (
	inferenceTaskStarted  = `{"header":{"event":"task-started","attributes":{}},"payload":{}}`
	inferenceTaskFinished = `{"header":{"event":"task-finished","attributes":{}},"payload":{"output":{}}}`
)

// A client that sends a file and hangs up within the run-task round trip: the queued audio is
// replayed into a session that already sent finish-task, so none of it reaches the upstream and
// none of it is billed.
func TestSession_QwenAudioStreamingBillsNoAudioQueuedBehindTheGoodbye(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), newQwenAudioStreaming(t), hooks)

	_, err := readFrame(t, h.client, 2*time.Second) // session.created
	require.NoError(t, err)

	for range 6 {
		require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(pcm24Append(1000))))
	}

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"input_audio_buffer.commit"}`)))
	require.NoError(t, h.client.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))

	// run-task, then teardown's finish-task, before task-started releases the gate.
	got := h.upstream.waitFor(t, 2)
	require.Len(t, got, 2)
	require.True(t, isAction("run-task")(got[0]))
	require.True(t, isAction("finish-task")(got[1]))

	h.upstream.send(t, inferenceTaskStarted)
	h.upstream.send(t, inferenceTaskFinished)

	summary := h.wait(t)
	require.Equal(t, CauseClientClosed, summary.Cause)

	<-hooks.ended
	require.Empty(t, hooks.usage, "no audio reached the upstream")
	require.Len(t, h.upstream.got(), 2, "nothing after the goodbye")
}

// A -streaming task that fails is still billed for the audio forwarded to it: no task-finished
// follows, so the session asks the dialect for its remainder once both pumps stopped.
func TestSession_QwenAudioStreamingBillsForwardedAudioWhenTheTaskFails(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), newQwenAudioStreaming(t), hooks)

	_, err := readFrame(t, h.client, 2*time.Second) // session.created
	require.NoError(t, err)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(pcm24Append(1000))))
	require.True(t, isAction("run-task")(h.upstream.waitFor(t, 1)[0]))

	h.upstream.send(t, inferenceTaskStarted)
	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(pcm24Append(1500))))
	require.Len(t, h.upstream.waitFor(t, 3), 3, "run-task and two audio frames")

	h.upstream.send(t, `{"header":{"event":"task-failed","error_code":"ResponseTimeout","error_message":"Response timeout!","attributes":{}},"payload":{}}`)

	summary := h.wait(t)
	require.Equal(t, CauseDialect, summary.Cause)
	require.True(t, summary.Failed())

	<-hooks.ended
	require.Len(t, hooks.usage, 1)
	require.InDelta(t, 3, hooks.usage[0].DurationSeconds, 1e-9, "2.5 s forwarded")
	require.Equal(t, "qwen_audio_task_tail", gjson.GetBytes(hooks.usage[0].Summary, "type").String())
}

func newLiveTranslate(t *testing.T) dialect.Dialect {
	t.Helper()

	d, err := bailian.NewLiveTranslate(bailian.LiveTranslateOptions{RequestedModel: "gpt-realtime-translate", ActualModel: "qwen3.8-livetranslate-flash-realtime"})
	require.NoError(t, err)

	return d
}

// translationAppend is 100 ms of 24 kHz PCM16 in a translation append.
var translationAppend = `{"type":"session.input_audio_buffer.append","audio":"` + base64.StdEncoding.EncodeToString(make([]byte, 4800)) + `"}`

// startTranslation opens a translation session and sends its first audio; with ack the
// upstream acknowledges the single update, so the audio goes upstream.
func startTranslation(t *testing.T, h *harness, ack bool) {
	t.Helper()

	h.upstream.send(t, `{"type":"session.created","session":{"id":"sess_lt_1"}}`)

	created, err := readFrame(t, h.client, 2*time.Second)
	require.NoError(t, err)
	require.Equal(t, "session.created", gjson.Get(created, "type").String())

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(translationAppend)))
	require.Equal(t, "session.update", gjson.Get(h.upstream.waitFor(t, 1)[0], "type").String())

	if ack {
		h.upstream.send(t, `{"type":"session.updated","session":{}}`)
		require.Len(t, h.upstream.waitFor(t, 2), 2, "the held audio")
	}
}

// readUntilClose returns the event types the client reads and the close frame that ends them.
func readUntilClose(t *testing.T, conn *websocket.Conn) ([]string, *websocket.CloseError) {
	t.Helper()

	var types []string

	for {
		frame, err := readFrame(t, conn, 5*time.Second)
		if err != nil {
			ce, ok := errors.AsType[*websocket.CloseError](err)
			require.True(t, ok, "the session ends with a close frame: %v", err)

			return types, ce
		}

		types = append(types, gjson.Get(frame, "type").String())
	}
}

// The upstream fails the flush of a client session.close (error, then close 1007): the client
// still gets session.closed before its close frame, and the session is recorded as failed, not as
// a completed goodbye.
func TestSession_TranslationGoodbyeTheUpstreamFailsIsAnsweredAndFailed(t *testing.T) {
	hooks := newRecordingHooks()
	h := newHarness(t, testConfig(), newLiveTranslate(t), hooks)
	startTranslation(t, h, true)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.close"}`)))
	require.Equal(t, "session.finish", gjson.Get(h.upstream.waitFor(t, 3)[2], "type").String())

	h.upstream.send(t, `{"type":"error","error":{"code":"COMMON_ERROR","message":"segment failed"}}`)

	h.upstream.mu.Lock()
	require.NoError(t, h.upstream.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1007, "segment failed"), time.Now().Add(time.Second)))
	h.upstream.mu.Unlock()

	types, ce := readUntilClose(t, h.client)
	require.Equal(t, []string{"error", "session.closed"}, types)
	require.Equal(t, websocket.CloseNormalClosure, ce.Code)

	summary := h.wait(t)
	require.Equal(t, CauseDialect, summary.Cause)
	require.Equal(t, "1000", summary.Code)
	require.True(t, summary.Failed())
	require.ErrorContains(t, summary.Err, "upstream error")
	require.False(t, summary.ClientFault())
}

func TestEndSummary_FailedGoodbyeIsAFailure(t *testing.T) {
	require.False(t, EndSummary{Cause: CauseDialect, Code: "1000"}.Failed())
	require.True(t, EndSummary{Cause: CauseDialect, Code: "1000", Err: errors.New("upstream did not finish")}.Failed())
}

// An init gate timeout leaves the upstream without any audio (it is held until the update is
// acknowledged): there is nothing to flush, so teardown neither sends session.finish nor waits
// the extended finish drain against an upstream that already stalled.
func TestSession_TranslationInitTimeoutSkipsTheFinishDrain(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	h := newHarness(t, cfg, newLiveTranslate(t), hooks)
	startTranslation(t, h, false)

	frame, err := readFrame(t, h.client, cfg.GateTimeout+3*time.Second)
	require.NoError(t, err)
	require.Equal(t, "upstream_init_timeout", gjson.Get(frame, "error.code").String())

	timedOut := time.Now()

	types, ce := readUntilClose(t, h.client)
	require.Empty(t, types)
	require.Equal(t, websocket.CloseInternalServerErr, ce.Code)

	summary := h.wait(t)
	require.Equal(t, CauseGateTimeout, summary.Cause)
	require.Less(t, time.Since(timedOut), 2*time.Second, "no finish drain")
	require.Zero(t, countFrames(h.upstream.got(), func(f string) bool { return strings.Contains(f, "session.finish") }))
}

// A restart that begins while an extended finish drain runs cuts it to what is left of the default
// drain, so teardown still fits the shutdown budget; the session keeps its first end cause.
func TestSession_ShutdownDuringTheExtendedFinishDrainCutsIt(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	lc := lifecycle.New()
	h := newHarnessWith(t, lc.Attach(t.Context()), cfg, &goodbyeDialect{drain: time.Minute}, hooks, false)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte("bye")))
	require.Equal(t, []string{"finish"}, h.upstream.waitFor(t, 1))

	start := time.Now()
	lc.Shutdown(nil)

	summary := h.wait(t)
	require.Equal(t, CauseDialect, summary.Cause)
	require.Less(t, time.Since(start), cfg.ResponseDrain+cfg.CloseGrace+time.Second)
}
