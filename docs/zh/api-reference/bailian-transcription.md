# 百炼文件转写（`/v1/audio/transcriptions`）

> 本功能仅存在于 `custom` 分支（Edint386/axonhub），上游 AxonHub 没有。

## 概述

阿里云百炼（DashScope）没有 OpenAI 兼容的 `/audio/transcriptions` 接口，所以上游的 OpenAI 音频通路用不了百炼的语音模型。在 `custom` 分支上，百炼渠道会自己调用百炼的原生接口，让下面这些模型可以用标准的 OpenAI 文件转写接口调用：

| 模型 | 网关怎么调百炼 | 上限 | 说明 |
|---|---|---|---|
| `qwen-audio-3.1-asr-flash` | 一次同步调用 | 音频 5 分钟、文件 26 MB 以内 | 带逐词时间戳。比较慢：大约要音频时长的 2 倍。 |
| `qwen-audio-3.1-asr-flash-message` | 在 inference WebSocket 上跑一个任务 | 默认 10 分钟 | 为短语音消息优化。 |
| `qwen-audio-3.1-asr-flash-filetrans` | 异步任务：上传 → 提交 → 轮询 → 下载结果 | 12 小时、文件 1 GB 以内 | 句级和词级时间戳，支持区分说话人。 |
| `paraformer-v2` | 异步任务 | 12 小时、文件 1 GB 以内 | 只有北京地域。语言 zh、en、ja、yue、ko、de、fr、ru。 |
| `paraformer-8k-v2` | 异步任务 | 12 小时、文件 1 GB 以内 | 8 kHz 电话录音，中文。 |

实时（WebSocket）语音识别和翻译见 [Realtime API](realtime.md)。

## 配置

1. 把模型加进百炼（`bailian` 类型）渠道的模型列表即可，不用配端点：网关会用渠道所在主机上的原生接口（base URL 去掉 `/compatible-mode/v1`，默认 `https://dashscope.aliyuncs.com`）。百炼、百炼 Responses、百炼 Anthropic 渠道上显式配置的 `openai/audio_transcriptions` 端点也会生效；用户自定义的、指向其他地域主机的聊天端点同样适用。
2. 给每个模型配价格：
   - `paraformer-v2` / `paraformer-8k-v2`：配 `duration_seconds` 计费项（**每秒**单价）。
   - `qwen-audio-*`：配 token 单价**或** `duration_seconds` 单价，二选一（两样都会记录，都配会收两次钱）。
