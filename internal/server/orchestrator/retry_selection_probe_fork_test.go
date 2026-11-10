// 职责: 守护重试计数的 fork 钩子：健康探测的同渠道重试和换渠道不计入渠道的真实请求数。
// 关联文件:
//   internal/server/orchestrator/outbound.go
//   internal/server/orchestrator/load_balancer.go
//   internal/server/orchestrator/channel_health_probe_runner.go

package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entrequest "github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestTrackCurrentChannelSelection_SkipsHealthProbeAttempts(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	defer client.Close()
	channelService := newTestChannelService(client)

	attempt := func(ctx context.Context, channelID int) {
		p := &PersistentOutboundTransformer{state: &PersistenceState{
			ChannelService:   channelService,
			CurrentCandidate: &ChannelModelsCandidate{Channel: &biz.Channel{Channel: &ent.Channel{ID: channelID}}},
		}}
		p.trackCurrentChannelSelection(ctx)
	}

	attempt(contexts.WithSource(ctx, entrequest.SourceTest), 1)
	attempt(contexts.WithSource(ctx, entrequest.SourceAPI), 2)
	attempt(ctx, 3)

	for channelID, want := range map[int]int64{1: 0, 2: 1, 3: 1} {
		metrics, err := channelService.GetChannelMetrics(ctx, channelID)
		require.NoError(t, err)
		require.Equal(t, want, metrics.RequestCount, "channel %d", channelID)
	}
}
