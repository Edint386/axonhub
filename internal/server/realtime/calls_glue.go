package realtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime/calls"
	"github.com/looplj/axonhub/internal/server/realtime/dialect"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

// recordTimeout bounds the detached persistence of a created call.
const recordTimeout = 10 * time.Second

// callIDNotFoundBody is the OpenAI-shaped error for unknown or foreign call ids.
const callIDNotFoundBody = `{"error":{"message":"No session found for the provided call_id","type":"invalid_request_error","code":"call_id_not_found","param":""}}`

// CallParams describes an inbound call-create request.
type CallParams struct {
	Request   *http.Request
	Prefix    string
	ClientIP  string
	UserAgent string
	APIKey    *ent.APIKey
	ProjectID int
	SessionID string
}

// CallResult is the HTTP answer for the client.
type CallResult struct {
	Status      int
	ContentType string
	Header      http.Header
	Body        []byte
}

// ControlParams describes a call control request (hangup).
type ControlParams struct {
	CallID    string
	Prefix    string
	APIKey    *ent.APIKey
	ProjectID int
}

func notFoundCall(callID string) *PrepareError {
	return &PrepareError{Status: http.StatusNotFound, Err: &httpclient.Error{
		Method:     http.MethodGet,
		URL:        callID,
		StatusCode: http.StatusNotFound,
		Status:     http.StatusText(http.StatusNotFound),
		Body:       []byte(callIDNotFoundBody),
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
	}}
}

