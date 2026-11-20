import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { analyzeFingerprint } from './fingerprint-core.ts';
import { decodeDetectorSnapshot } from './detector.ts';
import { findBankModel, judgeClaim, normalizeModelName } from './model-match.ts';

const dir = import.meta.dirname;
const bundle = JSON.parse(readFileSync(join(dir, 'data', 'bundle.json'), 'utf8'));
const { bank } = await decodeDetectorSnapshot(bundle, readFileSync(join(dir, 'data', 'shared_detector.bin.zst')));
const { models } = bank;

// The fields judgeClaim reads, for a prediction whose top model and top family agree.
const result = (prediction, extra = {}) => {
  const model = models.find((item) => item.id === prediction);
  return {
    prediction,
    family_prediction: model.family,
    family_probability: 0.95,
    results: [{ model: prediction, family: model.family }],
    ...extra,
  };
};

test('normalizes provider prefixes, dates, hosted version tags and effort suffixes', () => {
  assert.equal(normalizeModelName('anthropic/Claude-Opus-4.7-20260101-thinking'), 'claude-opus-4-7');
  assert.equal(normalizeModelName('gpt-5.6-sol-high'), 'gpt-5-6-sol');
  assert.equal(normalizeModelName('claude-haiku-4-5-20251001'), 'claude-haiku-4-5');
  assert.equal(normalizeModelName('claude-haiku-4.5'), 'claude-haiku-4-5');
  assert.equal(normalizeModelName('gpt-5.4-2026-03-05'), 'gpt-5-4');
  assert.equal(normalizeModelName('us.anthropic.claude-opus-4-7-v1:0'), 'claude-opus-4-7');
  assert.equal(normalizeModelName('anthropic.claude-haiku-4-5-20251001-v1:0'), 'claude-haiku-4-5');
  assert.equal(normalizeModelName('claude-opus-4-7@20260101'), 'claude-opus-4-7');
  assert.equal(normalizeModelName('claude-opus-4-7[1m]'), 'claude-opus-4-7');
  assert.equal(normalizeModelName('jp.anthropic.claude-haiku-4-5-20251001-v1:0'), 'claude-haiku-4-5');
  assert.equal(normalizeModelName('us-gov.anthropic.claude-sonnet-4-6-v1:0'), 'claude-sonnet-4-6');
  assert.equal(normalizeModelName('azure-openai-gpt-5.5'), 'gpt-5-5');
  // every bank id still resolves to itself
  for (const model of models) assert.equal(findBankModel(model.id, models)?.id, model.id);
});

test('finds bank models by id, dotted versions and slash display aliases', () => {
  assert.equal(findBankModel('claude-haiku-4-5', models)?.id, 'claude-haiku-4.5');
  assert.equal(findBankModel('claude-haiku-4.5', models)?.id, 'claude-haiku-4.5');
  assert.equal(findBankModel('openai/gpt-5.5', models)?.id, 'gpt-5.5');
  assert.equal(findBankModel('claude-opus-5-5', models)?.id, 'claude-opus-5.5');
  assert.equal(findBankModel('claude-opus-5', models)?.id, 'claude-opus-5');
  assert.equal(findBankModel('gpt-6-astra', models)?.id, 'gpt-6-astra');
  assert.equal(findBankModel('gpt-6-astra/6.1-sol', models)?.id, 'gpt-6-astra');
  assert.equal(findBankModel('gpt-6.1-sol', models)?.id, 'gpt-6-astra');
  for (const name of ['gpt-5.4-mini', 'gpt-5.5-pro', 'claude-opus-4-1', 'claude-3-7-sonnet', 'gpt-oss-120b', 'gemini-3-pro']) {
    assert.equal(findBankModel(name, models), undefined, name);
  }
});

test('judges a claimed bank model against the prediction', () => {
  assert.equal(judgeClaim('claude-opus-4-7', result('claude-opus-4.7'), models).kind, 'match');
  assert.equal(judgeClaim('us.anthropic.claude-opus-4-7-v1:0', result('claude-opus-4.7'), models).kind, 'match');
  assert.equal(judgeClaim('claude-opus-4-7', result('claude-opus-4.6'), models).kind, 'same-family');
  const different = judgeClaim('claude-opus-4-7', result('gpt-5.5'), models);
  assert.equal(different.kind, 'different-family');
  assert.equal(different.claimed.family, 'claude');
});

test('needs a confident family before calling it a different family', () => {
  assert.equal(judgeClaim('claude-opus-4-7', result('gpt-5.5', { family_probability: 0.6 }), models).kind, 'uncertain');
});

test('gives no verdict for models outside the bank, even of a bank family', () => {
  assert.equal(judgeClaim('claude-opus-4-1', result('gpt-6-sol'), models).kind, 'not-covered');
  assert.equal(judgeClaim('claude-opus-4-1', result('claude-opus-4.6'), models).kind, 'not-covered');
  assert.equal(judgeClaim('gemini-3-pro', result('gpt-6-sol'), models).kind, 'not-covered');
});

test('is uncertain when the top model and the most likely family disagree', () => {
  const split = { prediction: 'claude-opus-5.5', family_prediction: 'gpt', family_probability: 0.95, results: [{ model: 'claude-opus-5.5', family: 'claude' }] };
  assert.equal(judgeClaim('claude-opus-5.5', split, models).kind, 'uncertain');
  assert.equal(judgeClaim('gpt-5.5', split, models).kind, 'uncertain');
});

test('a real fixture score still agrees with the claimed-model verdict', async () => {
  const fixture = JSON.parse(readFileSync(join(dir, 'testdata', 'fixture.json'), 'utf8'));
  const scored = analyzeFingerprint(fixture.outputs, bank, (await decodeDetectorSnapshot(bundle, readFileSync(join(dir, 'data', 'shared_detector.bin.zst')))).detector);
  assert.equal(judgeClaim('claude-opus-4.7', scored, models).kind, 'match');
  assert.equal(judgeClaim('gpt-6-sol', scored, models).kind, 'different-family');
});
