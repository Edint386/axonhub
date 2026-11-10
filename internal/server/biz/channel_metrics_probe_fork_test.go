// 职责: 守护渠道统计恢复的 fork 钩子：重启后从请求记录恢复统计时，不把健康探测算进去。
// 关联文件:
//   internal/server/biz/channel_metrics.go
//   internal/server/biz/channel_health_probe.go

package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
)

func TestChannelMetrics_LoadAllFromExecutions_SkipsHealthProbes(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	now := time.Now().UTC().Truncate(time.Second)

	createRequest := func(source request.Source) int {
		return client.Request.Create().
			SetModelID("gpt-4").
			SetSource(source).
			SetStatus(request.StatusFailed).
			SetStream(false).
			SetRequestBody([]byte("{}")).
			SaveX(ctx).ID
	}
	createExecution := func(requestID, channelID int, status requestexecution.Status) {
		client.RequestExecution.Create().
			SetProjectID(1).
			SetRequestID(requestID).
			SetChannelID(channelID).
			SetModelID("gpt-4").
			SetRequestBody(objects.JSONRawMessage(`{}`)).
			SetStatus(status).
			SetCreatedAt(now.Add(-time.Minute)).
			SetUpdatedAt(now.Add(-time.Minute)).
			SaveX(ctx)
	}

	probe := createRequest(request.SourceTest)
	createExecution(probe, 1, requestexecution.StatusFailed)
	createExecution(probe, 1, requestexecution.StatusFailed)
	createExecution(createRequest(request.SourceAPI), 2, requestexecution.StatusFailed)
	createExecution(9999, 3, requestexecution.StatusCompleted) // request row missing: still counted

	metrics, err := (&ChannelService{}).loadAllChannelMetricsFromExecutions(ctx, client, now.Add(-10*time.Minute))
	require.NoError(t, err)
	require.NotContains(t, metrics, 1)
	require.Equal(t, int64(1), metrics[2].RequestCount)
	require.Equal(t, int64(1), metrics[2].ConsecutiveFailures)
	require.Equal(t, int64(1), metrics[3].RequestCount)
}