// CreateCall forwards a WebRTC SDP exchange (POST /v1/live, POST /v1/realtime/calls) to the
// selected channel and remembers the resulting call so sidebands and hangups can find it.
func (r *Relay) CreateCall(ctx context.Context, p CallParams) (*CallResult, *PrepareError) {
	ctx = orchestrator.PrepareRealtimeContext(ctx)

	inbound, err := calls.ParseRequest(p.Request, p.Prefix, r.Config.CallBodyLimit)
	if err != nil {
		status := calls.StatusCode(err)
		if status == 0 {
			status = http.StatusBadRequest
		}

		return nil, &PrepareError{Status: status, Err: err}
	}

	selection, err := r.Orchestrator.SelectRealtimeCandidates(ctx, inbound.Model, biz.RealtimeModeWebRTC, p.Request.Header)
	if err != nil {
		r.recordCallRejected(ctx, p, inbound.Model, err)

		return nil, &PrepareError{Status: 0, Err: err}
	}

	if selection == nil || len(selection.Candidates) == 0 {
		err := fmt.Errorf("%w: %s", biz.ErrInvalidModel, inbound.Model)
		r.recordCallRejected(ctx, p, inbound.Model, err)

		return nil, &PrepareError{Status: http.StatusNotFound, Err: err}
	}

	var (
		lastErr    error
		lastStatus int
	)

	for _, candidate := range selection.Candidates {
		if candidate == nil || candidate.Channel == nil {
			continue
		}

		ch := candidate.Channel
		actual := orchestrator.RealtimeActualModel(candidate, selection.RequestedModel)

		target, err := ch.RealtimeTarget(biz.RealtimeModeWebRTC)
		if err != nil {
			lastErr, lastStatus = err, http.StatusBadGateway
			continue
		}

		if target.Dialect == biz.RealtimeDialectBailian {
			lastErr, lastStatus = fmt.Errorf("%w: Bailian channels only support WebSocket sessions", ErrUnsupportedModel), http.StatusBadRequest
			continue
		}

		cred, err := ch.RealtimeCredential(ctx)
		if err != nil {
			lastErr, lastStatus = err, http.StatusBadGateway
			continue
		}

		if err := r.Orchestrator.RealtimeRPMAdmit(ctx, ch); err != nil {
			lastErr, lastStatus = err, http.StatusTooManyRequests
			continue
		}

		ctarget := calls.Target{APIBase: target.APIBase, Codex: target.Codex, Backend: target.Backend, LiveBase: target.LiveBase, OAuth: cred.OAuth}
		opts := r.encodeOptions(ctx, ch, actual)

		upstreamReq, err := calls.EncodeUpstream(inbound, p.Request.Header, p.Request.URL.Query(), ctarget, calls.Credential{Bearer: cred.Bearer, AccountID: cred.AccountID}, opts)
		if err != nil {
			lastErr, lastStatus = err, http.StatusBadRequest
			continue
		}

		// Same channel header overrides as the sideband and monitor dials, so one call is
		// created and attached under one upstream identity.
		ch.RealtimeHeaderOverrides(upstreamReq.Header)

		upstreamKey := UpstreamKeyForHealth(cred.Bearer, cred.OAuth)

		resp, err := r.forward(ctx, ch, upstreamReq)
		if err != nil {
			status := forwardStatus(err)

			if target.RecordHealth {
				// Health records carry the upstream's own status (0 for transport failures),
				// not the client-facing 502/504, so auto-disable rules match what upstream said.
				healthStatus := 0

				if herr, ok := errors.AsType[*httpclient.Error](err); ok {
					healthStatus = herr.StatusCode
				}

				healthCtx, cancel := xcontext.DetachWithTimeout(ctx, recordTimeout)
				RecordDialFailure(healthCtx, r.Services.Channel, ch, upstreamKey, healthStatus, err, false)
				cancel()
			}

			log.Warn(ctx, "realtime: call create failed",
				log.Int("channel_id", ch.ID),
				log.String("channel", ch.Name),
				log.String("variant", string(inbound.Variant)),
				log.Int("status", status),
				log.Cause(err),
			)

			lastErr, lastStatus = err, status

			continue
		}

		upstreamURL, _ := url.Parse(upstreamReq.URL)

		callID, ok := calls.ParseCallID(resp.Headers.Get("Location"), upstreamURL)
		if !ok {
			// The upstream call exists but cannot be addressed, so neither a hangup nor a
			// retry on the next candidate is possible: a retry would create a second live
			// call for the same offer. Fail the request here.
			log.Warn(ctx, "realtime: call create succeeded without a parseable Location", log.Int("channel_id", ch.ID), log.String("location", resp.Headers.Get("Location")))

			return nil, &PrepareError{Status: http.StatusBadGateway, Err: errors.New("upstream call create returned no call id")}
		}

		apiKeyID := 0
		if p.APIKey != nil {
			apiKeyID = p.APIKey.ID
		}

		binding := &calls.Binding{
			CallID:                callID,
			ChannelID:             ch.ID,
			ActualModel:           actual,
			RequestedModel:        selection.RequestedModel,
			Variant:               inbound.Variant,
			ProjectID:             p.ProjectID,
			APIKeyID:              apiKeyID,
			CredentialFingerprint: cred.Fingerprint,
			Target:                ctarget,
			CreatedAt:             time.Now(),
		}
		r.Bindings.Put(binding)

		// The call row must survive a client that hangs up (cancels the request) right after
		// the SDP answer; persist on a detached context.
		recordCtx, cancelRecord := xcontext.DetachWithTimeout(ctx, recordTimeout)
		r.recordCall(recordCtx, p, ch, selection.RequestedModel, actual, upstreamKey, inbound, callID, redactQuery(upstreamReq.URL))
		cancelRecord()

		if r.Config.MonitorCalls && inbound.Variant == calls.VariantOpenAI && !target.Codex {
			r.startMonitor(ctx, binding, ch, cred, p)
		}

		header := calls.ForwardedResponseHeaders(resp.Headers)
		header.Set("Location", calls.RewriteLocation(p.Prefix, inbound.Variant, callID))
		header.Set("Access-Control-Expose-Headers", "Location")

		contentType := resp.Headers.Get("Content-Type")
		if contentType == "" {
			contentType = "application/sdp"
		}

		header.Del("Content-Type")

		log.Info(ctx, "realtime: call created",
			log.String("call_id", callID),
			log.Int("channel_id", ch.ID),
			log.String("variant", string(inbound.Variant)),
			log.String("model", selection.RequestedModel),
		)

		return &CallResult{Status: resp.StatusCode, ContentType: contentType, Header: header, Body: resp.Body}, nil
	}

	if lastErr == nil {
		lastErr, lastStatus = fmt.Errorf("%w: %s", biz.ErrInvalidModel, inbound.Model), http.StatusNotFound
	}

	r.recordCallRejected(ctx, p, selection.RequestedModel, lastErr)

	return nil, &PrepareError{Status: lastStatus, Err: lastErr}
}

