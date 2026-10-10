// The channel price multiplier (XSN-11) is persisted per channel and applied by the
// backend cost calculation. useChannelModelPrices returns { multiplier, prices } and the
// save mutation requires the multiplier, so the price dialog must follow that contract
// and must not pre-multiply provider prices. An upstream dialog that treats the query
// result as a bare array crashes with "map is not a function" when opened.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('price query and save mutation carry the persisted channel multiplier', () => {
  const channels = read('features/channels/data/channels.ts');

  assert.match(channels, /multiplier: node\?\.modelPriceMultiplier \?\? 1/);
  assert.match(channels, /prices: \(node\?\.channelModelPrices \|\| \[\]\)\.map/);
  assert.match(channels, /saveChannelModelPrices\(channelId: \$channelId, multiplier: \$multiplier, input: \$input\)/);
});

test('price dialog reads, exports, and saves through the multiplier contract', () => {
  const dialog = read('features/channels/components/channels-model-price-dialog.tsx');

  assert.match(dialog, /reset\(mapServerPricesToFormData\(currentPrices\.prices\)\)/);
  assert.match(dialog, /setMultiplier\(currentPrices\.multiplier\)/);
  assert.match(dialog, /currentPrices\.prices\.length === 0/);
  assert.match(dialog, /const payload = currentPrices\.prices\.map\(/);
  assert.match(dialog, /savePrices\.mutateAsync\(\{\s*channelId: currentRow\.id,\s*multiplier,/);
});

test('provider prices are applied as-is because the backend applies the multiplier', () => {
  const dialog = read('features/channels/components/channels-model-price-dialog.tsx');

  assert.doesNotMatch(dialog, /\* multiplier\)/);
  assert.doesNotMatch(dialog, /buildItemsFromProviderModel\([^)]*multiplier/);
  assert.doesNotMatch(dialog, /mergeItemsWithProviderCost\([^)]*multiplier/);
});
