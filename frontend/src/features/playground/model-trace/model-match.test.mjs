import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { analyzeGlobalOutputs } from './fingerprint-core.ts';
import { findBankModel, judgeClaim, normalizeModelName } from './model-match.ts';

const bank = JSON.parse(readFileSync(join(import.meta.dirname, 'unified_bank.json'), 'utf8'));
const { models } = bank;

// The fields judgeClaim reads, for a prediction whose top model and top family agree.
const result = (prediction) => {
  const model = models.find((item) => item.id === prediction);
  return { prediction, family_prediction: model.family, results: [{ model: prediction, family: model.family }] };
};

test('normalizes provider prefixes, dates, hosted version tags and effort suffixes', () => {
  assert.equal(normalizeModelName('anthropic/Claude-Opus-4.7-20260101-thinking'), 'claude-opus-4-7');
  assert.equal(normalizeModelName('gpt-5.6-sol-high'), 'gpt-5-6-sol');
  assert.equal(normalizeModelName('claude-haiku-4-5-20251001'), 'claude-haiku-4-5');
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

test('finds bank models by name but not different models that share a prefix', () => {
  assert.equal(findBankModel('claude-haiku-4-5', models)?.id, 'claude-haiku-4-5-20251001');
  assert.equal(findBankModel('openai/gpt-5.5', models)?.id, 'gpt-5.5');
  assert.equal(findBankModel('claude-opus-5-5', models)?.id, 'claude-opus-5-5');
  assert.equal(findBankModel('claude-opus-5', models)?.id, 'claude-opus-5');
  for (const name of ['gpt-5.4-mini', 'gpt-5.5-pro', 'claude-opus-4-1', 'claude-3-7-sonnet', 'gpt-oss-120b', 'gemini-3-pro']) {
    assert.equal(findBankModel(name, models), undefined, name);
  }
});

test('judges a claimed bank model against the prediction', () => {
  assert.equal(judgeClaim('claude-opus-4-7', result('claude-opus-4-7'), models).kind, 'match');
  assert.equal(judgeClaim('us.anthropic.claude-opus-4-7-v1:0', result('claude-opus-4-7'), models).kind, 'match');
  assert.equal(judgeClaim('claude-opus-4-7', result('claude-opus-4-6'), models).kind, 'same-family');
  const different = judgeClaim('claude-opus-4-7', { ...result('gpt-5.5'), family_probability: 0.95 }, models);
  assert.equal(different.kind, 'different-family');
  assert.equal(different.claimed.family, 'claude');
});

test('needs a confident family before calling it a different family', () => {
  assert.equal(judgeClaim('claude-opus-4-7', { ...result('gpt-5.5'), family_probability: 0.6 }, models).kind, 'uncertain');
  // A real near split: one answer each from two Claude models and gpt-5.5.
  const parity = JSON.parse(readFileSync(join(import.meta.dirname, 'testdata', 'parity.json'), 'utf8'));
  const first = (name) => parity.cases.find((sample) => sample.name === name).outputs[0];
  const scored = analyzeGlobalOutputs(
    [first('three outputs from claude-opus-4-7'), first('three outputs from claude-sonnet-5-5'), first('three outputs from gpt-5.5')],
    bank
  );
  assert.ok(scored.family_probability < 0.8, `family probability ${scored.family_probability}`);
  assert.equal(judgeClaim('claude-opus-4-7', scored, models).kind, 'uncertain');
});

test('gives no verdict for models outside the bank, even of a bank family', () => {
  // ModelTrace is closed-set: an unseen sibling version is often attributed to another family.
  assert.equal(judgeClaim('claude-opus-4-1', result('gpt-6-sol'), models).kind, 'not-covered');
  assert.equal(judgeClaim('claude-opus-4-1', result('claude-opus-4-6'), models).kind, 'not-covered');
  assert.equal(judgeClaim('gemini-3-pro', result('gpt-6-sol'), models).kind, 'not-covered');
});

test('is uncertain when the top model and the most likely family disagree', () => {
  const split = { prediction: 'claude-opus-5-5', family_prediction: 'gpt', results: [{ model: 'claude-opus-5-5', family: 'claude' }] };
  assert.equal(judgeClaim('claude-opus-5-5', split, models).kind, 'uncertain');
  assert.equal(judgeClaim('gpt-5.5', split, models).kind, 'uncertain');

  // The same split on real scores: uniform random numbers sit between the families.
  let seed = 7;
  const rand = () => (seed = (seed * 1103515245 + 12345) % 2147483648) / 2147483648;
  for (let run = 0; run < 400; run += 1) {
    const outputs = [0, 1, 2].map(() => ({ text: Array.from({ length: 300 }, () => 1 + Math.floor(rand() * 355)).join(', '), expected_count: 300 }));
    const scored = analyzeGlobalOutputs(outputs, bank);
    if (scored.results[0].family === scored.family_prediction) continue;
    assert.equal(judgeClaim(scored.prediction, scored, models).kind, 'uncertain');
    return;
  }
  assert.fail('no split result found');
});
