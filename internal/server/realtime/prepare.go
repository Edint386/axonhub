package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime/calls"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/internal/server/realtime/dialect/bailian"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
)

// userAgent is sent on upstream dials.
const userAgent = "AxonHub-Realtime/1.0"

var (
	// ErrNoCandidate is returned when every candidate channel was rejected before dialing.
	ErrNoCandidate = errors.New("realtime: no channel accepted the session")
	// ErrChannelBusy is returned when every candidate was rejected by its concurrency or RPM limit.
	ErrChannelBusy = errors.New("realtime: all channels are at their concurrency or rate limit")
	// ErrUnsupportedModel is returned when the selected channel cannot serve the model (e.g. a
	// non-omni model on a Bailian channel).
	ErrUnsupportedModel = errors.New("realtime: model is not supported by the selected channel")
)

// Services groups the business services the relay persists through.
type Services struct {
	Request *biz.RequestService
	Usage   *biz.UsageLogService
	Quota   *biz.QuotaService
	Channel *biz.ChannelService
	Auth    *biz.AuthService
	System  *biz.SystemService
}

// Relay owns the shared state of the realtime feature: config, counters and bindings.
type Relay struct {
	Config       Config
	Orchestrator *orchestrator.ChatCompletionOrchestrator
	Services     Services
	Limits       *SessionLimits
	Bindings     *calls.Bindings

	// live tracks sessions, sidebands and monitors so Drain can wait for them at shutdown.
	live *liveSessions
}

// NewRelay creates the relay state.
func NewRelay(cfg Config, orch *orchestrator.ChatCompletionOrchestrator, services Services) *Relay {
	cfg = cfg.withDefaults()

	return &Relay{
		Config:       cfg,
		Orchestrator: orch,
		Services:     services,
		Limits:       NewSessionLimits(cfg.MaxSessions, cfg.MaxSessionsPerKey),
		Bindings:     calls.NewBindings(cfg.CallBindingTTL, cfg.CallBindingsPerProject),
		live:         newLiveSessions(),
	}
}

// SessionRequest describes an inbound GET /v1/realtime request.
type SessionRequest struct {
	// Model is the requested model (already defaulted for transcription sessions).
	Model string
	// Intent is the raw intent query value ("", "transcription", "quicksilver").
	Intent string
	// Query is the full inbound query (forwarded with model rewritten).
	Query url.Values
	// Headers are the inbound request headers (used for association conditions and stored
	// through the allowlist).
	Headers   http.Header
	Path      string
	ClientIP  string
	UserAgent string
	APIKey    *ent.APIKey
	ProjectID int
	SessionID string
}

// Prepared is a session whose upstream is connected; call Run after the client upgrade or
// Abort when the upgrade failed.
type Prepared struct {
	Session   *Session
	Channel   *biz.Channel
	Target    biz.RealtimeTarget
	Actual    string
	Requested string
	release   func()
}

// Run relays until the session ends and releases every slot afterwards.
func (p *Prepared) Run(client *websocket.Conn) EndSummary {
	defer p.release()

	return p.Session.Run(client)
}

// Abort closes the upstream and releases every slot.
func (p *Prepared) Abort(cause error) {
	defer p.release()

	p.Session.Abort(cause)
}

// PrepareError carries the HTTP status the api layer should answer with and the underlying
// error (an *httpclient.Error for rejected upstream handshakes).
type PrepareError struct {
	Status int
	Err    error
}

func (e *PrepareError) Error() string { return e.Err.Error() }

func (e *PrepareError) Unwrap() error { return e.Err }

// Prepare selects a channel, dials the upstream and builds the session. It must run before
// the client WebSocket is upgraded so that errors can be answered as plain HTTP.
func (r *Relay) Prepare(ctx context.Context, req SessionRequest) (*Prepared, *PrepareError) {
	apiKeyID := 0
	if req.APIKey != nil {
		apiKeyID = req.APIKey.ID
	}

	releaseLimit, err := r.acquireSession(apiKeyID)
	if err != nil {
		return nil, &PrepareError{Status: acquireStatus(err), Err: err}
	}

	prepared, perr := r.prepare(ctx, req)
	if perr != nil {
		releaseLimit()
		return nil, perr
	}

	inner := prepared.release
	prepared.release = func() {
		if inner != nil {
			inner()
		}

		releaseLimit()
	}

	return prepared, nil
}

