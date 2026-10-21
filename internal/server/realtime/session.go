package realtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/lifecycle"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
)

// EndCause explains why a session ended.
type EndCause string

const (
	CauseClientClosed   EndCause = "client_closed"
	CauseClientError    EndCause = "client_error"
	CauseUpstreamClosed EndCause = "upstream_closed"
	CauseUpstreamError  EndCause = "upstream_error"
	CauseWriteFailed    EndCause = "write_failed"
	CauseMaxDuration    EndCause = "max_duration"
	CauseIdle           EndCause = "idle"
	CausePolicy         EndCause = "policy"
	CauseShutdown       EndCause = "shutdown"
	CauseDialect        EndCause = "dialect"
	CauseGateTimeout    EndCause = "gate_timeout"
	CauseAborted        EndCause = "aborted"
)

// EndSummary describes a finished session.
type EndSummary struct {
	Cause EndCause
	Err   error
	// Code is the close code received from the peer that ended the session (0 when none).
	Code   string
	Reason string
	// Events counts upstream frames; Responses counts upstream response.created events.
	Events    int64
	Responses int64
	Duration  time.Duration
	// TimeToFirstFrame is the delay between Run and the first upstream frame (0 when none arrived).
	TimeToFirstFrame time.Duration
}

// Failed reports whether the session ended because of an upstream or gateway failure rather
// than a normal or client-driven close. Client faults (see ClientFault) are not failures.
func (e EndSummary) Failed() bool {
	switch e.Cause {
	case CauseUpstreamError, CauseWriteFailed, CauseDialect, CauseGateTimeout:
		return true
	case CauseUpstreamClosed:
		// 1005 is "no status received": gorilla reports it for an empty close payload and
		// mapCloseCode treats it as a normal close.
		return e.Code != "1000" && e.Code != "1001" && e.Code != "1005" && e.Code != ""
	default:
		return false
	}
}

// ClientFault reports whether the client side ended the session abnormally (its upgrade
// failed, its connection was lost, or writes to it failed). Such endings are the client's
// doing and must not count against the upstream channel.
func (e EndSummary) ClientFault() bool {
	return e.Cause == CauseAborted || e.Cause == CauseClientError
}

// Hooks receives session events. All methods are called from session goroutines; OnUsage and
// OnEnd receive contexts detached from the request so persistence survives cancellation.
type Hooks interface {
	// OnUsage records one usage unit extracted from the upstream stream.
	OnUsage(ctx context.Context, rec dialect.UsageRecord)
	// OnCheck runs the periodic policy re-check; a non-nil error ends the session with close
	// code 1008 after a GA error event carrying err.Error().
	OnCheck(ctx context.Context) error
	// OnFirstUpstreamFrame reports the time to the first upstream frame.
	OnFirstUpstreamFrame(d time.Duration)
	// OnEnd is called exactly once after both sockets are closed.
	OnEnd(ctx context.Context, summary EndSummary)
}

// NoopHooks is a Hooks implementation that does nothing.
type NoopHooks struct{}

func (NoopHooks) OnUsage(context.Context, dialect.UsageRecord) {}
func (NoopHooks) OnCheck(context.Context) error                { return nil }
func (NoopHooks) OnFirstUpstreamFrame(time.Duration)           {}
func (NoopHooks) OnEnd(context.Context, EndSummary)            {}

// SessionParams configures a Session.
type SessionParams struct {
	Config   Config
	Dialect  dialect.Dialect
	Upstream *websocket.Conn
	Hooks    Hooks
	IDs      *dialect.EventIDs
	// MaxDuration overrides Config.SessionDuration(dialect) when positive.
	MaxDuration time.Duration
	// Name is used in logs.
	Name string
	// Sideband marks a control channel of a WebRTC call: the media peer keeps listening after
	// this socket ends, so teardown must not cancel the in-flight response upstream.
	Sideband bool
}

