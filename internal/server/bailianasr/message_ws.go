package bailianasr

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

const (
	// messageStartTimeout bounds the handshake and the wait for task-started.
	messageStartTimeout = 15 * time.Second
	// messageIdleTimeout bounds the silence after all audio and finish-task were sent.
	messageIdleTimeout = 60 * time.Second
	// messageWriteTimeout bounds one frame write.
	messageWriteTimeout = 15 * time.Second
)

// messageSession is one started -message task: the socket is open and task-started has arrived,
// so audio may flow. Errors before this point are safe to retry (nothing was sent or billed);
// errors after it are not.
type messageSession struct {
	conn     *websocket.Conn
	taskID   string
	audio    io.ReadCloser
	interval time.Duration
	stopCtx  func() bool
}

// startMessage dials the inference WebSocket, sends run-task and waits for task-started.
func (e *jobExecutor) startMessage(ctx context.Context, req *httpclient.Request, j *job) (*messageSession, error) {
	taskID := strings.ReplaceAll(uuid.NewString(), "-", "")

	runTask, err := sjson.SetBytes(req.Body, "header.task_id", taskID)
	if err != nil {
		return nil, fmt.Errorf("failed to set the task id: %w", err)
	}

	audio, err := openMessageAudio(j.audio, *j.message)
	if err != nil {
		return nil, fmt.Errorf("failed to read audio: %w", err)
	}

	dialCtx, cancel := context.WithTimeout(ctx, messageStartTimeout)
	defer cancel()

	conn, resp, err := e.websocketDialer().DialContext(dialCtx, websocketURL(j.native+inferencePath), handshakeHeaders(req))
	if err != nil {
		audio.Close()
		return nil, handshakeError(ctx, resp, err)
	}

	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	s := &messageSession{conn: conn, taskID: taskID, audio: audio, interval: e.cfg.messageFrameInterval()}
	// A cancelled request closes the socket, which ends every blocked read and write.
	s.stopCtx = context.AfterFunc(ctx, func() { _ = conn.Close() })

	if err := s.awaitStart(ctx, runTask); err != nil {
		s.close()
		return nil, err
	}

	return s, nil
}

func (s *messageSession) awaitStart(ctx context.Context, runTask []byte) error {
	deadline := time.Now().Add(messageStartTimeout)
	_ = s.conn.SetWriteDeadline(deadline)

	if err := s.conn.WriteMessage(websocket.TextMessage, runTask); err != nil {
		return startError(ctx, s.taskID, err)
	}

	_ = s.conn.SetReadDeadline(deadline)

	for {
		_, frame, err := s.conn.ReadMessage()
		if err != nil {
			return startError(ctx, s.taskID, err)
		}

		root := gjson.ParseBytes(frame)

		switch root.Get("header.event").String() {
		case "task-started":
			_ = s.conn.SetReadDeadline(time.Time{})
			return nil
		case "task-failed":
			// Nothing was sent yet, so a retry cannot bill twice; unknown codes stay retryable.
			code, message := taskFailure(root)
			return newResponseError(statusForCode(code, http.StatusBadGateway), firstNonEmpty(code, "bailian_task_failed"),
				fmt.Sprintf("Bailian rejected the qwen-audio-asr-flash-message task %s: %s %s", s.taskID, code, message), "", nil)
		}
	}
}

func taskFailure(root gjson.Result) (string, string) {
	return root.Get("header.error_code").String(), root.Get("header.error_message").String()
}

func startError(ctx context.Context, taskID string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	return newResponseError(http.StatusGatewayTimeout, "bailian_task_not_started",
		fmt.Sprintf("Bailian did not start the qwen-audio-asr-flash-message task %s: %v", taskID, err), "", err)
}

// handshakeError reads DashScope's JSON error from a rejected handshake (bad key, model not
// enabled); a transport failure keeps a retryable 502.
func handshakeError(ctx context.Context, resp *http.Response, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if resp == nil {
		return newResponseError(http.StatusBadGateway, "bailian_unreachable",
			fmt.Sprintf("failed to connect to the Bailian inference WebSocket: %v", err), "", err)
	}

	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	code, message, requestID, _ := dashScopeError(body)

	if message == "" {
		message = fmt.Sprintf("the Bailian inference WebSocket handshake failed with HTTP %d", resp.StatusCode)
	}

	status := resp.StatusCode
	if status < http.StatusBadRequest {
		status = http.StatusBadGateway
	}

	return newResponseError(status, firstNonEmpty(code, "bailian_handshake_failed"), message, requestID, err)
}

