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
   - 上游 #2830 起，`dailyRequestStats` 等小写辅助方法都在 `dashboard_helpers.go`，`go generate` 不会再把它们挪进 `!!! WARNING !!!` 块，也不会编译失败。现在 `go generate` 只会调整 `dashboard.resolvers.go` 的 import 顺序（纯上游也一样，行为无变化）；这个文件 custom 没改过，用 `git checkout -- internal/server/gql/dashboard.resolvers.go` 还原，保持和上游一致。`test.yml` 在测试前也会跑 `make generate`，所以 custom 的小写辅助方法不能写进 `*.resolvers.go`，要放进 `*_helpers.go` 这类文件，否则生成步骤会报错、CI 失败。
   - 前端路由树 `frontend/src/routeTree.gen.ts` 用 `@tanstack/router-plugin` 的生成器重新生成，不手改。
4. **验证**：
   - 后端：`go test ./...`；`llm/` 是独立 Go 模块，要在 `llm/` 目录里跑 `go test ./...`。
   - 前端单元测试：`cd frontend && node --test "src/**/*.test.mjs"`。
   - 前端类型检查必须用 `./node_modules/.bin/tsc -p tsconfig.app.json --noEmit --pretty false`。根目录的 `tsconfig.json` 是 `"files": []`，直接跑什么都不检查。上游本身就有一批类型错误，所以判断标准是"这次合并有没有新增错误"：把错误去掉行号列号后，和合并前的 custom 逐条（按多重集合）对比。不要拿 custom 和上游按原文对比：custom 的渠道表单多一个 `priority` 字段，上游已有的同一个错误在 custom 里文字会不一样（比如 "21 more" 对 "20 more"），会被误判成 custom 独有。已知的老错误：`quota-badges.tsx` 的 `ProviderQuotaLimit`/`QuotaLimit` 不匹配（上游潜在问题，运行无影响，按规则 4 不改）。
   - 合并后再做一次语义复核：测试通过不代表 custom 功能和上游新逻辑配合正确，需要逐项检查 custom 功能有没有被新代码路径绕过。
   - 上游 #2917 起插件有独立身份（Plugin principal）：`authz.RunWithSystemBypass*` 对插件一律拒绝，系统设置读取是 host read（不用 bypass），上游的固定写操作走 `authz.RunAsHost`。custom 代码照此处理：读设置直接调用（如 `ChannelHealthProbeService.ScanPolicy`）；要保留调用方事务的写操作不要换成 `RunAsHost`（它会丢掉事务，提交后的缓存清理就不做了），宁可对插件拒绝（如健康探测策略的保存）；会写库的渠道测试入口要像 `tester.go` 一样先拒绝插件再写库（如 `ChannelHealthProbeRunner.RunManual`）。守护测试：`TestChannelHealthProbePolicy_PluginPrincipal`、`TestChannelHealthProbeRunManual_RejectsPluginsBeforePersisting`。
   - 上游 #2926 起插件可以在请求开始和每次尝试前改请求（`onRequest` / `onAttempt` 钩子）。插件运行时不够用时返回 429，并停止一切重试和切换（`state.PluginOverloaded`；`isLocalAdmissionError` 把它和渠道排队、本地 RPM 拒绝归为一类）。custom 自己的失败切换要跟着用 `isLocalAdmissionError`，不能只列排队和 RPM：别名目标切换的 `isAliasFailoverError` 就是这样，守护测试 `TestChatCompletionOrchestrator_Process_AliasFailoverStopsOnPluginOverload`。`NewChatCompletionOrchestrator` 多了 `PluginIntegration` 参数：正式代码照上游传 `orchestratorPlugins(...)`，测试传空值。实时语音不经过 `ChatCompletionOrchestrator.Process`，插件请求钩子对它不生效。百炼语音转写（`internal/server/bailianasr`）发的是 DashScope 原生 JSON，却标成 `openai/audio_transcriptions`；上游自己的转写请求是 multipart，`onAttempt` 本来就跳过，所以 custom 让 `onAttempt` 也跳过百炼转写请求（见第 5 节），否则插件一改请求体，存进请求记录的就不再是去掉音频的副本，而是音频本身或带签名的下载地址。已知小限制：请求体超过 26 MiB 或长度未知（分块上传）的百炼转写走大文件路径，在进入 `Process` 前就按客户端填的模型做了权限预检（`checkUploadModelAccess`）。插件 `onRequest` 若把模型改成 API Key 允许的另一个，这类请求会被预检拒掉，带长度且不超过 26 MiB 的请求则能通过；只会多拒、不会放过，暂不处理。
   - 模型检测的 `frontend/src/features/playground/model-trace/transport.ts` 照着 playground 聊天手写了一遍请求方式：接口地址、`X-Project-ID` / `X-Channel-ID` 请求头、`ensureFreshAccessToken` 鉴权、AI SDK 的 UI 消息流格式（`text-delta` / `finish` / `error`，`ai` 5 到 7 都是 v1 协议）。上游改了其中任何一项（或者 `ai` 升到新的大版本）要同步改它；`playground-hook.test.mjs`（盯版本号和请求写法）和 `transport.test.mjs`（重放后端的流样例 `llm/transformer/aisdk/testdata/aisdk-strop.stream.jsonl`）会报错提醒。
   - 实时语音（realtime）复制了几份上游逻辑，上游改了要同步：`internal/server/orchestrator/realtime_candidates.go` 里构造 `PersistenceState` 和 inbound 中间件顺序，对照 `orchestrator.go` 的 `Process`（上游 #2912 起含插件事件用的 `EventBus`，`TestSelectRealtime_QuotaRejectionPublishesPluginEvent` 会在它丢失时报错）；同文件的 `RealtimeRPMAdmit`，对照 `rate_limit_admission.go` 的 `withRateLimitAdmission`；`internal/server/realtime_routes.go` 里手写的 `/v1` 中间件列表，对照 `routes.go` 的 `apiMiddlewares`；同文件 `/codex` 镜像的中间件列表，对照 `codex_routes.go` 的 `common`。Codex 的 User-Agent 不用同步：`internal/server/realtime/calls_glue.go` 通过 custom 文件 `llm/transformer/openai/codex/live.go` 的 `codex.DefaultUserAgent()` 直接读上游的 `codexDefaultUserAgent`，上游改版本号会自动跟上，没有副本。