type endState struct {
	cause  EndCause
	err    error
	code   int
	reason string
}

// Session relays frames between one client WebSocket and one upstream WebSocket through a
// Dialect. Create it after the upstream is connected, then call Run once the client socket is
// upgraded (or Abort when the upgrade failed).
type Session struct {
	cfg      Config
	dialect  dialect.Dialect
	hooks    Hooks
	ids      *dialect.EventIDs
	name     string
	maxDur   time.Duration
	sideband bool
	upstream *websocket.Conn
	client   *websocket.Conn

	baseCtx context.Context
	ctx     context.Context
	cancel  context.CancelFunc

	cw *socketWriter
	uw *socketWriter

	started      time.Time
	firstFrameAt atomic.Int64
	lastActivity atomic.Int64
	events       atomic.Int64
	responses    atomic.Int64

	// respDone is non-nil while an upstream response is in progress and is closed when that
	// response ends, so teardown can wait for exactly the response that is streaming.
	respMu   sync.Mutex
	respDone chan struct{}

	gateMu     sync.Mutex
	gated      bool
	gateStart  time.Time
	gateQueue  [][]byte
	gateBytes  int64
	gateOpened chan struct{}

	upstreamPong atomic.Bool
	// upstreamDeadlineMu serialises the liveness deadline between the upstream pump (refresh),
	// the pong handler and the watchdog (which switches it off).
	upstreamDeadlineMu  sync.Mutex
	upstreamDeadlineOff bool
	upstreamPumpDone    chan struct{}
	clientPumpDone      chan struct{}
	checkNow            chan struct{}
	lastCheck           atomic.Int64

	usageCh chan dialect.UsageRecord

	endOnce sync.Once
	end     endState
	endCh   chan struct{}
}

// gateTimeoutHinter is an optional Dialect extension: a dialect-specific bound for the
// initialisation phase during which client frames are queued.
type gateTimeoutHinter interface {
	GateTimeout() time.Duration
}

// closeGraceHinter is an optional Dialect extension: a dialect-specific bound for the close
// handshake at teardown.
type closeGraceHinter interface {
	CloseGrace() time.Duration
}

// NewSession wires a session; it does not start any goroutine.
func NewSession(ctx context.Context, p SessionParams) *Session {
	cfg := p.Config.withDefaults()

	if h, ok := p.Dialect.(gateTimeoutHinter); ok {
		if d := h.GateTimeout(); d > 0 && d < cfg.GateTimeout {
			cfg.GateTimeout = d
		}
	}

	if h, ok := p.Dialect.(closeGraceHinter); ok {
		if d := h.CloseGrace(); d > 0 && d < cfg.CloseGrace {
			cfg.CloseGrace = d
		}
	}

	ids := p.IDs
	if ids == nil {
		ids = &dialect.EventIDs{}
	}

	hooks := p.Hooks
	if hooks == nil {
		hooks = NoopHooks{}
	}

	maxDur := p.MaxDuration
	if maxDur <= 0 {
		maxDur = cfg.SessionDuration(p.Dialect.Name())
	}

	return &Session{
		cfg:              cfg,
		dialect:          p.Dialect,
		hooks:            hooks,
		ids:              ids,
		name:             p.Name,
		maxDur:           maxDur,
		sideband:         p.Sideband,
		upstream:         p.Upstream,
		baseCtx:          ctx,
		gateOpened:       make(chan struct{}),
		upstreamPumpDone: make(chan struct{}),
		clientPumpDone:   make(chan struct{}),
		checkNow:         make(chan struct{}, 1),
		usageCh:          make(chan dialect.UsageRecord, 64),
		endCh:            make(chan struct{}),
	}
}

