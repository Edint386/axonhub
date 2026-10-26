import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import ts from 'typescript';

// Custom guard: upstream groups and sorts the provider quota popover. Channels
// that only carry a local AxonHub quota get their own 'local_quota' category
// and stay after every provider channel in every sort mode, which keeps them
// listed last as before upstream's redesign. The production helpers are
// extracted and executed, like upstream's own quota-badges tests do.
const source = readFileSync(join(import.meta.dirname, 'quota-badges.tsx'), 'utf8');
const ast = ts.createSourceFile('quota-badges.tsx', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
const helperNames = [
  'isOpenaiType',
  'isCommandCodeType',
  'isOllamaType',
  'isMinimaxType',
  'isZenmuxType',
  'getChannelCategory',
  'sortQuotaChannels',
  'buildQuotaDisplayItems',
];
const helperNodes = ast.statements.filter((node) => ts.isFunctionDeclaration(node) && helperNames.includes(node.name?.text));
assert.equal(helperNodes.length, helperNames.length, 'quota popover helpers must stay in quota-badges.tsx');
const { outputText } = ts.transpileModule(
  helperNodes.map((node) => `export ${node.getText(ast).replace(/^export\s+/, '')}`).join('\n'),
  { compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 } }
);
const { getChannelCategory, sortQuotaChannels, buildQuotaDisplayItems } = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString('base64')}`
);

const t = (key) => key;
const provider = (id, name, type, orderingWeight) => ({ id, name, type, orderingWeight, quotaStatus: { status: 'available' } });
const localOnly = (id, name, orderingWeight) => ({
  id,
  name,
  type: 'openai',
  orderingWeight,
  localQuota: { requests: 10 },
  quotaStatus: { status: 'unknown' },
});
const channels = [
  localOnly('1', 'aaa-local', 100),
  provider('2', 'zz-codex', 'codex', 1),
  provider('3', 'antigravity-x', 'antigravity', 5),
  localOnly('4', 'bbb-local', 50),
  provider('5', 'claude', 'claudecode', 2),
];

test('local-only quota channels get their own category', () => {
  assert.deepEqual(getChannelCategory(channels[0], t), { key: 'local_quota', label: 'quota.label.local_quota' });
  // A channel with both a provider quota and a local quota stays in its provider category.
  assert.equal(getChannelCategory({ ...provider('6', 'both', 'codex', 1), localQuota: { requests: 1 } }, t).key, 'codex');
});

test('local-only quota channels sort after every provider channel in every mode', () => {
  for (const mode of ['preferred', 'alphabetical', 'created']) {
    const sorted = sortQuotaChannels(channels, mode, t);
    const firstLocal = sorted.findIndex((c) => c.localQuota != null);
    assert.ok(firstLocal > 0, `${mode}: provider channels come first`);
    assert.ok(
      sorted.slice(firstLocal).every((c) => c.localQuota != null),
      `${mode}: every channel after the first local-only one is local-only`
    );
  }
});

test('grouped view shows the local quota section last', () => {
  const sorted = sortQuotaChannels(channels, 'alphabetical', t);
  const categories = Array.from(
    new Map(sorted.map((c) => [getChannelCategory(c, t).key, { ...getChannelCategory(c, t), count: 0 }])).values()
  );
  const items = buildQuotaDisplayItems(sorted, categories, 'all', t);
  const lastHeader = items.filter((item) => item.kind === 'header').at(-1);
  assert.equal(lastHeader?.cat.key, 'local_quota');
  assert.equal(items.at(-1).channel.localQuota != null, true);
});

test('local quota channels keep their own row in the deduplicated list', () => {
  // Every provider-level dedupe rule skips local-quota rows, so they are never
  // chosen as a representative (and never mutated by the shared-account merge).
  assert.match(source, /acc\.find\(\(c\) => c\.type === 'nanogpt' && !c\.localQuota\)/);
  assert.match(source, /acc\.find\(\(c\) => !c\.localQuota && isOpenaiType\(c\.type\)/);
  assert.match(source, /acc\.find\(\(c\) => !c\.localQuota && c\.accountKey === channel\.accountKey\)/);
  assert.match(
    source,
    /channels\.reduce\(\(acc: ProviderQuotaChannel\[\], channel: ProviderQuotaChannel\) => \{\n(?:\s*\/\/[^\n]*\n)*\s*if \(channel\.localQuota\) \{\n\s*acc\.push\(channel\);\n\s*return acc;/
  );
});
