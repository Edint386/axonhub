# Bailian File Transcription (`/v1/audio/transcriptions`)

> This feature is specific to the `custom` branch (Edint386/axonhub). It is not part of upstream AxonHub.

## Overview

Alibaba Bailian (DashScope) has no OpenAI-compatible `/audio/transcriptions` endpoint, so the
upstream OpenAI audio path cannot serve Bailian speech models. On the `custom` branch, a Bailian
channel serves these models on the standard OpenAI file-transcription API by calling Bailian's
native APIs itself:

| Model | How the gateway calls Bailian | Limits | Notes |
|---|---|---|---|
| `qwen-audio-3.1-asr-flash` | One synchronous call | 5 min of audio, file ≤ 26 MB | Word timestamps. Slow: about 2× the audio length. |
| `qwen-audio-3.1-asr-flash-message` | One task on the inference WebSocket | 10 min by default | Tuned for short voice messages. |
| `qwen-audio-3.1-asr-flash-filetrans` | Async job: upload → submit → poll → download | 12 h, file ≤ 1 GB | Sentence and word timestamps, speaker diarization. |
| `paraformer-v2` | Async job | 12 h, file ≤ 1 GB | Beijing region only. Languages zh, en, ja, yue, ko, de, fr, ru. |
| `paraformer-8k-v2` | Async job | 12 h, file ≤ 1 GB | 8 kHz telephone audio, Chinese. |

For realtime (WebSocket) speech recognition and translation, see the [Realtime API](realtime.md).

## Setup

1. Add the models to a Bailian (`bailian` type) channel's model list. No endpoint configuration is
   needed: the gateway uses the native API on the channel's host (the base URL without
   `/compatible-mode/v1`, `https://dashscope.aliyuncs.com` by default). An explicit
   `openai/audio_transcriptions` endpoint on a Bailian, Bailian Responses or Bailian Anthropic
   channel is honoured too, and so is a user-configured chat endpoint with another region's host.
2. Give each model a price:
   - `paraformer-v2` / `paraformer-8k-v2`: a `duration_seconds` price item (price **per second**).
   - `qwen-audio-*`: token prices **or** a `duration_seconds` price, not both (both are recorded, so
     both would be charged).
3. For files larger than 26 MB, raise the reverse proxy's body limit (for example nginx
   `client_max_body_size 1100m;`) and see [Large files](#large-files).

## Request

`multipart/form-data`, as for OpenAI:

| Field | Meaning |
|---|---|
| `model` | One of the models above |
| `file` | The audio file |
| `url` / `file_url` | Instead of `file`: a public `http(s)` URL that Bailian downloads itself. Supported for `qwen-audio-3.1-asr-flash` (the URL path must end in an audio extension such as `.mp3` or `.wav`) and the three async models, not for `-message`. |
| `response_format` | `json` (default), `text`, `srt`, `vtt`, `verbose_json`, `diarized_json` |
| `language` | Sent as Bailian's language hint. Codes a model does not support are dropped (automatic detection). |
| `prompt` | Context text for `qwen-audio-3.1-asr-flash` and `-message` |
| `timestamp_granularities[]` | `word` adds `words` to `verbose_json` |
| `stream` | `true` returns `transcript.text.delta` events and a final `transcript.text.done` (with usage); only `json` and `text` formats |
| `sample_rate` | Only for raw `.pcm` files sent to `-message` (default 16000) |

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

`diarized_json` turns on Bailian speaker diarization; speakers are labelled `A`, `B`, …
`verbose_json` reports `language` as an empty string when no `language` was sent (Bailian does not
report the detected language).

## Long audio

The async models upload the file to Bailian's temporary storage, submit a task and poll it until
the transcript is ready, all inside one HTTP request.

- Use `stream=true` for audio longer than about 10 minutes: the SSE response starts as soon as
  Bailian has accepted the audio, and the gateway sends `: keep-alive` comments until the
  transcript arrives. Without streaming the client sees nothing until the end, so its timeout
  must cover the whole job.
- Every job must finish within the server's `llm_request_timeout` (default 600 s). Bailian usually
  finishes a file in minutes but does not guarantee it; for multi-hour files raise
  `llm_request_timeout` (for example to `1800s`).
- The pipeline's per-attempt response timeouts (`non_stream_response_timeout` /
  `stream_first_event_timeout`) do not apply to these models (async jobs, `-message` and the
  slow `qwen-audio-3.1-asr-flash`); only the request timeout bounds them.