// Abort closes the upstream without ever running the relay (the client upgrade failed).
func (s *Session) Abort(cause error) {
	ctx, cancel := xcontext.DetachWithTimeout(s.baseCtx, s.cfg.WriteTimeout)
	defer cancel()

	for _, frame := range s.dialect.CloseUpstream() {
		_ = s.upstream.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
		_ = s.upstream.WriteMessage(websocket.TextMessage, frame)
	}

	_ = s.upstream.WriteControl(websocket.CloseMessage, closePayload(websocket.CloseNormalClosure, "client upgrade failed"), time.Now().Add(s.cfg.WriteTimeout))
	_ = s.upstream.Close()

	s.hooks.OnEnd(ctx, EndSummary{Cause: CauseAborted, Err: cause})
}

// Run relays until either side ends the session and returns the summary. It blocks for the
// whole session; the HTTP handler must not return before it does.
func (s *Session) Run(client *websocket.Conn) EndSummary {
	s.client = client
	s.started = time.Now()
	s.touch()

	ctx, cancel := context.WithCancel(s.baseCtx)
	s.ctx, s.cancel = ctx, cancel

	s.cw = newSocketWriter("client", client, s.cfg)
	s.uw = newSocketWriter("upstream", s.upstream, s.cfg)

	var helpers sync.WaitGroup

	helpers.Add(2)

	go func() {
		defer helpers.Done()

		s.cw.run(ctx)
	}()

	go func() {
		defer helpers.Done()

		s.uw.run(ctx)
	}()

	s.setupSocket(client, true)
	s.setupSocket(s.upstream, false)

	helpers.Add(3)

	go s.pinger(ctx, &helpers, s.cw)
	go s.pinger(ctx, &helpers, s.uw)
	go s.watchdog(ctx, &helpers)

	var accounting sync.WaitGroup

	accounting.Add(1)

	go s.accounting(&accounting)

	// Dialect start: initial upstream frames and the gate.
	toUpstream, gated := s.dialect.Start()
	if gated {
		s.gateMu.Lock()
		s.gated = true
		s.gateStart = time.Now()
		s.gateMu.Unlock()
	}

	for _, frame := range toUpstream {
		if err := s.uw.enqueue(ctx, websocket.TextMessage, frame); err != nil {
			s.finish(CauseWriteFailed, err, 0, "")
		}
	}

	var pumps sync.WaitGroup

	pumps.Add(2)

	go s.upstreamPump(ctx, &pumps)
	go s.clientPump(ctx, &pumps)

	<-s.endCh

	s.teardown(ctx)
	cancel()
	pumps.Wait()
	helpers.Wait()

	close(s.usageCh)
	accounting.Wait()

	summary := s.summary()

	endCtx, endCancel := xcontext.DetachWithTimeout(s.baseCtx, 10*time.Second)
	defer endCancel()

	s.hooks.OnEnd(endCtx, summary)

	return summary
}

func (s *Session) summary() EndSummary {
	st := s.end

	var ttff time.Duration
	if at := s.firstFrameAt.Load(); at > 0 {
		ttff = time.Duration(at - s.started.UnixNano())
	}

	code := ""
	if st.code != 0 {
		code = itoa(st.code)
	}

	return EndSummary{
		Cause:            st.cause,
		Err:              st.err,
		Code:             code,
		Reason:           st.reason,
		Events:           s.events.Load(),
		Responses:        s.responses.Load(),
		Duration:         time.Since(s.started),
		TimeToFirstFrame: ttff,
	}
}

func itoa(n int) string {
	const digits = "0123456789"

	if n == 0 {
		return "0"
	}

	neg := n < 0
	if neg {
		n = -n
	}

	var buf [20]byte

	i := len(buf)

	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}

	if neg {
		i--
		buf[i] = '-'
	}

	return string(buf[i:])
}

// finish records the first end cause and unblocks Run. Later calls are ignored.
func (s *Session) finish(cause EndCause, err error, code int, reason string) {
	s.endOnce.Do(func() {
		s.end = endState{cause: cause, err: err, code: code, reason: reason}
		close(s.endCh)
	})
}

func (s *Session) ending() bool {
	select {
	case <-s.endCh:
		return true
	default:
		return false
	}
}

