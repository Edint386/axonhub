import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('channel status switch keeps permission gating and supports skipping confirmation', () => {
  const columns = read('features/channels/components/channels-columns.tsx');

  assert.match(columns, /useSkipChannelStatusConfirmation/);
  assert.match(columns, /useUpdateChannelStatus/);
  assert.match(columns, /if \(!channelPermissions\.canWrite\) \{\s*return <Badge/);
  assert.match(columns, /if \(isArchived \|\| updateChannelStatus\.isPending\)/);
  assert.match(columns, /if \(!skipStatusConfirmation\) \{\s*setDialogOpen\(true\)/);
  assert.match(columns, /updateChannelStatus\.mutateAsync\(\{[\s\S]*status: newStatus/);
  assert.match(columns, /disabled=\{isArchived \|\| updateChannelStatus\.isPending\}/);
});
