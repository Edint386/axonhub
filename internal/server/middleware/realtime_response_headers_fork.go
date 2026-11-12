// 职责: 捕获实时语音 HTTP 通话的下游响应头并交给 RequestService 保存。
// 关联文件:
//   internal/server/biz/request_response_headers_fork.go
//   internal/server/realtime_routes.go
// 遵循的 Rules:
//   .agents/rules/go-general.md
// 注意事项:
//   WebSocket 没有逐条响应头，保持跳过，只处理实时语音的 HTTP 调用。

package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

type realtimeResponseHeadersStore interface {
	UpdateRequestResponseHeaders(context.Context, int, http.Header) error
}

// WithRealtimeResponseHeaders captures downstream response headers for realtime HTTP calls.
func WithRealtimeResponseHeaders(store realtimeResponseHeadersStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.IsWebsocket() {
			c.Next()
			return
		}

		var requestID int

		ctx := contexts.WithRequestRecordObserver(c.Request.Context(), func(id int) { requestID = id })
		c.Request = c.Request.WithContext(ctx)
		writer := &realtimeResponseHeadersWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		c.Next()

		if requestID == 0 {
			return
		}

		writer.capture()

		persistCtx, cancel := xcontext.DetachWithTimeout(c.Request.Context(), 10*time.Second)
		defer cancel()

		if err := store.UpdateRequestResponseHeaders(persistCtx, requestID, writer.headers); err != nil {
			log.Warn(persistCtx, "Failed to save downstream response headers", log.Cause(err), log.Int("request_id", requestID))
		}
	}
}

type realtimeResponseHeadersWriter struct {
	gin.ResponseWriter

	headers http.Header
}

// Unwrap preserves http.ResponseController support through this capture wrapper.
func (w *realtimeResponseHeadersWriter) Unwrap() http.ResponseWriter {
	if unwrapper, ok := w.ResponseWriter.(interface{ Unwrap() http.ResponseWriter }); ok {
		return unwrapper.Unwrap()
	}

	return w.ResponseWriter
}

func (w *realtimeResponseHeadersWriter) capture() {
	if w.headers == nil {
		w.headers = w.Header().Clone()
	}
}

func (w *realtimeResponseHeadersWriter) WriteHeaderNow() {
	w.capture()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *realtimeResponseHeadersWriter) Write(body []byte) (int, error) {
	w.capture()
	return w.ResponseWriter.Write(body)
}

func (w *realtimeResponseHeadersWriter) WriteString(body string) (int, error) {
	w.capture()
	return w.ResponseWriter.WriteString(body)
}

func (w *realtimeResponseHeadersWriter) Flush() {
	w.capture()
	w.ResponseWriter.Flush()
}

// FlushError preserves flush errors while still capturing headers before the first flush.
func (w *realtimeResponseHeadersWriter) FlushError() error {
	w.capture()
	w.ResponseWriter.WriteHeaderNow()

	var writer http.ResponseWriter = w.ResponseWriter

	for {
		if flusher, ok := writer.(interface{ FlushError() error }); ok {
			return flusher.FlushError()
		}

		if unwrapper, ok := writer.(interface{ Unwrap() http.ResponseWriter }); ok {
			writer = unwrapper.Unwrap()
			continue
		}

		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
			return nil
		}

		return http.ErrNotSupported
	}
}