// run sends the audio (paced) and finish-task while relaying the task's frames to emit, until
// task-finished. It always closes the session and returns only after its goroutines ended, so
// the audio source is no longer read once it returns.
func (s *messageSession) run(ctx context.Context, emit func(*httpclient.StreamEvent) error) error {
	ctx, cancel := context.WithCancel(ctx)

	var wg sync.WaitGroup

	defer func() {
		cancel()
		s.close()
		wg.Wait()
	}()

	frames := make(chan wsFrame)
	sendDone := make(chan error, 1)

	wg.Add(2)

	go func() {
		defer wg.Done()
		s.readFrames(ctx, frames)
	}()

	go func() {
		defer wg.Done()
		sendDone <- s.sendAudio(ctx)
	}()

	// sent is nil once the audio is out, so the loop stops selecting on it.
	sent := (<-chan error)(sendDone)

	var (
		idle    <-chan time.Time
		sendErr error
	)

	idleTimer := time.NewTimer(messageIdleTimeout)
	defer idleTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return s.lostError(ctx, ctx.Err())
		case err := <-sent:
			// A failed write usually means Bailian closed the socket after a task-failed frame
			// the reader has not delivered yet, so keep reading; the read then fails as well.
			if errors.Is(err, errAudioRead) {
				return s.lostError(ctx, err)
			}

			sendErr = err
			sent = nil
			idleTimer.Reset(messageIdleTimeout)
			idle = idleTimer.C
		case f := <-frames:
			if f.err != nil {
				return s.lostError(ctx, errors.Join(sendErr, f.err))
			}

			if idle != nil {
				idleTimer.Reset(messageIdleTimeout)
			}

			root := gjson.ParseBytes(f.data)
			event := root.Get("header.event").String()

			switch event {
			case "result-generated", "task-finished":
				if err := emit(&httpclient.StreamEvent{Type: event, Data: f.data}); err != nil {
					return err
				}

				if event == "task-finished" {
					return nil
				}
			case "task-failed":
				// The audio already started flowing, so the task may be billed: no retry.
				code, message := taskFailure(root)
				return billedTaskError(s.taskID, code, message)
			}
		case <-idle:
			return newResponseError(http.StatusFailedDependency, "bailian_task_timeout",
				fmt.Sprintf("Bailian did not finish the qwen-audio-asr-flash-message task %s within %s after the audio ended",
					s.taskID, messageIdleTimeout), "", errBilledJobFailed)
		}
	}
}

// lostError reports a task cut off after audio started flowing; it carries pipeline.ErrNoRetry
// and is never retried, on this channel or another, because a new task would bill the audio
// again.
func (s *messageSession) lostError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return newResponseError(http.StatusGatewayTimeout, "bailian_task_unfinished",
			fmt.Sprintf("the request ended before the qwen-audio-asr-flash-message task %s finished (%v)", s.taskID, ctx.Err()),
			"", errors.Join(ctx.Err(), pipeline.ErrNoRetry))
	}

	return newResponseError(http.StatusFailedDependency, "bailian_connection_lost",
		fmt.Sprintf("the qwen-audio-asr-flash-message task %s ended early: %v", s.taskID, err), "", errBilledJobFailed)
}

// errAudioRead marks a failure reading the local audio: the task can never finish, so the
// session ends at once instead of waiting for Bailian.
var errAudioRead = errors.New("failed to read audio")

type wsFrame struct {
	data []byte
	err  error
}

func (s *messageSession) readFrames(ctx context.Context, frames chan<- wsFrame) {
	for {
		kind, data, err := s.conn.ReadMessage()
		if err == nil && kind != websocket.TextMessage {
			continue
		}

		select {
		case frames <- wsFrame{data: data, err: err}:
		case <-ctx.Done():
			return
		}

		if err != nil {
			return
		}
	}
}

