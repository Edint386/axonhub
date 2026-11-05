package realtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/lifecycle"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// goodbyeDialect models a client-requested goodbye (the translation protocol's session.close):
// the client's "bye" ends the session with 1000, teardown sends "finish" and the upstream's
// "finished" becomes the client's "closed" event.
type goodbyeDialect struct {
	passDialect

	drain time.Duration
}

func (d *goodbyeDialect) ClientFrame(frame []byte) (dialect.Output, error) {
	if string(frame) == "bye" {
		return dialect.Output{Close: &dialect.CloseRequest{Code: websocket.CloseNormalClosure, Reason: "bye"}}, nil
	}

	return d.passDialect.ClientFrame(frame)
}

func (d *goodbyeDialect) UpstreamFrame(frame []byte) (dialect.Output, error) {
	if string(frame) == "finished" {
		return dialect.Output{ToClient: [][]byte{[]byte("closed")}, UpstreamFinished: true}, nil
	}

	return dialect.Output{ToClient: [][]byte{frame}}, nil
}

func (d *goodbyeDialect) CloseUpstream() [][]byte { return [][]byte{[]byte("finish")} }

func (d *goodbyeDialect) FinishDrain() bool { return true }

func (d *goodbyeDialect) FinishDrainTimeout() time.Duration { return d.drain }

func TestEndSummary_DialectGoodbyeIsNotAFailure(t *testing.T) {
	require.False(t, EndSummary{Cause: CauseDialect, Code: "1000"}.Failed())
	require.True(t, EndSummary{Cause: CauseDialect, Code: "1011"}.Failed())
	require.True(t, EndSummary{Cause: CauseDialect, Code: "1003"}.Failed())
}

// The upstream flushes for longer than ResponseDrain: the dialect's FinishDrainTimeout keeps the
// session relaying, the flushed frames and the goodbye event reach the client before its 1000
// close frame, and the session is recorded as completed.
func TestSession_ClientGoodbyeDrainsFlushedFramesBeforeClosing(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	require.Less(t, cfg.ResponseDrain, 700*time.Millisecond)

	h := newHarness(t, cfg, &goodbyeDialect{drain: 3 * time.Second}, hooks)

	go func() {
		got := h.upstream.waitFor(t, 1)
		if len(got) > 0 && got[len(got)-1] == "finish" {
			time.Sleep(700 * time.Millisecond)
			h.upstream.send(t, "late translation")
			h.upstream.send(t, "finished")
		}
	}()

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte("bye")))

	var frames []string

	for {
		frame, err := readFrame(t, h.client, 5*time.Second)
		if err != nil {
			ce, ok := errors.AsType[*websocket.CloseError](err)
			require.True(t, ok, "the session ends with a close frame: %v", err)
			require.Equal(t, websocket.CloseNormalClosure, ce.Code)

			break
		}

		frames = append(frames, frame)
	}

	require.Equal(t, []string{"late translation", "closed"}, frames)

	summary := h.wait(t)
	require.Equal(t, CauseDialect, summary.Cause)
	require.False(t, summary.Failed())

	finishes := 0

	for _, frame := range h.upstream.got() {
		if strings.HasPrefix(frame, "finish") {
			finishes++
		}
	}

	require.Equal(t, 1, finishes)
}

// A server restart keeps the default drain: the extended finish drain must not outlast the
// shutdown budget.
func TestSession_ShutdownIgnoresTheExtendedFinishDrain(t *testing.T) {
	hooks := newRecordingHooks()
	cfg := testConfig()
	lc := lifecycle.New()
	h := newHarnessWith(t, lc.Attach(t.Context()), cfg, &goodbyeDialect{drain: time.Minute}, hooks, false)

	require.NoError(t, h.client.WriteMessage(websocket.TextMessage, []byte("ping")))
	h.upstream.waitFor(t, 1)

	start := time.Now()
	lc.Shutdown(nil)

	summary := h.wait(t)
	require.Equal(t, CauseShutdown, summary.Cause)
	require.Less(t, time.Since(start), cfg.ResponseDrain+cfg.CloseGrace+time.Second)
}