func (r *Relay) prepare(ctx context.Context, req SessionRequest) (*Prepared, *PrepareError) {
	ctx = orchestrator.PrepareRealtimeContext(ctx)

	budgetCtx, cancel := context.WithTimeout(ctx, r.Config.ConnectBudget)
	defer cancel()

	deadline, _ := budgetCtx.Deadline()

	// budgetSpent reports that the connect budget is gone; nothing may be admitted or dialed
	// once it is, so no RPM unit is burnt on a candidate that can never reach upstream.
	budgetSpent := func() bool {
		return budgetCtx.Err() != nil || time.Until(deadline) <= 0
	}

	selection, err := r.Orchestrator.SelectRealtime(budgetCtx, orchestrator.RealtimeSelectionRequest{
		Model:   req.Model,
		Mode:    biz.RealtimeModeWebSocket,
		Intent:  req.Intent,
		Headers: req.Headers,
	})
	if err != nil {
		return nil, &PrepareError{Status: 0, Err: err}
	}

	if selection == nil || len(selection.Candidates) == 0 {
		return nil, &PrepareError{Status: http.StatusNotFound, Err: fmt.Errorf("%w: %s", biz.ErrInvalidModel, req.Model)}
	}

	var (
		lastErr    error
		lastStatus int
		// tried counts the candidates the loop examined; busy those rejected by their
		// concurrency or RPM limit. When every tried candidate was busy the answer is 429.
		tried int
		busy  int
	)

	for _, candidate := range selection.Candidates {
		if candidate == nil || candidate.Channel == nil {
			continue
		}

		if budgetSpent() {
			// Keep a busy answer when every candidate so far was busy; otherwise the budget
			// itself is the reason the session never connected.
			if lastErr == nil || busy != tried {
				lastErr, lastStatus = ErrDialTimeout, http.StatusGatewayTimeout
			}

			break
		}

		tried++

		ch := candidate.Channel
		actual := orchestrator.RealtimeActualModel(candidate, selection.RequestedModel)

		target, err := ch.RealtimeTarget(biz.RealtimeModeWebSocket)
		if err != nil {
			lastErr, lastStatus = err, http.StatusBadGateway
			continue
		}

		// The selector already drops these; re-checked so a stale candidate cannot be dialed.
		if target.Dialect == biz.RealtimeDialectBailian {
			if req.Intent == "transcription" {
				lastErr, lastStatus = fmt.Errorf("%w: Bailian channels do not support transcription sessions", ErrUnsupportedModel), http.StatusBadRequest
				continue
			}

			if !bailian.Supported(actual) {
				lastErr, lastStatus = fmt.Errorf("%w: %s is not a Qwen-Omni realtime model", ErrUnsupportedModel, actual), http.StatusBadRequest
				continue
			}
		} else if req.Intent == "quicksilver" && target.Dialect != biz.RealtimeDialectGA {
			lastErr, lastStatus = fmt.Errorf("%w: intent=quicksilver requires an OpenAI channel", ErrUnsupportedModel), http.StatusBadRequest
			continue
		}

		cred, err := ch.RealtimeCredential(ctx)
		if err != nil {
			lastErr, lastStatus = err, http.StatusBadGateway
			continue
		}

		// Execution rows read the channel key from the request container (like the chat
		// path's key provider does); record the one this dial uses.
		if !cred.OAuth {
			contexts.WithChannelAPIKey(ctx, cred.Bearer)
		}

		releaseSlot := func() {}

		if mgr := r.Orchestrator.RealtimeLimiter(); mgr != nil {
			if lim := mgr.GetOrCreate(ch); lim != nil {
				if err := lim.Acquire(budgetCtx); err != nil {
					busy++
					lastErr, lastStatus = err, http.StatusTooManyRequests

					continue
				}

				releaseSlot = func() { lim.Release() }
			}
		}

		if err := r.Orchestrator.RealtimeRPMAdmit(ctx, ch); err != nil {
			releaseSlot()

			busy++
			lastErr, lastStatus = err, http.StatusTooManyRequests

			continue
		}

		wsURL := buildSessionURL(target.WSURL, req, actual)
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+cred.Bearer)
		headers.Set("User-Agent", userAgent)

		if cred.AccountID != "" {
			headers.Set("Chatgpt-Account-Id", cred.AccountID)
		}

		ch.RealtimeHeaderOverrides(headers)

		handshake := r.Config.HandshakeTimeout
		if remaining := time.Until(deadline); remaining < handshake {
			handshake = remaining
		}

		if handshake <= 0 {
			releaseSlot()

			lastErr, lastStatus = ErrDialTimeout, http.StatusGatewayTimeout

			break
		}

		upstreamKey := UpstreamKeyForHealth(cred.Bearer, cred.OAuth)

		// The performance record starts here so its first-frame latency covers the handshake.
		dialStart := time.Now()

		conn, _, err := DialUpstream(budgetCtx, ch.HTTPClient, wsURL, headers, handshake)
		if err != nil {
			releaseSlot()

			// status answers the client; upstreamStatus (0 for transport failures) feeds the
			// health record.
			status := http.StatusBadGateway
			upstreamStatus := 0

			var herr *httpclient.Error
			if errors.As(err, &herr) {
				status, upstreamStatus = herr.StatusCode, herr.StatusCode
			} else if errors.Is(err, ErrDialTimeout) {
				status = http.StatusGatewayTimeout
			}

			if target.RecordHealth {
				pctx, pcancel := xcontext.DetachWithTimeout(ctx, persistTimeout)
				RecordDialFailure(pctx, r.Services.Channel, ch, upstreamKey, upstreamStatus, err, true)
				pcancel()
			}

			log.Warn(ctx, "realtime: upstream dial failed",
				log.Int("channel_id", ch.ID),
				log.String("channel", ch.Name),
				log.String("url", redactQuery(wsURL)),
				log.Int("status", status),
				log.Cause(err),
			)

			lastErr, lastStatus = UpstreamFailure(err, status), status

			continue
		}

		ids := &dialect.EventIDs{}

		d, err := r.newDialect(target, selection.RequestedModel, actual, ids, req)
		if err != nil {
			releaseSlot()
			_ = conn.Close()

			lastErr, lastStatus = err, http.StatusBadRequest

			continue
		}

		accounting := NewAccounting(AccountingParams{
			RequestService:  r.Services.Request,
			UsageLogService: r.Services.Usage,
			QuotaService:    r.Services.Quota,
			ChannelService:  r.Services.Channel,
			AuthService:     r.Services.Auth,
			Channel:         ch,
			APIKey:          req.APIKey,
			ProjectID:       req.ProjectID,
			RequestedModel:  selection.RequestedModel,
			ActualModel:     actual,
			Format:          llm.APIFormatOpenAIRealtime,
			UpstreamKey:     upstreamKey,
			UpstreamURL:     redactQuery(wsURL),
			Method:          http.MethodGet,
			Path:            req.Path,
			Query:           StoredQuery(req.Query),
			Headers:         StoredHeaders(req.Headers),
			ClientIP:        req.ClientIP,
			UserAgent:       req.UserAgent,
			StartedAt:       dialStart,
			Kind:            "session",
			SessionID:       req.SessionID,
			Dialect:         d.Name(),
			Stream:          true,
		})

		session := NewSession(ctx, SessionParams{
			Config:   r.Config,
			Dialect:  d,
			Upstream: conn,
			Hooks:    accounting,
			IDs:      ids,
			Name:     req.SessionID,
		})

		log.Info(ctx, "realtime: upstream connected",
			log.String("session", req.SessionID),
			log.Int("channel_id", ch.ID),
			log.String("channel", ch.Name),
			log.String("model", selection.RequestedModel),
			log.String("actual_model", actual),
			log.String("dialect", d.Name()),
		)

		return &Prepared{
			Session:   session,
			Channel:   ch,
			Target:    target,
			Actual:    actual,
			Requested: selection.RequestedModel,
			release:   releaseSlot,
		}, nil
	}

	if lastErr == nil {
		return nil, &PrepareError{Status: http.StatusNotFound, Err: fmt.Errorf("%w: %s", biz.ErrInvalidModel, req.Model)}
	}

	if tried > 0 && busy == tried {
		lastErr = fmt.Errorf("%w: %w", ErrChannelBusy, lastErr)
		lastStatus = http.StatusTooManyRequests
	}

	// Leave a failed row behind so admins can see why the session never started. The client
	// may already have hung up during the connect budget, so persist on a detached context.
	pctx, pcancel := xcontext.DetachWithTimeout(ctx, persistTimeout)
	defer pcancel()

	NewAccounting(AccountingParams{
		RequestService: r.Services.Request,
		APIKey:         req.APIKey,
		ProjectID:      req.ProjectID,
		RequestedModel: selection.RequestedModel,
		ActualModel:    selection.RequestedModel,
		Method:         http.MethodGet,
		Path:           req.Path,
		Query:          StoredQuery(req.Query),
		Headers:        StoredHeaders(req.Headers),
		ClientIP:       req.ClientIP,
		UserAgent:      req.UserAgent,
		Kind:           "session",
		SessionID:      req.SessionID,
	}).RecordHandshakeFailure(pctx, lastErr)

	return nil, &PrepareError{Status: lastStatus, Err: lastErr}
}

