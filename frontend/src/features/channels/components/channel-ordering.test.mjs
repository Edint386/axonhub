import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { applyOrderingMove } from './channel-ordering.ts';

const makeItem = (id, orderingWeight, priority = 0) => ({ id, orderingWeight, priority });

const assertStrictlyDescending = (values) => {
  for (let index = 1; index < values.length; index += 1) {
    assert.ok(values[index - 1] > values[index], `${values[index - 1]} must be greater than ${values[index]}`);
  }
};

test('priority move splits tied priorities into fixed descending steps', () => {
  const items = [makeItem('c', 0, 10), makeItem('a', 0, 10), makeItem('b', 0, 10)];

  const result = applyOrderingMove(items, 0, 'priority');

  assert.equal(result.ok, true);
  assert.deepEqual(
    result.items.map((item) => item.id),
    ['c', 'a', 'b']
  );
  assert.deepEqual(
    result.items.map((item) => item.priority),
    [30, 20, 10]
  );
});

test('equal neighboring weights trigger a strict deterministic rebalance', () => {
  const items = [makeItem('a', 50), makeItem('moved', 10), makeItem('b', 50)];

  const result = applyOrderingMove(items, 1, 'weight');

  assert.equal(result.ok, true);
  assertStrictlyDescending(result.items.map((item) => item.orderingWeight));
  assert.deepEqual(
    result.items.map((item) => item.orderingWeight),
    [100, 50, 0]
  );
});

test('adjacent neighboring weights trigger a strict deterministic rebalance', () => {
  const items = [makeItem('a', 51), makeItem('moved', 10), makeItem('b', 50)];

  const result = applyOrderingMove(items, 1, 'weight');

  assert.equal(result.ok, true);
  assert.deepEqual(
    result.items.map((item) => item.orderingWeight),
    [100, 50, 0]
  );
});

test('moving above weight 100 rebalances within the valid range', () => {
  const items = [makeItem('moved', 20), makeItem('top', 100), makeItem('bottom', 40)];

  const result = applyOrderingMove(items, 0, 'weight');

  assert.equal(result.ok, true);
  assert.equal(result.items[0].orderingWeight, 100);
  assert.equal(result.items.at(-1).orderingWeight, 0);
  assertStrictlyDescending(result.items.map((item) => item.orderingWeight));
});

test('moving below weight 0 rebalances within the valid range', () => {
  const items = [makeItem('top', 60), makeItem('bottom', 0), makeItem('moved', 90)];

  const result = applyOrderingMove(items, 2, 'weight');

  assert.equal(result.ok, true);
  assert.equal(result.items[0].orderingWeight, 100);
  assert.equal(result.items.at(-1).orderingWeight, 0);
  assertStrictlyDescending(result.items.map((item) => item.orderingWeight));
});

test('an available local gap changes only the moved item', () => {
  const items = [makeItem('top', 90), makeItem('moved', 5), makeItem('bottom', 70)];

  const result = applyOrderingMove(items, 1, 'weight');

  assert.equal(result.ok, true);
  assert.equal(result.items[0], items[0]);
  assert.notEqual(result.items[1], items[1]);
  assert.equal(result.items[2], items[2]);
  assert.deepEqual(
    result.items.map((item) => item.orderingWeight),
    [90, 80, 70]
  );
});

test('a 102-item weight rebalance is refused without mutating the input', () => {
  const items = Array.from({ length: 102 }, (_, index) => makeItem(String(index), 50));
  const snapshot = structuredClone(items);

  const result = applyOrderingMove(items, 50, 'weight');

  assert.deepEqual(result, { ok: false, reason: 'weight-capacity', items });
  assert.deepEqual(items, snapshot);
});

test('bulk ordering mode and capacity messages exist in both locales', () => {
  const srcRoot = join(import.meta.dirname, '..', '..', '..');
  const requiredKeys = [
    'channels.dialogs.bulkOrdering.priorityDescription',
    'channels.dialogs.bulkOrdering.priorityMode',
    'channels.dialogs.bulkOrdering.weightMode',
    'channels.dialogs.bulkOrdering.priorityDragHint',
    'channels.dialogs.bulkOrdering.duplicatePriorityHint',
    'channels.dialogs.bulkOrdering.priority',
    'channels.dialogs.bulkOrdering.weightCapacityError',
  ];

  for (const locale of ['en', 'zh-CN']) {
    const messages = JSON.parse(readFileSync(join(srcRoot, 'locales', locale, 'channels.json'), 'utf8'));
    for (const key of requiredKeys) {
      assert.equal(typeof messages[key], 'string', `${locale} is missing ${key}`);
      assert.notEqual(messages[key].trim(), '', `${locale} has an empty ${key}`);
    }
  }
});
