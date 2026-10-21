package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// allowedStoredHeaders are the client headers persisted on request rows. Credentials,
// WebSocket handshake headers and device attestation are never stored.
var allowedStoredHeaders = []string{
	"User-Agent", "Origin", "Accept", "Content-Type", "Openai-Beta", "Openai-Alpha",
	"X-Session-Id", "Session-Id", "Thread-Id", "Originator", "X-Request-Id",
	"Ah-Trace-Id", "Ah-Thread-Id", "X-Trace-Id", "X-Thread-Id",
}

// allowedStoredQuery are the query parameters persisted on request rows.
var allowedStoredQuery = []string{"model", "intent", "call_id"}

// StoredHeaders returns the allowlisted subset of client headers.
func StoredHeaders(h http.Header) http.Header {
	out := http.Header{}

	for _, name := range allowedStoredHeaders {
		if values := h.Values(name); len(values) > 0 {
			out[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}

	return out
}

// StoredQuery returns the allowlisted subset of query parameters.
func StoredQuery(q url.Values) url.Values {
	out := url.Values{}

	for _, name := range allowedStoredQuery {
		if values, ok := q[name]; ok {
			out[name] = append([]string(nil), values...)
		}
	}

	return out
}

// AccountingParams wires the persistence services for one relay session.
type AccountingParams struct {
	RequestService  *biz.RequestService
	UsageLogService *biz.UsageLogService
	QuotaService    *biz.QuotaService
	ChannelService  *biz.ChannelService
	AuthService     *biz.AuthService

	Channel   *biz.Channel
	APIKey    *ent.APIKey
	ProjectID int

	// RequestedModel is the client-facing model name; ActualModel the upstream one.
	RequestedModel string
	ActualModel    string
	Format         llm.APIFormat

	// UpstreamKey identifies the channel credential for health records (the bearer value for
	// API keys, objects.OAuthCredentialRef for OAuth channels).
	UpstreamKey string
	UpstreamURL string

	// Request identity persisted on every row.
	Method    string
	Path      string
	Query     url.Values
	Headers   http.Header
	ClientIP  string
	UserAgent string

	// StartedAt is when the upstream dial began; it anchors the performance record so the
	// first-frame latency covers the handshake. Zero means "now".
	StartedAt time.Time

	// Kind is "session", "sideband" or "monitor"; SessionID the relay session id.
	Kind      string
	SessionID string
	Dialect   string
	// Stream marks the performance record as streaming traffic.
	Stream bool
}

// Accounting implements Hooks: one Request/Execution/UsageLog triple per usage unit, a session
// summary row at the end, periodic policy re-checks and channel health records.
type Accounting struct {
	p AccountingParams

	mu             sync.Mutex
	started        time.Time
	firstFrame     *time.Time
	usageRows      int
	promptTokens   int64
	completionToks int64
	lastErr        error
}

// NewAccounting creates the hooks for a session that starts now.
func NewAccounting(p AccountingParams) *Accounting {
	if p.Format == "" {
		p.Format = llm.APIFormatOpenAIRealtime
	}

	if p.Method == "" {
		p.Method = http.MethodGet
	}

	if p.Kind == "" {
		p.Kind = "session"
	}

	started := p.StartedAt
	if started.IsZero() {
		started = time.Now()
	}

	return &Accounting{p: p, started: started}
}

var _ Hooks = (*Accounting)(nil)

func (a *Accounting) baseRequest(body []byte) *httpclient.Request {
	return &httpclient.Request{
		Method:      a.p.Method,
		URL:         a.p.Path,
		Path:        a.p.Path,
		Query:       a.p.Query,
		Headers:     a.p.Headers,
		ClientIP:    a.p.ClientIP,
		UserAgent:   a.p.UserAgent,
		ContentType: "application/json",
		JSONBody:    body,
		RequestType: llm.RequestTypeRealtime.String(),
		APIFormat:   a.p.Format.String(),
	}
}

func (a *Accounting) llmRequest() *llm.Request {
	return &llm.Request{
		Model:       a.p.RequestedModel,
		RequestType: llm.RequestTypeRealtime,
		APIFormat:   a.p.Format,
		Stream:      lo.ToPtr(true),
	}
}

func (a *Accounting) upstreamRequest(body []byte) httpclient.Request {
	return httpclient.Request{
		Method:      a.p.Method,
		URL:         a.p.UpstreamURL,
		ContentType: "application/json",
		JSONBody:    body,
		RequestType: llm.RequestTypeRealtime.String(),
		APIFormat:   a.p.Format.String(),
	}
}

// OnUsage persists one usage unit as a completed request with its execution and usage log.
func (a *Accounting) OnUsage(ctx context.Context, rec dialect.UsageRecord) {
	if a.p.RequestService == nil || a.p.UsageLogService == nil || a.p.Channel == nil {
		return
	}

	ctx = biz.WithModelLibraryPriceMemo(ctx)

	summary := rec.Summary
	if len(summary) == 0 {
		summary = a.recordSummary(rec)
	}

	model := a.p.ActualModel
	if rec.Model != "" {
		model = rec.Model
	}

	usage := rec.Usage
	if usage == nil {
		usage = &llm.Usage{}
	}

	req, err := a.p.RequestService.CreateRequest(ctx, a.llmRequest(), a.baseRequest(summary), a.p.Format)
	if err != nil {
		log.Warn(ctx, "realtime: failed to create usage request row", log.String("session", a.p.SessionID), log.Cause(err))
		return
	}

	if err := a.p.RequestService.UpdateRequestChannelID(ctx, req.ID, a.p.Channel.ID); err != nil {
		log.Debug(ctx, "realtime: failed to set request channel", log.Cause(err))
	}

	exec, err := a.p.RequestService.CreateRequestExecution(ctx, a.p.Channel, model, req, a.upstreamRequest(summary), a.p.Format, false)
	if err != nil {
		log.Warn(ctx, "realtime: failed to create usage execution row", log.String("session", a.p.SessionID), log.Cause(err))
	}

	var apiKeyID *int
	if a.p.APIKey != nil {
		apiKeyID = lo.ToPtr(a.p.APIKey.ID)
	}

	if _, err := a.p.UsageLogService.CreateUsageLog(ctx, biz.CreateUsageLogParams{
		RequestID:     req.ID,
		ProjectID:     a.p.ProjectID,
		ChannelID:     a.p.Channel.ID,
		ActualModelID: model,
		Usage:         usage,
		Source:        usagelog.SourceAPI,
		Format:        a.p.Format.String(),
		APIKeyID:      apiKeyID,
	}); err != nil {
		log.Warn(ctx, "realtime: failed to create usage log", log.String("session", a.p.SessionID), log.Cause(err))
	}

	externalID := rec.ResponseID
	if externalID == "" {
		externalID = rec.ItemID
	}

	if exec != nil {
		if err := a.p.RequestService.UpdateRequestExecutionFinalized(ctx, exec.ID, requestexecution.StatusCompleted, "", externalID, summary, nil, model); err != nil {
			log.Debug(ctx, "realtime: failed to finalize usage execution", log.Cause(err))
		}
	}

	if err := a.p.RequestService.UpdateRequestFinalized(ctx, req.ID, request.StatusCompleted, externalID, summary, nil); err != nil {
		log.Debug(ctx, "realtime: failed to finalize usage request", log.Cause(err))
	}

	a.mu.Lock()
	a.usageRows++
	a.promptTokens += usage.PromptTokens
	a.completionToks += usage.CompletionTokens
	a.mu.Unlock()
}

func (a *Accounting) recordSummary(rec dialect.UsageRecord) []byte {
	payload := map[string]any{"kind": rec.Kind}
	if rec.ResponseID != "" {
		payload["response_id"] = rec.ResponseID
	}

	if rec.ItemID != "" {
		payload["item_id"] = rec.ItemID
	}

	if rec.Model != "" {
		payload["model"] = rec.Model
	}

	if rec.DurationSeconds > 0 {
		payload["seconds"] = rec.DurationSeconds
	}

	data, _ := json.Marshal(payload)

	return data
}

// OnCheck re-validates the API key, the channel and both quotas. The quotas are evaluated
// against the freshly loaded key and channel so that limits changed mid-session apply; only a
// key that is gone or invalid ends the session, transient lookup errors are logged and ignored
// (like the quota checks, which fail open).
func (a *Accounting) OnCheck(ctx context.Context) error {
	ch := a.p.Channel

	if a.p.ChannelService != nil && ch != nil {
		fresh := a.p.ChannelService.GetEnabledChannel(ch.ID)
		if fresh == nil {
			return errors.New("channel disabled")
		}

		ch = fresh
	}

	key := a.p.APIKey

	if key != nil && a.p.AuthService != nil && key.Type != apikey.TypeNoauth {
		fresh, err := a.p.AuthService.AuthenticateAPIKey(ctx, key.Key)

		switch {
		case err == nil && fresh != nil:
			key = fresh
		case ent.IsNotFound(err) || errors.Is(err, biz.ErrInvalidAPIKey):
			return errors.New("api key is no longer valid")
		default:
			log.Warn(ctx, "realtime: api key re-check failed; keeping the session", log.String("session", a.p.SessionID), log.Cause(err))
		}
	}

	if a.p.QuotaService == nil {
		return nil
	}

	if key != nil {
		if profile := key.GetActiveProfile(); profile != nil && profile.Quota != nil {
			result, err := a.p.QuotaService.CheckAPIKeyQuota(ctx, key.ID, profile.Quota)
			if err != nil {
				log.Debug(ctx, "realtime: api key quota check failed", log.Cause(err))
			} else if !result.Allowed {
				return fmt.Errorf("quota exceeded: %s", result.Message)
			}
		}
	}

	if ch != nil && ch.Settings != nil && ch.Settings.Quota != nil {
		result, err := a.p.QuotaService.CheckChannelQuota(ctx, ch.ID, ch.Settings.Quota, ch.Settings.QuotaTimezone)
		if err != nil {
			log.Debug(ctx, "realtime: channel quota check failed", log.Cause(err))
		} else if !result.Allowed {
			return fmt.Errorf("channel quota exceeded: %s", result.Message)
		}
	}

	return nil
}

// OnFirstUpstreamFrame records the time to first frame.
func (a *Accounting) OnFirstUpstreamFrame(time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.firstFrame == nil {
		a.firstFrame = lo.ToPtr(time.Now())
	}
}

// OnEnd writes the session summary row and the channel performance record.
func (a *Accounting) OnEnd(ctx context.Context, summary EndSummary) {
	a.mu.Lock()
	first := a.firstFrame
	rows := a.usageRows
	a.mu.Unlock()

	body, _ := json.Marshal(map[string]any{
		"kind":        a.p.Kind,
		"session_id":  a.p.SessionID,
		"dialect":     a.p.Dialect,
		"model":       a.p.RequestedModel,
		"query":       a.p.Query,
		"cause":       string(summary.Cause),
		"close_code":  summary.Code,
		"events":      summary.Events,
		"responses":   summary.Responses,
		"usage_rows":  rows,
		"duration_ms": summary.Duration.Milliseconds(),
	})

	status := request.StatusCompleted
	execStatus := requestexecution.StatusCompleted
	errMsg := ""

	switch {
	case summary.Failed():
		status = request.StatusFailed
		execStatus = requestexecution.StatusFailed
		errMsg = endSummaryMessage(summary)
	case summary.ClientFault():
		// The client's doing (its upgrade failed or its socket broke): recorded as canceled,
		// neither a failure of the channel nor a completed session.
		status = request.StatusCanceled
		execStatus = requestexecution.StatusCanceled
		errMsg = endSummaryMessage(summary)
	}

	a.writeSummaryRow(ctx, body, status, execStatus, errMsg, summary.Duration, first)

	if a.p.ChannelService != nil && a.p.Channel != nil {
		perf := &biz.PerformanceRecord{
			ChannelID:      a.p.Channel.ID,
			APIKey:         a.p.UpstreamKey,
			Source:         request.SourceAPI,
			StartTime:      a.started,
			FirstTokenTime: first,
			EndTime:        time.Now(),
			Stream:         a.p.Stream,
			// A client fault is neutral for the channel, like a cancelled chat request: neither
			// a success (which would clear auto-disable counters and feed the latency EWMAs)
			// nor a failure (RecordPerformance skips failure accounting for canceled records).
			Success:          !summary.Failed() && !summary.ClientFault(),
			Canceled:         summary.ClientFault(),
			RequestCompleted: true,
			ErrorMessage:     errMsg,
		}

		a.p.ChannelService.AsyncRecordPerformance(ctx, perf)
	}
}

// endSummaryMessage describes why a session ended: the error, else the cause and reason.
func endSummaryMessage(summary EndSummary) string {
	if summary.Err != nil {
		return summary.Err.Error()
	}

	if summary.Reason != "" {
		return fmt.Sprintf("%s: %s", summary.Cause, summary.Reason)
	}

	return string(summary.Cause)
}

func (a *Accounting) writeSummaryRow(ctx context.Context, body []byte, status request.Status, execStatus requestexecution.Status, errMsg string, duration time.Duration, first *time.Time) {
	if a.p.RequestService == nil {
		return
	}

	req, err := a.p.RequestService.CreateRequest(ctx, a.llmRequest(), a.baseRequest(body), a.p.Format)
	if err != nil {
		log.Warn(ctx, "realtime: failed to create session summary row", log.String("session", a.p.SessionID), log.Cause(err))
		return
	}

	metrics := &biz.LatencyMetrics{LatencyMs: lo.ToPtr(duration.Milliseconds())}
	if first != nil {
		metrics.FirstTokenLatencyMs = lo.ToPtr(first.Sub(a.started).Milliseconds())
	}

	if a.p.Channel != nil {
		if err := a.p.RequestService.UpdateRequestChannelID(ctx, req.ID, a.p.Channel.ID); err != nil {
			log.Debug(ctx, "realtime: failed to set summary channel", log.Cause(err))
		}

		exec, err := a.p.RequestService.CreateRequestExecution(ctx, a.p.Channel, a.p.ActualModel, req, a.upstreamRequest(body), a.p.Format, false)
		if err != nil {
			log.Debug(ctx, "realtime: failed to create summary execution", log.Cause(err))
		} else if err := a.p.RequestService.UpdateRequestExecutionFinalized(ctx, exec.ID, execStatus, errMsg, a.p.SessionID, json.RawMessage(body), metrics, a.p.ActualModel); err != nil {
			log.Debug(ctx, "realtime: failed to finalize summary execution", log.Cause(err))
		}
	}

	if err := a.p.RequestService.UpdateRequestFinalized(ctx, req.ID, status, a.p.SessionID, json.RawMessage(body), metrics); err != nil {
		log.Debug(ctx, "realtime: failed to finalize summary request", log.Cause(err))
	}
}

// RecordHandshakeFailure writes a failed request row for a session that never connected.
func (a *Accounting) RecordHandshakeFailure(ctx context.Context, cause error) {
	body, _ := json.Marshal(map[string]any{
		"kind":       a.p.Kind,
		"session_id": a.p.SessionID,
		"model":      a.p.RequestedModel,
		"query":      a.p.Query,
		"error":      cause.Error(),
	})

	a.writeSummaryRow(ctx, body, request.StatusFailed, requestexecution.StatusFailed, cause.Error(), time.Since(a.started), nil)
}

// dialFailureBodyLimit caps the upstream error body appended to the health record's error
// message for keyword rules (the chat path's errorMatchBodyLimit).
const dialFailureBodyLimit = 8 * 1024

// RecordDialFailure feeds a failed upstream handshake or call-create into channel health and,
// through the asynchronous RecordPerformance consumer, into the API-key auto-disable rules
// (the consumer evaluates them for every completed failure, so nothing is evaluated here).
//
// statusCode is the upstream HTTP status of a rejected handshake and 0 when the failure
// happened before any HTTP response; like the chat path's ExtractErrorCode such transport
// failures are recorded as 500, never as the 502/504 the client is answered with, so rules
// aimed at genuine upstream 502/504 answers do not match local network errors.
func RecordDialFailure(ctx context.Context, channels *biz.ChannelService, ch *biz.Channel, upstreamKey string, statusCode int, err error, stream bool) {
	if channels == nil || ch == nil {
		return
	}

	if statusCode == 0 {
		statusCode = http.StatusInternalServerError
	}

	now := time.Now()
	channels.AsyncRecordPerformance(ctx, &biz.PerformanceRecord{
		ChannelID:          ch.ID,
		APIKey:             upstreamKey,
		Source:             request.SourceAPI,
		StartTime:          now,
		EndTime:            now,
		Stream:             stream,
		Success:            false,
		RequestCompleted:   true,
		ResponseStatusCode: statusCode,
		ErrorMessage:       dialFailureMessage(err),
	})
}

// dialFailureMessage builds the error message keyword rules match against, the way the chat
// path does: the error text plus, for a rejected handshake, the upstream body truncated to
// dialFailureBodyLimit.
func dialFailureMessage(err error) string {
	if err == nil {
		return ""
	}

	msg := err.Error()

	var herr *httpclient.Error
	if !errors.As(err, &herr) || len(herr.Body) == 0 {
		return msg
	}

	body := herr.Body
	if len(body) > dialFailureBodyLimit {
		body = body[:dialFailureBodyLimit]
	}

	return msg + "\n" + string(body)
}

// UpstreamKeyForHealth returns the credential identifier used by channel health records.
func UpstreamKeyForHealth(bearer string, oauth bool) string {
	if oauth {
		return objects.OAuthCredentialRef
	}

	return strings.TrimSpace(bearer)
}
