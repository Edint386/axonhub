package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// RealtimeHandlers serves the OpenAI Realtime API surface: WebSocket sessions, WebRTC call
// signalling and sideband relays. It is custom (fork) code wired by realtime_routes.go.
type RealtimeHandlers struct {
	relay          *realtime.Relay
	orch           *orchestrator.ChatCompletionOrchestrator
	corsEnabled    bool
	allowedOrigins []string
}

// NewRealtimeHandlers builds the handlers. corsEnabled/allowedOrigins mirror server.cors so
// the WebSocket origin policy agrees with the HTTP one.
func NewRealtimeHandlers(relay *realtime.Relay, orch *orchestrator.ChatCompletionOrchestrator, corsEnabled bool, allowedOrigins []string) *RealtimeHandlers {
	return &RealtimeHandlers{relay: relay, orch: orch, corsEnabled: corsEnabled, allowedOrigins: allowedOrigins}
}

// Relay exposes the shared relay state (used by route tests).
func (h *RealtimeHandlers) Relay() *realtime.Relay { return h.relay }

// Session serves GET {prefix}/realtime: a new session (model=), a transcription session
// (intent=transcription) or a sideband attach (call_id=).
func (h *RealtimeHandlers) Session(prefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := websocketHandshakeError(c.Request); err != nil {
			JSONError(c, http.StatusBadRequest, err)
			return
		}

		if middleware.IsRealtimeBetaRequest(c.Request) {
			JSONError(c, http.StatusBadRequest, errors.New("the Realtime API beta protocol (OpenAI-Beta: realtime=v1) was removed by OpenAI on 2026-05-12; remove the header and use the GA protocol"))
			return
		}

		query := c.Request.URL.Query()

		if callID := strings.TrimSpace(query.Get("call_id")); callID != "" {
			h.serveSideband(c, prefix, callID)
			return
		}

		intent := strings.TrimSpace(query.Get("intent"))
		model := strings.TrimSpace(query.Get("model"))

		if intent == "transcription" && model == "" {
			model = h.relay.Config.DefaultTranscriptionModel
		}

		if model == "" {
			JSONError(c, http.StatusBadRequest, errors.New("the model query parameter is required"))
			return
		}

		ctx := c.Request.Context()
		apiKey, _ := contexts.GetAPIKey(ctx)
		projectID, _ := contexts.GetProjectID(ctx)

		if !h.originAllowed(c.Request, apiKey) {
			JSONError(c, http.StatusForbidden, errors.New("origin not allowed"))
			return
		}

		sessionID := realtimeSessionID(c)

		prepared, perr := h.relay.Prepare(ctx, realtime.SessionRequest{
			Model:     model,
			Intent:    intent,
			Query:     query,
			Headers:   c.Request.Header,
			Path:      c.Request.URL.Path,
			ClientIP:  c.ClientIP(),
			UserAgent: c.Request.UserAgent(),
			APIKey:    apiKey,
			ProjectID: projectID,
			SessionID: sessionID,
		})
		if perr != nil {
			h.writePrepareError(c, perr)
			return
		}

		h.runPrepared(c, prepared)
	}
}

// Sideband serves GET {prefix}/live/:call_id (Codex v3 sideband).
func (h *RealtimeHandlers) Sideband(prefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := websocketHandshakeError(c.Request); err != nil {
			JSONError(c, http.StatusBadRequest, err)
			return
		}

		h.serveSideband(c, prefix, strings.TrimSpace(c.Param("call_id")))
	}
}

