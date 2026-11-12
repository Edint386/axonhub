// 职责: 为实时语音的 HTTP 通话记录保存下游响应头。
// 关联文件:
//   internal/server/middleware/realtime_response_headers_fork.go
//   internal/server/realtime_routes.go
// 遵循的 Rules:
//   .agents/rules/biz-services.md
// 注意事项:
//   实时语音 HTTP 调用不经过 orchestrator 的终态更新，需要单独的响应头持久化入口。

package biz

import (
	"context"
	"net/http"
)

// UpdateRequestResponseHeaders persists the downstream response headers of a request.
func (s *RequestService) UpdateRequestResponseHeaders(ctx context.Context, requestID int, headers http.Header) error {
	data, err := s.responseHeadersForStorage(ctx, headers)
	if err != nil || data == nil {
		return err
	}

	return s.saveRequestUpdate(ctx, requestID, s.entFromContext(ctx).Request.Update().SetResponseHeaders(data))
}
