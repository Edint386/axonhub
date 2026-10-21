package realtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime/calls"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// SidebandRequest describes an inbound sideband attach (GET /v1/live/{id},
// GET /v1/realtime?call_id=).
type SidebandRequest struct {
	CallID    string
	Prefix    string
	Query     url.Values
	Headers   http.Header
	Path      string
	ClientIP  string
	UserAgent string
	APIKey    *ent.APIKey
	ProjectID int
	SessionID string
}

// dedupHooks drops response usage the call already recorded (monitor + sideband).
type dedupHooks struct {
	Hooks

	binding *calls.Binding
}

func (d dedupHooks) OnUsage(ctx context.Context, rec dialect.UsageRecord) {
	if rec.Kind == "response" && rec.ResponseID != "" && !d.binding.MarkResponseSeen(rec.ResponseID) {
		return
	}

	d.Hooks.OnUsage(ctx, rec)
}

// PrepareSideband dials the upstream sideband of a call the caller created and returns a
// passthrough session.
func (r *Relay) PrepareSideband(ctx context.Context, req SidebandRequest) (*Prepared, *PrepareError) {
	ctx = orchestrator.PrepareRealtimeContext(ctx)

	binding, ch, cred, perr := r.lookupBinding(ctx, req.CallID, req.APIKey, req.ProjectID)
	if perr != nil {
		return nil, perr
	}

	apiKeyID := 0
	if req.APIKey != nil {
		apiKeyID = req.APIKey.ID
	}

	releaseLimit, err := r.acquireSession(apiKeyID)
	if err != nil {
		return nil, &PrepareError{Status: acquireStatus(err), Err: err}
	}

	wsURL, err := calls.SidebandURL(binding.Variant, binding.Target, binding.CallID)
	if err != nil {
		releaseLimit()
		return nil, &PrepareError{Status: http.StatusBadGateway, Err: err}
	}

	headers := calls.SidebandHeaders(req.Headers, calls.Credential{Bearer: cred.Bearer, AccountID: cred.AccountID}, r.encodeOptions(ctx, ch, binding.ActualModel))
	ch.RealtimeHeaderOverrides(headers)

	budgetCtx, cancel := context.WithTimeout(ctx, r.Config.ConnectBudget)
	defer cancel()

	conn, _, err := DialUpstream(budgetCtx, ch.HTTPClient, wsURL, headers, r.Config.HandshakeTimeout)
	if err != nil {
		releaseLimit()

		status := http.StatusBadGateway

		if herr, ok := errors.AsType[*httpclient.Error](err); ok {
			status = herr.StatusCode
		} else if errors.Is(err, ErrDialTimeout) {
			status = http.StatusGatewayTimeout
		}

		log.Warn(ctx, "realtime: sideband dial failed",
			log.String("call_id", binding.CallID),
			log.Int("channel_id", ch.ID),
			log.String("url", redactQuery(wsURL)),
			log.Int("status", status),
			log.Cause(err),
		)

		return nil, &PrepareError{Status: status, Err: err}
	}

	ids := &dialect.EventIDs{}
	d := dialect.NewGA(dialect.GAOptions{
		RequestedModel: binding.RequestedModel,
		ActualModel:    binding.ActualModel,
		// Codex sideband variants are relayed verbatim; the OpenAI flow keeps the model
		// mapping hidden like a WebSocket session does.
		RewriteModel: binding.Variant == calls.VariantOpenAI,
		IDs:          ids,
	})

	accounting := NewAccounting(AccountingParams{
		RequestService:  r.Services.Request,
		UsageLogService: r.Services.Usage,
		QuotaService:    r.Services.Quota,
		ChannelService:  r.Services.Channel,
		AuthService:     r.Services.Auth,
		Channel:         ch,
		APIKey:          req.APIKey,
		ProjectID:       req.ProjectID,
		RequestedModel:  binding.RequestedModel,
		ActualModel:     binding.ActualModel,
		Format:          llm.APIFormatOpenAIRealtime,
		UpstreamKey:     UpstreamKeyForHealth(cred.Bearer, cred.OAuth),
		UpstreamURL:     redactQuery(wsURL),
		Method:          http.MethodGet,
		Path:            req.Path,
		Query:           StoredQuery(req.Query),
		Headers:         StoredHeaders(req.Headers),
		ClientIP:        req.ClientIP,
		UserAgent:       req.UserAgent,
		Kind:            "sideband",
		SessionID:       req.SessionID,
		Dialect:         d.Name(),
		Stream:          true,
	})

	session := NewSession(ctx, SessionParams{
		Config:      r.Config,
		Dialect:     d,
		Upstream:    conn,
		Hooks:       dedupHooks{Hooks: accounting, binding: binding},
		IDs:         ids,
		Name:        req.SessionID,
		MaxDuration: r.Config.SessionDuration("ga"),
		Sideband:    true,
	})

	log.Info(ctx, "realtime: sideband connected",
		log.String("call_id", binding.CallID),
		log.Int("channel_id", ch.ID),
		log.String("variant", string(binding.Variant)),
	)

	return &Prepared{
		Session:   session,
		Channel:   ch,
		Actual:    binding.ActualModel,
		Requested: binding.RequestedModel,
		release:   releaseLimit,
	}, nil
}

// sidebandIdle is the idle timeout used by call monitors (calls rarely exceed an hour).
const sidebandIdle = 15 * time.Minute
