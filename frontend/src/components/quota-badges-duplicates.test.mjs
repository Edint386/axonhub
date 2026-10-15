import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// Custom guard: an upstream merge once re-added the Ollama quota branch and its
// popover block next to the existing ones, so Ollama Cloud channels showed every
// usage bar twice. Each provider condition must appear at most once.
const source = readFileSync(join(import.meta.dirname, 'quota-badges.tsx'), 'utf8');

function repeated(pattern) {
  const counts = new Map();
  for (const [match] of source.matchAll(pattern)) {
    counts.set(match, (counts.get(match) ?? 0) + 1);
  }
  return [...counts].filter(([, count]) => count > 1).map(([match, count]) => `${match} (x${count})`);
}

test('quota popover renders each provider block once', () => {
  assert.deepEqual(repeated(/\{is[A-Za-z]+Type\(channel\.type\) && \(/g), []);
  assert.deepEqual(repeated(/\{channel\.type === '[a-z_]+' &&/g), []);
});

test('quota percentage handles each provider branch once', () => {
  assert.deepEqual(repeated(/else if \(is[A-Za-z]+Type\(channel\.type\)\)/g), []);
  assert.deepEqual(repeated(/else if \(channel\.type === [^)]+\)/g), []);
});
