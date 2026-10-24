import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// Custom guard: upstream groups the provider quota popover by channel category.
// Channels that only carry a local AxonHub quota get their own section after
// every provider category, which keeps them listed last as before the redesign.
const source = readFileSync(join(import.meta.dirname, 'quota-badges.tsx'), 'utf8');

test('local-only quota channels form the last popover category', () => {
  const start = source.indexOf('function getChannelCategory(');
  assert.ok(start !== -1, 'getChannelCategory should exist');
  const head = source.slice(start, source.indexOf('const channelType', start));

  assert.match(head, /channel\.localQuota != null && channel\.quotaStatus\?\.status === 'unknown'/);
  assert.match(head, /key: 'local_quota', label: t\('quota\.label\.local_quota'\), order: 100/);

  // Every provider category order stays below the local-quota section.
  const orders = [...source.slice(start, source.indexOf('\n}\n', start)).matchAll(/order: (\d+)/g)].map((m) => Number(m[1]));
  assert.equal(Math.max(...orders), 100);
  assert.ok(orders.filter((o) => o !== 100).every((o) => o < 100));
});

test('local quota channels keep their own row in the deduplicated list', () => {
  assert.match(source, /const dedupedChannels = channels\.reduce\([^)]*\) => \{\n\s*if \(channel\.localQuota\) \{\n\s*acc\.push\(channel\);\n\s*return acc;/);
});
