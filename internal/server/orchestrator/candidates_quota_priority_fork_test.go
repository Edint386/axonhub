// 职责: 守护额度备用渠道的 fork 钩子：备用渠道先按 custom 渠道优先级排序，再按重试次数截断。
// 关联文件:
//   internal/server/orchestrator/candidates.go
//   internal/server/orchestrator/candidates_priority.go
//   internal/server/orchestrator/candidates_quota.go

package orchestrator

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

func TestLoadBalancedSelector_quota_fallback_follows_channel_priority(t *testing.T) {
	high := quotaRoutingCandidate(4, "")
	high.Channel.Priority = 100
	selector, _ := newQuotaRoutingSelector(
		[]*ChannelModelsCandidate{
			quotaRoutingCandidate(1, ""),
			quotaRoutingCandidate(2, ""),
			quotaRoutingCandidate(3, ""),
			high,
		},
		map[int]*biz.QuotaChannelStatus{
			1: quotaRoutingStatus(providerquotastatus.StatusAvailable),
			2: quotaStickyOnlyStatus(),
			3: quotaStickyOnlyStatus(),
			4: quotaStickyOnlyStatus(),
		},
		biz.QuotaRoutingSettings{DefaultMode: objects.QuotaRoutingModeBackpressure},
	)

	got, err := selector.Select(context.Background(), &llm.Request{Model: "model"})

	require.NoError(t, err)
	require.Equal(t, []int{1, 4, 2}, quotaRoutingIDs(got))
}
