# Detector snapshot

Self-hosted scoring data for the playground model check. Runtime loading uses only these files:

- `bundle.json` — `lmfpd-data-v1` metadata: `built_at`, model ids / display names / families, and the
  SHA-256 of the uncompressed `shared_detector.bin`
- `shared_detector.bin.zst` — zstd-compressed official `shared_detector.bin` (`shared-detector-v2`)

Do not add `unified_bank.json` (about 15 MB) or `unified_reference.jsonl`. The UI never fetches
`lm.ikale.io` or the `lmfpd-data` CDN.

Refresh from an lm-detector checkout (see `../README.md`):

```bash
node src/features/playground/model-trace/scripts/refresh-detector.mjs /path/to/lm-detector
```
