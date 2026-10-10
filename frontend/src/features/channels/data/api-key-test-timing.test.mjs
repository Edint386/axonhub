// API key tests (XSN-4) return first-byte, first-token, and total timings plus the
// stream mode. zod strips undeclared keys while parsing, so the result schema must
// declare every timing field the queries request, or the dialog never shows them.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

const timingFields = ['ttfbMs', 'ttftMs', 'totalMs', 'stream'];

test('API key test result schema keeps the timing fields', () => {
  const schema = read('features/channels/data/schema.ts');
  const start = schema.indexOf('export const testAPIKeyResultSchema = z.object({');
  assert.ok(start !== -1, 'testAPIKeyResultSchema should exist');
  const block = schema.slice(start, schema.indexOf('});', start));

  for (const field of timingFields) {
    assert.match(block, new RegExp(`\\b${field}:`), `testAPIKeyResultSchema must declare ${field}`);
  }
});

test('API key test queries request the timing fields', () => {
  const channels = read('features/channels/data/channels.ts');

  for (const name of ['TEST_CHANNEL_API_KEYS_MUTATION', 'TEST_CHANNEL_API_KEY_MUTATION']) {
    const start = channels.indexOf(`const ${name} = \``);
    assert.ok(start !== -1, `${name} should exist`);
    const block = channels.slice(start, channels.indexOf('`;', start));
    for (const field of timingFields) {
      assert.match(block, new RegExp(`\\b${field}\\b`), `${name} must request ${field}`);
    }
  }
});
