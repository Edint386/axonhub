# Realtime API (voice) Reference

> This feature is specific to the `custom` branch (Edint386/axonhub). It is not part of upstream AxonHub.

## Overview

AxonHub relays the **OpenAI Realtime API** (GA protocol) so that voice clients can talk to
upstream realtime models through the gateway's authentication, channel selection, quotas and
usage logging. These upstreams are supported:

| Upstream | How | Notes |
|---|---|---|
| OpenAI (`gpt-realtime*`) | WebSocket relay, WebRTC call signalling | Protocol is passed through unchanged. |
| Codex (ChatGPT login) voice | WebRTC call signalling (`POST /v1/live`) + sideband relay | Codex desktop/CLI voice chat. |
| Alibaba Bailian Qwen-Omni-Realtime (`qwen3.8-omni-flash-realtime`, `qwen3.5-omni-*-realtime`) | WebSocket relay with protocol translation | GA clients work unchanged; the gateway translates the Bailian dialect. |
| Alibaba Bailian Qwen-ASR realtime (`qwen3-asr-flash-realtime`) | WebSocket transcription sessions | GA transcription clients are translated; beta-protocol clients (Bailian's own protocol) are relayed unchanged. See [Bailian real-time speech recognition](#bailian-real-time-speech-recognition-qwen-asr). |
| Alibaba Bailian Paraformer realtime (`paraformer-realtime-v2`, `paraformer-realtime-8k-v2`) | WebSocket transcription sessions | GA transcription clients are translated to the Bailian inference protocol (`run-task` / binary PCM / `finish-task`). Beta-protocol clients are rejected. See [Bailian Paraformer realtime](#bailian-paraformer-realtime). |
| Alibaba Bailian Qwen-Audio ASR realtime (`qwen-audio-3.1-asr-flash-streaming`, `qwen-audio-3.1-asr-flash-message`) | WebSocket transcription sessions | Same inference protocol and behaviour as Paraformer. See [Bailian Qwen-Audio ASR realtime](#bailian-qwen-audio-asr-realtime). |
| Alibaba Bailian Qwen-LiveTranslate (`qwen3.8-livetranslate-flash-realtime`) | WebSocket translation sessions (`/v1/realtime/translations`) | OpenAI realtime translation clients are translated to the Bailian protocol. See [Realtime translation](#realtime-translation-v1realtimetranslations). |

For non-realtime file transcription on Bailian (`POST /v1/audio/transcriptions`, including long
audio), see [Bailian file transcription](bailian-transcription.md).

Audio media for WebRTC calls flows **directly between the client and OpenAI**; the gateway only
forwards the SDP signalling and relays the sideband WebSocket.

## Endpoints

All routes exist under `/v1` and, for Codex clients whose base URL ends in `/codex`, under `/codex`
(the `/codex` mirror is gated by the Codex compatibility switch and requires a real API key).

| Route | Purpose |
|---|---|
| `GET /v1/realtime?model=<model>` | WebSocket session (OpenAI GA protocol) |
| `GET /v1/realtime?intent=transcription[&model=]` | Transcription session; `AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL` is used when `model` is absent. A Bailian Qwen-ASR, Paraformer or Qwen-Audio ASR realtime model is always a transcription session, with or without `intent` |
| `GET /v1/realtime/translations?model=<model>` | Realtime translation session (OpenAI realtime translation protocol), Bailian Qwen-LiveTranslate only |
| `GET /v1/realtime?call_id=<id>` | Sideband attach to a WebRTC call created through the gateway |
| `GET /v1/live/{call_id}` | Codex v3 sideband attach |
| `POST /v1/live` | Codex WebRTC call create (multipart `sdp` + `session`) |
| `POST /v1/realtime/calls` | Standard WebRTC call create (`application/sdp`, multipart or JSON `{sdp, session}`) |
| `POST /v1/realtime/calls/{call_id}/hangup` | End a call created through `/v1/realtime/calls` on an OpenAI channel |

Not supported (answered with a JSON 404 explaining why): `POST /v1/realtime/client_secrets` and
`POST /v1/realtime/translations/client_secrets` (would hand out an unmetered upstream credential),
`POST /v1/realtime/translations/calls` (translation over WebRTC), `accept`/`reject`/`refer` (SIP calls never
pass through the gateway), `GET /v1/live?model=` (GPT-Live websocket transport), the removed beta
`POST /v1/realtime/sessions` and `/v1/realtime/transcription_sessions`, and the removed beta
protocol (`OpenAI-Beta: realtime=v1` is rejected with 400) for every model except Bailian Qwen-ASR
models, whose own protocol is that shape. Paraformer realtime models reject the beta protocol.

### Authentication

Send the AxonHub API key as `Authorization: Bearer <key>`. Browser clients may use the OpenAI
subprotocol convention instead: `Sec-WebSocket-Protocol: realtime, openai-insecure-api-key.<key>`.
The key token is removed from the handshake before it reaches logs or the upstream. Browsers must
offer the `realtime` subprotocol. With the `server.cors` block disabled, the no-auth principal may
only connect without an `Origin` header or from the same origin.

### Example (openai-python, GA)

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

`openai-node` hard-codes `wss://`, so it needs an HTTPS AxonHub URL.

## Channel configuration

A model is routed exactly like a chat model: associate it with a channel (Models page) or list it
in the channel's model list with "fall back to channels" enabled. Which channels can serve realtime:

| Channel type | Realtime target |
|---|---|
| `openai`, `openai_responses` | Derived from the channel base URL: `wss://<base>/realtime` |
| `bailian`, `bailian_responses`, `bailian_anthropic` | `wss://dashscope[-intl].aliyuncs.com/api-ws/v1/realtime` (shared public host; `qwen3.8` worked there in our probe, see the workspace host note below) |
| `codex`, `fenno` (ChatGPT login or PAT relay) | WebRTC calls (`POST /v1/live`, `/v1/realtime/calls`) via the Codex live base; WebSocket sessions only with an explicit `openai/realtime` endpoint plus an API key (PAT relay). OAuth credentials never serve WebSocket sessions |
| Any other type | Requires an explicit endpoint |

An explicit endpoint overrides the derived target: open the channel's **Endpoints** dialog and add
`openai/realtime` with an **https** base URL (never `wss://`; the gateway converts the scheme) and
an optional path (default `/realtime`).

**Bailian workspace host.** Bailian documents `qwen3.8-omni-flash-realtime` on the
workspace-bound host; in our live probe (2026-10-22) the shared public host served it as well. If
your account requires the workspace host, add an explicit endpoint with base URL
`https://<WorkspaceId>.cn-beijing.maas.aliyuncs.com/api-ws/v1` (or `ap-southeast-1`) and send a
workspace header, if required, through the channel's header overrides (`X-DashScope-WorkSpace`).

**Bailian free tier.** When a model's free quota is used up and the account is in "use free tier
only" mode, Bailian ends every session right after the first `session.update` with close code
1007; the client receives an `error` event carrying Bailian's message and the session row records
it. Disable that mode in the Bailian console (or wait for the quota reset) to continue.

Do not pin a chat model's protocol to `openai/realtime` in the model-protocol selector: that
format only serves `/v1/realtime` and chat requests would fail.

### Bailian translation notes

- GA clients send 24 kHz PCM. `qwen3.8-omni-*` and `qwen3.5-omni-*` accept 24 kHz directly
  (verified live for 3.8); for older omni models the gateway resamples input to 16 kHz. Output is
  always 24 kHz PCM.
- The gateway always sets a voice from the model family's list in its initial `session.update`
  (`Tina` for 3.8/3.5, `Cherry` for legacy) unless the client chose a valid one: Bailian's own
  default voice `Chelsie` is rejected by `qwen3.8` when the first response is generated.
- `session.finish` is only sent to `qwen3.5` and legacy models at teardown; `qwen3.8` rejects it,
  so the gateway just closes the socket.
- `output_modalities: ["audio"]` is sent to Bailian as `["text","audio"]`; the text events are
  forwarded as `response.output_text.*`.
- Voices must come from the model family's list (qwen3.8, qwen3.5 or legacy; for example `Tina`
  and `Cindy` exist on 3.8 and 3.5, `Cherry` only on legacy models). Unknown GA voices and
  voices of another family are dropped.
- Not available on Bailian: `audio/pcmu`, `audio/pcma` (session is closed with 1003),
  `noise_reduction`, `speed`, `eagerness`, `tracing`, `truncation` (logged and ignored).
  `session.prompt` makes the whole `session.update` fail with an `error` event; send
  `instructions` instead. `tool_choice: "none"` is honoured; only the object form is ignored.
- `conversation.item.truncate` is acknowledged locally; `conversation.item.retrieve` is answered
  from the gateway's item cache. `input_image` content is converted to Bailian's image buffer and
  binds to the next audio commit.

## Bailian real-time speech recognition (Qwen-ASR)

`qwen3-asr-flash-realtime` (and its dated snapshots) on a Bailian channel serve transcription-only
sessions: audio in, text out, no spoken answer. Add the model to the Bailian channel's model list.
Two client styles work:

**OpenAI GA transcription clients.** Connect to
`/v1/realtime?intent=transcription&model=qwen3-asr-flash-realtime` (or omit `model` and set
`AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL=qwen3-asr-flash-realtime`, or map the client's
transcription model to it with a model association). The gateway translates the session:

- Client audio is 24 kHz PCM as GA requires; the gateway resamples it to 16 kHz.
- Bailian accepts only **one** session configuration per session, before any audio (a second one
  closes the session). The gateway therefore answers every client `session.update` itself and
  sends one merged configuration right before the first audio, holding that audio until Bailian
  acknowledges it. A `session.update` after audio started is answered with an `error` event
  (code `unsupported_on_upstream`); reconnect to change the configuration.
- Mapped fields: `audio.input.turn_detection` (`server_vad` with `threshold`,
  `prefix_padding_ms`, `silence_duration_ms` clamped to 200–6000 ms; `semantic_vad` becomes
  `server_vad` with a silence window chosen by `eagerness`; `null` is manual mode) and
  `audio.input.transcription.language` / a single-element `languages` (ISO 639-1, ISO 639-3 and
  regional zh codes are normalised; unknown codes fall back to automatic detection). Dropped:
  `prompt`, `keywords`, `delay`, `noise_reduction`, `include`. The transcription `model` the
  client names is echoed back but the upstream model is the one the channel selected.
- Bailian streams a cumulative, revisable hypothesis; the gateway turns it into append-only
  `conversation.item.input_audio_transcription.delta` events (only text two hypotheses agree on),
  and the `completed` event always carries the authoritative transcript.
- `input_audio_buffer.clear` is answered with an `error` (Bailian cannot clear its buffer).
  `input_audio_buffer.commit` is forwarded in manual mode; in `server_vad` mode it ends the
  utterance in progress (useful for a last sentence without trailing silence) and is answered
  with an `input_audio_buffer_commit_empty` error when no speech is in progress. `response.*` and
  `conversation.item.*` events are rejected: there is no conversation.
- Usage: Bailian reports running totals for the whole session on every completion; the gateway
  reports and bills each utterance's difference (`usage.type` is `tokens`).

**Beta-protocol clients (Bailian's own protocol).** Clients that send `OpenAI-Beta: realtime=v1`
(or the `openai-beta.realtime-v1` subprotocol), such as code written against Bailian's examples,
connect to `/v1/realtime?model=qwen3-asr-flash-realtime` and talk Bailian's protocol directly
(flat `session.update`, 16 kHz audio, `conversation.item.input_audio_transcription.text`,
`session.finish`). The gateway forwards every frame unchanged, only echoing the requested model
name, and bills each utterance from the difference of Bailian's running usage totals. Beta
clients are rejected (400) for any other model.

On close the gateway sends `session.finish` (unless the client already did) and waits up to
`AXONHUB_REALTIME_RESPONSE_DRAIN` for Bailian to transcribe the utterance in progress, so its usage
is recorded.

## Bailian Paraformer realtime

`paraformer-realtime-v2` (16 kHz) and `paraformer-realtime-8k-v2` (8 kHz) on a Bailian channel
serve transcription-only sessions over Bailian's **inference** WebSocket
(`wss://…/api-ws/v1/inference`), not the `/realtime` path used by Qwen-Omni and Qwen-ASR. Add
the model to the Bailian channel's model list. Connect with a GA transcription client:

`/v1/realtime?intent=transcription&model=paraformer-realtime-v2` (or omit `model` and set
`AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL`, or map the client's transcription model with a
model association). The gateway:

- Answers `session.created` itself (Paraformer is silent until `run-task`).
- Accepts 24 kHz PCM as GA requires and resamples to 16 kHz (or 8 kHz for the `8k` model).
  `audio/pcmu` and `audio/pcma` (G.711, 8 kHz) are accepted and decoded.
- On the first audio, sends one `run-task` with the merged configuration, holds further audio
  until `task-started`, then streams **binary** PCM frames. A later `session.update` is answered
  with an `error` (code `unsupported_on_upstream`); reconnect to change the configuration.
- Mapped fields: `audio.input.turn_detection` (`server_vad` with `silence_duration_ms` clamped to
  200–6000 ms, default 1300; `semantic_vad` is echoed as `semantic_vad` with `eagerness`, enables
  Bailian `semantic_punctuation_enabled`, and folds `eagerness` into `max_sentence_silence` the
  same way Qwen-ASR does — high 400 / medium 800 / low 1200; `null` is manual mode with a long
  silence window so `finish-task` usually ends the utterance) and
  `audio.input.transcription.language` / a single-element `languages` (zh/en/ja/yue/ko/de/fr/ru;
  unknown codes fall back to automatic detection).
- Turns each Paraformer sentence (`result-generated`) into GA
  `conversation.item.input_audio_transcription.delta` / `completed` events. Usage is billed by
  **duration seconds** (`usage.type` is `duration`); Bailian reports a running total per final
  sentence and the gateway bills the difference. To turn those seconds into AxonHub cost, add a
  channel model-price item with `itemCode: duration_seconds` and `usage_per_unit` set to the
  vendor's **per-second** rate (unlike token items, this rate is not scaled by 1e6).
- `input_audio_buffer.commit` ends the sentence in progress **now**: the gateway sends
  `finish-task`, so Paraformer finalises the sentence without waiting for
  `max_sentence_silence` of trailing silence, and the `completed` event follows within about one
  round trip. Client frames are held until `task-finished`; the next audio then starts a new task
  on the same connection (the gateway sends `run-task` again and holds that audio until
  `task-started`). A client with its own voice activity detection should send a commit as soon
  as the speaker stops instead of streaming trailing silence. As on OpenAI, every commit is
  answered: with `input_audio_buffer.committed` and a `completed` event, or with an error. A
  commit of less than 100 ms of audio since the previous commit point is answered with
  `input_audio_buffer_commit_empty`. The commit point is the previous commit, or where
  Paraformer's own VAD ended the previous sentence: the speech end plus `silence_duration_ms`
  for `server_vad`, the speech end for `semantic_vad`; in manual mode only commits count.
  Committed audio in which Paraformer heard no speech becomes an empty user item
  (`input_audio_buffer.committed`, `conversation.item.added`, `completed` with an empty
  `transcript`, `conversation.item.done`), so clients should skip empty transcripts.
  `audio_start_ms` / `audio_end_ms` count from the start of the session across commits, even
  though each commit starts a new Paraformer task. Bailian closes the
  connection when no new task starts within 60 s of the previous one, which ends the session.
- On close, sends `finish-task` (unless a commit's finish is already in flight) and waits up to
  `AXONHUB_REALTIME_RESPONSE_DRAIN` for `task-finished`; a session with no task running closes at
  once. A client that closes its own socket does not receive the last `completed` event (the
  sentence is still billed), so commit and wait for `completed` before closing.

Beta-protocol clients (`OpenAI-Beta: realtime=v1`) are rejected for Paraformer models (they only
work for Qwen-ASR).

## Bailian Qwen-Audio ASR realtime

`qwen-audio-3.1-asr-flash-streaming` and `qwen-audio-3.1-asr-flash-message` (and their dated
snapshots) on a Bailian channel serve GA transcription sessions exactly like Paraformer: the same
inference WebSocket, the same commit behaviour (every commit answered, `commit_empty` under
100 ms, session timeline across commits) and the same close/drain rules. Add the model to the
channel and connect with `/v1/realtime?intent=transcription&model=qwen-audio-3.1-asr-flash-streaming`.
Beta-protocol clients are rejected. Both models are served in Beijing and Singapore (Paraformer is
Beijing only).

Differences from Paraformer:

- Audio is sent to Bailian at 16 kHz (24 kHz PCM and G.711 are converted as for Paraformer).
- `turn_detection`: `server_vad` `silence_duration_ms` → `max_sentence_silence` (200–6000 ms,
  default **800**); `semantic_vad` eagerness → high 400 / medium 800 / low 1200 ms (no semantic
  punctuation is sent); `null` (manual) → 6000 ms. The gateway sends `heartbeat: true`, so a client
  that streams silence between utterances keeps the task alive.
- `audio.input.noise_reduction` selects Bailian's VAD model and is echoed back: `near_field` →
  `near_meeting_16k`, `far_field` → `far_field_meeting_16k`, `null`/absent → Bailian's default (far
  field); any other type is `invalid_value`.
- `transcription.language` uses Qwen-Audio's 30 languages (zh en ja ko vi th id ms tl hi ar fr de es
  pt ru it nl sv da fi no el pl cs hu ro bg hr sk). There is no Cantonese (`yue`, `zh-HK` →
  automatic). Filipino is `tl` (`fil` is accepted). Unknown codes → automatic detection.
- `-streaming` sends live partial deltas. `-message` (tuned for short voice messages) sends no
  partial text: each sentence arrives once, about 1 s after the speech ends, as one delta plus
  `completed`.
- Billing:
  - `-message` reports tokens per sentence (`completed.usage` is `{"type":"tokens",…}`); the usage
    log also carries the reported seconds. Configure token prices (per 1M) **or** a
    `duration_seconds` price for it, not both, or it is billed twice.
  - `-streaming` reports no usage at all. AxonHub bills the seconds of audio it actually forwarded
    (whole seconds, rounded up, at most 1 s over the real audio per session), also when a task
    fails or the upstream drops. `completed.usage` is `{"type":"duration","seconds":N}`; configure
    a `duration_seconds` price for it.
- Push-to-talk clients that stop streaming entirely: Bailian fails a task after about 23 s without
  any audio (`CLIENT_ERROR`), so commit at the end of each turn.

## Realtime translation (`/v1/realtime/translations`)

AxonHub relays the OpenAI realtime translation WebSocket protocol to Bailian Qwen-LiveTranslate
(`qwen3.8-livetranslate-flash-realtime`). The official SDK clients work against it: Python
`client.realtime.translations.connect(model=...)`, Node `OpenAIRealtimeTranslationWS`, Go
`Realtime.ConnectTranslation`.

- **Endpoint:** `GET /v1/realtime/translations?model=<model>` (WebSocket), with the same API-key
  auth as `/v1/realtime` (also mirrored under `/codex`). Only Qwen-LiveTranslate 3.8 models are
  accepted here, and they are refused on `/v1/realtime` with a 400 that points here.
- **Setup:** add `qwen3.8-livetranslate-flash-realtime` to a Bailian channel and give it token
  prices. Bailian bills about 7 tokens per second of input audio and 12.5 per second of output audio.
- **Client events:** `session.update` (`audio.output.language`, `audio.input.transcription`,
  `audio.input.noise_reduction`), `session.input_audio_buffer.append` (base64 24 kHz PCM16) and
  `session.close`.
- **Server events:** `session.created`, `session.updated`, `session.closed`,
  `session.output_audio.delta` (24 kHz PCM16), `session.output_transcript.delta`,
  `session.input_transcript.delta` (only when `audio.input.transcription` is set) and `error`.
- **Languages:** 60 target languages. 29 are translated to speech (zh, en, ar, de, fr, es, pt, id,
  it, ko, ru, th, vi, ja, tr, hi, ms, nl, ur, nb, sv, da, he, fi, pl, is, cs, fil, fa); the others
  are text only and produce no audio deltas. BCP-47 tags are normalised (en-US → en, zh-CN / zh-TW
  / zh-Hant-TW → zh, zh-HK / zh-Hant-HK → yue (text only), no → nb, tl → fil). The default is `en`.
  An unsupported code returns an `invalid_value` error event and the session stays open.
- **How it maps:** Bailian accepts only one configuration per session, so the gateway answers
  `session.update` itself and sends one merged configuration to Bailian just before the first
  audio. The target language is fixed from then on (a later change returns
  `unsupported_on_upstream`; reconnect to switch). The upstream voice is
  `AXONHUB_REALTIME_LIVETRANSLATE_VOICE` (default `Tina`; Bailian's own default voice fails every
  response).
- **Ending:** `session.close` waits up to 15 s for the last sentence, then sends `session.closed`
  and closes with 1000 (also when the upstream fails during that wait; the session is then logged as
  failed). Any upstream error ends the session with close code 1011.
- **Billing / logs:** each translated sentence (each upstream `response.done`) creates one request
  row and one usage row under the Bailian model; each session also gets a summary row. The client
  receives no usage events. `noise_reduction` and `transcription.model` are echoed back but have no
  effect.


## Codex voice (Codex desktop / CLI)

1. Point Codex at AxonHub (`base_url = "https://host/v1"` or `https://host/codex`).
2. Add the Codex voice model (`gpt-live-1-codex` for v3, `gpt-realtime-1.5` for v1) to the Codex
   channel's model list so it can be selected.
3. **Codex ≥ 0.146 sends the sideband WebSocket to `api.openai.com` by default** (with the
   AxonHub key, which fails there). Add to `~/.codex/config.toml` (root level):

   ```toml
   # base_url = "https://host/v1"
   experimental_realtime_ws_base_url = "https://host/v1"
   # base_url = "https://host/codex"
   experimental_realtime_ws_base_url = "https://host/codex/realtime"
   ```

The gateway forwards `POST /v1/live` to the ChatGPT backend (`/realtime/calls?intent=quicksilver&architecture=avas`)
with the channel's OAuth identity and relays `GET /v1/live/{call_id}` to `wss://api.openai.com/v1/live/{call_id}`.
Only the API key that created a call may attach to its sideband.

## Accounting

- Every upstream `response.done` is recorded as one completed request with its usage log (so
  Requests quotas count **turns**, not sessions); input transcription usage is recorded separately
  under the transcription model.
- A session summary request (events, responses, duration, close cause) is written when the
  session ends. A failed handshake leaves a failed request row.
- Each WebRTC call is recorded as one request with a zero-token usage log. For calls on OpenAI
  channels the gateway additionally opens its own read-only sideband (`AXONHUB_REALTIME_MONITOR_CALLS`)
  to capture `response.done` usage.
- Audio tokens are stored in the usage log but **priced at the model's text rate**; configure
  prices for realtime and transcription models or costs are empty.
- Every 30 s (`AXONHUB_REALTIME_QUOTA_CHECK_INTERVAL`) the session re-checks the API-key quota,
  the channel quota, the channel status and the API key; a failed check sends a GA `error` event
  and closes with 1008.
- Client-caused endings (client hang-up, client connection lost, failed client upgrade) are
  recorded as cancelled, not as channel failures, so they do not feed channel auto-disable.
- On server shutdown a live session gets up to ResponseDrain + CloseGrace
  (`AXONHUB_REALTIME_RESPONSE_DRAIN` + `AXONHUB_REALTIME_CLOSE_GRACE`) to finish and persist its
  usage; the client receives close code 1012.

## Configuration (environment variables)

| Variable | Default | Meaning |
|---|---|---|
| `AXONHUB_REALTIME_CONNECT_BUDGET` | `8s` | Total time for selection + upstream handshake before the client is upgraded |
| `AXONHUB_REALTIME_HANDSHAKE_TIMEOUT` | `5s` | Per-candidate upstream handshake |
| `AXONHUB_REALTIME_MAX_SESSION_DURATION` | OpenAI `60m`, Bailian `119m` | Hard session limit (closes with 1000) |
| `AXONHUB_REALTIME_IDLE_TIMEOUT` | `10m` | No data frames in either direction |
| `AXONHUB_REALTIME_PING_INTERVAL` | `30s` | Keepalive pings on both sockets |
| `AXONHUB_REALTIME_WRITE_TIMEOUT` | `10s` | Bound for a single WebSocket write |
| `AXONHUB_REALTIME_CLOSE_GRACE` | `4s` | Wait for close handshakes before force-closing |
| `AXONHUB_REALTIME_RESPONSE_DRAIN` | `5s` | After a client hang-up mid-response, how long to keep reading upstream for `response.done` |
| `AXONHUB_REALTIME_MAX_MESSAGE_SIZE` | `16777216` | Read limit per frame |
| `AXONHUB_REALTIME_WRITE_QUEUE_FRAMES` | `16` | Frames one socket writer buffers before producers block |
| `AXONHUB_REALTIME_GATE_TIMEOUT` | `10s` | Bound for the dialect initialisation phase (client frames are queued meanwhile) |
| `AXONHUB_REALTIME_MAX_SESSIONS` | `256` | Concurrent sessions, sidebands and monitors |
| `AXONHUB_REALTIME_MAX_SESSIONS_PER_KEY` | `8` | Per API key |
| `AXONHUB_REALTIME_CALL_BINDING_TTL` | `2h` | How long a WebRTC call id is remembered |
| `AXONHUB_REALTIME_CALL_BINDINGS_PER_PROJECT` | `1024` | Maximum remembered WebRTC calls per project |
| `AXONHUB_REALTIME_QUOTA_CHECK_INTERVAL` | `30s` | In-session re-check of quotas, channel status and API key |
| `AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL` | `gpt-4o-transcribe` | Model for `intent=transcription` without `model` |
| `AXONHUB_REALTIME_LIVETRANSLATE_VOICE` | `Tina` | Bailian voice for spoken translations on `/v1/realtime/translations` |
| `AXONHUB_REALTIME_MONITOR_CALLS` | `true` | Gateway-owned usage monitor for OpenAI WebRTC calls |
| `AXONHUB_CODEX_LIVE_FORWARD_ATTESTATION` | `true` | Forward `x-oai-attestation` to the Codex backend |
| `AXONHUB_REALTIME_CALL_BODY_LIMIT` | `1048576` | Maximum call-create body |

## Known limitations

- Beta protocol clients are rejected except for Bailian Qwen-ASR models; audio tokens are billed
  at the text rate.
- Bailian Qwen-ASR, Paraformer and Qwen-Audio ASR transcription sessions cannot be reconfigured after
  the first audio, cannot clear the audio buffer, and have no `prompt`/`keywords` (noise reduction
  only selects Qwen-Audio's VAD model).
- Translation sessions cannot change the target language after the first audio and have no WebRTC
  or ephemeral-key path.
- One realtime session holds one channel concurrency slot (`MaxConcurrent`) for its whole duration.
- No mid-session channel failover; a channel disabled mid-session ends the session at the next check.
- Call bindings are per process (multi-replica deployments cannot share them).
- WebRTC calls whose sideband does not pass through the gateway are recorded once with zero usage.
- Codex voice token usage is not reported by the upstream and stays unmetered. Paraformer duration-billed transcription is metered when the channel model price includes a `duration_seconds` item (`usage_per_unit` = price **per second**, not per million tokens); without that item the UsageLog still stores the duration in the request summary but `TotalCost` stays empty.
- Browser WebRTC posts need `server.cors.enabled` with the page origin allowed.
- The OpenAI and Codex paths were verified against fake upstream servers only; Bailian was verified
  with a live key.
