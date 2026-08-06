import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('channel system settings retain upstream prompts and local status preference', () => {
  const dialog = read('features/channels/components/channels-system-settings-dialog.tsx');
  const english = read('locales/en/channels.json');
  const chinese = read('locales/zh-CN/channels.json');

  assert.match(dialog, /usePermissions/);
  assert.match(dialog, /testSystemPrompt/);
  assert.match(dialog, /testUserPrompt/);
  assert.match(dialog, /useSkipChannelStatusConfirmation/);
  assert.match(dialog, /skipStatusConfirmationDraft/);
  assert.match(dialog, /setSkipStatusConfirmation\(skipStatusConfirmationDraft\)/);
  assert.match(dialog, /skip-status-confirmation/);
  assert.match(english, /channels\.dialogs\.systemSettings\.statusConfirmation\.skipLabel/);
  assert.match(chinese, /channels\.dialogs\.systemSettings\.statusConfirmation\.skipLabel/);
});
