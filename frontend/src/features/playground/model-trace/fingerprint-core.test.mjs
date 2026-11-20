import assert from 'node:assert/strict';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { decodeDetectorSnapshot } from './detector.ts';
import { analyzeFingerprint, minimumNumbers, parseNumbers } from './fingerprint-core.ts';

const dir = import.meta.dirname;
const bundle = JSON.parse(readFileSync(join(dir, 'data', 'bundle.json'), 'utf8'));
const compressed = readFileSync(join(dir, 'data', 'shared_detector.bin.zst'));
const fixture = JSON.parse(readFileSync(join(dir, 'testdata', 'fixture.json'), 'utf8'));
const loaded = await decodeDetectorSnapshot(bundle, compressed);
const { bank, detector } = loaded;

test('loads the lm-detector snapshot and not the old ModelTrace bank', () => {
  assert.equal(bundle.schema, 'lmfpd-data-v1');
  assert.equal(bundle.detector_schema, 'shared-detector-v2');
  assert.equal(bank.models.length, 75);
  assert.ok(bank.models.some((model) => model.id === 'claude-opus-4.7'));
  assert.equal(existsSync(join(dir, 'unified_bank.json')), false);
  assert.equal(existsSync(join(dir, 'testdata', 'parity.json')), false);
  assert.equal(existsSync(join(dir, 'testdata', 'make-parity.mjs')), false);
});

test('a known reference answer set ranks its model and family', () => {
  const result = analyzeFingerprint(fixture.outputs, bank, detector);
  assert.equal(result.used_outputs, 3);
  assert.equal(result.method, 'shared-detector-v2');
  assert.notEqual(result.decision, 'not_model');
  assert.equal(result.prediction, fixture.source.model);
  assert.equal(result.family_prediction, fixture.source.family);
  assert.ok(result.probability != null && result.probability > 0.5, `probability ${result.probability}`);
  assert.ok(result.family_probability > 0.8, `family probability ${result.family_probability}`);
  assert.equal(result.results[0].model, fixture.source.model);
});

test('parseNumbers keeps the longest in-range run and splits on words', () => {
  assert.deepEqual(parseNumbers('1, 2, 3 then 400, 5, 6, 7, 8'), [5, 6, 7, 8]);
  assert.deepEqual(parseNumbers('12 355 0 356 7'), [12, 355, 7]);
  assert.deepEqual(parseNumbers('no numbers'), []);
});

test('minimumNumbers follows the shared 55% / at-least-80 rule', () => {
  assert.equal(minimumNumbers(300), 165);
  assert.equal(minimumNumbers(100), 80);
  assert.equal(minimumNumbers(0), 80);
});

test('rejects a set with no usable answer', () => {
  assert.throws(() => analyzeFingerprint([{ text: '1, 2, 3', expected_count: 300 }], bank, detector));
});

test('does not rank answers that do not look like a language model', () => {
  const outputs = [0, 1, 2].map(() => ({
    text: Array.from({ length: 300 }, (_, index) => 1 + (index % 355)).join(', '),
    expected_count: 300,
  }));
  const result = analyzeFingerprint(outputs, bank, detector);
  assert.equal(result.decision, 'not_model');
  assert.equal(result.results.length, 0);
});
