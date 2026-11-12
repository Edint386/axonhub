// 职责: 守护实时语音 HTTP 响应头会被保存到请求记录。
// 关联文件:
//   internal/server/middleware/realtime_response_headers_fork.go
//   internal/server/biz/request_response_headers_fork.go
// 遵循的 Rules:
//   .agents/rules/go-general.md
// 注意事项:
//   WebSocket 请求跳过该中间件，这里只验证普通 HTTP 请求。

package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
)

type realtimeResponseHeadersStoreForTest struct {
	requestID int
	headers   http.Header
}

func (s *realtimeResponseHeadersStoreForTest) UpdateRequestResponseHeaders(_ context.Context, requestID int, headers http.Header) error {
	s.requestID = requestID
	s.headers = headers.Clone()

	return nil
}

func TestWithRealtimeResponseHeadersPersistsAfterRequestCreation(t *testing.T) {
	store := &realtimeResponseHeadersStoreForTest{}
	router := gin.New()
	router.Use(WithRealtimeResponseHeaders(store))
	router.POST("/test", func(c *gin.Context) {
		contexts.NotifyRequestRecord(c.Request.Context(), 42)
		c.Header("X-Request-Header", "captured")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/test", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, 42, store.requestID)
	require.Equal(t, "captured", store.headers.Get("X-Request-Header"))
}