// persistTimeout bounds the detached contexts prepare-time persistence runs on.
const persistTimeout = 10 * time.Second

// newDialect picks the translation layer for the target.
func (r *Relay) newDialect(target biz.RealtimeTarget, requested, actual string, ids *dialect.EventIDs, req SessionRequest) (dialect.Dialect, error) {
	switch target.Dialect {
	case biz.RealtimeDialectBailian:
		return bailian.New(bailian.Options{
			RequestedModel:    requested,
			ActualModel:       actual,
			IDs:               ids,
			WantTranscription: false,
		})
	default:
		return dialect.NewGA(dialect.GAOptions{
			RequestedModel:            requested,
			ActualModel:               actual,
			RewriteModel:              req.Intent != "quicksilver",
			IDs:                       ids,
			TranscriptionModelAllowed: transcriptionModelFilter(req.APIKey),
		}), nil
	}
}

// transcriptionModelFilter restricts the transcription models a session may bill
// (session.audio.input.transcription.model) to the API key profile's ModelIDs, the list
// checkApiKeyModelAccess enforces for the session model. It is nil (allow all) when the key
// has no active profile or the profile lists no models.
func transcriptionModelFilter(key *ent.APIKey) func(model string) bool {
	profile := key.GetActiveProfile()
	if profile == nil || len(profile.ModelIDs) == 0 {
		return nil
	}

	allowed := slices.Clone(profile.ModelIDs)

	return func(model string) bool {
		return slices.Contains(allowed, model)
	}
}