5. **汇报 → 等所有者同意 → 推送。**

## 4. 项目约束（来自 AGENTS.md，再次强调）

- 不要运行 lint 或构建命令，除非被明确要求。
- 不要重启开发服务器。
- 总结类文件放在 `.agents/summary`。

## 5. custom 挂在上游文件里的钩子（同步时最容易被悄悄冲掉）

这些行删掉后代码照样能编译，所以合并时必须逐个确认还在。每一处都有守护测试：

| 位置 | 内容 | 守护测试 |
|---|---|---|
| `llm/pipeline/pipeline.go`、`stream.go`；`internal/server/orchestrator/orchestrator.go`（`state.AliasFailoverEnabled`）、`state.go`、`rate_limit_tracking.go` | 多目标别名的"同渠道免费切换"挂接点（标有 `// fork:`） | `cd llm && go test ./pipeline/ -run Alias`；`go test ./internal/server/orchestrator/ -run Alias`；`go test ./internal/server/orchestrator/ -run RateLimitTracking`（含 `alias_failover.go` 的 520/402 守护：配合上游 #2609，这两个状态码不在别名目标间免费切换，渠道把它们列为可重试时也一样，之后交给上游 `CanRetry`；由 `TestIsAliasFailoverError` 的 520/402 行守护） |
| `internal/server/orchestrator/orchestrator.go` PersistenceState | `QuotaService: processor.QuotaService`（渠道额度） | `TestChatCompletionOrchestrator_Process_SkipsChannelOverQuota` |
| `internal/server/orchestrator/orchestrator.go` `Process` 的插件 attempt 阶段 | `skipAttemptHooksForBailianASR(hooks.attemptStage(outbound))`：插件 `onAttempt` 跳过百炼语音转写请求（上游 #2926，见 `plugin_hooks_bailian_asr.go`） | `TestCreateTranscription_BailianSkipsPluginAttemptHooks`、`TestSkipAttemptHooksForBailianASR` |
| `internal/server/biz/channel_settings.go` `UpdateChannelSettingsInput` / `Apply`；`internal/server/gql/axonhub.graphql` `input UpdateChannelSettingsInput` | 上游的"只改部分设置"接口（#2699）带上 custom 的 `quota`、`quotaTimezone`、`healthProbe`（标有 `// custom:`）。少了它们，带这些设置的 patch 会被 GraphQL 当成未知字段拒绝，或把这些设置丢掉。目前 UI 的弹窗都带渠道字段、仍走完整的 `updateChannel`，这个接口主要给直接调 GraphQL 的调用方，以及上游以后改成走它的弹窗 | `TestUpdateChannelSettingsInput_Apply_CustomFields`、`TestChannelService_UpdateChannelSettings_KeepsCustomFields`、`TestUpdateChannelSettingsInputSchemaHasCustomFields` |
| `internal/server/biz/channel.go` `forgetLimitersAndReloadAfterCommit`（custom 的"提交后清理"函数，删渠道和批量删渠道用） | 同时调用上游的 `forgetAPIKeySelectionState`（#2548 每渠道 Key 选择状态）。上游在删除路径里直接调用，custom 把它并进这个函数 | `TestChannelDelete_ForgetsAPIKeySelectionState` |
| `frontend/src/features/channels/components/channels-table.tsx` | `keepVisible`：优先级列和操作列一样不被裁切（文件其余部分跟随上游） | `channels-table-priority.test.mjs` |
| `frontend/src/components/quota-badges.tsx` `getChannelCategory` 开头 | 只有本地额度的渠道归到 `local_quota` 分组；`sortQuotaChannels` 里 custom 的一行让它们在三种排序方式下都排在最后；本地额度渠道在去重时保留成单独一行（NanoGPT、OpenAI 兼容、ZenMux 共享账号三条去重规则都跳过它们） | `quota-badges-local-category.test.mjs`（直接运行上游的排序和分组函数）、`features/system/data/channel-quota-badge.test.mjs` |
| `internal/server/gql/analytics.resolvers.go` `AnalyticsMetadata`、`AnalyticsDailyStats` | `AnalyticsMetadata` 开头的 `authz.WithScopeDecision(ctx, scopes.ScopeReadDashboard)`（只有看板权限的用户也能用"使用以来"）；`AnalyticsDailyStats` 里紧跟 `buildAnalyticsWhere` 的 `applyAnalyticsDailyStatsDateBounds`（没选时间范围时，趋势图只查图上显示的 30 天，避免扫全表） | `TestAnalyticsMetadataAllowsDashboardScope`、`TestAnalyticsDailyStatsResolverAppliesDefaultDateBounds`、`TestAnalyticsDailyStatsAddsDefaultDateBounds` |
| `internal/server/orchestrator/pass_through.go` `responsesTransportAdaptationEnabled` | `PassThroughBodyBlocked` 时返回 false | `TestResponsesTransportAdaptationHonoursPassThroughBodyBlocked` |
| `internal/server/orchestrator/pass_through.go` `isPassThroughEnabled` | `passThroughBodySupportsRawRequest`：multipart 请求不走请求体、响应和流式透传。百炼语音转写的上游格式和入站格式相同，只靠这一行挡住透传 | `TestIsPassThroughEnabled_SkipsMultipartRawRequest` |
| `internal/server/orchestrator/outbound.go` `TransformRequest`、`NextChannel`、`PrepareForRetry` | `PassThroughBodyBlocked`：渠道设置（转换选项、推理强度映射、默认 max tokens）或出站中间件改写了请求时置为 true，每次尝试前清零，透传时原始请求体就不会盖掉改写结果 | `TestTransformRequest_BlocksPassThroughAfterSemanticRewrite`、`TestPersistentOutboundTransformer_PrepareForRetry` |
| `internal/server/biz/channel_llm.go` `buildChannel` | `ch.setModelPriceMultiplier(c.ModelPriceMultiplier)`（渠道价格倍率，倍率 0 = 免费） | `TestUsageCost_ZeroChannelMultiplierSurvivesChannelBuild` |
| `internal/server/biz/usage_log.go` 模型库价格兜底 | 兜底计费也乘渠道倍率 | `TestUsageCost_ModelLibraryFallbackAppliesChannelMultiplier` |
| `frontend/src/features/channels/components/channels-model-price-dialog.tsx` | 倍率随价格一起存到渠道（打开时 `setMultiplier(currentPrices.multiplier)`，保存时带 `multiplier`），计费时由后端乘；只在关闭弹窗时把倍率重置为 1（上游在模型库数据加载时也会重置，会冲掉已保存的倍率）；从模型库填价格只乘上游的汇率（#2662），不乘倍率；倍率输入框 `step='any'`（`0.01` 会让浏览器拒绝保存 0.875 这类倍率） | `frontend/src/features/channels/data/channel-price-multiplier.test.mjs` |
| `internal/objects/price.go`（`PriceItemCodeDurationSeconds`）、`internal/server/biz/cost_calc.go`（新增 `scaleUnits`、`computeItemSubtotalScaled`、`durationSecondsQuantity`；上游的 `unitsInMillionTokens`、`computeItemSubtotal` 改成转调它们；`computeUsageCostWithItems` 里的 `case objects.PriceItemCodeDurationSeconds`）、`internal/server/gql/price.graphql`（`enum PriceItemCode` 的 `duration_seconds`）、`llm/model.go`（`Usage.DurationSeconds`）；`frontend/src/features/channels/data/schema.ts`、`frontend/src/components/model-price-editor.tsx`、`frontend/src/components/price-schedule-editor.tsx`、`frontend/src/features/channels/components/channels-model-price-dialog.tsx` 的价格项列表，两个价格编辑器里的“每秒”文案，`frontend/src/locales/{en,zh-CN}/base.json` 的 `price.itemCodes.duration_seconds`、`price.mode_usage_per_second` | 实时语音转写按秒计费的 `duration_seconds` 价格项（百炼 Paraformer；OpenAI GA 转写的 duration 用量也走这一项），按秒计价、不除以一百万（代码里标有 `custom:`，JSON 文件没法加标记）。少了价格项列表、枚举或后端这几处，按秒价格会存不了、打不开或不计费；少了文案或翻译只是显示不对。上游以后在 `computeItemSubtotal` 里新增的计价分支，合并进 `computeItemSubtotalScaled` 时要用 `scaleUnits(..., unitDivisor)`，不能用 `unitsInMillionTokens` | `internal/server/biz/cost_duration_seconds_fork_test.go`、`TestPriceItemCodeEnumHasDurationSeconds`（`internal/server/gql/price_item_code_fork_test.go`）、`frontend/src/features/realtime/duration-price-item.test.mjs`；常量或 `Usage.DurationSeconds` 被删掉时 `go test` 编译失败 |
| `internal/server/routes.go` `SetupRoutes` | `registerRealtimeRoutes(server, handlers, services)`（实时语音路由，`// fork:`） | `TestSetupRoutes_RegistersRealtimeRoutes`（`internal/server/realtime_routes_fork_test.go`） |
| `internal/server/biz/channel_llm.go` `buildNonDefaultEndpointOutbound` switch 开头 | `case llm.APIFormatOpenAIRealtime.String(): return newRealtimeEndpointOutbound(), nil`（没有它带 `openai/realtime` 端点的渠道会整个被丢弃） | `TestRealtimeEndpoint_ForkHookInBuildNonDefaultEndpointOutbound` |
| `internal/server/biz/channel_llm.go` `buildChannelWithOutbounds`（别名循环之后） | `wrapBailianASROutbounds(c, ch, outbounds) // fork: see channel_bailian_asr.go`：百炼（`bailian` 类型）渠道绑定在主出站上的 `Outbounds` 项包上 `bailianasr`（用户自己配了 `openai/chat_completions` 端点时，比如换到别的地域，那个端点也包上，并用它自己的地址），`/v1/audio/transcriptions` 的 qwen-audio-3.1-asr-flash(-filetrans/-message)、paraformer-v2、paraformer-8k-v2 不用配端点就能用；`ch.Outbound` 本身保持上游类型（上游测试 `TestBailianChannel_ExistingProtocolsUnchanged` 要求） | `TestBailianASR_ForkHookWrapsDefaultOutbounds`、`TestBailianASR_UserChatEndpointIsWrapped`（`internal/server/biz/channel_bailian_asr_fork_test.go`） |
| `internal/server/biz/channel_llm.go` `buildNonDefaultEndpointOutbound` switch 之前 | `if out, ok, err := bailianASREndpointOutbound(c, ch, ep, baseURL); ok { // fork: ...` + `return out, err`：百炼三种渠道上显式配的 `openai/audio_transcriptions` 端点也走 `bailianasr`（没有它会用通用 OpenAI 出站，去 compatible-mode 拿 404） | `TestBailianASR_ForkHookInBuildNonDefaultEndpointOutbound` |
| `internal/server/api/openai.go` `CreateTranscription` 开头 | `if handlers.serveBailianTranscription(c) { // fork: see bailian_transcription_upload.go` + `return`：百炼转写的三样东西都从这里进：超过上游上限（单文件 26 MiB、请求体 50/64 MiB）的大文件先边收边落盘、再从磁盘流式传到百炼临时存储（只限 qwen-audio-3.1-asr-flash-filetrans、paraformer-v2、paraformer-8k-v2）；`url`/`file_url` 链接代替文件；这几个长任务模型 `stream=true` 时强制发 keep-alive。没有它这三样都退回上游行为（大文件 400、链接报 file is required、长任务期间连接空闲）。不是百炼模型的请求（OpenAI、Groq 等）原样交给上游：不占上传名额、不落盘、报错和上游一字不差（模型字段在文件后面时，先在内存里记下最多 64 MiB 原始请求体；超过 64 MiB 时，还不知道模型的文件从不占名额，不超过 26 MiB 的才落盘，超过 26 MiB 的直接丢掉、只记大小，接着读到模型字段：不是百炼模型就照样回上游的错误，是百炼大文件模型就回 413 `model_after_large_file`，请客户端把 model 字段放在文件前面）。不超过 26 MiB 的文件一律把真实音频交给上游（被渠道或 Key 映射到别的模型也不会收到占位内容），只有超过 26 MiB 的才走占位请求体。大文件名额：全局 `AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY`（默认 2）、每个 API Key `AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY`（默认 1），拿名额前先按 API Key 的模型列表检查（不允许就直接 422），音频传到百炼临时存储后立即释放名额（轮询期间不占）；大文件落盘（超过 26 MiB 的部分才开始计）合计不超过 `AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB`（默认 4096），超出回 429 `upload_storage_busy`；不支持大文件的百炼模型收到超过 26 MiB 的文件直接回 400 file too large，不落盘；model 字段去掉首尾空白再匹配；落盘失败只回固定文案，路径只写服务端日志。落盘文件一创建就删掉文件名（句柄还开着，数据照常能读），请求结束关闭句柄就释放磁盘，进程重启、崩溃或被杀时也由系统自动释放；Windows 删不掉打开的文件，仍是随请求删除，残留的由下一次落盘时顺手清掉（最多每小时一次，只删 `axonhub-bailian-asr-*` 且两小时以上没写过的）。开始落盘后，客户端超过 2 分钟（`AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS`）不发数据、或者等了 60 秒还没收到 1 MiB（408 `request_body_too_slow`），或到了请求超时，就断开、释放名额（408 或超时错误），不会一直占着。另外 `bailianasr` 的 committed 事件依赖上游两处行为：`llm/pipeline/empty_response.go` 把非空 `Transcription.Raw` 算作内容、OpenAI 转写入站流只转发 `TranscriptionStreamEvent`，上游改了这两处要看同一组测试 | `TestCreateTranscription_ForkHookServesBailianFirst`，以及同文件（`internal/server/api/bailian_transcription_upload_test.go`）的大文件、链接、keep-alive 行为测试（`TestCreateTranscription_BailianAsyncStreamCommitsBeforeTheResult` 守护 committed 事件）；`bailian_transcription_upload_limits_test.go`（`TestCreateTranscription_OtherModelsBehaveExactlyAsUpstream` 等）；`bailian_transcription_upload_review4_test.go`（每 Key 名额、最低速率、模型列表预检、名额随上传完成释放、落盘总量、≤26 MiB 交真实音频、未决文件不占名额） |
| `llm/pipeline/pipeline.go` `Process`（紧跟 `ErrPreCommitBufferExceeded` 那条判断之后） | `if isNoRetryError(lastErr) { // fork: see no_retry_fork.go` + `return nil, lastErr`：错误里带 `pipeline.ErrNoRetry`（custom 新文件 `llm/pipeline/no_retry_fork.go`）时整个请求立即结束，不再同渠道重试、不换渠道、不换别名目标或模型。百炼文件转写在任务已经提交、已经（或将要）扣费之后出的错都带这个标记（结果下载失败、结果解析失败、任务查不到、请求结束时任务还在跑、-message 音频发出后断线或失败、flash 请求发出后断线或请求超时、提交结果未确认，以及之后 10 分钟内同样请求被拒的 409 `bailian_submit_pending`），没有它会换到另一个百炼渠道把同一段音频再提交一次、再扣一次钱 | `cd llm && go test ./pipeline/ -run NoRetry`（`TestProcess_NoRetryErrorEndsThePipeline`，`llm/pipeline/no_retry_fork_test.go`）；`TestPipeline_BilledJobIsNeverResubmitted`（`internal/server/bailianasr/review_regressions_test.go`） |
| `llm/pipeline/pipeline.go` `processRequest`（`effectiveResponseTimeouts` 的下一行） | `streamFirstEventTimeout, nonStreamTimeout = jobResponseTimeouts(httpReq, ...) // fork: see no_retry_fork.go`：请求元数据带 `pipeline.MetadataLongRunningJob`（百炼文件转写的三种调用都会设置：异步文件任务、-message 任务和 flash；flash 要等约 2 倍音频时长、SSE 事件最后才到）时，系统或渠道设置的“首字超时”“非流式响应超时”不作用于这次尝试。没有它，超时会把正在跑、正在计费的长任务掐断，再换渠道重新提交 | `cd llm && go test ./pipeline/ -run LongRunning`（`TestProcess_LongRunningJobIgnoresResponseTimeouts`） |
| `internal/server/orchestrator/performance.go` `OnOutboundRawRequest`（`perf.Source = ...` 下一行）、`OnOutboundRawError`（`MarkCanceled` 后面那个 `else`）和 `OnOutboundLlmResponse`（`MarkSuccess()` 下一行）；`internal/server/orchestrator/outbound.go` `markPerformanceTerminal`（`ts.perf.MarkSuccess()` 下一行）；`internal/server/biz/channel_metrics.go` `PerformanceRecord` 的 `LatencyExcluded` 字段和 `recordSuccess` 里重置连续失败之后的 `if perf.LatencyExcluded { // fork: ...` + `return` | `perf.LatencyExcluded = excludesLatency(request) // fork: see performance_bailian_asr.go`：百炼文件转写（flash、异步任务、-message）一次要几秒到几十分钟，它和百炼聊天共用一个渠道。成功时照样算成功（清零连续失败、清自动禁用计数），但不进延迟平均值，免得负载均衡因为一次长转写把聊天流量挪走。`} else if !markTranscriptionFailure(perf, err) { // fork: see performance_bailian_asr.go`：转写失败时，和渠道好坏无关的失败（用户的音频或参数被拒 400/413、请求超时时任务还在跑、用别的请求的 Key 查询共用任务被拒、共用的那次提交在别的请求里失败、10 分钟内重复提交被拒的 409）既不算成功也不算失败；其余失败照常计入，并记成客户端实际拿到的状态码（424、504、502 等），而不是上游统一记的 500。`markTranscriptionSuccess(..., ...) // fork: see performance_bailian_asr.go`（非流式和流式各一行）：转写结果来自缓存、来自缓存里别的请求提交的任务、或来自并发相同请求共用的那次提交时，这次尝试没用本渠道的 Key 干活，成功也既不算成功也不算失败（不清零这个 Key 的连续失败、不推进轮询游标）。少了 `OnOutboundRawRequest` 那行或 `recordSuccess` 那段会让延迟分数被拉高；少了 `OnOutboundRawError` 那行，用户传坏文件也会累加渠道的连续失败、触发自动禁用；少了两行 `markTranscriptionSuccess`，一把已吊销的 Key 也会因为别人的结果被记成功 | `TestPerformanceRecording_BailianTranscriptionsAreExcludedFromLatency`、`TestPerformanceRecording_BailianTranscriptionFailuresKeepTheirMeaning`、`TestPerformanceRecording_ReusedTranscriptionSuccessIsNeutral`（`internal/server/orchestrator/performance_bailian_asr_test.go`）、`TestRecordPerformance_LatencyExcludedSuccessKeepsTheLatencyScores`（`internal/server/biz/channel_bailian_asr_metrics_fork_test.go`）；字段被删掉时 `go test` 编译失败 |
| `frontend/src/features/channels/data/schema.ts` `configurableChannelEndpointApiFormats` | `'openai/realtime'`（端点对话框能选它） | `frontend/src/features/realtime/realtime-endpoint.test.mjs` |
| `internal/server/server.go` `Shutdown` | `defer drainRealtimeSessions(ctx) // fork: see realtime_shutdown.go`（让被劫持的实时会话先收尾、落库用量，再让 fx 关数据库） | `TestServerShutdown_DrainsRealtimeSessions`（`internal/server/realtime_shutdown_fork_test.go`） |
| `frontend/src/features/playground/index.tsx` | 模型检测（ModelTrace）入口：`PlaygroundMode` 状态行（`// custom:`）、侧栏标题下的 `<PlaygroundModeSwitch>`、聊天卡片和侧栏底部“重试/清除”按钮 className 里的 `playgroundMode === 'trace' && 'hidden'`、聊天卡片后面的 `<ModelTracePanel>` | `frontend/src/features/playground/model-trace/playground-hook.test.mjs` |
| `frontend/src/config/nav-items.ts` | 侧栏“渠道健康”入口（`/channel-health`）。上游 #2919 把侧栏改成模块和分组后，它放在“网关”（gateway）分组的“渠道”后面。上游测试 `nav-items.test.mjs` 写死了全部侧栏地址，custom 在期望列表里加了 `/channel-health` 一行（标有 `// custom:`） | `frontend/src/config/nav-items.test.mjs`（`flattened nav urls keep the full set of sidebar routes`） |
| `frontend/src/features/channels/components/channels-system-settings-dialog.tsx` | “跳过状态确认”开关（本地偏好，`use-channel-status-confirmation-preference`）。上游 #2919 把弹窗改成左侧选分区、一次显示一个分区后，它是单独的 `statusConfirmation` 分区：`SettingsSectionKey`、`sections` 列表和渲染分支三处都要有，少了列表项开关就点不到 | `frontend/src/features/channels/data/channels-system-settings.test.mjs` |
| `internal/server/biz/channel_apikey.go` `DisableAPIKey`、`EnableAPIKey`、`EnableAllAPIKeys`、`EnableSelectedAPIKeys`、`DeleteDisabledAPIKeys` | 缓存刷新和“渠道被自动禁用”通知等事务提交后再做（`runAfterCommit` / `reloadChannelsAfterCommit`，custom 提交 6cf28c8f；a46b535d 让刷新改用脱离事务的 context），上游用的是立即的 `asyncReloadChannels`；上游 #2912 的插件凭证事件照上游位置发布。通知要用不带已提交事务的 context（那几行标有 `custom:`）：上游 #2900 起，带着已提交事务的 context 读系统设置会去查那个事务并失败，webhook 就悄悄发不出去 | `TestChannelService_DisableAPIKey_DefersReloadUntilCommit`、`TestChannelService_DisableLastAPIKey_ReloadsWithServiceClientAfterCommit`（`channel_auto_disable_test.go`）、`TestDisableAPIKey_AfterCommitWebhookFires`（`channel_apikey_after_commit_fork_test.go`）；三个启用函数和 `DeleteDisabledAPIKeys` 的提交后刷新没有专门的守护测试，手工检查 |
| `docs/{en,zh}/index.md` API 参考表 | Realtime API 和百炼文件转写两行 | —（没有守护测试，手工检查） |
| `internal/server/gql/model.resolvers.go` `PreviewChannelModelEntries` | `previewChannelModelEntries(&ch) // fork: see channel_model_preview_fork.go`：上游 #2859 的别名预览改用 `GetModelEntryGroups`，多目标别名的每个目标都显示。上游的 `GetModelEntries` 只留每个别名的第一个目标，和 custom 路由（7dcf5d13）不一致 | `TestPreviewChannelModelEntries_MultiTargetAliasShowsEveryTarget`（`internal/server/gql/channel_model_preview_fork_test.go`） |
| `internal/server/orchestrator/candidates.go` `Select`（`quotaGate.Filter` 的下一行） | `quotaFallbackCandidates(orderQuotaFallbackByChannelPriority(lastResort), ...) // fork: see candidates_priority.go`：上游 #2974 把额度紧张（sticky-only）的渠道留作最后备用，但只按模型优先级排序；custom 先按渠道优先级（高的在前）排一遍，再交给上游去重、按重试次数截断，免得高优先级的备用渠道被截掉。聊天和实时语音都走这里 | `TestLoadBalancedSelector_quota_fallback_follows_channel_priority`（`internal/server/orchestrator/candidates_quota_priority_fork_test.go`） |
| `internal/server/orchestrator/outbound.go` `trackCurrentChannelSelection` 及它在 `NextChannel`、`PrepareForRetry` 里的 3 处调用 | 函数多了 `ctx` 参数，开头 `// fork:` 一段：请求来源是 `SourceTest`（渠道健康探测）时不调用 `IncrementChannelSelection`。探测的同渠道重试和换渠道不算真实流量，和 `TrackSelection`、`RecordPerformance` 一致 | `TestTrackCurrentChannelSelection_SkipsHealthProbeAttempts`（`internal/server/orchestrator/retry_selection_probe_fork_test.go`） |
| `internal/server/biz/channel_metrics.go` `scanChannelMetricExecutions` 的查询条件 | `// fork:` 一行 `requestexecution.Not(requestexecution.HasRequestWith(request.SourceEQ(request.SourceTest)))`：重启后从请求记录恢复渠道统计时跳过健康探测；找不到请求行的记录照常计入 | `TestChannelMetrics_LoadAllFromExecutions_SkipsHealthProbes`（`internal/server/biz/channel_metrics_probe_fork_test.go`） |
| `CLAUDE.md`、`AGENTS.md` | 指向本文件的引用行 | — |