// recordCallRejected leaves a failed request row behind for a call that was never created,
// like recordRejected does for sessions.
func (r *Relay) recordCallRejected(ctx context.Context, p CallParams, requestedModel string, cause error) {
	pctx, pcancel := xcontext.DetachWithTimeout(ctx, recordTimeout)
	defer pcancel()

	NewAccounting(AccountingParams{
		RequestService: r.Services.Request,
		APIKey:         p.APIKey,
		ProjectID:      p.ProjectID,
		RequestedModel: requestedModel,
		ActualModel:    requestedModel,
		Method:         http.MethodPost,
		Path:           p.Request.URL.Path,
		Query:          StoredQuery(p.Request.URL.Query()),
		Headers:        StoredHeaders(p.Request.Header),
		ClientIP:       p.ClientIP,
		UserAgent:      p.UserAgent,
		Kind:           "call",
		SessionID:      p.SessionID,
	}).RecordHandshakeFailure(pctx, cause)
}

// encodeOptions resolves the User-Agent policy for a channel.
func (r *Relay) encodeOptions(ctx context.Context, ch *biz.Channel, actual string) calls.EncodeOptions {
	pass := false

	if r.Services.System != nil {
		if v, err := r.Services.System.UserAgentPassThrough(ctx); err == nil {
			pass = v
		}
	}

	if ch.Settings != nil && ch.Settings.PassThroughUserAgent != nil {
		pass = *ch.Settings.PassThroughUserAgent
	}

	ua := userAgent
	if ch.Type == "codex" || ch.Type == "fenno" {
		// The Codex CLI fingerprint the Codex transformer presents upstream.
		ua = codex.DefaultUserAgent()
	}

	return calls.EncodeOptions{
		ActualModel:          actual,
		ForwardAttestation:   r.Config.ForwardAttestation,
		UserAgent:            ua,
		UserAgentPassThrough: pass,
	}
}

// forwardStatus maps a forward() error to the status answered to the client: the upstream
// status for rejected requests, 504 for deadlines and network timeouts, 502 otherwise.
func forwardStatus(err error) int {
	if herr, ok := errors.AsType[*httpclient.Error](err); ok {
		return herr.StatusCode
	}

	if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		return http.StatusGatewayTimeout
	}

	return http.StatusBadGateway
}

// forward executes an upstream HTTP request through the channel client without following
// redirects. Non-2xx responses come back as *httpclient.Error.
func (r *Relay) forward(ctx context.Context, ch *biz.Channel, req *calls.UpstreamRequest) (*httpclient.Response, error) {
	hc := ch.HTTPClient
	if hc == nil {
		hc = httpclient.NewHttpClient()
	}

	hc = hc.WithRejectRedirects()

	return hc.Do(ctx, &httpclient.Request{
		Method:      req.Method,
		URL:         req.URL,
		Headers:     req.Header,
		ContentType: req.Header.Get("Content-Type"),
		Body:        req.Body,
		RequestType: llm.RequestTypeRealtime.String(),
		APIFormat:   llm.APIFormatOpenAIRealtime.String(),
	})
}