func (s *Session) touch() {
	s.lastActivity.Store(time.Now().UnixNano())
}

func (s *Session) setupSocket(conn *websocket.Conn, isClient bool) {
	conn.SetReadLimit(s.cfg.MaxMessageSize)
	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.PongWait))

	conn.SetPongHandler(func(string) error {
		if !isClient {
			s.upstreamPong.Store(true)
		}

		s.refreshDeadline(conn, isClient)

		return nil
	})
}

// refreshDeadline re-arms the liveness deadline of one socket. The upstream deadline is
// guarded so the watchdog can switch it off without a stale re-arm slipping in between.
func (s *Session) refreshDeadline(conn *websocket.Conn, isClient bool) {
	if isClient {
		_ = conn.SetReadDeadline(time.Now().Add(s.cfg.PongWait))
		return
	}

	s.upstreamDeadlineMu.Lock()
	defer s.upstreamDeadlineMu.Unlock()

	if s.upstreamDeadlineOff {
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(s.cfg.PongWait))
}

// disableUpstreamDeadline stops enforcing the upstream liveness deadline (the idle timer
// still applies).
func (s *Session) disableUpstreamDeadline() {
	s.upstreamDeadlineMu.Lock()
	defer s.upstreamDeadlineMu.Unlock()

	s.upstreamDeadlineOff = true
	_ = s.upstream.SetReadDeadline(time.Time{})
}

func (s *Session) pinger(ctx context.Context, wg *sync.WaitGroup, w *socketWriter) {
	defer wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error(ctx, "panic in realtime pinger", log.String("session", s.name), log.Any("panic", recovered))
		}
	}()

	ticker := time.NewTicker(s.cfg.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.ping(); err != nil && !errors.Is(err, websocket.ErrCloseSent) {
				return
			}
		}
	}
}

func (s *Session) watchdog(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error(ctx, "panic in realtime watchdog", log.String("session", s.name), log.Any("panic", recovered))
			s.finish(CauseWriteFailed, errors.New("watchdog panic"), 0, "")
		}
	}()

	var shutdown <-chan struct{}
	if lc := lifecycle.FromContext(s.baseCtx); lc != nil {
		shutdown = lc.Context().Done()
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	checks := time.NewTicker(s.cfg.QuotaCheckInterval)
	defer checks.Stop()

	pongProbe := time.NewTimer(s.cfg.PingInterval + s.cfg.PingInterval/2)
	defer pongProbe.Stop()

	for {
		select {
		case <-ctx.Done():
			// The request context went away before the session ended (a future middleware
			// deadline, say): end it instead of leaving Run waiting on a close frame.
			s.finish(CauseShutdown, context.Cause(ctx), 0, "")
			return
		case <-shutdown:
			s.finish(CauseShutdown, nil, 0, "")
			return
		case <-pongProbe.C:
			if !s.upstreamPong.Load() {
				// The upstream never answered a ping (Bailian keepalive is undocumented):
				// stop enforcing the liveness deadline and rely on the idle timer instead.
				s.disableUpstreamDeadline()
			}
		case <-ticker.C:
			now := time.Now()

			s.gateMu.Lock()
			gateTimedOut := s.gated && now.Sub(s.gateStart) > s.cfg.GateTimeout
			s.gateMu.Unlock()

			if gateTimedOut {
				s.sendClientError("server_error", "upstream_init_timeout", "upstream session initialisation timed out", "")
				s.finish(CauseGateTimeout, errors.New("gate timeout"), websocket.CloseInternalServerErr, "upstream session initialisation timed out")

				return
			}

			if now.Sub(s.started) > s.maxDur {
				s.finish(CauseMaxDuration, nil, websocket.CloseNormalClosure, "session duration limit reached")
				return
			}

			if now.Sub(time.Unix(0, s.lastActivity.Load())) > s.cfg.IdleTimeout {
				s.finish(CauseIdle, nil, websocket.CloseNormalClosure, "idle timeout")
				return
			}
		case <-checks.C:
			if s.runCheck(ctx) {
				return
			}
		case <-s.checkNow:
			if time.Since(time.Unix(0, s.lastCheck.Load())) < s.cfg.QuotaCheckInterval/6 {
				continue
			}

			if s.runCheck(ctx) {
				return
			}
		}
	}
}

