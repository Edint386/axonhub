// Guard test for the custom (fork) realtime endpoint format.
//
// The realtime relay adds the 'openai/realtime' entry to configurableChannelEndpointApiFormats
// in the upstream-owned schema.ts. Removing it still type-checks, so this test fails loudly when
// an upstream merge drops it. It also pins the locale labels that live in the custom
// realtime.json files. The per-second billing lines are guarded in duration-price-item.test.mjs.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('the endpoints dialog offers openai/realtime (fork hook in schema.ts)', () => {
  const schema = read('features/channels/data/schema.ts');
  assert.match(
    schema,
    /configurableChannelEndpointApiFormats[^;]*'openai\/realtime'/,
    "configurableChannelEndpointApiFormats must contain 'openai/realtime' (fork hook)"
  );
});

test('both locales label the openai/realtime format', () => {
  for (const locale of ['en', 'zh-CN']) {
    const catalog = JSON.parse(read(join('locales', locale, 'realtime.json')));
    assert.ok(
      catalog['channels.dialogs.fields.apiFormat.formats.openai/realtime'],
      `${locale} is missing the openai/realtime format label`
    );
  }
});
