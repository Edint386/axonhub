package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/realtime"
)

// TestWritePrepareError_RecordsCauseForAccessLog guards the access-log contract: a rejected
// realtime request must register its cause on the gin context (the access log prints
// c.Errors) on both rendering paths, so "model not found" is diagnosable without debug logs.
func TestWritePrepareError_RecordsCauseForAccessLog(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name       string
		perr       *realtime.PrepareError
		wantStatus int
	}{
		{
			name:       "selection error rendered through the orchestrator transformer",
			perr:       &realtime.PrepareError{Status: 0, Err: errors.New("model not found: no-such-model")},
			wantStatus: http.StatusInternalServerError, // no orchestrator in this test: generic body
		},
		{
			name:       "gateway error rendered as plain JSON",
			perr:       &realtime.PrepareError{Status: http.StatusTooManyRequests, Err: realtime.ErrTooManySessions},
			wantStatus: http.StatusTooManyRequests,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=no-such-model", nil)

			(&RealtimeHandlers{}).writePrepareError(c, tt.perr)

			require.Equal(t, tt.wantStatus, w.Code)
			require.Len(t, c.Errors, 1)
			require.ErrorIs(t, c.Errors[0].Err, tt.perr.Err)
		})
	}
}
