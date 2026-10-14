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

test('mergeChannelSettingsForUpdate keeps the channel quota timezone', () => {
  const start = source.indexOf('export function mergeChannelSettingsForUpdate');
  const end = source.indexOf('export function mergeOverrideParameters', start);
  const block = source.slice(start, end);

  // Unloaded stays undefined (omitted, so the backend keeps the stored value) and
  // must not be coerced to null, which would clear it.
  assert.match(block, /quotaTimezone:\s*pick\('quotaTimezone',\s*existing\?\.quotaTimezone\)/);
});

test('channel queries request quotaTimezone with the quota fields', () => {
  const channels = readFileSync(join(import.meta.dirname, '../data/channels.ts'), 'utf8');
  const fragment = channels.match(/const CHANNEL_QUOTA_FIELDS = `([\s\S]*?)`;/);
  assert.ok(fragment, 'CHANNEL_QUOTA_FIELDS should exist');
  assert.match(fragment[1], /\bquotaTimezone\b/);
});
