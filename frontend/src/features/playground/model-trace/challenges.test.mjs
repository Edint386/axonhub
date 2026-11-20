import assert from 'node:assert/strict';
import test from 'node:test';
import { generateChallenges } from './challenges.ts';

test('generates multilingual challenges without crypto.randomUUID', () => {
  const original = crypto.randomUUID;
  crypto.randomUUID = () => {
    throw new Error('crypto.randomUUID should not be used');
  };
  try {
    const challenges = generateChallenges(3);
    assert.equal(challenges.length, 3);
    const counts = new Set();
    for (const challenge of challenges) {
      assert.match(challenge.id, /^probe-\d+-[0-9a-f]{16}$/);
      assert.ok(challenge.expected_count >= 292 && challenge.expected_count <= 332);
      assert.ok(!counts.has(challenge.expected_count), 'lengths should be unique');
      counts.add(challenge.expected_count);
      assert.ok(challenge.prompt.length > 40);
    }
  } finally {
    crypto.randomUUID = original;
  }
});
