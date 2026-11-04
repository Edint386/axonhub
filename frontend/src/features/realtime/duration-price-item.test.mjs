// Guard test for realtime per-second billing (the duration_seconds price item).
//
// Its frontend lines live in upstream-owned files: the price item code lists, the Zod enum in
// schema.ts and the per-second mode label in both price editors. Most removals still
// type-check. Without the list or enum entries a saved per-second price fails Zod validation
// when it loads or saves; without the label lines the mode reads "Per 1M Tokens".
// See CUSTOM_BRANCH_RULES.md section 5.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('every price item code list includes duration_seconds', () => {
  for (const file of [
    'components/model-price-editor.tsx',
    'components/price-schedule-editor.tsx',
    'features/channels/components/channels-model-price-dialog.tsx',
  ]) {
    assert.match(read(file), /^const priceItemCodes = \[[^\]]*'duration_seconds'[^\]]*\] as const;/m, file);
  }

  assert.match(read('features/channels/data/schema.ts'), /^export const priceItemCodeSchema = z\.enum\(\[[^\]]*'duration_seconds'[^\]]*\]\);/m);
});

test('both price editors label duration_seconds per second', () => {
  assert.match(read('components/model-price-editor.tsx'), /^\s*<SelectItem value='usage_per_unit'>\{itemCode === 'duration_seconds' \? t\('price\.mode_usage_per_second'\)/m);
  assert.match(read('components/price-schedule-editor.tsx'), /^\s*<SelectItem value='usage_per_unit'>\{currentCode === 'duration_seconds' \? t\('price\.mode_usage_per_second'\)/m);
});

test('both locales name the duration item and the per-second mode', () => {
  for (const locale of ['en', 'zh-CN']) {
    const base = JSON.parse(read(`locales/${locale}/base.json`));

    assert.ok(base['price.itemCodes.duration_seconds'], `${locale}: price.itemCodes.duration_seconds`);
    assert.ok(base['price.mode_usage_per_second'], `${locale}: price.mode_usage_per_second`);
  }
});