// UpstreamFailure marks err as an upstream-origin failure (a rejected handshake, a dial or
// call-create transport error) so the api layer sends it through the admin's upstream error
// policy instead of echoing it. Like the chat outbound transformers, it turns the failure
// into an llm.ResponseError: a rejected handshake gets its status and the error detail parsed
// from the upstream body (the body itself, as the message, when it is not an OpenAI error);
// any other error gets status (502 for network failures, 504 for timeouts) and its text. The
// status survives the policy and the inbound error transformer. Transport errors stay
// reachable through errors.Is; the upstream answer through UpstreamAnswer.
func UpstreamFailure(err error, status int) error {
	if err == nil {
		return nil
	}

	if pipeline.IsUpstreamError(err) {
		return err
	}

	var herr *httpclient.Error
	if errors.As(err, &herr) {
		return pipeline.WrapUpstreamError(&llm.ResponseError{
			StatusCode: herr.StatusCode,
			Cause:      &upstreamAnswer{answer: herr},
			Detail:     upstreamErrorDetail(herr),
		})
	}

	if status == 0 {
		status = http.StatusBadGateway
	}

	return pipeline.WrapUpstreamError(&llm.ResponseError{
		StatusCode: status,
		Cause:      err,
		Detail: llm.ErrorDetail{
			Message: err.Error(),
			Type:    "upstream_error",
		},
	})
}

