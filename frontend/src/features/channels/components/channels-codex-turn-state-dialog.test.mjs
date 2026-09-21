import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const source = readFileSync(join(import.meta.dirname, 'channels-codex-turn-state-dialog.tsx'), 'utf8');

test('turn-state dialog shows live status and recent probe records', () => {
  assert.match(source, /useCodexTurnStateRuntime\(currentRow\.id, open\)/);
  assert.match(source, /CodexTurnStateIndicator phase=\{runtime\?\.phase/);
  assert.match(source, /channels\.dialogs\.codexTurnState\.events\.title/);
  assert.match(source, /runtime\.recentEvents\.map/);
});
