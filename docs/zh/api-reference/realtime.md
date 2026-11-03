# Realtime API（实时语音）参考

> 本功能仅存在于 `custom` 分支（Edint386/axonhub），上游 AxonHub 没有。

## 概述

AxonHub 可以中继 **OpenAI Realtime API**（正式版协议），让语音客户端经过网关的鉴权、选渠道、配额和用量记录去访问上游的实时模型。支持这些上游：

| 上游 | 方式 | 说明 |
|---|---|---|
| OpenAI（`gpt-realtime*`） | WebSocket 中继、WebRTC 信令转发 | 协议原样透传。 |
| Codex（ChatGPT 登录）语音 | WebRTC 信令转发（`POST /v1/live`）+ sideband 中继 | Codex 桌面端 / CLI 的语音聊天。 |
| 阿里云百炼 Qwen-Omni-Realtime（`qwen3.8-omni-flash-realtime`、`qwen3.5-omni-*-realtime`） | WebSocket 中继 + 协议翻译 | 正式版客户端无需改动，网关负责翻译百炼方言。 |
| 阿里云百炼 Qwen-ASR 实时识别（`qwen3-asr-flash-realtime`） | WebSocket 转写会话 | 正式版转写客户端由网关翻译；beta 协议客户端（即百炼自己的协议）原样转发。见[百炼实时语音识别](#百炼实时语音识别qwen-asr)。 |

WebRTC 通话的音频**由客户端直连 OpenAI**，网关只转发 SDP 信令并中继 sideband WebSocket。

## 接口

所有路由都在 `/v1` 下；base URL 以 `/codex` 结尾的 Codex 客户端还可以用 `/codex` 镜像（受 Codex 兼容开关控制，必须带真实 API Key）。

| 路由 | 用途 |
|---|---|
| `GET /v1/realtime?model=<model>` | WebSocket 会话（OpenAI 正式版协议） |
| `GET /v1/realtime?intent=transcription[&model=]` | 转写会话；不带 `model` 时用 `AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL`。百炼 Qwen-ASR 模型不管带不带 `intent` 都是转写会话 |
| `GET /v1/realtime?call_id=<id>` | 附着到经网关创建的 WebRTC 通话（sideband） |
| `GET /v1/live/{call_id}` | Codex v3 sideband |
| `POST /v1/live` | Codex WebRTC 建连（multipart 的 `sdp` + `session`） |
| `POST /v1/realtime/calls` | 标准 WebRTC 建连（`application/sdp`、multipart 或 JSON `{sdp, session}`） |
| `POST /v1/realtime/calls/{call_id}/hangup` | 挂断经 `/v1/realtime/calls` 在 OpenAI 渠道上创建的通话 |

不支持（返回带原因的 JSON 404）：`POST /v1/realtime/client_secrets`（会把上游临时凭证交给调用方、绕过计费）、`accept`/`reject`/`refer`（SIP 通话不经过网关）、`GET /v1/live?model=`（GPT-Live 的 WebSocket 传输）、已被 OpenAI 下线的 beta 接口 `POST /v1/realtime/sessions` 和 `/v1/realtime/transcription_sessions`，以及已下线的 beta 协议（带 `OpenAI-Beta: realtime=v1` 会收到 400）——百炼 Qwen-ASR 模型除外，它们自己的协议就是这个形状。

### 鉴权

用 `Authorization: Bearer <key>` 传 AxonHub 的 API Key。浏览器客户端可以用 OpenAI 的子协议写法：`Sec-WebSocket-Protocol: realtime, openai-insecure-api-key.<key>`，网关会在握手阶段把 key 摘掉，不进日志也不发给上游。浏览器必须提供 `realtime` 子协议。`server.cors` 未开启时，no-auth 主体只能在不带 `Origin` 或同源的情况下连接。

### 示例（openai-python，正式版）

```python
from openai import OpenAI

client = OpenAI(api_key="ah-...", base_url="http://localhost:8090/v1")

with client.realtime.connect(model="qwen3.5-omni-plus-realtime") as conn:
    conn.session.update(session={
        "type": "realtime",
        "output_modalities": ["audio"],
        "audio": {"input": {"format": {"type": "audio/pcm", "rate": 24000}}},
    })
    conn.input_audio_buffer.append(audio=b64_pcm_24k_chunk)
    ...
```

`openai-node` 写死了 `wss://`，所以 AxonHub 需要有 HTTPS 地址。

## 渠道配置

模型的路由方式和聊天模型一样：在"模型"页做关联，或者把它写进渠道的模型列表并开启"找不到时回退到渠道"。哪些渠道能提供 realtime：

| 渠道类型 | realtime 目标 |
|---|---|
| `openai`、`openai_responses` | 从渠道 base URL 推导：`wss://<base>/realtime` |
| `bailian`、`bailian_responses`、`bailian_anthropic` | `wss://dashscope[-intl].aliyuncs.com/api-ws/v1/realtime`（公共域名；我们实测 `qwen3.8` 在这里也能用，见下面的业务空间域名说明） |
| `codex`、`fenno`（ChatGPT 登录或 PAT 中继） | WebRTC 通话（`POST /v1/live`、`/v1/realtime/calls`）走 Codex live 地址；WebSocket 会话只有在显式配置了 `openai/realtime` 端点并且有 API Key（PAT 中继）时才支持。OAuth 凭证永远不提供 WebSocket 会话 |
| 其它类型 | 必须显式配置端点 |

显式端点优先于推导：打开渠道的**端点**对话框，添加 `openai/realtime`，base URL 填 **https**（不要填 `wss://`，网关会自己换协议），路径可选（默认 `/realtime`）。

**百炼业务空间域名。** 百炼文档把 `qwen3.8-omni-flash-realtime` 放在业务空间专属域名下；我们 2026-10-22 的实测里公共域名也能直接服务它。如果你的账号要求用业务空间域名，就加一条显式端点，base URL 填 `https://<WorkspaceId>.cn-beijing.maas.aliyuncs.com/api-ws/v1`（或 `ap-southeast-1`），如需工作空间头，用渠道的 header 覆盖加 `X-DashScope-WorkSpace`。

**百炼免费额度。** 某个模型的免费额度用完、且账号处于"仅使用免费额度"模式时，百炼会在第一条 `session.update` 之后立刻以 1007 关闭会话；客户端会收到一条带百炼原话的 `error` 事件，会话记录里也会记下这个原因。到百炼控制台关闭该模式（或等额度重置）即可继续。

不要在模型协议选择里把聊天模型固定到 `openai/realtime`：这个格式只服务 `/v1/realtime`，聊天请求会直接报错。

### 百炼翻译说明

- 正式版客户端发送 24 kHz PCM。`qwen3.8-omni-*` 和 `qwen3.5-omni-*` 直接接受 24 kHz（3.8 已实测）；更老的 omni 模型由网关重采样到 16 kHz。输出始终是 24 kHz PCM。
- 网关在第一条 `session.update` 里总会设置一个属于该模型家族的音色（3.8/3.5 用 `Tina`，老模型用 `Cherry`），除非客户端选了合法音色：百炼自己的默认音色 `Chelsie` 在 `qwen3.8` 生成第一条回复时会被拒绝。
- 收尾时只对 `qwen3.5` 和老模型发送 `session.finish`；`qwen3.8` 不认这个事件，网关直接关闭连接。
- `output_modalities: ["audio"]` 发给百炼时变成 `["text","audio"]`，文本事件以 `response.output_text.*` 转发。
- 音色必须来自模型所属家族的列表（qwen3.8、qwen3.5 或老模型；例如 `Tina`、`Cindy` 在 3.8 和 3.5 上都有，`Cherry` 只有老模型有）。不认识的 GA 音色和别的家族的音色会被丢弃。
- 百炼不支持：`audio/pcmu`、`audio/pcma`（会话以 1003 关闭）、`noise_reduction`、`speed`、`eagerness`、`tracing`、`truncation`（记日志后忽略）。`session.prompt` 会让整条 `session.update` 失败并返回 `error` 事件，请改用 `instructions`。`tool_choice: "none"` 会生效，只有对象形式会被忽略。
- `conversation.item.truncate` 由网关本地应答；`conversation.item.retrieve` 从网关的 item 缓存应答。`input_image` 内容会转成百炼的图片缓冲，附着到下一次音频提交。

## 百炼实时语音识别（Qwen-ASR）

百炼渠道上的 `qwen3-asr-flash-realtime`（及其带日期的快照）提供纯转写会话：音频进、文字出，没有语音回答。把这个模型加进百炼渠道的模型列表即可。支持两种客户端：

**OpenAI 正式版转写客户端。** 连接 `/v1/realtime?intent=transcription&model=qwen3-asr-flash-realtime`（也可以不带 `model`，把 `AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL` 设成 `qwen3-asr-flash-realtime`，或者用模型关联把客户端的转写模型映射过去）。网关负责翻译：

- 客户端按正式版要求发 24 kHz PCM，网关重采样到 16 kHz。
- 百炼一个会话**只接受一次**配置，而且必须在音频之前（第二次会直接断开）。所以网关自己应答客户端的每条 `session.update`，在第一段音频之前把合并后的配置一次性发给百炼，并把这段音频压到百炼确认之后再发。音频开始后再发 `session.update` 会收到 `error` 事件（code `unsupported_on_upstream`），要改配置请重连。
- 会翻译的字段：`audio.input.turn_detection`（`server_vad` 的 `threshold`、`prefix_padding_ms`、`silence_duration_ms`，静音时长限制在 200–6000 毫秒；`semantic_vad` 会按 `eagerness` 换成对应静音时长的 `server_vad`；`null` 是手动模式）以及 `audio.input.transcription.language` / 只有一个元素的 `languages`（ISO 639-1、ISO 639-3 和带地区的 zh 代码都会规范化，不认识的代码改为自动识别语种）。会丢弃：`prompt`、`keywords`、`delay`、`noise_reduction`、`include`。客户端填的转写 `model` 会原样回显，但上游实际用的是渠道选中的模型。
- 百炼推送的是不断累积、还会改写的识别假设；网关把它转成只追加的 `conversation.item.input_audio_transcription.delta`（只发两次假设一致的文字），`completed` 事件里始终是最终的权威文本。
- `input_audio_buffer.clear` 会收到 `error`（百炼没法清空缓冲区）。`input_audio_buffer.commit` 在手动模式下正常转发；在 `server_vad` 模式下会结束正在说的那句话（适合最后一句后面没有静音的情况），没有正在说的话时返回 `input_audio_buffer_commit_empty` 错误。`response.*` 和 `conversation.item.*` 事件会被拒绝：转写会话没有对话。
- 用量：百炼每次完成都报整个会话的累计值；网关按每句话的差值上报和计费（`usage.type` 为 `tokens`）。

**beta 协议客户端（百炼自己的协议）。** 发送 `OpenAI-Beta: realtime=v1`（或子协议 `openai-beta.realtime-v1`）的客户端——比如照百炼示例写的代码——连接 `/v1/realtime?model=qwen3-asr-flash-realtime`，直接说百炼的协议（扁平的 `session.update`、16 kHz 音频、`conversation.item.input_audio_transcription.text`、`session.finish`）。网关原样转发每一帧，只把模型名回显成请求的名字，并按百炼累计用量的差值给每句话计费。其它模型的 beta 客户端仍然被拒绝（400）。

关闭时网关会发送 `session.finish`（客户端已经发过就不再发），并最多等待 `AXONHUB_REALTIME_RESPONSE_DRAIN`，让百炼把正在说的那句话转写完，保证它的用量被记录。

## Codex 语音（Codex 桌面端 / CLI）

1. 把 Codex 指向 AxonHub（`base_url = "https://host/v1"` 或 `https://host/codex`）。
2. 把 Codex 语音模型（v3 是 `gpt-live-1-codex`，v1 是 `gpt-realtime-1.5`）加进 Codex 渠道的模型列表，否则选不到。
3. **Codex ≥ 0.146 默认把 sideband WebSocket 发到 `api.openai.com`**（带的是 AxonHub 的 Key，会失败）。在 `~/.codex/config.toml` 根级加：

   ```toml
   # base_url = "https://host/v1" 的填法
   experimental_realtime_ws_base_url = "https://host/v1"
   # base_url = "https://host/codex" 的填法
   experimental_realtime_ws_base_url = "https://host/codex/realtime"
   ```

网关把 `POST /v1/live` 用渠道的 OAuth 身份转发到 ChatGPT 后端（`/realtime/calls?intent=quicksilver&architecture=avas`），并把 `GET /v1/live/{call_id}` 中继到 `wss://api.openai.com/v1/live/{call_id}`。只有创建通话的那把 API Key 可以附着它的 sideband。

## 记账

- 上游每个 `response.done` 记成一条已完成的请求和对应的用量记录（所以 Requests 配额按**回合**计数，不按会话）；输入转写的用量单独按转写模型记。
- 会话结束时写一条会话摘要请求（事件数、回合数、时长、关闭原因）。握手失败也会留下一条失败的请求。
- 每个 WebRTC 通话记一条请求和一条 0 token 的用量。OpenAI 渠道上的通话，网关还会自己开一个只读 sideband（`AXONHUB_REALTIME_MONITOR_CALLS`）来捕获 `response.done` 的用量。
- 音频 token 会存进用量记录，但**按模型的文字价计费**；记得给 realtime 和转写模型配价格，否则成本为空。
- 每 30 秒（可用 `AXONHUB_REALTIME_QUOTA_CHECK_INTERVAL` 调整）复查一次 API Key 配额、渠道配额、渠道状态和 API Key 本身；不通过就发一个 GA `error` 事件并以 1008 关闭。
- 客户端引起的结束（客户端挂断、客户端连接丢失、客户端升级失败）记为已取消，不算渠道失败，所以不会触发渠道自动禁用。
- 服务端关闭时，进行中的会话最多有 ResponseDrain + CloseGrace（`AXONHUB_REALTIME_RESPONSE_DRAIN` + `AXONHUB_REALTIME_CLOSE_GRACE`）的时间来收尾并落库用量；客户端收到关闭码 1012。

## 配置（环境变量）

| 变量 | 默认值 | 含义 |
|---|---|---|
| `AXONHUB_REALTIME_CONNECT_BUDGET` | `8s` | 升级客户端前，选渠道 + 上游握手的总预算 |
| `AXONHUB_REALTIME_HANDSHAKE_TIMEOUT` | `5s` | 单个候选渠道的握手超时 |
| `AXONHUB_REALTIME_MAX_SESSION_DURATION` | OpenAI `60m`，百炼 `119m` | 会话时长上限（以 1000 关闭） |
| `AXONHUB_REALTIME_IDLE_TIMEOUT` | `10m` | 两个方向都没有数据帧 |
| `AXONHUB_REALTIME_PING_INTERVAL` | `30s` | 两侧 socket 的心跳间隔 |
| `AXONHUB_REALTIME_WRITE_TIMEOUT` | `10s` | 单次 WebSocket 写入的超时 |
| `AXONHUB_REALTIME_CLOSE_GRACE` | `4s` | 强制关闭前等待关闭握手的时间 |
| `AXONHUB_REALTIME_RESPONSE_DRAIN` | `5s` | 客户端在回复中途挂断后，继续读上游等 `response.done` 的时间 |
| `AXONHUB_REALTIME_MAX_MESSAGE_SIZE` | `16777216` | 单帧读取上限 |
| `AXONHUB_REALTIME_WRITE_QUEUE_FRAMES` | `16` | 单个 socket 写队列缓冲的帧数，满了生产方会阻塞 |
| `AXONHUB_REALTIME_GATE_TIMEOUT` | `10s` | 方言初始化阶段的超时（期间客户端帧先排队） |
| `AXONHUB_REALTIME_MAX_SESSIONS` | `256` | 并发会话数（含 sideband 和监听） |
| `AXONHUB_REALTIME_MAX_SESSIONS_PER_KEY` | `8` | 每把 API Key 的并发数 |
| `AXONHUB_REALTIME_CALL_BINDING_TTL` | `2h` | WebRTC 通话 id 的记忆时长 |
| `AXONHUB_REALTIME_CALL_BINDINGS_PER_PROJECT` | `1024` | 每个项目最多记住的 WebRTC 通话数 |
| `AXONHUB_REALTIME_QUOTA_CHECK_INTERVAL` | `30s` | 会话内复查配额、渠道状态和 API Key 的间隔 |
| `AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL` | `gpt-4o-transcribe` | `intent=transcription` 不带 `model` 时的模型 |
| `AXONHUB_REALTIME_MONITOR_CALLS` | `true` | 网关自建的 OpenAI 通话用量监听 |
| `AXONHUB_CODEX_LIVE_FORWARD_ATTESTATION` | `true` | 是否把 `x-oai-attestation` 转发给 Codex 后端 |
| `AXONHUB_REALTIME_CALL_BODY_LIMIT` | `1048576` | 建连请求体上限 |

## 已知限制

- 除百炼 Qwen-ASR 模型外，beta 协议客户端都会被拒绝；音频 token 按文字价计费。
- 百炼 Qwen-ASR 转写会话在第一段音频之后不能再改配置，不能清空音频缓冲区，也没有 `prompt`/`keywords`/降噪。
- 一个实时会话在整个时长内占用渠道的一个并发槽（`MaxConcurrent`）。
- 会话中途不做渠道故障转移；渠道中途被禁用，会话在下一次检查时结束。
- 通话绑定只在单进程内（多副本部署不共享）。
- 客户端 sideband 不经过网关的 WebRTC 通话只记一条 0 用量的请求。
- Codex 语音的 token 用量上游不提供；按时长计费的转写不计费。
- 浏览器直接发起 WebRTC 需要开启 `server.cors.enabled` 并允许页面的 origin。
- OpenAI 和 Codex 链路只用假上游服务器验证过；百炼用真实 Key 联调过。