## Large files

For `qwen-audio-3.1-asr-flash-filetrans`, `paraformer-v2` and `paraformer-8k-v2`, the gateway accepts
files up to `AXONHUB_BAILIAN_ASR_MAX_UPLOAD_MB` (default 1024). It writes the upload to disk as it
arrives and streams it from disk to Bailian, so the audio is never held in memory. The temp file
has no name on disk and is released when the request ends, also if the process crashes. All
other models keep the upstream 26 MB limit. Use the exact model name: an alias gets the normal
limits.

- Send the `model` field **before** the file (the OpenAI Python SDK does). If a file larger than
  64 MB arrives before the model field, the gateway cannot keep it and answers `413`
  (`model_after_large_file`).
- Large uploads in progress: at most `AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY` (default 2) in
  total and `AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY` (default 1) per API key; others wait up to 30 s,
  then get `429` with `Retry-After`. A slot is freed as soon as the audio has reached Bailian, not
  when the transcript is ready.
- All temp files together are limited to `AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB` (default 4096);
  over that, `429` (`upload_storage_busy`).
- A client that stops sending for `AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS` (default 120) or sends
  less than 1 MB per minute is cut off with `408`.
- An API key whose profile does not allow the model gets `422` before anything is stored.

## Retries and duplicate protection

A Bailian job is billed once it has been submitted, so the gateway never submits a job twice by
itself: once a task exists, a failure is returned to the client without a retry, channel switch or
model fallback. Failures before the submit are retried as usual.

If a request ends before its task has finished (for example at `llm_request_timeout`), the task
keeps running at Bailian. Sending the same request again (same file, model and options, with the
same API key) within 30 minutes picks up that task instead of paying again, on any channel that
uses the same Bailian endpoint. This does not apply to audio given by URL, which always starts a
new task. If the answer to a submit was lost (`502 bailian_submit_unconfirmed`), the same upload
is refused with `409 bailian_submit_pending` for 10 minutes, because the first task may still be
running. Clients should use a long timeout and turn off automatic retries (`max_retries=0` in the
OpenAI SDKs).

Transcription jobs do not affect the channel's latency scores, so long jobs do not push chat
traffic away from a Bailian channel.

## Billing and logs

- Every request records the billed audio seconds (`duration_seconds`); `qwen-audio-*` models also
  record prompt and completion tokens.
- Request and execution logs never contain the audio. A URL is stored only as its host and file
  extension (for example `https://host/<redacted>.mp3`), so signed links and tokens are not kept.
- Known gap: if a job finishes at Bailian after its request has already ended and nobody sends the
  request again, the gateway never records that job's usage.

## Configuration (environment variables)

| Variable | Default | Meaning |
|---|---|---|
| `AXONHUB_BAILIAN_ASR_MAX_UPLOAD_MB` | `1024` | Largest upload for the async models |
| `AXONHUB_BAILIAN_ASR_UPLOAD_CONCURRENCY` | `2` | Large uploads in progress at once; others wait up to 30 s, then get `429` with `Retry-After` |
| `AXONHUB_BAILIAN_ASR_UPLOAD_PER_KEY` | `1` | Large uploads one API key may have in progress at once |
| `AXONHUB_BAILIAN_ASR_SPOOL_DIR` | system temp dir | Where large uploads are written; use a disk directory if `/tmp` is a RAM disk |
| `AXONHUB_BAILIAN_ASR_SPOOL_BUDGET_MB` | `4096` | Total size of all temp files at once |
| `AXONHUB_BAILIAN_ASR_BODY_IDLE_SECONDS` | `120` | How long a large upload may stall before it is cut off (`408`) |
| `AXONHUB_BAILIAN_ASR_MESSAGE_MAX_SECONDS` | `600` | Longest audio for `-message` (compressed files are limited by size, about 40 KB per second) |
| `AXONHUB_BAILIAN_ASR_MESSAGE_PACE` | `4` | How fast `-message` audio is sent, in multiples of real time (`0` = no pause) |

## Known limitations

- `/v1/audio/translations` and `/v1/audio/speech` are not served for Bailian.
- For async models, `stream=true` sends the text only after the whole job has finished (the
  keep-alive comments only keep the connection open).
- `-message` accepts WAV, PCM, mp3, ogg/opus, ogg/speex, aac and amr-nb; WAV and PCM are converted to
  16 kHz mono by the gateway, compressed formats are sent as they are.
- The resume cache is per gateway process and per API key; it is lost on restart.
