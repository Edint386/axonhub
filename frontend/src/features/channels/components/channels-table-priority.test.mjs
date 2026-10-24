import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// Custom guard: the priority cell edits inline, so like the action column it
// must not be clipped by the table's overflow-hidden cells. The table file is
// upstream's; this keepVisible condition is the only custom change in it.
const source = readFileSync(join(import.meta.dirname, 'channels-table.tsx'), 'utf8');

test('priority column is kept visible in both header and body cells', () => {
  assert.match(source, /const keepVisible = header\.column\.id === 'action' \|\| header\.column\.id === 'priority';/);
  assert.match(source, /const keepVisible = cell\.column\.id === 'action' \|\| cell\.column\.id === 'priority';/);
  assert.equal((source.match(/\$\{keepVisible \? 'overflow-visible whitespace-nowrap'/g) || []).length, 2);
});
