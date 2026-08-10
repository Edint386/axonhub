import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('redesigned request usage column retains mobile defaults and cache-hit color tiers', () => {
  const columns = read('features/requests/components/requests-columns.tsx');

  assert.match(columns, /DEFAULT_HIDDEN_COLUMN_IDS/);
  assert.match(columns, /DEFAULT_MOBILE_HIDDEN_COLUMN_IDS/);
  assert.match(columns, /id:\s*'usage'/);
  assert.match(columns, /const readCacheTokens = usageLog\.promptCachedTokens/);
  assert.match(columns, /const writeCacheTokens = usageLog\.promptWriteCachedTokens/);
  assert.match(columns, /function getCacheHitRateColor\(rate: number\): string/);
  assert.match(columns, /rate >= 98[\s\S]*rate >= 90[\s\S]*rate >= 75[\s\S]*rate >= 50[\s\S]*rate >= 20/);
  assert.match(columns, /const isLowHitRate = hitRate < 80 && promptTokens >= 40000/);
  assert.match(columns, /isLowHitRate\s*\? 'font-medium text-red-600 dark:text-red-400'\s*:\s*getCacheHitRateColor\(hitRate\)/);
  assert.match(columns, /requests\.columns\.cacheHitRate/);
});
