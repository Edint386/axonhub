import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('quota badges retain upstream provider states and render channel-local quota', () => {
  const quotaTypes = read('features/system/data/quotas.ts');
  const quotaBadges = read('components/quota-badges.tsx');

  assert.match(quotaTypes, /providerQuotaStatus != null \|\| node\.settings\?\.quota != null/);
  assert.match(quotaBadges, /isClineUnavailablePassQuotaData\(quota\.quotaData\)/);
  assert.match(quotaBadges, /if \(!quota && !channel\.localQuota\) return null/);
  assert.match(quotaBadges, /getWorstQuotaStatus\(channel\.quotaStatus\?\.status, getLocalQuotaStatus\(channel\)\)/);
  assert.match(quotaBadges, /quota\.label\.local_requests/);
  assert.match(quotaBadges, /quota\.label\.local_total_tokens/);
  assert.match(quotaBadges, /quota\.label\.local_cost/);
});

test('local quota channels are not collapsed into provider-level rows', () => {
  const quotaBadges = read('components/quota-badges.tsx');

  assert.match(quotaBadges, /if \(channel\.localQuota\) \{\s*acc\.push\(channel\);\s*return acc;/);
  assert.match(quotaBadges, /isLocalOnlyQuotaChannel/);
});