// websocketHandshakeError validates the client's opening handshake before the relay dials an
// upstream: the request must be a WebSocket upgrade with Sec-WebSocket-Version 13 and a
// Sec-WebSocket-Key that is the base64 encoding of 16 bytes (RFC 6455 §4.1), the same checks
// the upgrader makes later. A malformed handshake would otherwise dial upstream, burn RPM and
// then abort when the upgrade fails. It returns nil for a valid handshake.
func websocketHandshakeError(r *http.Request) error {
	if !websocket.IsWebSocketUpgrade(r) {
		return errors.New("websocket upgrade required")
	}

	if version := r.Header.Get("Sec-WebSocket-Version"); version != "13" {
		return fmt.Errorf("unsupported Sec-WebSocket-Version %q; 13 is required", version)
	}

	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return errors.New("missing Sec-WebSocket-Key")
	}

	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return errors.New("invalid Sec-WebSocket-Key: expected the base64 encoding of 16 bytes")
	}

	return nil
}

func (h *RealtimeHandlers) serveSideband(c *gin.Context, prefix, callID string) {
	ctx := c.Request.Context()
	apiKey, _ := contexts.GetAPIKey(ctx)
	projectID, _ := contexts.GetProjectID(ctx)

	if !h.originAllowed(c.Request, apiKey) {
		JSONError(c, http.StatusForbidden, errors.New("origin not allowed"))
		return
	}

	prepared, perr := h.relay.PrepareSideband(ctx, realtime.SidebandRequest{
		CallID:    callID,
		Prefix:    prefix,
		Query:     c.Request.URL.Query(),
		Headers:   c.Request.Header,
		Path:      c.Request.URL.Path,
		ClientIP:  c.ClientIP(),
		UserAgent: c.Request.UserAgent(),
		APIKey:    apiKey,
		ProjectID: projectID,
		SessionID: realtimeSessionID(c),
	})
	if perr != nil {
		h.writePrepareError(c, perr)
		return
	}

	h.runPrepared(c, prepared)
}

// runPrepared upgrades the client socket and blocks for the whole session.
func (h *RealtimeHandlers) runPrepared(c *gin.Context, prepared *realtime.Prepared) {
	upgrader := websocket.Upgrader{
		Subprotocols: []string{"realtime"},
		CheckOrigin:  func(*http.Request) bool { return true }, // validated by originAllowed
	}

	responseHeaders := c.Writer.Header().Clone()

	conn, err := upgrader.Upgrade(c.Writer, c.Request, responseHeaders)
	if err != nil {
		log.Warn(c.Request.Context(), "realtime: client upgrade failed", log.Cause(err))
		prepared.Abort(err)

		return
	}

	defer func() { _ = conn.Close() }()

	prepared.Run(conn)
}

// CreateCall serves POST {prefix}/live and POST {prefix}/realtime/calls.
func (h *RealtimeHandlers) CreateCall(prefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		apiKey, _ := contexts.GetAPIKey(ctx)
		projectID, _ := contexts.GetProjectID(ctx)

		result, perr := h.relay.CreateCall(ctx, realtime.CallParams{
			Request:   c.Request,
			Prefix:    prefix,
			ClientIP:  c.ClientIP(),
			UserAgent: c.Request.UserAgent(),
			APIKey:    apiKey,
			ProjectID: projectID,
			SessionID: realtimeSessionID(c),
		})
		if perr != nil {
			h.writePrepareError(c, perr)
			return
		}

		for key, values := range result.Header {
			for _, v := range values {
				c.Writer.Header().Add(key, v)
			}
		}

		c.Data(result.Status, result.ContentType, result.Body)
	}
}

// Hangup serves POST {prefix}/realtime/calls/:call_id/hangup.
func (h *RealtimeHandlers) Hangup(prefix string) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		apiKey, _ := contexts.GetAPIKey(ctx)
		projectID, _ := contexts.GetProjectID(ctx)

		result, perr := h.relay.Hangup(ctx, realtime.ControlParams{
			CallID:    strings.TrimSpace(c.Param("call_id")),
			Prefix:    prefix,
			APIKey:    apiKey,
			ProjectID: projectID,
		})
		if perr != nil {
			h.writePrepareError(c, perr)
			return
		}

		for key, values := range result.Header {
			for _, v := range values {
				c.Writer.Header().Add(key, v)
			}
		}

		c.Data(result.Status, result.ContentType, result.Body)
	}
}