3. 文件超过 26 MB 时，要放开反向代理的请求体上限（比如 nginx `client_max_body_size 1100m;`），并见[大文件](#大文件)。

## 请求

`multipart/form-data`，和 OpenAI 一样：

| 字段 | 含义 |
|---|---|
| `model` | 上表中的模型 |
| `file` | 音频文件 |
| `url` / `file_url` | 代替 `file`：公网 `http(s)` 链接，由百炼自己下载。支持 `qwen-audio-3.1-asr-flash`（链接路径要以 `.mp3`、`.wav` 等音频扩展名结尾）和三个异步模型，`-message` 不支持。 |
| `response_format` | `json`（默认）、`text`、`srt`、`vtt`、`verbose_json`、`diarized_json` |
| `language` | 作为百炼的语言提示发送。模型不支持的代码会被去掉（改为自动识别）。 |
| `prompt` | 给 `qwen-audio-3.1-asr-flash` 和 `-message` 的上下文文字 |
| `timestamp_granularities[]` | 填 `word` 时 `verbose_json` 带上 `words` |
| `stream` | `true` 时返回 `transcript.text.delta` 事件和最后的 `transcript.text.done`（带用量）；只支持 `json` 和 `text` 格式 |
| `sample_rate` | 只用于发给 `-message` 的裸 `.pcm` 文件（默认 16000） |

```bash
curl https://<gateway>/v1/audio/transcriptions -H "Authorization: Bearer $KEY" \
  -F model=paraformer-v2 -F file=@meeting.mp3 -F response_format=srt
```

```python
from openai import OpenAI

client = OpenAI(api_key="ah-...", base_url="https://<gateway>/v1", timeout=1800, max_retries=0)
with open("meeting.mp3", "rb") as f:
    text = client.audio.transcriptions.create(model="qwen-audio-3.1-asr-flash-filetrans", file=f)
```

`diarized_json` 会打开百炼的说话人区分，说话人标成 `A`、`B`……；没有传 `language` 时，`verbose_json` 的 `language` 是空字符串（百炼不返回识别出的语言）。

## 长音频

异步模型会把文件传到百炼的临时存储、提交任务、轮询直到出结果，全部在一次 HTTP 请求里完成。

- 超过约 10 分钟的音频建议用 `stream=true`：百炼一接收音频，SSE 响应就开始，网关会一直发 `: keep-alive` 直到结果出来。不用流式时客户端到最后才收到内容，所以超时时间要覆盖整个任务。
- 每个任务都必须在服务器的 `llm_request_timeout`（默认 600 秒）内完成。百炼通常几分钟处理完一个文件，但不保证；几小时的文件请调大 `llm_request_timeout`（比如 `1800s`）。
- 流水线的单次超时（`non_stream_response_timeout` / `stream_first_event_timeout`）对这些模型（异步任务、`-message` 和比较慢的 `qwen-audio-3.1-asr-flash`）不生效，只受请求总超时限制。

## 大文件

`qwen-audio-3.1-asr-flash-filetrans`、`paraformer-v2`、`paraformer-8k-v2` 可以上传最大 `AXONHUB_BAILIAN_ASR_MAX_UPLOAD_MB`（默认 1024）MB 的文件。网关边收边写到磁盘，再从磁盘流式传给百炼，音频不会整个读进内存。临时文件在磁盘上没有文件名，请求结束就释放，进程崩溃也会自动释放。其他模型仍是上游的 26 MB 上限。请用准确的模型名：用别名会按普通上限处理。

- 请把 `model` 字段放在文件**前面**（OpenAI Python SDK 就是这样）。如果超过 64 MB 的文件先于 model 字段到达，网关没法保留它，会回 `413`（`model_after_large_file`）。
- 同时进行的大文件上传：全局最多 `AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY`（默认 2）个，每个 API Key 最多 `AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY`（默认 1）个；其余最多等 30 秒，之后回带 `Retry-After` 的 `429`。音频一传到百炼就释放名额，不用等转写完成。
- 所有临时文件合计不超过 `AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB`（默认 4096）；超出回 `429`（`upload_storage_busy`）。
- 客户端停止发送超过 `AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS`（默认 120）秒，或者每分钟发不到 1 MB，会被断开并回 `408`。
- API Key 的配置不允许这个模型时，什么都不存、直接回 `422`。

## 重试和防重复提交

百炼的任务一提交就会计费，所以网关自己绝不会把同一个任务提交两次：任务一旦提交，出错时直接返回给客户端，不重试、不换渠道、不换模型。提交之前的失败照常重试。

如果请求在任务完成之前就结束了（比如到了 `llm_request_timeout`），任务仍会在百炼那边继续跑。30 分钟内用同一个 API Key 把同样的请求（同一个文件、模型和参数）再发一次，会接着用那个任务，不会再花钱；落到任何使用同一百炼地址的渠道都可以。用链接（URL）给的音频不适用，每次都会新建任务。如果提交后没收到百炼的答复（`502 bailian_submit_unconfirmed`），10 分钟内同样的上传会被拒绝（`409 bailian_submit_pending`），因为第一次的任务可能还在跑。所以客户端应该设置较长的超时，并关掉自动重试（OpenAI SDK 里设 `max_retries=0`）。

转写任务不影响渠道的延迟评分，所以长任务不会把聊天流量挤离百炼渠道。

## 计费和日志

- 每个请求都会记录计费的音频秒数（`duration_seconds`）；`qwen-audio-*` 模型还会记录输入、输出 token。
- 请求日志和执行日志里不保存音频。链接只保存主机名和文件扩展名（比如 `https://host/<redacted>.mp3`），带签名的链接和 token 不会被存下来。
- 已知缺口：如果任务在请求结束后才在百炼那边跑完，而且之后没有人重发同样的请求，网关就不会记下这个任务的用量。

## 配置（环境变量）

| 变量 | 默认值 | 含义 |
|---|---|---|
| `AXONHUB_BAILIAN_ASR_MAX_UPLOAD_MB` | `1024` | 异步模型的最大上传大小 |
| `AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY` | `2` | 同时进行的大文件上传数；其余最多等 30 秒，之后返回带 `Retry-After` 的 `429` |
| `AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY` | `1` | 每个 API Key 同时进行的大文件上传数 |
| `AXONHUB_BAILIAN_ASR_SPOOL_DIR` | 系统临时目录 | 大文件写到哪里；`/tmp` 是内存盘时请改成磁盘目录 |
| `AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB` | `4096` | 所有临时文件合计的上限 |
| `AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS` | `120` | 大文件上传最多可以停顿多少秒，超过就断开（`408`） |
| `AXONHUB_BAILIAN_ASR_MESSAGE_MAX_SECONDS` | `600` | `-message` 的最长音频（压缩格式按大小限制，约每秒 40 KB） |
| `AXONHUB_BAILIAN_ASR_MESSAGE_PACE` | `4` | `-message` 发送音频的速度，单位是实时速度的倍数（`0` 表示不停顿） |

## 已知限制

- 百炼不提供 `/v1/audio/translations` 和 `/v1/audio/speech`。
- 异步模型用 `stream=true` 时，文字要等整个任务完成后才发出（keep-alive 只是保持连接）。
- `-message` 支持 WAV、PCM、mp3、ogg/opus、ogg/speex、aac、amr-nb；WAV 和 PCM 由网关转成 16 kHz 单声道，压缩格式原样发送。
- 续用任务的缓存在网关进程内、按 API Key 区分，重启后丢失。
