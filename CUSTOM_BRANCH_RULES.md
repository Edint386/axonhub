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

同步方式是**合并（merge）**，不再用 cherry-pick 复制上游提交。复制出来的提交编号是新的，Git 和 GitHub 会一直把上游原提交算作"落后"（2026-09-17 到 2026-10-15 用 cherry-pick 期间累积到 117 个）。2026-10-15 做了一次不改代码的"标记合并"（`cb6eae4d3`，`git merge -s ours`，事先逐个确认这 117 个提交的内容都已在 custom 里），从那以后 `git merge-base` 和"落后数"都是准的。

一次合并会带进多个上游提交，上游提交保留原编号、原作者，所有冲突在同一个合并提交里解决。2026-09-06 那次一口气合并 68 个提交、冲掉了多个 custom 功能，所以现在要求：经常同步、每次少量，并且先逐个模拟、再真合并、最后核对两者结果一致。

1. **只读预检**（合并前）：
   - `git fetch upstream unstable`，记下这次要合并的上游版本：`UP=$(git rev-parse upstream/unstable)`。后面真合并的必须就是这个编号，避免预检和合并之间上游又有新提交。
   - 缺少的上游提交：`git log --reverse --format='%h %s' $(git merge-base HEAD $UP)..$UP`。再拿这些提交标题和 custom 独有的提交（`git log --format=%s $UP..HEAD`）对一遍：如果有人手工 cherry-pick 过其中某个，真合并会把同一个改动加两次，要先查清楚。
   - 在临时 worktree 里按上游顺序逐个模拟 cherry-pick（只为了一个一个看清冲突，模拟结果不进正式历史），记下最终结果的提交编号 `SIM`。
   - 检查规则 1：custom 没改过的文件，合并后必须与上游完全一致。
   - 检查规则 2：重叠文件里 custom 的改动（相对上游的 diff）合并前后保持不变。
   - 在模拟结果上跑测试，然后向所有者汇报冲突的来龙去脉和推荐方案。
2. **合并**：在真实分支上执行 `git -c merge.conflictStyle=diff3 merge --no-ff $UP`。
   - 提交信息写 `merge: sync upstream/unstable (<短编号>) into custom`，正文列出带进来的上游提交标题，末尾加 `Co-Authored-By` 署名行。
   - 冲突按模拟时的方式解决。多数是"两边在同一处各自新增"，两边都保留。
   - Git 有时会把 custom 里已有的代码在别处再加一遍（同一段代码换了位置时它认不出来），所以合并后要检查有没有重复的函数或代码块。
   - 必须同时保住第 5 节列出的 custom 钩子。
   - 合并完核对 `git diff $SIM HEAD` 为空，即真合并的结果和逐个模拟的结果完全一样。不一样就撤销（`git reset --hard <合并前编号>`），查清原因再说。
3. **重新生成**（规则 3）：
   - GraphQL / ent：`cd internal/server/gql && go generate`。之后删除 gqlgen 留在 `*.resolvers.go` 末尾的 `!!! WARNING !!!` 注释块；`TestNoUnimplementedResolverStubs` 会拦截残留的 `not implemented` 占位函数（上游自带的 `Nodes` 除外）。
   - 前端路由树 `frontend/src/routeTree.gen.ts` 用 `@tanstack/router-plugin` 的生成器重新生成，不手改。
