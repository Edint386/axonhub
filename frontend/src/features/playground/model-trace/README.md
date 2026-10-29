# Model check (ModelTrace)

Custom feature of the `custom` branch. The playground's "Model check" view sends ModelTrace's
number-choice prompts through `/admin/playground/chat` to the channel and model picked in the sidebar,
3 in parallel, and scores the answers in the browser against ModelTrace's fingerprint bank.

Source: [xqy2006/ModelTrace](https://github.com/xqy2006/ModelTrace) at `6cd4a8f` (MIT, see `LICENSE`).

| File | From ModelTrace |
|---|---|
| `fingerprint-core.ts` | `static/fingerprint-core.js`, typed, same arithmetic |
| `challenges.ts` | `static/challenge-browser.js`, without `crypto.randomUUID` (missing on plain-HTTP pages) |
| `unified_bank.json` | `data/unified_bank.json`, minified |
| `runner.ts`, `transport.ts`, `model-match.ts`, `model-trace-panel.tsx` | AxonHub glue, not from ModelTrace |

The only lines in upstream-owned code are in `../index.tsx`; `playground-hook.test.mjs` guards them.
`transport.ts` copies how the playground chat calls the backend (endpoint, `X-Project-ID` /
`X-Channel-ID`, token refresh, AI SDK UI message stream events); the same test pins those parts, and
`transport.test.mjs` replays the backend's stream fixture.

Differences from ModelTrace's own API tester:

- Prompts go through AxonHub, so project prompt injection and channel parameter overrides still apply.
- The AI SDK stream carries no finish reason, so answers cut off by a token limit or content filter
  cannot be told apart from complete ones. An answer whose stream ends without a `finish` event
  (dropped upstream, or a relay that never sends a finish reason) counts only if it has at least 90%
  of the numbers asked for.
- A "different family" verdict needs at least 80% family probability; below that the result is
  shown as uncertain.
- Requests run 3 at a time. A 429/408 re-sends the same prompt after ModelTrace's backoff
  (1 s per retry plus jitter, at most 3 retries) and lowers the concurrency; 5xx is left to
  AxonHub's own retry policy.
- Verdicts are given only when the model sent upstream is one of the bank's models. ModelTrace is
  closed-set, so a model outside the bank (even a sibling version) gets no verdict.

## Updating the bank

With a ModelTrace checkout at `$MT` (run from `frontend/`):

```bash
python3 -c "import json,sys; json.dump(json.load(open(sys.argv[1])), open(sys.argv[2],'w'), ensure_ascii=False, separators=(',',':'))" "$MT/data/unified_bank.json" src/features/playground/model-trace/unified_bank.json
```

```bash
cp "$MT/static/fingerprint-core.js" /tmp/modeltrace-core.mjs
```

```bash
node src/features/playground/model-trace/testdata/make-parity.mjs /tmp/modeltrace-core.mjs "$MT/data/unified_bank.json" "$MT/data/claude_reference.jsonl" "$MT/data/gpt_reference.jsonl" src/features/playground/model-trace/testdata/parity.json "$(git -C "$MT" rev-parse --short HEAD)"
```

Then diff ModelTrace's `static/fingerprint-core.js` and `static/challenge-browser.js` against the
commit above and port any change, update the commit in this file and the file headers, and run
`node --test "src/features/playground/model-trace/*.test.mjs"`. `make-parity.mjs` and
`model-match.test.mjs` use fixed bank model ids; adjust them if ModelTrace renames or drops a model.
