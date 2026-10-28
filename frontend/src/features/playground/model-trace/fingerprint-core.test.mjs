import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { analyzeGlobalOutputs, minimumNumbers, parseNumbers } from './fingerprint-core.ts';

const bank = JSON.parse(readFileSync(join(import.meta.dirname, 'unified_bank.json'), 'utf8'));
// Outputs of the original ModelTrace static/fingerprint-core.js on reference answers.
const parity = JSON.parse(readFileSync(join(import.meta.dirname, 'testdata', 'parity.json'), 'utf8'));

function assertClose(actual, expected, path) {
  if (typeof expected === 'number') {
    assert.ok(Math.abs(actual - expected) <= 1e-12 * Math.max(1, Math.abs(expected)), `${path}: ${actual} != ${expected}`);
    return;
  }
  if (Array.isArray(expected)) {
    assert.equal(actual.length, expected.length, `${path}: length`);
    expected.forEach((item, index) => assertClose(actual[index], item, `${path}[${index}]`));
    return;
  }
  if (expected && typeof expected === 'object') {
    for (const key of Object.keys(expected)) {
      if (key === 'method') continue; // the port labels the method in English
      assertClose(actual[key], expected[key], `${path}.${key}`);
    }
    return;
  }
  assert.equal(actual, expected, path);
}

for (const sample of parity.cases) {
  test(`matches the original ModelTrace scoring: ${sample.name}`, () => {
    assertClose(analyzeGlobalOutputs(sample.outputs, bank), sample.expected, 'result');
  });
}

test('the bank and fixtures cover the same models', () => {
  const ids = bank.models.map((model) => model.id).sort();
  for (const sample of parity.cases) {
    assert.deepEqual(sample.expected.results.map((item) => item.model).sort(), ids);
  }
});

test('parseNumbers keeps the longest in-range run and splits on words', () => {
  assert.deepEqual(parseNumbers('1, 2, 3 then 400, 5, 6, 7, 8'), [5, 6, 7, 8]);
  assert.deepEqual(parseNumbers('12 355 0 356 7'), [12, 355, 7]);
  assert.deepEqual(parseNumbers('no numbers'), []);
});

test('minimumNumbers follows ModelTrace (55% of the request, at least 80)', () => {
  assert.equal(minimumNumbers(300), 165);
  assert.equal(minimumNumbers(100), 80);
  assert.equal(minimumNumbers(0), 80);
});

test('rejects a set with no usable answer', () => {
  assert.throws(() => analyzeGlobalOutputs([{ text: '1, 2, 3', expected_count: 300 }], bank));
});