// runCheck runs the policy hook; it returns true when the session was ended.
func (s *Session) runCheck(ctx context.Context) bool {
	s.lastCheck.Store(time.Now().UnixNano())

	// Bound by the session context so a check in flight cannot hold Run past teardown.
	checkCtx, cancel := context.WithTimeout(ctx, min(s.cfg.QuotaCheckInterval, 10*time.Second))
	defer cancel()

	if err := s.hooks.OnCheck(checkCtx); err != nil {
		log.Info(ctx, "realtime session closed by policy", log.String("session", s.name), log.Cause(err))
		s.sendClientError("invalid_request_error", "policy_violation", err.Error(), "")
		s.finish(CausePolicy, err, websocket.ClosePolicyViolation, err.Error())

		return true
	}

	return false
}

func (s *Session) sendClientError(errType, code, message, param string) {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.WriteTimeout)
	defer cancel()

	_ = s.cw.enqueue(ctx, websocket.TextMessage, dialect.ErrorEvent(s.ids, errType, code, message, param, ""))
}

func (s *Session) accounting(wg *sync.WaitGroup) {
	defer wg.Done()
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error(s.baseCtx, "panic in realtime accounting", log.String("session", s.name), log.Any("panic", recovered))
		}
	}()

	for rec := range s.usageCh {
		ctx, cancel := xcontext.DetachWithTimeout(s.baseCtx, 10*time.Second)
		s.hooks.OnUsage(ctx, rec)
		cancel()

		select {
		case s.checkNow <- struct{}{}:
		default:
		}
	}
}

func (s *Session) clientPump(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(s.clientPumpDone)
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error(ctx, "panic in realtime client pump", log.String("session", s.name), log.Any("panic", recovered))
			s.finish(CauseWriteFailed, errors.New("client pump panic"), 0, "")
		}
	}()

	for {
		msgType, data, err := s.client.ReadMessage()
		if err != nil {
			if code, reason, ok := closeInfo(err); ok {
				s.finish(CauseClientClosed, nil, code, reason)
			} else if ctx.Err() == nil && !s.ending() {
				s.finish(CauseClientError, err, 0, "")
			}

			return
		}

		s.touch()
		s.refreshDeadline(s.client, true)

		if msgType != websocket.TextMessage {
			s.sendClientError("invalid_request_error", "invalid_frame", "binary frames are not supported; send JSON text frames", "")
			continue
		}

		if s.queueIfGated(ctx, data) {
			continue
		}

		out, err := s.dialect.ClientFrame(data)
		if err != nil {
			log.Debug(ctx, "realtime client frame translation failed", log.String("session", s.name), log.Cause(err))
			s.sendClientError("invalid_request_error", "invalid_event", err.Error(), "")

			continue
		}

		if !s.apply(ctx, out) {
			return
		}
	}
}

