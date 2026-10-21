package server

import (
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
)

// TestSetupRoutes_RegistersRealtimeRoutes guards the one-line fork hook in routes.go
// (`registerRealtimeRoutes(server, handlers, services)`). Dropping that line still compiles,
// so this test goes through SetupRoutes itself and checks the realtime routes exist.
func TestSetupRoutes_RegistersRealtimeRoutes(t *testing.T) {
	h := newCodexRouteHarness(t)
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: h.db, SystemService: h.system, APIKeyService: h.keys, AllowNoAuth: true})

	engine := gin.New()
	ipCfg, err := middleware.NewIPAccessControlConfig(false, nil, "")
	require.NoError(t, err)

	SetupRoutes(
		&Server{Engine: engine, Config: Config{LLMRequestTimeout: time.Second}},
		Handlers{OpenAI: h.openAI},
		h.db,
		Services{AuthService: auth, SystemService: h.system, RequestService: h.requests},
		ipCfg,
	)

	want := map[string]bool{
		"GET /v1/realtime":                        false,
		"GET /v1/live/:call_id":                   false,
		"POST /v1/live":                           false,
		"POST /v1/realtime/calls":                 false,
		"POST /v1/realtime/calls/:call_id/hangup": false,
	}

	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}

	for key, found := range want {
		require.True(t, found, "route %s is missing: the registerRealtimeRoutes fork hook in routes.go was dropped", key)
	}
}
