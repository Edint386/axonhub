import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const featureDir = dirname(fileURLToPath(import.meta.url));
const read = (name) => readFileSync(join(featureDir, name), 'utf8');
const grade = read('probe-grade.ts');
const page = read('index.tsx');
const actions = read('use-probe-actions.ts');
const settings = read('components/probe-settings-sheet.tsx');
const matrix = read('components/channel-matrix-table.tsx');

test('primary model metrics use the ordered enabled model', () => {
  assert.match(grade, /channel\.primaryModelID/);
  assert.match(grade, /configuredPrimary[\s\S]*return configuredPrimary/);
  assert.match(grade, /return channel\.models\.find\(\(model\) => model\.enabled\)/);
  assert.match(grade, /primaryModelOf\(channel\)\?\.p95Ms/);
});

test('missing first-token metrics stay unknown and failures stay degraded or worse', () => {
  assert.match(grade, /if \(firstTokenMsOf\(latestProbe\) == null\) \{[\s\S]*return 'unknown'/);
  assert.match(grade, /if \(probes\.length === 0\) \{[\s\S]*return 'skipped'/);
  assert.doesNotMatch(grade, /skips > probes\.length/);
  assert.match(grade, /if \(fails > 0\) \{[\s\S]*return 'degraded'/);
  assert.match(grade, /if \(rate === 0\) \{[\s\S]*return 'error'/);
});

test('problem status is implemented in the status dropdown filter', () => {
  assert.match(page, /statusFilter === 'problem'[\s\S]*PROBLEM_GRADES\.includes\(grade\)/);
  assert.doesNotMatch(page, /filters\.badOnly/);
});

test('manual probe targets reserve synchronously and settle all requests', () => {
  assert.match(actions, /useRef\(new Set<string>\(\)\)/);
  assert.match(actions, /if \(keys\.some\(\(key\) => probingRef\.current\.has\(key\)\)\)/);
  assert.match(actions, /Promise\.allSettled\(/);
  assert.match(actions, /finally \{[\s\S]*releaseTargets/);
});

test('settings uses the real-model catalog and preserves drag order', () => {
  assert.match(settings, /policy\.availableModels\.filter/);
  assert.match(settings, /arrayMove\(current, oldIndex, newIndex\)/);
  assert.doesNotMatch(settings, /\.sort\(\)/);
});

test('model rows grade the run instead of showing its raw status', () => {
  assert.match(matrix, /gradeOfRun\(primaryModel\.latestRun, thresholdMs\)/);
  assert.match(matrix, /gradeOfRun\(model\.latestRun, thresholdMs\)/);
  assert.doesNotMatch(matrix, /latestRun\?\.status/);
});

test('latest first-token latency never falls back to an older run', () => {
  assert.match(grade, /const latest = probes\[probes\.length - 1\]/);
  assert.match(grade, /latest\?\.status === 'healthy' \? firstTokenMsOf\(latest\) : null/);
  assert.doesNotMatch(grade, /for \(let i = runs\.length - 1/);
});
