package realtime

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/lifecycle"
	"github.com/looplj/axonhub/internal/server/realtime/calls"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// monitorAttachBackoff is the wait after each failed initial attach: a freshly created call
// may take a moment to become attachable, so the monitor retries briefly like Codex does.
var monitorAttachBackoff = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 800 * time.Millisecond}

// monitorReconnectBackoff is the wait before each reconnect attempt after the monitor socket
// dropped abnormally while the call may still be live.
var monitorReconnectBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// monitorStop records why the monitor stopped reading its socket.
type monitorStop int32

const (
	// monitorStopNone means the read itself ended (close frame, drop or idle timeout).
	monitorStopNone monitorStop = iota
	// monitorStopShutdown means the pinger closed the socket because the server is shutting down.
	monitorStopShutdown
	// monitorStopDeadline means the pinger closed the socket because the binding TTL passed.
	monitorStopDeadline
	// monitorStopCallEnded means the binding is gone (hangup or expiry), so the call is over.
	monitorStopCallEnded
)

// startMonitor opens a gateway-owned read-only sideband on an OpenAI-family WebRTC call so
// that response.done usage is recorded even when the client never attaches through the
// gateway. ctx is the PrepareRealtimeContext-wrapped request context: the monitor detaches
// from its cancellation but keeps its values (system bypass, request container, lifecycle
// manager) so persistence runs with the same rights as the call itself. A monitor counts
// toward the global session cap only, never the caller's per-key slots, and stops with the
// call, the binding TTL or server shutdown.
func (r *Relay) startMonitor(ctx context.Context, binding *calls.Binding, ch *biz.Channel, cred biz.RealtimeCredential, p CallParams) {
	release, err := r.acquireSession(0)
	if err != nil {
		log.Warn(ctx, "realtime: call monitor not started, session cap reached or server draining; usage is only recorded if a client sideband attaches",
			log.String("call_id", binding.CallID),
			log.Int("channel_id", ch.ID),
			log.Cause(err),
		)

		return
	}

	wsURL, err := calls.SidebandURL(binding.Variant, binding.Target, binding.CallID)
	if err != nil {
		release()

		log.Warn(ctx, "realtime: call monitor not started", log.String("call_id", binding.CallID), log.Int("channel_id", ch.ID), log.Cause(err))

		return
	}

	base := context.WithoutCancel(ctx)

	accounting := NewAccounting(AccountingParams{
		RequestService:  r.Services.Request,
		UsageLogService: r.Services.Usage,
		QuotaService:    r.Services.Quota,
		ChannelService:  r.Services.Channel,
		AuthService:     r.Services.Auth,
		Channel:         ch,
		APIKey:          p.APIKey,
		ProjectID:       p.ProjectID,
		RequestedModel:  binding.RequestedModel,
		ActualModel:     binding.ActualModel,
		Format:          llm.APIFormatOpenAIRealtime,
		UpstreamKey:     UpstreamKeyForHealth(cred.Bearer, cred.OAuth),
		UpstreamURL:     redactQuery(wsURL),
		Method:          http.MethodGet,
		Path:            p.Request.URL.Path,
		Query:           StoredQuery(p.Request.URL.Query()),
		Headers:         StoredHeaders(p.Request.Header),
		ClientIP:        p.ClientIP,
		UserAgent:       p.UserAgent,
		Kind:            "monitor",
		SessionID:       p.SessionID,
		Dialect:         "ga",
		Stream:          true,
	})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+cred.Bearer)
	headers.Set("User-Agent", userAgent)

	if cred.AccountID != "" {
		headers.Set("Chatgpt-Account-Id", cred.AccountID)
	}

	ch.RealtimeHeaderOverrides(headers)

	m := &monitor{
		relay:    r,
		binding:  binding,
		ch:       ch,
		wsURL:    wsURL,
		headers:  headers,
		hooks:    dedupHooks{Hooks: accounting, binding: binding},
		deadline: time.Now().Add(r.Config.CallBindingTTL),
	}

	go func() {
		defer release()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(base, "panic in realtime call monitor", log.String("call_id", binding.CallID), log.Any("panic", recovered))
			}
		}()

		m.run(base)
	}()
}

