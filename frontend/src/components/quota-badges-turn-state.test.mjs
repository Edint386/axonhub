import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const quotaBadges = readFileSync(join(import.meta.dirname, 'quota-badges.tsx'), 'utf8');

test('Codex quota rows show a turn-state indicator after the channel name', () => {
  assert.match(quotaBadges, /CodexTurnStateIndicator/);
  assert.match(quotaBadges, /channel\.type === 'codex' && channel\.codexTurnStateRuntime\?\.phase/);
  assert.match(quotaBadges, /<span className='text-foreground font-medium'>\{channel\.name\}<\/span>/);
});