实时语音功能的其余代码全是 custom 新文件：`llm/realtime.go`、`llm/transformer/openai/codex/live*.go`、`internal/server/biz/channel_endpoint_realtime*.go`、`internal/server/orchestrator/realtime_candidates*.go`、`internal/server/middleware/realtime_subprotocol*.go`、`internal/server/realtime/**`、`internal/server/api/realtime*.go`、`internal/server/realtime_*.go`（路由、关机、ASR 和各类路由测试）、`internal/server/biz/cost_duration_seconds_fork_test.go`、`internal/server/gql/price_item_code_fork_test.go`（按秒计费的守护测试）、`frontend/src/locales/{en,zh-CN}/realtime.json`、`frontend/src/features/realtime/`、`docs/{en,zh}/api-reference/realtime.md`。

百炼文件转写（`/v1/audio/transcriptions` 走 DashScope 原生接口）的其余代码全是 custom 新文件：`internal/server/bailianasr/**`、`internal/server/biz/channel_bailian_asr*.go`、`internal/server/api/bailian_transcription_fork_test.go`、`internal/server/api/bailian_transcription_plugin_hooks_fork_test.go`、`internal/server/api/bailian_transcription_upload*.go`、`internal/server/orchestrator/performance_bailian_asr*.go`、`internal/server/orchestrator/plugin_hooks_bailian_asr*.go`、`llm/pipeline/no_retry_fork*.go`、`docs/{en,zh}/api-reference/bailian-transcription.md`。

