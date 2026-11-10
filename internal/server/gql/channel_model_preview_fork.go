// 职责: 为渠道别名预览列出每个请求模型对应的全部实际模型，和 custom 的多目标别名路由一致。
// 关联文件:
//   internal/server/gql/model.resolvers.go
//   internal/server/gql/channel_model_entries.go
//   internal/server/biz/channel_llm.go
// 遵循的 Rules:
//   .agents/rules/go-general.md
// 注意事项:
//   上游的 GetModelEntries 每个别名只保留第一个目标；custom 路由（7dcf5d13）用 GetModelEntryGroups 使用全部目标，预览必须跟路由一致。

package gql

import "github.com/looplj/axonhub/internal/server/biz"

// previewChannelModelEntries flattens GetModelEntryGroups so a multi-target
// alias lists every target, sorted like the other model entry lists.
func previewChannelModelEntries(ch *biz.Channel) []biz.ChannelModelEntry {
	groups := ch.GetModelEntryGroups()
	entries := make([]biz.ChannelModelEntry, 0, len(groups))
	for _, group := range groups {
		entries = append(entries, group...)
	}
	return sortChannelModelEntries(entries)
}