// monitor is one gateway-owned read-only sideband on a WebRTC call.
type monitor struct {
	relay    *Relay
	binding  *calls.Binding
	ch       *biz.Channel
	wsURL    string
	headers  http.Header
	hooks    Hooks
	deadline time.Time
	// shutdown fires when the server lifecycle manager shuts down; nil when none is attached.
	shutdown <-chan struct{}

	events, responses int64
}

// run attaches to the call, reads it (reconnecting across drops) until it ends and records
// the summary.
func (m *monitor) run(ctx context.Context) {
	if lc := lifecycle.FromContext(ctx); lc != nil {
		m.shutdown = lc.Context().Done()
	}

	conn, err := m.attach(ctx)
	if err != nil {
		log.Info(ctx, "realtime: call monitor could not attach", log.String("call_id", m.binding.CallID), log.Cause(err))

		return
	}

	started := time.Now()

	var summary EndSummary

	for {
		stop, readErr := m.pump(ctx, conn)
		_ = conn.Close()

		next, end := m.afterDrop(ctx, stop, readErr)
		if next == nil {
			summary = end

			break
		}

		log.Info(ctx, "realtime: call monitor reconnected", log.String("call_id", m.binding.CallID), log.Cause(readErr))

		conn = next
	}

	summary.Events = m.events
	summary.Responses = m.responses
	summary.Duration = time.Since(started)

	endCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()

	m.hooks.OnEnd(endCtx, summary)
}

// attach dials the monitor socket, retrying briefly because a freshly created call may not
// be attachable yet. Shutdown, the deadline or a removed binding end the attempts early.
func (m *monitor) attach(ctx context.Context) (*websocket.Conn, error) {
	for attempt := 0; ; attempt++ {
		conn, err := m.dial(ctx)
		if err == nil {
			return conn, nil
		}

		if attempt >= len(monitorAttachBackoff) {
			return nil, err
		}

		if stop := m.wait(monitorAttachBackoff[attempt]); stop != monitorStopNone {
			return nil, err
		}
	}
}

// dial opens one monitor socket within the connect budget.
func (m *monitor) dial(ctx context.Context) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, m.relay.Config.ConnectBudget)
	defer cancel()

	conn, _, err := DialUpstream(dialCtx, m.ch.HTTPClient, m.wsURL, m.headers, m.relay.Config.HandshakeTimeout)
	if err != nil {
		return nil, err
	}

	conn.SetReadLimit(m.relay.Config.MaxMessageSize)

	return conn, nil
}

// pump reads one monitor socket until it ends, recording response usage, while a pinger
// keeps it alive and closes it on server shutdown or at the deadline. It returns why the
// pinger closed the socket (monitorStopNone when the read itself ended) and the read error.
func (m *monitor) pump(ctx context.Context, conn *websocket.Conn) (monitorStop, error) {
	var reason atomic.Int32

	stop := make(chan struct{})
	defer close(stop)

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(ctx, "panic in realtime monitor pinger", log.String("call_id", m.binding.CallID), log.Any("panic", recovered))
			}
		}()

		ticker := time.NewTicker(m.relay.Config.PingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-stop:
				return
			case <-m.shutdown:
				reason.Store(int32(monitorStopShutdown))
				_ = conn.WriteControl(websocket.CloseMessage, closePayload(websocket.CloseGoingAway, "server restarting"), time.Now().Add(time.Second))
				_ = conn.Close()

				return
			case <-ticker.C:
				if time.Now().After(m.deadline) {
					reason.Store(int32(monitorStopDeadline))
					_ = conn.WriteControl(websocket.CloseMessage, closePayload(websocket.CloseNormalClosure, "call monitor deadline reached"), time.Now().Add(time.Second))
					_ = conn.Close()

					return
				}

				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(m.relay.Config.WriteTimeout))
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(sidebandIdle))

		msgType, data, err := conn.ReadMessage()
		if err != nil {
			return monitorStop(reason.Load()), err
		}

		if msgType != websocket.TextMessage {
			continue
		}

		m.events++

		switch dialect.EventType(data) {
		case "response.created":
			m.responses++
		case "response.done":
			if usage := gjson.GetBytes(data, "response.usage"); usage.Exists() {
				rec := dialect.UsageRecord{
					Kind:       "response",
					ResponseID: gjson.GetBytes(data, "response.id").String(),
					Usage:      dialect.GAUsage(usage),
					Summary:    []byte(gjson.GetBytes(data, "response").Raw),
				}

				usageCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
				m.hooks.OnUsage(usageCtx, rec)
				cancel()
			}
		}
	}
}

