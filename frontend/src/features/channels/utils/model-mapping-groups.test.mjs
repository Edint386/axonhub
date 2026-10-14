import assert from 'node:assert/strict';
import test from 'node:test';
import {
  addAliasTarget,
  connectorLayout,
  groupModelMappings,
  hasModelMapping,
  removeMappingAt,
  removeModelAlias,
  renameModelAlias,
  replaceMappingTarget,
} from './model-mapping-groups.ts';

const mappings = [
  { from: 'general', to: 'openai/gpt-oss-20b' },
  { from: 'translation', to: 'qwen/qwen3-32b' },
  { from: 'translation', to: 'groq/compound-mini' },
  { from: 'qwen3.5-plus', to: 'qwen/qwen3.6-27b' },
  { from: 'general', to: 'groq/compound' },
];

test('groups aliases in first-appearance order and keeps entry indexes', () => {
  const groups = groupModelMappings(mappings);

  assert.deepEqual(
    groups.map((group) => group.alias),
    ['general', 'translation', 'qwen3.5-plus']
  );
  assert.deepEqual(groups[0].targets, [
    { to: 'openai/gpt-oss-20b', index: 0 },
    { to: 'groq/compound', index: 4 },
  ]);
  assert.equal(groups[1].targets.length, 2);
  assert.deepEqual(groupModelMappings([]), []);
});

test('adds a target next to the alias it belongs to', () => {
  const next = addAliasTarget(mappings, 'translation', 'qwen/qwen3.6-27b');

  assert.deepEqual(next[3], { from: 'translation', to: 'qwen/qwen3.6-27b' });
  assert.equal(next.length, mappings.length + 1);
  assert.deepEqual(addAliasTarget([], 'fast', 'a'), [{ from: 'fast', to: 'a' }]);
  assert.equal(mappings.length, 5, 'the input list is not mutated');
});

test('renames an alias and merges into an existing one without duplicates', () => {
  const renamed = renameModelAlias(mappings, 'translation', 'tr');
  assert.deepEqual(
    renamed.filter((mapping) => mapping.from === 'tr').map((mapping) => mapping.to),
    ['qwen/qwen3-32b', 'groq/compound-mini']
  );

  const merged = renameModelAlias(
    [
      { from: 'a', to: 'x' },
      { from: 'b', to: 'x' },
      { from: 'b', to: 'y' },
    ],
    'b',
    'a'
  );
  assert.deepEqual(merged, [
    { from: 'a', to: 'x' },
    { from: 'a', to: 'y' },
  ]);
});

test('removes a single target or a whole alias', () => {
  assert.deepEqual(
    removeModelAlias(mappings, 'general').map((mapping) => mapping.from),
    ['translation', 'translation', 'qwen3.5-plus']
  );
  assert.deepEqual(removeMappingAt(mappings, 4).at(-1), { from: 'qwen3.5-plus', to: 'qwen/qwen3.6-27b' });
  assert.deepEqual(replaceMappingTarget(mappings, 0, 'groq/compound-mini')[0], { from: 'general', to: 'groq/compound-mini' });
  assert.ok(hasModelMapping(mappings, 'general', 'groq/compound'));
  assert.ok(!hasModelMapping(mappings, 'general', 'qwen/qwen3-32b'));
});

test('connector curves start at the alias middle and end at each target middle', () => {
  const single = connectorLayout(1, 56, 36, 8, 4);
  assert.equal(single.height, 36);
  assert.deepEqual(single.start, { x: 4, y: 18 });
  assert.deepEqual(single.ends, [{ x: 52, y: 18 }]);
  assert.equal(single.paths[0], 'M 4 18 C 28 18, 28 18, 52 18');

  const three = connectorLayout(3, 56, 36, 8, 4);
  assert.equal(three.height, 3 * 36 + 2 * 8);
  assert.equal(three.start.y, three.height / 2);
  assert.deepEqual(
    three.ends.map((end) => end.y),
    [18, 62, 106]
  );
  assert.equal(three.paths.length, 3);
  assert.ok(three.paths.every((path) => path.startsWith(`M 4 ${three.height / 2} C 28`)));

  assert.equal(connectorLayout(0).paths.length, 1, 'an empty group still draws one connector');
});