func (s *Session) upstreamPump(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	defer close(s.upstreamPumpDone)
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Error(ctx, "panic in realtime upstream pump", log.String("session", s.name), log.Any("panic", recovered))
			s.finish(CauseWriteFailed, errors.New("upstream pump panic"), 0, "")
		}
	}()

	first := true

	for {
		msgType, data, err := s.upstream.ReadMessage()
		if err != nil {
			if code, reason, ok := closeInfo(err); ok {
				s.finish(CauseUpstreamClosed, nil, code, reason)
			} else if code, reason, ok := oversizedClose(s.upstream, err); ok {
				// The upstream broke the 125-byte control-frame limit (Bailian does for its
				// quota message); gorilla has already answered 1002, but the client still
				// gets the real code and reason.
				log.Warn(ctx, "realtime upstream sent an oversized close frame", log.String("session", s.name), log.Int("code", code), log.String("reason", reason))
				s.finish(CauseUpstreamClosed, nil, code, reason)
			} else if ctx.Err() == nil && !s.ending() {
				s.finish(CauseUpstreamError, err, 0, "")
			}

			return
		}

		if first {
			first = false

			s.firstFrameAt.Store(time.Now().UnixNano())
			s.hooks.OnFirstUpstreamFrame(time.Since(s.started))
		}

		s.touch()
		s.refreshDeadline(s.upstream, false)

		if msgType != websocket.TextMessage {
			log.Debug(ctx, "realtime upstream sent a binary frame; ignored", log.String("session", s.name), log.Int("bytes", len(data)))
			continue
		}

		s.events.Add(1)

		out, err := s.dialect.UpstreamFrame(data)
		if err != nil {
			log.Warn(ctx, "realtime upstream frame translation failed; forwarding raw frame", log.String("session", s.name), log.Cause(err))

			out = dialect.Output{ToClient: [][]byte{data}}
		}

		if !s.apply(ctx, out) {
			return
		}
	}
}

// apply records usage and state changes, then writes the translated frames. A pump keeps
// reading whatever happens to the writers: usage and response state are handled before any
// write, frames for a closed or failed writer are dropped, and the pump only returns when the
// session context is done (its own socket's read error ends it otherwise). This is what lets
// teardown read response.done after the client has already gone.
func (s *Session) apply(ctx context.Context, out dialect.Output) bool {
	for _, rec := range out.Usage {
		select {
		case s.usageCh <- rec:
		case <-ctx.Done():
			return false
		}
	}

	if out.ResponseInProgress != nil {
		s.setResponseInProgress(*out.ResponseInProgress)
	}

	// Once the session is ending, client input is not forwarded any more: teardown owns the
	// upstream writer (response.cancel, dialect goodbye, close frame).
	if !s.ending() {
		for _, frame := range out.ToUpstream {
			if err := s.uw.enqueue(ctx, websocket.TextMessage, frame); err != nil {
				s.writerFailed(err, false)
				break
			}
		}
	}

	for _, frame := range out.ToClient {
		if err := s.cw.enqueue(ctx, websocket.TextMessage, frame); err != nil {
			s.writerFailed(err, true)
			break
		}
	}

	if out.OpenGate {
		if !s.openGate(ctx) {
			return false
		}
	}

	if out.Close != nil {
		s.finish(CauseDialect, nil, out.Close.Code, out.Close.Reason)
	}

	return ctx.Err() == nil
}

// writerFailed classifies an enqueue error. A writer that was closed by teardown, or stopped
// because its peer's close was already echoed, is not a failure; a socket write error ends the
// session with a cause attributed to the side that failed.
func (s *Session) writerFailed(err error, isClient bool) {
	if s.ending() || errors.Is(err, errWriterClosed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}

	if errors.Is(err, websocket.ErrCloseSent) {
		// gorilla echoed the peer's close: the pump reading that socket reports the close
		// (with its code) right after.
		return
	}

	if isClient {
		s.finish(CauseClientError, err, 0, "")
		return
	}

	s.finish(CauseWriteFailed, err, 0, "")
}

// setResponseInProgress tracks the upstream response state for teardown.
func (s *Session) setResponseInProgress(inProgress bool) {
	s.respMu.Lock()
	defer s.respMu.Unlock()

	if inProgress {
		s.responses.Add(1)

		if s.respDone == nil {
			s.respDone = make(chan struct{})
		}

		return
	}

	if s.respDone != nil {
		close(s.respDone)
		s.respDone = nil
	}
}

