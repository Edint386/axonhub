#!/usr/bin/env node
// Rebuild data/bundle.json and data/shared_detector.bin.zst from an lm-detector checkout.
// Usage (from frontend/): node src/features/playground/model-trace/scripts/refresh-detector.mjs /path/to/lm-detector

import { createHash } from 'node:crypto';
import { execSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { constants, zstdCompressSync } from 'node:zlib';

const source = process.argv[2];
if (!source) {
  console.error('Usage: node src/features/playground/model-trace/scripts/refresh-detector.mjs /path/to/lm-detector');
  process.exit(1);
}

const root = resolve(source);
const dest = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'data');
const bank = JSON.parse(readFileSync(join(root, 'data', 'unified_bank.json'), 'utf8'));
const detectorBytes = readFileSync(join(root, 'data', 'shared_detector.bin'));
const sha256 = createHash('sha256').update(detectorBytes).digest('hex');
const compressed = zstdCompressSync(detectorBytes, { params: { [constants.ZSTD_c_compressionLevel]: 19 } });
const commit = execSync('git rev-parse HEAD', { cwd: root, encoding: 'utf8' }).trim();

const models = bank.models.map(({ id, display_name, family, family_name, response_count }) => ({
  id,
  display_name,
  family,
  family_name,
  response_count,
}));

const bundle = {
  schema: 'lmfpd-data-v1',
  detector_schema: 'shared-detector-v2',
  built_at: bank.built_at,
  models,
  files: {
    'shared_detector.bin': {
      path: 'shared_detector.bin.zst',
      bytes: compressed.length,
      sha256,
    },
  },
  source: {
    repo: 'https://github.com/Ikaleio/lm-detector',
    commit,
  },
};

writeFileSync(join(dest, 'shared_detector.bin.zst'), compressed);
writeFileSync(join(dest, 'bundle.json'), JSON.stringify(bundle));
console.log(
  JSON.stringify(
    {
      dest,
      commit,
      built_at: bank.built_at,
      models: models.length,
      uncompressed: detectorBytes.length,
      compressed: compressed.length,
      sha256,
    },
    null,
    2,
  ),
);