4. **验证**：
   - 后端：`go test ./...`；`llm/` 是独立 Go 模块，要在 `llm/` 目录里跑 `go test ./...`。
   - 前端单元测试：`cd frontend && node --test "src/**/*.test.mjs"`。
   - 前端类型检查必须用 `./node_modules/.bin/tsc -p tsconfig.app.json --noEmit --pretty false`。根目录的 `tsconfig.json` 是 `"files": []`，直接跑什么都不检查。上游本身就有一批类型错误，所以判断标准是"这次合并有没有新增错误"：把错误去掉行号列号后，和合并前的 custom 逐条（按多重集合）对比。不要拿 custom 和上游按原文对比：custom 的渠道表单多一个 `priority` 字段，上游已有的同一个错误在 custom 里文字会不一样（比如 "21 more" 对 "20 more"），会被误判成 custom 独有。已知的老错误：`quota-badges.tsx` 的 `ProviderQuotaLimit`/`QuotaLimit` 不匹配（上游潜在问题，运行无影响，按规则 4 不改）。
   - 合并后再做一次语义复核：测试通过不代表 custom 功能和上游新逻辑配合正确，需要逐项检查 custom 功能有没有被新代码路径绕过。
   - 实时语音（realtime）复制了几份上游逻辑，上游改了要同步：`internal/server/orchestrator/realtime_candidates.go` 里构造 `PersistenceState` 和 inbound 中间件顺序，对照 `orchestrator.go` 的 `Process`；同文件的 `RealtimeRPMAdmit`，对照 `rate_limit_admission.go` 的 `withRateLimitAdmission`；`internal/server/realtime_routes.go` 里手写的 `/v1` 中间件列表，对照 `routes.go` 的 `apiMiddlewares`；同文件 `/codex` 镜像的中间件列表，对照 `codex_routes.go` 的 `common`。Codex 的 User-Agent 不用同步：`internal/server/realtime/calls_glue.go` 通过 custom 文件 `llm/transformer/openai/codex/live.go` 的 `codex.DefaultUserAgent()` 直接读上游的 `codexDefaultUserAgent`，上游改版本号会自动跟上，没有副本。
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
| `internal/server/biz/channel_settings.go` `UpdateChannelSettingsInput` / `Apply`；`internal/server/gql/axonhub.graphql` `input UpdateChannelSettingsInput` | 上游的"只改部分设置"接口（#2699）带上 custom 的 `quota`、`quotaTimezone`、`healthProbe`（标有 `// custom:`）。少了它们，带这些设置的 patch 会被 GraphQL 当成未知字段拒绝，或把这些设置丢掉。目前 UI 的弹窗都带渠道字段、仍走完整的 `updateChannel`，这个接口主要给直接调 GraphQL 的调用方，以及上游以后改成走它的弹窗 | `TestUpdateChannelSettingsInput_Apply_CustomFields`、`TestChannelService_UpdateChannelSettings_KeepsCustomFields`、`TestUpdateChannelSettingsInputSchemaHasCustomFields` |
| `internal/server/biz/channel.go` `forgetLimitersAndReloadAfterCommit`（custom 的"提交后清理"函数，删渠道和批量删渠道用） | 同时调用上游的 `forgetAPIKeySelectionState`（#2548 每渠道 Key 选择状态）。上游在删除路径里直接调用，custom 把它并进这个函数 | `TestChannelDelete_ForgetsAPIKeySelectionState` |
| `frontend/src/features/channels/components/channels-table.tsx` | `keepVisible`：优先级列和操作列一样不被裁切（文件其余部分跟随上游） | `channels-table-priority.test.mjs` |
| `frontend/src/components/quota-badges.tsx` `getChannelCategory` 开头 | 只有本地额度的渠道归到 `local_quota` 分组；`sortQuotaChannels` 里 custom 的一行让它们在三种排序方式下都排在最后；本地额度渠道在去重时保留成单独一行（NanoGPT、OpenAI 兼容、ZenMux 共享账号三条去重规则都跳过它们） | `quota-badges-local-category.test.mjs`（直接运行上游的排序和分组函数）、`features/system/data/channel-quota-badge.test.mjs` |
| `internal/server/orchestrator/pass_through.go` `responsesTransportAdaptationEnabled` | `PassThroughBodyBlocked` 时返回 false | `TestResponsesTransportAdaptationHonoursPassThroughBodyBlocked` |
| `internal/server/biz/channel_llm.go` `buildChannel` | `ch.setModelPriceMultiplier(c.ModelPriceMultiplier)`（渠道价格倍率，倍率 0 = 免费） | `TestUsageCost_ZeroChannelMultiplierSurvivesChannelBuild` |
| `internal/server/biz/usage_log.go` 模型库价格兜底 | 兜底计费也乘渠道倍率 | `TestUsageCost_ModelLibraryFallbackAppliesChannelMultiplier` |
| `internal/server/routes.go` `SetupRoutes` | `registerRealtimeRoutes(server, handlers, services)`（实时语音路由，`// fork:`） | `TestSetupRoutes_RegistersRealtimeRoutes`（`internal/server/realtime_routes_fork_test.go`） |
| `internal/server/biz/channel_llm.go` `buildNonDefaultEndpointOutbound` switch 开头 | `case llm.APIFormatOpenAIRealtime.String(): return newRealtimeEndpointOutbound(), nil`（没有它带 `openai/realtime` 端点的渠道会整个被丢弃） | `TestRealtimeEndpoint_ForkHookInBuildNonDefaultEndpointOutbound` |
| `frontend/src/features/channels/data/schema.ts` `configurableChannelEndpointApiFormats` | `'openai/realtime'`（端点对话框能选它） | `frontend/src/features/realtime/realtime-endpoint.test.mjs` |
| `internal/server/server.go` `Shutdown` | `defer drainRealtimeSessions(ctx) // fork: see realtime_shutdown.go`（让被劫持的实时会话先收尾、落库用量，再让 fx 关数据库） | `TestServerShutdown_DrainsRealtimeSessions`（`internal/server/realtime_shutdown_fork_test.go`） |
| `docs/{en,zh}/index.md` API 参考表 | Realtime API 那一行 | —（没有守护测试，手工检查） |
| `CLAUDE.md`、`AGENTS.md` | 指向本文件的引用行 | — |

实时语音功能的其余代码全是 custom 新文件：`llm/realtime.go`、`llm/transformer/openai/codex/live*.go`、`internal/server/biz/channel_endpoint_realtime*.go`、`internal/server/orchestrator/realtime_candidates*.go`、`internal/server/middleware/realtime_subprotocol*.go`、`internal/server/realtime/**`、`internal/server/api/realtime.go`、`internal/server/realtime_routes*.go`、`internal/server/realtime_shutdown*.go`、`frontend/src/locales/{en,zh-CN}/realtime.json`、`frontend/src/features/realtime/`、`docs/{en,zh}/api-reference/realtime.md`。

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