// NotSupported answers routes the relay deliberately does not implement.
func (h *RealtimeHandlers) NotSupported(reason string) gin.HandlerFunc {
	return func(c *gin.Context) {
		JSONError(c, http.StatusNotFound, fmt.Errorf("%s is not supported by this gateway: %s", c.Request.URL.Path, reason))
	}
}

// forwardedUpstreamErrorHeaders are copied from a rejected upstream answer onto the client
// response: the retry hint and the upstream request ids clients quote in support requests.
var forwardedUpstreamErrorHeaders = []string{"Retry-After", "X-Request-Id", "Openai-Request-Id"}

// writePrepareError renders a relay error as the same JSON the chat path would produce.
//
// Upstream-origin failures (rejected handshakes, dial and call-create transport errors) go
// through transformOrchestratorError so the admin's upstream error policy (hidden or custom
// message) applies and raw upstream bodies and addresses do not leak; the relay marks them
// with realtime.UpstreamFailure and this function marks any it receives unmarked. Gateway
// errors (limits, parse errors, call_id_not_found) are rendered as they are. Selection errors
// (Status 0) are mapped exactly like chat errors.
func (h *RealtimeHandlers) writePrepareError(c *gin.Context, perr *realtime.PrepareError) {
	ctx := c.Request.Context()
	err := perr.Err

	var httpErr *httpclient.Error
	fromHTTP := errors.As(err, &httpErr)

	if !realtime.IsGatewayError(err) && (fromHTTP || errors.Is(err, realtime.ErrDialNetwork) || errors.Is(err, realtime.ErrDialTimeout)) {
		err = realtime.UpstreamFailure(err, perr.Status)
	}

	if perr.Status == 0 || fromHTTP || pipeline.IsUpstreamError(err) {
		if answer, ok := realtime.UpstreamAnswer(err); ok {
			for _, name := range forwardedUpstreamErrorHeaders {
				if value := answer.Headers.Get(name); value != "" {
					c.Header(name, value)
				}
			}
		}

		httpErr := transformOrchestratorError(ctx, err, h.orch)
		writeErrorBody(c, httpErr.StatusCode, httpErr.Body)

		return
	}

	status := perr.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}

	JSONError(c, status, err)
}

// writeErrorBody renders an error body as JSON. Upstreams answer text/plain 500s and HTML
// 403s too; those bodies cannot be rendered as a raw JSON message (gin would send an empty
// body), so they are wrapped in an OpenAI-shaped error.
func writeErrorBody(c *gin.Context, status int, body []byte) {
	if json.Valid(body) {
		c.JSON(status, json.RawMessage(body))
		return
	}

	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}

	c.JSON(status, objects.ErrorResponse{
		Error: objects.Error{
			Type:    "upstream_error",
			Message: message,
		},
	})
}

// originAllowed implements the WebSocket origin policy: a real API key may connect from any
// origin; the no-auth principal only from no origin, the same origin, or a CORS-allowed one.
func (h *RealtimeHandlers) originAllowed(r *http.Request, apiKey *ent.APIKey) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}

	if apiKey != nil && apiKey.Type != apikey.TypeNoauth {
		return true
	}

	if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Host, r.Host) {
		return true
	}

	if h.corsEnabled {
		for _, allowed := range h.allowedOrigins {
			if allowed == "*" || strings.EqualFold(strings.TrimRight(allowed, "/"), strings.TrimRight(origin, "/")) {
				return true
			}
		}
	}

	return false
}

// realtimeSessionID reuses the trace/session id set by the trace middleware when present.
func realtimeSessionID(c *gin.Context) string {
	if id, ok := shared.GetSessionID(c.Request.Context()); ok && id != "" {
		return id
	}

	return "realtime-" + uuid.NewString()
}
