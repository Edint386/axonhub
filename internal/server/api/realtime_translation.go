package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/realtime"
)

// TranslationSession serves GET {prefix}/realtime/translations?model=: an OpenAI realtime
// translation session over WebSocket (the official SDKs' realtime.translations clients),
// relayed to a Bailian Qwen-LiveTranslate realtime model.
func (h *RealtimeHandlers) TranslationSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := websocketHandshakeError(c.Request); err != nil {
			JSONError(c, http.StatusBadRequest, err)
			return
		}

		if middleware.IsRealtimeBetaRequest(c.Request) {
			JSONError(c, http.StatusBadRequest, errors.New("realtime translation sessions have no beta protocol; remove the OpenAI-Beta: realtime=v1 header"))
			return
		}

		query := c.Request.URL.Query()

		for _, name := range []string{"intent", "call_id"} {
			if query.Has(name) {
				JSONError(c, http.StatusBadRequest, errors.New("the "+name+" query parameter is not supported on realtime translation sessions"))
				return
			}
		}

		model := strings.TrimSpace(query.Get("model"))
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

		prepared, perr := h.relay.Prepare(ctx, realtime.SessionRequest{
			Model:       model,
			Query:       query,
			Headers:     c.Request.Header,
			Path:        c.Request.URL.Path,
			ClientIP:    c.ClientIP(),
			UserAgent:   c.Request.UserAgent(),
			APIKey:      apiKey,
			ProjectID:   projectID,
			SessionID:   realtimeSessionID(c),
			Translation: true,
		})
		if perr != nil {
			h.writePrepareError(c, perr)
			return
		}

		h.runPrepared(c, prepared)
	}
}