// responseDone returns a channel closed when the response currently streaming ends, or nil
// when none is in progress.
func (s *Session) responseDone() <-chan struct{} {
	s.respMu.Lock()
	defer s.respMu.Unlock()

	if s.respDone == nil {
		return nil
	}

	return s.respDone
}

// queueIfGated stores a client frame while the dialect initialises the upstream session.
func (s *Session) queueIfGated(ctx context.Context, data []byte) bool {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()

	if !s.gated {
		return false
	}

	limit := s.cfg.MaxMessageSize * 2
	if s.gateBytes+int64(len(data)) > limit || len(s.gateQueue) >= 512 {
		log.Warn(ctx, "realtime gate queue overflow", log.String("session", s.name))
		s.sendClientError("server_error", "upstream_init_overflow", "too many frames were sent before the upstream session was ready", "")
		s.finish(CauseGateTimeout, errors.New("gate overflow"), websocket.CloseInternalServerErr, "upstream session initialisation overflow")

		return true
	}

	s.gateQueue = append(s.gateQueue, data)
	s.gateBytes += int64(len(data))

	return true
}

// openGate flushes the queued client frames through the dialect in order and lets new client
// frames through. Flushing happens under the gate mutex so no later frame can overtake.
func (s *Session) openGate(ctx context.Context) bool {
	s.gateMu.Lock()
	defer s.gateMu.Unlock()

	if !s.gated {
		return true
	}

	s.gated = false
	queued := s.gateQueue
	s.gateQueue = nil
	s.gateBytes = 0

	defer close(s.gateOpened)

	for _, frame := range queued {
		out, err := s.dialect.ClientFrame(frame)
		if err != nil {
			s.sendClientError("invalid_request_error", "invalid_event", err.Error(), "")
			continue
		}

		if out.OpenGate {
			out.OpenGate = false
		}

		if !s.apply(ctx, out) {
			return false
		}
	}

	return true
}

// teardown closes both sides in the order required to keep usage and close codes intact.
func (s *Session) teardown(ctx context.Context) {
	st := s.end

	clientInitiated := st.cause == CauseClientClosed || st.cause == CauseClientError
	upstreamInitiated := st.cause == CauseUpstreamClosed || st.cause == CauseUpstreamError

	// 0. The side that ended the session gets nothing more from us: gorilla already echoed its
	// close, or its socket is dead. Stop that writer now so the other pump never blocks on a
	// full queue during the drain, and so the grace wait below only covers real handshakes.
	if clientInitiated {
		s.cw.stop()
	}

	if upstreamInitiated {
		s.uw.stop()
	}

	upstreamAlive := !upstreamInitiated

	select {
	case <-s.upstreamPumpDone:
		upstreamAlive = false
	default:
	}

	// Steps 1 and 2 share one write budget so a stalled upstream cannot stretch teardown.
	ioCtx, ioCancel := context.WithTimeout(context.Background(), s.cfg.WriteTimeout)
	defer ioCancel()

	// 1. The client went away (or a gateway limit hit) while a response is streaming: cancel it
	// upstream and wait briefly for response.done so its usage is recorded. A sideband is only
	// the control channel of a WebRTC call whose media peer is still listening, so its response
	// is left alone.
	if upstreamAlive && !s.sideband {
		if done := s.responseDone(); done != nil {
			_ = s.uw.enqueue(ioCtx, websocket.TextMessage, []byte(`{"type":"response.cancel"}`))

			drain := time.NewTimer(s.cfg.ResponseDrain)
			select {
			case <-done:
			case <-s.upstreamPumpDone:
			case <-drain.C:
			}

			drain.Stop()
		}
	}

	// 2. Dialect-specific goodbye frames (e.g. Bailian session.finish).
	if upstreamAlive {
		for _, frame := range s.dialect.CloseUpstream() {
			_ = s.uw.enqueue(ioCtx, websocket.TextMessage, frame)
		}
	}

	// 3. Close frames. gorilla echoes a received close automatically, so only the side that did
	// not initiate gets one from us. An abnormal upstream close with a reason is also surfaced
	// as a GA error event, because the close frame can only carry 123 bytes of it.
	clientCode, clientReason, upCode, upReason := closeCodes(st)

	if st.cause == CauseUpstreamClosed && st.reason != "" && clientCode != websocket.CloseNormalClosure {
		s.sendClientError("server_error", "upstream_closed", st.reason, "")
	}

	if !clientInitiated {
		s.cw.enqueueClose(clientCode, clientReason)
	}

	if !upstreamInitiated {
		s.uw.enqueueClose(upCode, upReason)
	}

	// 4. Grace period for the close handshakes: for each side we sent a close to, wait until it
	// was written and then until the peer's echo was read (the pump exits on the CloseError),
	// so the socket is not closed with unread data in the kernel buffer (which would turn the
	// close into an RST on some stacks).
	grace := time.NewTimer(s.cfg.CloseGrace)
	defer grace.Stop()

	handshakes := make([]<-chan struct{}, 0, 4)

	if !clientInitiated {
		handshakes = append(handshakes, s.cw.done, s.clientPumpDone)
	}

	if !upstreamInitiated {
		handshakes = append(handshakes, s.uw.done, s.upstreamPumpDone)
	}

waitHandshakes:
	for _, done := range handshakes {
		select {
		case <-done:
		case <-grace.C:
			break waitHandshakes
		}
	}

	// 5. Force both sockets closed so blocked readers return, then stop the writers.
	if !clientInitiated {
		s.cw.emergencyClose(clientCode, clientReason)
	}

	if !upstreamInitiated {
		s.uw.emergencyClose(upCode, upReason)
	}

	_ = s.client.Close()
	_ = s.upstream.Close()
	s.cw.stop()
	s.uw.stop()

	log.Info(ctx, "realtime session ended",
		log.String("session", s.name),
		log.String("cause", string(st.cause)),
		log.Int("peer_close_code", st.code),
		log.Int64("events", s.events.Load()),
		log.Int64("responses", s.responses.Load()),
		log.Duration("duration", time.Since(s.started)),
	)
}

