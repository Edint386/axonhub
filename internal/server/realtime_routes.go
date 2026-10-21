package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/realtime"
)

// registerRealtimeRoutes mounts the OpenAI Realtime API relay (custom / fork feature).
//
// It is called from SetupRoutes next to registerCodexRoutes. The handler is built here from
// the chat orchestrator and the shared services so that no upstream fx wiring changes; the
// /codex mirror duplicates the Codex middleware list because registerCodexRoutes keeps it in
// a local variable.
func registerRealtimeRoutes(server *Server, handlers Handlers, services Services) {
	if handlers.OpenAI == nil {
		return
	}

	// Any chat-family orchestrator works: realtime only borrows its selection chain, services,
	// limiter and rate-limit tracker.
	var orch *orchestrator.ChatCompletionOrchestrator

	for _, h := range []*api.ChatCompletionHandlers{handlers.OpenAI.ChatCompletionHandlers, handlers.OpenAI.ResponseCompletionHandlers} {
		if h != nil && h.ChatCompletionOrchestrator != nil {
			orch = h.ChatCompletionOrchestrator
			break
		}
	}

	if orch == nil {
		return
	}

	relay := realtime.NewRelay(realtime.LoadConfig(), orch, realtime.Services{
		Request: orch.RequestService,
		Usage:   orch.UsageLogService,
		Quota:   orch.QuotaService,
		Channel: orch.ChannelService,
		Auth:    services.AuthService,
		System:  services.SystemService,
	})

	// Server.Shutdown waits for this relay's live sessions before fx closes the database.
	registerRealtimeDrainer(relay.Drain)

	rt := api.NewRealtimeHandlers(relay, orch, server.Config.CORS.Enabled, server.Config.CORS.AllowedOrigins)

	// /v1 surface: the same middleware stack as the Responses WebSocket route, plus the
	// browser subprotocol lifter in front of the API-key middleware.
	wsMiddlewares := []gin.HandlerFunc{
		middleware.WithIPBlocklist(services.SystemService),
		middleware.WithRealtimeSubprotocolAuth(),
		middleware.WithAPIKeyConfig(services.AuthService, nil),
		middleware.WithSource(request.SourceAPI),
		middleware.WithResponseHeaders(services.RequestService),
		middleware.WithThread(server.Config.Trace, services.ThreadService),
		middleware.WithTrace(server.Config.Trace, services.TraceService),
	}

	httpMiddlewares := append([]gin.HandlerFunc{realtimeBodyLimit(relay.Config.CallBodyLimit)}, wsMiddlewares...)
	httpMiddlewares = append(httpMiddlewares, middleware.WithTimeout(server.Config.LLMRequestTimeout))

	mountRealtime(server, "/v1", rt, wsMiddlewares, httpMiddlewares)

	// /codex mirror for clients whose base_url ends in /codex (gated like the Codex surface).
	if handlers.CodexCompatibility != nil {
		codexWS := []gin.HandlerFunc{
			middleware.WithIPBlocklist(services.SystemService),
			handlers.CodexCompatibility.Enabled,
			middleware.WithRealtimeSubprotocolAuth(),
			middleware.WithStrictAPIKeyConfig(services.AuthService, nil),
			middleware.WithSource(request.SourceAPI),
			middleware.WithResponseHeaders(services.RequestService),
			middleware.WithThread(server.Config.Trace, services.ThreadService),
			middleware.WithTrace(server.Config.Trace, services.TraceService),
		}

		codexHTTP := append([]gin.HandlerFunc{realtimeBodyLimit(relay.Config.CallBodyLimit)}, codexWS...)
		codexHTTP = append(codexHTTP, middleware.WithTimeout(server.Config.LLMRequestTimeout))

		mountRealtime(server, "/codex", rt, codexWS, codexHTTP)
	}
}

func mountRealtime(server *Server, prefix string, rt *api.RealtimeHandlers, wsMiddlewares, httpMiddlewares []gin.HandlerFunc) {
	ws := server.Group(prefix, wsMiddlewares...)
	ws.GET("/realtime", rt.Session(prefix))
	ws.GET("/live/:call_id", rt.Sideband(prefix))
	ws.GET("/live", rt.NotSupported("the GPT-Live websocket transport requires an OpenAI API key on the client; use the WebRTC transport"))

	h := server.Group(prefix, httpMiddlewares...)
	h.POST("/live", rt.CreateCall(prefix))
	h.POST("/realtime/calls", rt.CreateCall(prefix))
	h.POST("/realtime/calls/:call_id/hangup", rt.Hangup(prefix))
	h.POST("/realtime/calls/:call_id/accept", rt.NotSupported("SIP calls are not created through the gateway"))
	h.POST("/realtime/calls/:call_id/reject", rt.NotSupported("SIP calls are not created through the gateway"))
	h.POST("/realtime/calls/:call_id/refer", rt.NotSupported("SIP calls are not created through the gateway"))
	h.POST("/realtime/client_secrets", rt.NotSupported("ephemeral client secrets would bypass gateway metering"))
	h.POST("/realtime/sessions", rt.NotSupported("the beta sessions endpoint was removed by OpenAI"))
	h.POST("/realtime/transcription_sessions", rt.NotSupported("the beta sessions endpoint was removed by OpenAI"))
}

// realtimeBodyLimit caps call-create bodies before any middleware reads them.
func realtimeBodyLimit(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil && limit > 0 {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}

		c.Next()
	}
}