// recordCall persists the call as one completed request with a zero-token usage log so it
// counts toward request quotas.
func (r *Relay) recordCall(ctx context.Context, p CallParams, ch *biz.Channel, requested, actual, upstreamKey string, inbound *calls.Request, callID, upstreamURL string) {
	summary, _ := json.Marshal(map[string]any{
		"kind":      "call",
		"call_id":   callID,
		"variant":   string(inbound.Variant),
		"model":     requested,
		"session":   json.RawMessage(nonEmptyJSON(inbound.Session)),
		"sdp_bytes": len(inbound.SDP),
	})

	acc := NewAccounting(AccountingParams{
		RequestService:  r.Services.Request,
		UsageLogService: r.Services.Usage,
		QuotaService:    r.Services.Quota,
		ChannelService:  r.Services.Channel,
		AuthService:     r.Services.Auth,
		Channel:         ch,
		APIKey:          p.APIKey,
		ProjectID:       p.ProjectID,
		RequestedModel:  requested,
		ActualModel:     actual,
		UpstreamKey:     upstreamKey,
		UpstreamURL:     upstreamURL,
		Method:          http.MethodPost,
		Path:            p.Request.URL.Path,
		Query:           StoredQuery(p.Request.URL.Query()),
		Headers:         StoredHeaders(p.Request.Header),
		ClientIP:        p.ClientIP,
		UserAgent:       p.UserAgent,
		Kind:            "call",
		SessionID:       p.SessionID,
		Dialect:         "ga",
	})

	acc.OnUsage(ctx, dialect.UsageRecord{Kind: "call", ResponseID: callID, Usage: &llm.Usage{}, Summary: summary})
}

func nonEmptyJSON(raw json.RawMessage) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("null")
	}

	return raw
}

// Hangup forwards POST /v1/realtime/calls/{id}/hangup for a call the caller created.
func (r *Relay) Hangup(ctx context.Context, p ControlParams) (*CallResult, *PrepareError) {
	ctx = orchestrator.PrepareRealtimeContext(ctx)

	binding, ch, cred, perr := r.lookupBinding(ctx, p.CallID, p.APIKey, p.ProjectID)
	if perr != nil {
		return nil, perr
	}

	if binding.Variant != calls.VariantOpenAI || binding.Target.Codex {
		return nil, &PrepareError{Status: http.StatusNotFound, Err: errors.New("hangup is only available for calls created through /realtime/calls on an OpenAI channel; Codex ends its calls through the sideband")}
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+cred.Bearer)
	header.Set("Accept", "application/json")
	header.Set("User-Agent", userAgent)
	ch.RealtimeHeaderOverrides(header)

	resp, err := r.forward(ctx, ch, &calls.UpstreamRequest{
		Method: http.MethodPost,
		URL:    strings.TrimRight(binding.Target.APIBase, "/") + "/realtime/calls/" + url.PathEscape(binding.CallID) + "/hangup",
		Header: header,
	})
	if err != nil {
		return nil, &PrepareError{Status: forwardStatus(err), Err: err}
	}

	r.Bindings.Delete(binding.CallID)

	contentType := resp.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}

	return &CallResult{Status: resp.StatusCode, ContentType: contentType, Header: calls.ForwardedResponseHeaders(resp.Headers), Body: resp.Body}, nil
}

// lookupBinding resolves a call the caller owns together with its channel and pinned credential.
func (r *Relay) lookupBinding(ctx context.Context, callID string, apiKey *ent.APIKey, projectID int) (*calls.Binding, *biz.Channel, biz.RealtimeCredential, *PrepareError) {
	binding, ok := r.Bindings.Get(callID)
	if !ok || binding.ProjectID != projectID {
		return nil, nil, biz.RealtimeCredential{}, notFoundCall(callID)
	}

	apiKeyID := 0
	if apiKey != nil {
		apiKeyID = apiKey.ID
	}

	if binding.APIKeyID != apiKeyID {
		return nil, nil, biz.RealtimeCredential{}, notFoundCall(callID)
	}

	ch := r.Services.Channel.GetEnabledChannel(binding.ChannelID)
	if ch == nil {
		return nil, nil, biz.RealtimeCredential{}, notFoundCall(callID)
	}

	cred, err := ch.RealtimeCredentialByFingerprint(ctx, binding.CredentialFingerprint)
	if err != nil {
		log.Debug(ctx, "realtime: pinned call credential unavailable", log.String("call_id", callID), log.Cause(err))

		return nil, nil, biz.RealtimeCredential{}, notFoundCall(callID)
	}

	return binding, ch, cred, nil
}
