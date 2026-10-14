# Custom 分支规则（Edint386/axonhub `custom`）

本仓库的 `custom` 分支是在上游 [looplj/axonhub](https://github.com/looplj/axonhub) `unstable` 之上维护的定制分支。凡是同步上游、修改代码、提交或推送，都必须遵守本文件。本文件是 custom 独有的，上游没有，同步时不会冲突。

## 1. 仓库与分支

- `origin` = `Edint386/axonhub`（自己的仓库）；`upstream` = `looplj/axonhub`（只读）。
- 只往 `origin` 推送 `custom` 分支，并且只做快进推送（fast-forward）。**绝不推送到 upstream，也不在上游开 PR、评论或做任何改动。**
- 提交（commit）和推送（push）都必须等仓库所有者明确要求后才做。
- 默认不建备份分支（所有者在 2026-10-14 明确不需要），除非被要求。

## 2. 四条合并规则

1. **custom 没改过的逻辑采用上游。**
2. **custom 明确增加的功能重新接回。**
3. **生成文件跟随源定义重新生成**，不手工合并。
4. **不因为主观判断上游"不合理"而额外修改上游。** 只有客观故障才允许覆盖上游，并且必须留注释标记和守护测试（见第 5 节）。

另外：**所有未提交内容都需要逐条复核**之后才能提交。

## 3. 同步上游的流程

1. **只读预检**（合并前）：
   - `git fetch upstream unstable`，按提交标题对比找出 custom 缺少的上游提交。custom 用 cherry-pick 同步，`git rev-list` 的"落后数"会虚高，以标题对比为准。
   - 在临时 worktree 里按上游顺序逐个模拟 cherry-pick，记录冲突。
   - 检查规则 1：custom 没改过的文件，合并后必须与上游完全一致。
   - 检查规则 2：重叠文件里 custom 的改动（相对上游的 diff）合并前后保持不变。
   - 在模拟结果上跑测试，然后向所有者汇报冲突的来龙去脉和推荐方案。
2. **合并**：按上游顺序逐个 cherry-pick，保留上游原作者、原时间和原提交信息，末尾加 `Co-Authored-By` 署名行。
   - 冲突多数是"两边在同一处各自新增"，两边都保留。
   - 必须同时保住第 5 节列出的 custom 钩子。
3. **重新生成**（规则 3）：
   - GraphQL / ent：`cd internal/server/gql && go generate`。之后删除 gqlgen 留在 `*.resolvers.go` 末尾的 `!!! WARNING !!!` 注释块；`TestNoUnimplementedResolverStubs` 会拦截残留的 `not implemented` 占位函数（上游自带的 `Nodes` 除外）。
   - 前端路由树 `frontend/src/routeTree.gen.ts` 用 `@tanstack/router-plugin` 的生成器重新生成，不手改。
4. **验证**：
   - 后端：`go test ./...`；`llm/` 是独立 Go 模块，要在 `llm/` 目录里跑 `go test ./...`。
   - 前端单元测试：`cd frontend && node --test "src/**/*.test.mjs"`。
   - 前端类型检查必须用 `./node_modules/.bin/tsc -p tsconfig.app.json --noEmit --pretty false`。根目录的 `tsconfig.json` 是 `"files": []`，直接跑什么都不检查。上游本身就有一批类型错误，所以判断标准是"这次合并有没有新增错误"：把错误去掉行号列号后，和合并前的 custom 逐条（按多重集合）对比。不要拿 custom 和上游按原文对比：custom 的渠道表单多一个 `priority` 字段，上游已有的同一个错误在 custom 里文字会不一样（比如 "21 more" 对 "20 more"），会被误判成 custom 独有。已知的老错误：`quota-badges.tsx` 的 `ProviderQuotaLimit`/`QuotaLimit` 不匹配（上游潜在问题，运行无影响，按规则 4 不改）。
   - 合并后再做一次语义复核：测试通过不代表 custom 功能和上游新逻辑配合正确，需要逐项检查 custom 功能有没有被新代码路径绕过。
5. **汇报 → 等所有者同意 → 推送。**

## 4. 项目约束（来自 AGENTS.md，再次强调）

- 不要运行 lint 或构建命令，除非被明确要求。
- 不要重启开发服务器。
- 总结类文件放在 `.agent/summary`。

## 5. custom 挂在上游文件里的钩子（同步时最容易被悄悄冲掉）

这些行删掉后代码照样能编译，所以合并时必须逐个确认还在。每一处都有守护测试：

| 位置 | 内容 | 守护测试 |
|---|---|---|
| `llm/pipeline/pipeline.go`、`stream.go`；`internal/server/orchestrator/orchestrator.go`（`state.AliasFailoverEnabled`）、`state.go`、`rate_limit_tracking.go` | 多目标别名的"同渠道免费切换"挂接点（标有 `// fork:`） | `cd llm && go test ./pipeline/ -run Alias`；`go test ./internal/server/orchestrator/ -run 'Alias\|RateLimitTracking'` |
| `internal/server/orchestrator/orchestrator.go` PersistenceState | `QuotaService: processor.QuotaService`（渠道额度） | `TestChatCompletionOrchestrator_Process_SkipsChannelOverQuota` |
| `internal/server/orchestrator/pass_through.go` `responsesTransportAdaptationEnabled` | `PassThroughBodyBlocked` 时返回 false | `TestResponsesTransportAdaptationHonoursPassThroughBodyBlocked` |
| `internal/server/biz/channel_llm.go` `buildChannel` | `ch.setModelPriceMultiplier(c.ModelPriceMultiplier)`（渠道价格倍率，倍率 0 = 免费） | `TestUsageCost_ZeroChannelMultiplierSurvivesChannelBuild` |
| `internal/server/biz/usage_log.go` 模型库价格兜底 | 兜底计费也乘渠道倍率 | `TestUsageCost_ModelLibraryFallbackAppliesChannelMultiplier` |
| `CLAUDE.md`、`AGENTS.md` | 指向本文件的引用行 | — |

## 6. 有意覆盖上游的地方（规则 4 的例外）

| 位置 | 原因 | 何时撤销 |
|---|---|---|
| `llm/transformer/xai/subscription/constants.go` `CLIClientVersion = "1.0.13"` | 上游的 `0.2.114` 会被 xAI CLI 代理拒绝（"Your Grok CLI version is outdated"） | 上游把版本提到 1.0.13 或更高后，改回上游值并删掉 custom 注释；守护测试 `client_version_test.go` |

## 7. 已决定不再恢复的旧 custom 功能

- **图片 data-URL 内容校验**（`ef76069f`）：所有者在 2026-10-14 决定不需要，图片校验一律跟随上游。不要恢复，也不要再把它报告为"被覆盖的功能"。
- **OpenCode Go 的 workspaceId / Cookie 设置**：上游 #2204 有意删除，额度改用官方接口加渠道 API Key 查询，跟随上游。

## 8. 与所有者沟通

- 所有者自称"代码小白"，用通俗的中文交流。
- 先说结论，再列需要所有者决定或检查的事项；技术细节（提交列表、规则检查、测试数量）放最后或从简，被问到再展开。
- 背后的检查和测试照样做全，只是汇报要短。
- 提交信息用英文、约定式格式（如 `fix(channels): ...`），末尾带署名行。