// upstreamAnswer carries the upstream's rejected-handshake answer behind the ResponseError
// that UpstreamFailure builds. It deliberately has no Unwrap: were the *httpclient.Error
// reachable through the error chain, the inbound error transformer would render its raw body
// even after the upstream error policy masked the message.
type upstreamAnswer struct {
	answer *httpclient.Error
}

func (a *upstreamAnswer) Error() string { return a.answer.Error() }

// UpstreamAnswer returns the upstream HTTP answer behind an error (one marked by
// UpstreamFailure or a raw *httpclient.Error), for the response headers the api layer
// forwards to the client.
func UpstreamAnswer(err error) (*httpclient.Error, bool) {
	var held *upstreamAnswer
	if errors.As(err, &held) {
		return held.answer, true
	}

	var herr *httpclient.Error
	if errors.As(err, &herr) {
		return herr, true
	}

	return nil, false
}

// upstreamErrorDetail reads the OpenAI error object of a rejected handshake body. Any other
// body (text/plain 500s, HTML 403s, JSON of another shape) becomes the message itself.
func upstreamErrorDetail(herr *httpclient.Error) llm.ErrorDetail {
	var parsed struct {
		Error llm.ErrorDetail `json:"error"`
	}

	if json.Valid(herr.Body) && json.Unmarshal(herr.Body, &parsed) == nil && parsed.Error.Message != "" {
		if parsed.Error.Type == "" {
			parsed.Error.Type = "upstream_error"
		}

		return parsed.Error
	}

	message := strings.TrimSpace(string(herr.Body))
	if message == "" {
		message = herr.Status
	}

	if message == "" {
		message = http.StatusText(herr.StatusCode)
	}

	return llm.ErrorDetail{Message: message, Type: "upstream_error"}
}

// IsGatewayError reports whether err was produced by the gateway itself rather than relayed
// from an upstream: session and channel admission limits, model/intent rejections and the
// call_id_not_found answer. The api layer never applies the upstream error policy to these.
func IsGatewayError(err error) bool {
	if err == nil {
		return false
	}

	for _, gateway := range []error{
		ErrTooManySessions, ErrChannelBusy, ErrNoCandidate, ErrUnsupportedModel,
		orchestrator.ErrRealtimeRPMExceeded, orchestrator.ErrLocalRPMExhausted,
	} {
		if errors.Is(err, gateway) {
			return true
		}
	}

	var herr *httpclient.Error
	if errors.As(err, &herr) && herr.StatusCode == http.StatusNotFound && string(herr.Body) == callIDNotFoundBody {
		return true
	}

	return false
}

// buildSessionURL appends the forwarded query to the upstream WebSocket URL: the model is
// rewritten to the upstream name, transcription sessions carry no model, and every other
// parameter (intent, call_id) is forwarded unchanged.
func buildSessionURL(wsURL string, req SessionRequest, actual string) string {
	query := url.Values{}

	for key, values := range req.Query {
		if key == "model" {
			continue
		}

		query[key] = append([]string(nil), values...)
	}

	if req.Intent != "transcription" {
		query.Set("model", actual)
	}

	if encoded := query.Encode(); encoded != "" {
		if strings.Contains(wsURL, "?") {
			return wsURL + "&" + encoded
		}

		return wsURL + "?" + encoded
	}

	return wsURL
}

// redactQuery strips query values that could carry secrets before logging a URL.
func redactQuery(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	q := u.Query()
	for key := range q {
		switch strings.ToLower(key) {
		case "model", "intent", "call_id", "dcid", "architecture":
		default:
			q.Set(key, "***")
		}
	}

	u.RawQuery = q.Encode()

	return u.String()
}