// afterDrop decides what happens after the monitor socket ended: it returns a new
// connection to keep reading from, or the summary the monitor finishes with.
//
// Our own stops (shutdown, deadline) and the idle timeout end the monitor as they are. A
// close frame from the upstream normally means the call ended, but the backend may also
// drop just this sideband, so one immediate re-dial tells the two apart: 404/410 confirms
// the call is over and removes the binding. An abnormal drop (1006, network error) or a
// failed re-dial is retried with backoff while the binding exists and the deadline is not
// reached, so the rest of the call stays metered.
func (m *monitor) afterDrop(ctx context.Context, stop monitorStop, readErr error) (*websocket.Conn, EndSummary) {
	if stop != monitorStopNone {
		return nil, stopSummary(stop)
	}

	if isTimeout(readErr) {
		return nil, EndSummary{Cause: CauseIdle}
	}

	closed := EndSummary{Cause: CauseUpstreamClosed}

	code, reason, closeFrame := closeInfo(readErr)
	if closeFrame {
		closed.Code, closed.Reason = itoa(code), reason

		conn, err := m.dial(ctx)
		if err == nil {
			return conn, EndSummary{}
		}

		if callEnded(err) {
			m.relay.Bindings.Delete(m.binding.CallID)

			return nil, closed
		}

		readErr = err
	}

	for _, wait := range monitorReconnectBackoff {
		if stop := m.wait(wait); stop != monitorStopNone {
			if stop == monitorStopCallEnded {
				return nil, closed
			}

			return nil, stopSummary(stop)
		}

		conn, err := m.dial(ctx)
		if err == nil {
			return conn, EndSummary{}
		}

		if callEnded(err) {
			m.relay.Bindings.Delete(m.binding.CallID)

			return nil, closed
		}

		readErr = err
	}

	if closeFrame {
		// The upstream said goodbye properly; failing to verify afterwards is not a failure.
		return nil, closed
	}

	return nil, EndSummary{Cause: CauseUpstreamError, Err: readErr}
}

// wait sleeps for d unless server shutdown, the deadline or the end of the call (binding
// removed) intervene; it reports which one did, or monitorStopNone after a full sleep.
func (m *monitor) wait(d time.Duration) monitorStop {
	if _, ok := m.relay.Bindings.Get(m.binding.CallID); !ok {
		return monitorStopCallEnded
	}

	remaining := time.Until(m.deadline)
	if remaining <= 0 {
		return monitorStopDeadline
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	deadline := time.NewTimer(remaining)
	defer deadline.Stop()

	select {
	case <-m.shutdown:
		return monitorStopShutdown
	case <-deadline.C:
		return monitorStopDeadline
	case <-timer.C:
	}

	if _, ok := m.relay.Bindings.Get(m.binding.CallID); !ok {
		return monitorStopCallEnded
	}

	return monitorStopNone
}

// stopSummary maps a stop reason to the (non-failed) summary cause it ends with.
func stopSummary(stop monitorStop) EndSummary {
	switch stop {
	case monitorStopShutdown:
		return EndSummary{Cause: CauseShutdown}
	case monitorStopDeadline:
		return EndSummary{Cause: CauseMaxDuration}
	default:
		return EndSummary{Cause: CauseUpstreamClosed}
	}
}

// callEnded reports whether a sideband dial was rejected because the call no longer exists.
func callEnded(err error) bool {
	var herr *httpclient.Error

	return errors.As(err, &herr) && (herr.StatusCode == http.StatusNotFound || herr.StatusCode == http.StatusGone)
}
