// 职责: 守护别名预览的 fork 钩子：多目标别名要显示每一个目标。
// 关联文件:
//   internal/server/gql/channel_model_preview_fork.go
//   internal/server/gql/model.resolvers.go

package gql

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestPreviewChannelModelEntries_MultiTargetAliasShowsEveryTarget(t *testing.T) {
	resolver := &queryResolver{&Resolver{}}
	entries, err := resolver.PreviewChannelModelEntries(authz.WithTestBypass(t.Context()), ChannelModelPreviewInput{
		SupportedModels: []string{"gpt-4o-mini", "gpt-4.1-mini"},
		Settings: &objects.ChannelSettings{ModelMappings: []objects.ModelMapping{
			{From: "fast", To: "gpt-4o-mini"},
			{From: "fast", To: "gpt-4.1-mini"},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, []*biz.ChannelModelEntry{
		{RequestModel: "fast", ActualModel: "gpt-4o-mini", Source: "mapping"},
		{RequestModel: "fast", ActualModel: "gpt-4.1-mini", Source: "mapping"},
		{RequestModel: "gpt-4.1-mini", ActualModel: "gpt-4.1-mini", Source: "direct"},
		{RequestModel: "gpt-4o-mini", ActualModel: "gpt-4o-mini", Source: "direct"},
	}, entries)
}
