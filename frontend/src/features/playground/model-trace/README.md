# Model check (lm-detector)

Custom feature of the `custom` branch. The playground's "Model check" view sends
number-choice prompts through `/admin/playground/chat` to the channel and model
picked in the sidebar, 3 in parallel, and scores the answers in the browser
against a snapshot of [lm-detector](https://github.com/Ikaleio/lm-detector)'s
shared detector (`shared-detector-v2`).

The method comes from [xqy2006/ModelTrace](https://github.com/xqy2006/ModelTrace)
and [Ikaleio/lm-detector](https://github.com/Ikaleio/lm-detector) (MIT, see
`LICENSE` and `vendor/lm-detector/LICENSE`). AxonHub keeps its own playground
entry, sidebar project/channel/model selection, and `/admin/playground/chat`
transport; only the scoring algorithm and fingerprint data come from lm-detector.
The previous ModelTrace Hellinger / Ordered-block port and `unified_bank.json`
are not kept.

Pinned source: Ikaleio/lm-detector `70159b7536a2ceb55c478c8c9472da81d3627e59`
(2026-11-19). Detector / bank `built_at`: `2026-11-19T05:26:09.347356+00:00`
(UTC; 2026-11-19 13:26 Asia/Shanghai). See `vendor/lm-detector/SOURCE.md`.

| File                                                                   | Role                                                                                                   |
| ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `vendor/lm-detector/`                                                  | Browser-safe modules copied from lm-detector `shared/` (scoring + multilingual `challenge-browser.js`) |
| `data/bundle.json`                                                     | Model list, schemas, checksums (`lmfpd-data-v1`)                                                       |
| `data/shared_detector.bin.zst`                                         | Official `shared_detector.bin`, zstd-compressed and lazy-loaded                                        |
| `detector.ts`, `detector-load.ts`, `fingerprint-core.ts`               | Snapshot loader and adapter around `analyzeSharedOutputs`                                              |
| `challenges.ts`                                                        | Re-exports the official multilingual generator (without `crypto.randomUUID`)                           |
| `sha256.ts`                                                            | Snapshot checksum; falls back to JavaScript when `crypto.subtle` is missing on plain HTTP              |
| `runner.ts`, `transport.ts`, `model-match.ts`, `model-trace-panel.tsx` | AxonHub glue, not from lm-detector                                                                     |

The only lines in upstream-owned code are in `../index.tsx`; `playground-hook.test.mjs` guards them.
`transport.ts` copies how the playground chat calls the backend (endpoint, `X-Project-ID` /
`X-Channel-ID`, token refresh, AI SDK UI message stream events); the same test pins those parts, and
`transport.test.mjs` replays the backend's stream fixture.

The snapshot is self-hosted next to the feature. Opening the check view fetches
`bundle.json` and `shared_detector.bin.zst` from the AxonHub frontend assets
(`?url`); it does not download from `lm.ikale.io` at runtime. Offline and
self-hosted deployments work as long as these two files are served with the UI.

Differences from lm-detector's own website / CLI:

- Prompts go through AxonHub, so project prompt injection and channel parameter overrides still apply.
- Official detect/SDK HTTP clients are not used. Requests stay on `/admin/playground/chat`.
- The AI SDK stream carries no finish reason, so answers cut off by a token limit or content filter
  cannot be told apart from complete ones. An answer whose stream ends without a `finish` event
  (dropped upstream, or a relay that never sends a finish reason) counts only if it has at least 90%
  of the numbers asked for.
- A "different family" verdict needs at least 80% family probability; below that the result is
  shown as uncertain.
- Requests run 3 at a time. A 429/408 re-sends the same prompt after a short backoff
  (1 s per retry plus jitter, at most 3 retries) and lowers the concurrency; 5xx is left to
  AxonHub's own retry policy.
- A stream is cancelled once the answer runs 10% past the asked-for count of numbers: some models
  keep listing numbers far past the challenge's count, burning tokens for nothing. Most models
  write exactly the asked-for count and stop, so they are not cut. The trailing number of a cut
  answer is dropped (it may be a fragment) and the answer still counts normally.
- Verdicts are given only when the model sent upstream is one of the bank's models. The detector is
  closed-set, so a model outside the bank (even a sibling version) gets no verdict.
- Tokenizer detection is not implemented: the playground stream has no `usage`.

## Updating the detector snapshot

With an [Ikaleio/lm-detector](https://github.com/Ikaleio/lm-detector) checkout at `$LM` (run from
`frontend/`). Use a tagged data commit, not an arbitrary web/UI change:

```bash
node src/features/playground/model-trace/scripts/refresh-detector.mjs "$LM"
```

The script writes `data/bundle.json` and `data/shared_detector.bin.zst` from `$LM/data/unified_bank.json`
and `$LM/data/shared_detector.bin`, records the source commit and `built_at`, and does not include
the 15 MB `unified_bank.json` or `unified_reference.jsonl`. Then:

1. Copy any browser-safe scoring changes from `$LM/shared/` into `vendor/lm-detector/`, keeping the
   plain-HTTP `crypto.randomUUID` workaround in `challenge-browser.js` and explicit `.ts` import
   extensions. Do not copy Node-only files (`node:fs`, `detect-request.ts`, `sdk/ts/src/data.ts`).
2. Update the pinned commit and `built_at` in this file and in `vendor/lm-detector/SOURCE.md`.
3. If model ids or display names changed, adjust `model-match.test.mjs` (for example slash aliases
   such as `gpt-6-astra/6.1-sol`).
4. If you want a new scoring fixture, replace `testdata/fixture.json` with three accepted answers
   from an official reference collection of a bank model.
5. Run `node --test "src/features/playground/model-trace/*.test.mjs"` and a frontend production build.

The snapshot must keep working without `lm.ikale.io`. Do not switch the loader to download official
CDN data at runtime.
