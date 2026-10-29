// Guard test for the custom ModelTrace check view.
//
// The feature lives in this folder; the only lines it adds to upstream-owned code are in
// features/playground/index.tsx (mode state, mode switch, hiding the chat card and its sidebar
// buttons, mounting the panel).
// Removing any of them still type-checks, so this test fails loudly when an upstream merge drops one.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('the playground page mounts the ModelTrace check view (custom hook in index.tsx)', () => {
  const page = read('features/playground/index.tsx');
  assert.match(page, /import \{ ModelTracePanel, PlaygroundModeSwitch, type PlaygroundMode \} from '\.\/model-trace\/model-trace-panel';/);
  assert.match(page, /useState<PlaygroundMode>\('chat'\)/);
  assert.match(page, /<PlaygroundModeSwitch value=\{playgroundMode\} onValueChange=\{setPlaygroundMode\} \/>/);
  assert.match(
    page,
    /'shadow-soft border-border bg-card flex h-full min-h-0 flex-col rounded-2xl border p-6',\s*playgroundMode === 'trace' && 'hidden'/,
    'the chat card must hide in check mode'
  );
  assert.match(
    page,
    /cn\('space-y-2 border-t p-4', playgroundMode === 'trace' && 'hidden'\)/,
    'the chat-only Retry/Clear buttons must hide in check mode'
  );
  assert.match(page, /<ModelTracePanel hidden=\{playgroundMode !== 'trace'\} modelSource=\{modelSource\} channelId=\{selectedChannel\} model=\{model\} \/>/);
});

// transport.ts re-implements how the playground chat calls the backend. These pin the parts it copies,
// so an upstream change to any of them fails here instead of silently breaking the check.
test('checks still send requests the way playground chat does', () => {
  const page = read('features/playground/index.tsx');
  const sync = 'upstream changed how playground chat calls the backend; sync model-trace/transport.ts';
  assert.match(page, /api: '\/admin\/playground\/chat'/, sync);
  assert.match(page, /'X-Project-ID': selectedProjectId \|\| ''/, sync);
  assert.match(page, /headers\['X-Channel-ID'\] = selectedChannelRef\.current/, sync);
  assert.match(page, /const token = await ensureFreshAccessToken\(\);/, sync);
  assert.match(page, /new DefaultChatTransport\(/, sync);
  // The SSE events transport.ts parses (text-delta, finish, error) are the AI SDK UI message stream
  // (protocol v1, unchanged from ai 5 to 7). transport.test.mjs replays the backend's own stream fixture.
  const pkg = JSON.parse(readFileSync(join(srcRoot, '..', 'package.json'), 'utf8'));
  assert.match(pkg.dependencies.ai, /^\^7\./, sync);
});

test('both locales define the same ModelTrace labels', () => {
  const en = JSON.parse(read('locales/en/modelTrace.json'));
  const zh = JSON.parse(read('locales/zh-CN/modelTrace.json'));
  assert.deepEqual(Object.keys(zh).sort(), Object.keys(en).sort());
  const panel = read('features/playground/model-trace/model-trace-panel.tsx');
  const used = [...panel.matchAll(/t\('(modelTrace\.[^']+)'/g)].map((match) => match[1]);
  for (const status of ['waiting', 'running', 'throttled', 'accepted', 'rejected', 'failed', 'cancelled']) used.push(`modelTrace.probe.status.${status}`);
  for (const status of ['running', 'done', 'cancelled']) used.push(`modelTrace.history.status.${status}`);
  for (const key of used) assert.ok(en[key] && zh[key], `missing locale key ${key}`);
  const variables = (text) => [...text.matchAll(/\{\{(\w+)\}\}/g)].map((match) => match[1]).sort();
  for (const key of Object.keys(en)) assert.deepEqual(variables(zh[key]), variables(en[key]), `${key} uses different variables`);
});
