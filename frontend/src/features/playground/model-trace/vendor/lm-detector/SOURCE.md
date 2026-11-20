# lm-detector vendor snapshot

Browser-safe scoring and challenge modules copied from [Ikaleio/lm-detector](https://github.com/Ikaleio/lm-detector).

- Commit: `70159b7536a2ceb55c478c8c9472da81d3627e59` (2026-11-19, `feat(data): add Hy4 and Hy4 Flash from Tencent`)
- Detector / bank `built_at`: `2026-11-19T05:26:09.347356+00:00`
- License: MIT (see `LICENSE`; copyright still mentions xqy2006, as in the upstream repo)

Copied from `shared/`:

- `fingerprint-core.js`
- `positional.ts`
- `shared-detector.ts`
- `detector-codec.ts`
- `challenge-browser.js` (only change: replace `crypto.randomUUID` so plain-HTTP pages still work)

`types.ts` is a slimmed scoring-only subset of `shared/types.ts` (the original imports request/throughput types that are not used for scoring and are not needed in the frontend).

Do not vendor Node-only files such as `detect-request.ts`, `sdk/ts/src/data.ts`, or anything that imports `node:fs`.

Relative TypeScript imports in the copied files use explicit `.ts` extensions so AxonHub's `node --test` runner can load them. `challenge-browser.js` is patched as noted above.