模型检测（ModelTrace）的其余代码全是 custom 新文件：`frontend/src/features/playground/model-trace/`（算法从 [xqy2006/ModelTrace](https://github.com/xqy2006/ModelTrace) 移植，MIT，许可证在同目录 `LICENSE`；更新指纹库的方法见同目录 `README.md`）、`frontend/src/locales/{en,zh-CN}/modelTrace.json`。只走上游已有的 `/admin/playground/chat` 接口，后端没有改动。

## 6. 有意覆盖上游的地方（规则 4 的例外）

| 位置 | 原因 | 何时撤销 |
|---|---|---|
| `llm/transformer/xai/subscription/constants.go` `CLIClientVersion = "1.0.13"` | 上游的 `0.2.114` 会被 xAI CLI 代理拒绝（"Your Grok CLI version is outdated"） | 上游把版本提到 1.0.13 或更高后，改回上游值并删掉 custom 注释；守护测试 `client_version_test.go` |

## 7. 已决定不再恢复的旧 custom 功能

- **图片 data-URL 内容校验**（`ef76069f`）：所有者在 2026-10-14 决定不需要，图片校验一律跟随上游。不要恢复，也不要再把它报告为"被覆盖的功能"。
- **OpenCode Go 的 workspaceId / Cookie 设置**：上游 #2204 有意删除，额度改用官方接口加渠道 API Key 查询，跟随上游。
- **请求列表里缓存命中率的固定分档颜色**（`bedd17a3`，`getCacheHitRateColor`）：上游 #2883 加了可调的“指标颜色”（工具栏按钮，缓存命中率、首字延迟、吞吐量各自可调分界线，存在浏览器本地），所有者在 2026-11-12 决定改用上游的。不要恢复固定分档；`requests-columns-cache.test.mjs` 只检查命中率仍然上色。

## 8. 与所有者沟通

- 所有者自称"代码小白"，用通俗的中文交流。
- 先说结论，再列需要所有者决定或检查的事项；技术细节（提交列表、规则检查、测试数量）放最后或从简，被问到再展开。
- 背后的检查和测试照样做全，只是汇报要短。
- 提交信息用英文、约定式格式（如 `fix(channels): ...`），末尾带署名行。
