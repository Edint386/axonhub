# Realtime API (voice) Reference

> This feature is specific to the `custom` branch (Edint386/axonhub). It is not part of upstream AxonHub.

## Overview

AxonHub relays the **OpenAI Realtime API** (GA protocol) so that voice clients can talk to
upstream realtime models through the gateway's authentication, channel selection, quotas and
usage logging. Three upstream families are supported:

| Upstream | How | Notes |
|---|---|---|
| OpenAI (`gpt-realtime*`) | WebSocket relay, WebRTC call signalling | Protocol is passed through unchanged. |
| Codex (ChatGPT login) voice | WebRTC call signalling (`POST /v1/live`) + sideband relay | Codex desktop/CLI voice chat. |
| Alibaba Bailian Qwen-Omni-Realtime (`qwen3.8-omni-flash-realtime`, `qwen3.5-omni-*-realtime`) | WebSocket relay with protocol translation | GA clients work unchanged; the gateway translates the Bailian dialect. |

Audio media for WebRTC calls flows **directly between the client and OpenAI**; the gateway only
forwards the SDP signalling and relays the sideband WebSocket.

## Endpoints

All routes exist under `/v1` and, for Codex clients whose base URL ends in `/codex`, under `/codex`
(the `/codex` mirror is gated by the Codex compatibility switch and requires a real API key).

| Route | Purpose |
|---|---|
| `GET /v1/realtime?model=<model>` | WebSocket session (OpenAI GA protocol) |
| `GET /v1/realtime?intent=transcription[&model=]` | Transcription session; `AXONHUB_REALTIME_DEFAULT_TRANSCRIPTION_MODEL` is used when `model` is absent |
| `GET /v1/realtime?call_id=<id>` | Sideband attach to a WebRTC call created through the gateway |
| `GET /v1/live/{call_id}` | Codex v3 sideband attach |
| `POST /v1/live` | Codex WebRTC call create (multipart `sdp` + `session`) |
| `POST /v1/realtime/calls` | Standard WebRTC call create (`application/sdp`, multipart or JSON `{sdp, session}`) |
| `POST /v1/realtime/calls/{call_id}/hangup` | End a call created through `/v1/realtime/calls` on an OpenAI channel |

Not supported (answered with a JSON 404 explaining why): `POST /v1/realtime/client_secrets`
(would hand out an unmetered upstream credential), `accept`/`reject`/`refer` (SIP calls never
pass through the gateway), `GET /v1/live?model=` (GPT-Live websocket transport), the removed beta
`POST /v1/realtime/sessions` and `/v1/realtime/transcription_sessions`, and the removed beta
protocol (`OpenAI-Beta: realtime=v1` is rejected with 400).

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
| `AXONHUB_REALTIME_MONITOR_CALLS` | `true` | Gateway-owned usage monitor for OpenAI WebRTC calls |
| `AXONHUB_CODEX_LIVE_FORWARD_ATTESTATION` | `true` | Forward `x-oai-attestation` to the Codex backend |
| `AXONHUB_REALTIME_CALL_BODY_LIMIT` | `1048576` | Maximum call-create body |

## Known limitations

- Beta protocol clients are rejected; audio tokens are billed at the text rate.
- One realtime session holds one channel concurrency slot (`MaxConcurrent`) for its whole duration.
- No mid-session channel failover; a channel disabled mid-session ends the session at the next check.
- Call bindings are per process (multi-replica deployments cannot share them).
- WebRTC calls whose sideband does not pass through the gateway are recorded once with zero usage.
- Codex voice token usage is not reported by the upstream; duration-billed transcription is unmetered.
- Browser WebRTC posts need `server.cors.enabled` with the page origin allowed.
- The OpenAI and Codex paths were verified against fake upstream servers only; Bailian was verified
  with a live key.