// closeCodes maps the end cause to the close frames sent to each side.
func closeCodes(st endState) (clientCode int, clientReason string, upCode int, upReason string) {
	switch st.cause {
	case CauseClientClosed:
		return websocket.CloseNormalClosure, "", websocket.CloseNormalClosure, "client closed"
	case CauseClientError:
		return websocket.CloseNormalClosure, "", websocket.CloseNormalClosure, "client connection lost"
	case CauseUpstreamClosed:
		return mapCloseCode(st.code), st.reason, websocket.CloseNormalClosure, ""
	case CauseUpstreamError:
		return websocket.CloseInternalServerErr, "upstream connection lost", websocket.CloseNormalClosure, ""
	case CauseWriteFailed:
		return websocket.CloseInternalServerErr, "relay write failed", websocket.CloseInternalServerErr, "relay write failed"
	case CauseMaxDuration:
		return websocket.CloseNormalClosure, "session duration limit reached", websocket.CloseNormalClosure, "session duration limit reached"
	case CauseIdle:
		return websocket.CloseNormalClosure, "idle timeout", websocket.CloseNormalClosure, "idle timeout"
	case CausePolicy:
		return websocket.ClosePolicyViolation, st.reason, websocket.CloseNormalClosure, "policy"
	case CauseShutdown:
		return websocket.CloseServiceRestart, "server restarting", websocket.CloseGoingAway, "server restarting"
	case CauseDialect:
		code := st.code
		if code == 0 {
			code = websocket.CloseInternalServerErr
		}

		return code, st.reason, websocket.CloseNormalClosure, ""
	case CauseGateTimeout:
		return websocket.CloseInternalServerErr, st.reason, websocket.CloseNormalClosure, ""
	default:
		return websocket.CloseInternalServerErr, "", websocket.CloseInternalServerErr, ""
	}
}