// sendAudio streams the audio in 100 ms frames, pacing them at the configured multiple of real
// time, then sends finish-task.
func (s *messageSession) sendAudio(ctx context.Context) error {
	buf := make([]byte, messageFrameBytes)
	started := time.Now()

	for frame := 1; ; frame++ {
		n, err := io.ReadFull(s.audio, buf)
		if n > 0 {
			if werr := s.write(websocket.BinaryMessage, buf[:n]); werr != nil {
				return werr
			}
		}

		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("%w: %w", errAudioRead, err)
		}

		if s.interval > 0 {
			if err := sleepContext(ctx, time.Until(started.Add(time.Duration(frame)*s.interval))); err != nil {
				return err
			}
		}
	}

	finish := fmt.Sprintf(`{"header":{"action":"finish-task","task_id":%q,"streaming":"duplex"},"payload":{"input":{}}}`, s.taskID)

	return s.write(websocket.TextMessage, []byte(finish))
}

func (s *messageSession) write(kind int, data []byte) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(messageWriteTimeout))
	return s.conn.WriteMessage(kind, data)
}

func (s *messageSession) close() {
	if s.stopCtx != nil {
		s.stopCtx()
	}

	_ = s.conn.Close()
	_ = s.audio.Close()
}

// websocketDialer keeps the channel's proxy and TLS settings when the inner executor is the
// channel HTTP client, as the realtime relay's dialer does: the proxy function for HTTP proxies,
// the transport's DialContext for SOCKS proxies (the channel client applies those there and its
// proxy function returns none), and a TLS config pinned to HTTP/1.1.
func (e *jobExecutor) websocketDialer() *websocket.Dialer {
	dialer := &websocket.Dialer{HandshakeTimeout: messageStartTimeout, Proxy: http.ProxyFromEnvironment}

	if hc, ok := e.inner.(*httpclient.HttpClient); ok {
		if proxy := hc.ProxyFunc(); proxy != nil {
			dialer.Proxy = proxy
		}

		if native := hc.GetNativeClient(); native != nil {
			if transport, ok := native.Transport.(*http.Transport); ok && transport != nil {
				if transport.DialContext != nil {
					dialer.NetDialContext = transport.DialContext
				}

				if transport.TLSClientConfig != nil {
					dialer.TLSClientConfig = transport.TLSClientConfig.Clone()
					dialer.TLSClientConfig.NextProtos = []string{"http/1.1"}
				}
			}
		}
	}

	if dialer.TLSClientConfig == nil {
		dialer.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}
	}

	return dialer
}

func websocketURL(httpURL string) string {
	switch {
	case strings.HasPrefix(httpURL, "https://"):
		return "wss://" + strings.TrimPrefix(httpURL, "https://")
	case strings.HasPrefix(httpURL, "http://"):
		return "ws://" + strings.TrimPrefix(httpURL, "http://")
	default:
		return httpURL
	}
}

// producerStream is a stream fed by a goroutine: first the given events, then whatever produce
// emits. Close cancels produce and waits for it, so nothing runs after the stream is closed.
type producerStream struct {
	events chan *httpclient.StreamEvent
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	cur    *httpclient.StreamEvent
	once   sync.Once
}

func newProducerStream(
	ctx context.Context,
	first []*httpclient.StreamEvent,
	produce func(ctx context.Context, emit func(*httpclient.StreamEvent) error) error,
) *producerStream {
	ctx, cancel := context.WithCancel(ctx)
	s := &producerStream{events: make(chan *httpclient.StreamEvent, 16), cancel: cancel, done: make(chan struct{})}

	emit := func(ev *httpclient.StreamEvent) error {
		select {
		case s.events <- ev:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	go func() {
		var err error
		for _, ev := range first {
			if err = emit(ev); err != nil {
				break
			}
		}

		if err == nil {
			err = produce(ctx, emit)
		}

		// done closes first: once Next has seen the end, Err already has the result.
		s.err = err
		close(s.done)
		close(s.events)
	}()

	return s
}

func (s *producerStream) Next() bool {
	ev, ok := <-s.events
	if !ok {
		return false
	}

	s.cur = ev

	return true
}

func (s *producerStream) Current() *httpclient.StreamEvent { return s.cur }

// Err is only meaningful once Next returned false.
func (s *producerStream) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

func (s *producerStream) Close() error {
	s.once.Do(func() {
		s.cancel()

		for range s.events { //nolint:revive // drain so the producer is never stuck on a send
		}

		<-s.done
	})

	return nil
}
