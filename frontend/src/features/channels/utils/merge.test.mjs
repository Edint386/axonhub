import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const source = readFileSync(join(import.meta.dirname, 'merge.ts'), 'utf8');

test('mergeChannelSettingsForUpdate preserves providerQuota and healthProbe', () => {
  const start = source.indexOf('export function mergeChannelSettingsForUpdate');
  const end = source.indexOf('export function mergeOverrideParameters', start);
  assert.ok(start !== -1, 'mergeChannelSettingsForUpdate should exist');
  assert.ok(end !== -1 && end > start, 'mergeOverrideParameters should follow mergeChannelSettingsForUpdate');

  const block = source.slice(start, end);
  assert.match(block, /providerQuota:\s*pick\('providerQuota',\s*existing\?\.providerQuota\s*\?\?\s*null\)/);
  assert.match(block, /healthProbe:\s*pick\('healthProbe',\s*existing\?\.healthProbe\s*\?\?\s*null\)/);
});
