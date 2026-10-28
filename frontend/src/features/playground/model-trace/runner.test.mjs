import assert from 'node:assert/strict';
import test from 'node:test';
import { generateChallenges } from './challenges.ts';
import { runProbes } from './runner.ts';

const goodAnswer = (count) => Array.from({ length: count }, (_, index) => (index * 37) % 355 + 1).join(', ');

// A fake sender that answers each prompt as `plan` says, in call order, after a tick.
function fakeSender(plan) {
  const calls = [];
  let inFlight = 0;
  let maxInFlight = 0;
  const send = (prompt, { signal, onText }) => {
    const step = plan[calls.length] ?? 'good';
    calls.push(prompt);
    inFlight += 1;
    maxInFlight = Math.max(maxInFlight, inFlight);
    return new Promise((resolve, reject) => {
      const finish = () => {
        inFlight -= 1;
        const count = Number(prompt.match(/ (\d+) 个 1 到 355/)[1]);
        if (step === 'fail') reject(new Error('502 upstream error'));
        else if (step === '429') reject(Object.assign(new Error('429 rate limited'), { status: 429 }));
        else if (step === 'short') resolve({ text: '1, 2, 3', finished: true });
        // no finish event: nearly complete, or cut off after 200 numbers
        else if (step === 'unfinished') resolve({ text: goodAnswer(count), finished: false });
        else if (step === 'cut') resolve({ text: goodAnswer(200), finished: false });
        else {
          const text = goodAnswer(count);
          onText(text.slice(0, 20));
          resolve({ text, finished: true });
        }
      };
      if (step === 'hang') {
        signal?.addEventListener('abort', () => {
          inFlight -= 1;
          reject(new DOMException('aborted', 'AbortError'));
        });
        return;
      }
      setTimeout(finish, 1);
    });
  };
  return { send, calls, maxInFlight: () => maxInFlight };
}

test('sends three challenges in parallel and stops once three answers are valid', async () => {
  const sender = fakeSender([]);
  const attempts = await runProbes({ challenges: generateChallenges(6), send: sender.send });
  assert.equal(sender.calls.length, 3);
  assert.equal(sender.maxInFlight(), 3);
  assert.deepEqual(
    attempts.map((attempt) => attempt.status),
    ['accepted', 'accepted', 'accepted', 'waiting', 'waiting', 'waiting']
  );
  assert.ok(attempts[0].parsedNumbers >= attempts[0].minimumNumbers);
});

test('replaces failed and too-short answers, never more than three at once', async () => {
  const sender = fakeSender(['fail', 'good', 'short', 'good', 'good']);
  const attempts = await runProbes({ challenges: generateChallenges(6), send: sender.send });
  assert.equal(sender.calls.length, 5);
  assert.ok(sender.maxInFlight() <= 3);
  assert.deepEqual(
    attempts.map((attempt) => attempt.status),
    ['failed', 'accepted', 'rejected', 'accepted', 'accepted', 'waiting']
  );
  assert.equal(attempts[0].error, '502 upstream error');
});

test('gives up after every challenge was used', async () => {
  const sender = fakeSender(['fail', 'fail', 'fail', 'short', 'fail', 'good']);
  const attempts = await runProbes({ challenges: generateChallenges(6), send: sender.send });
  assert.equal(sender.calls.length, 6);
  assert.equal(attempts.filter((attempt) => attempt.status === 'accepted').length, 1);
});

test('stopping cancels the requests in flight and sends no more', async () => {
  const sender = fakeSender(['hang', 'hang', 'hang']);
  const controller = new AbortController();
  const done = runProbes({ challenges: generateChallenges(6), send: sender.send, signal: controller.signal });
  await new Promise((resolve) => setTimeout(resolve, 5));
  controller.abort();
  const attempts = await done;
  assert.equal(sender.calls.length, 3);
  assert.deepEqual(
    attempts.slice(0, 3).map((attempt) => attempt.status),
    ['cancelled', 'cancelled', 'cancelled']
  );
  assert.ok(attempts.every((attempt) => attempt.error === undefined), 'a stopped request is not an error');
});

test('re-sends a rate-limited challenge after a backoff and runs fewer requests at once', async () => {
  const sender = fakeSender(['429', 'good', 'good', 'good']);
  const delays = [];
  const attempts = await runProbes({
    challenges: generateChallenges(6),
    send: sender.send,
    rateLimitDelayMs: (retry) => {
      delays.push(retry);
      return 1;
    },
  });
  assert.equal(sender.calls.length, 4);
  assert.equal(sender.calls[3], sender.calls[0], 'the same challenge is re-sent');
  assert.deepEqual(delays, [1]);
  assert.deepEqual(
    attempts.map((attempt) => attempt.status),
    ['accepted', 'accepted', 'accepted', 'waiting', 'waiting', 'waiting']
  );
  assert.equal(attempts[0].rateLimitRetries, 1);
});

test('a channel that keeps rate limiting does not use up the other challenges', async () => {
  const sender = fakeSender(Array(20).fill('429'));
  const attempts = await runProbes({ challenges: generateChallenges(6), send: sender.send, rateLimitDelayMs: () => 1 });
  // At most 3 challenges x (1 + 3 retries); the run stops once one challenge is still limited.
  assert.ok(sender.calls.length >= 4 && sender.calls.length <= 12, `${sender.calls.length} calls`);
  assert.ok(sender.maxInFlight() <= 3);
  assert.deepEqual(
    attempts.map((attempt) => attempt.status),
    ['failed', 'failed', 'failed', 'waiting', 'waiting', 'waiting']
  );
  assert.ok(attempts.some((attempt) => attempt.rateLimitRetries === 3));
});

test('an answer without the finish event counts only when nearly complete', async () => {
  const sender = fakeSender(['unfinished', 'cut', 'good', 'good']);
  const attempts = await runProbes({ challenges: generateChallenges(6), send: sender.send });
  assert.deepEqual(
    attempts.map((attempt) => attempt.status),
    ['accepted', 'failed', 'accepted', 'accepted', 'waiting', 'waiting']
  );
  assert.equal(attempts[0].unfinished, true);
  assert.equal(attempts[1].errorCode, 'incomplete');
  assert.equal(attempts[2].unfinished, undefined);
});

test('stopping during a rate-limit backoff ends the run', async () => {
  const sender = fakeSender(['429', 'hang', 'hang']);
  const controller = new AbortController();
  const done = runProbes({ challenges: generateChallenges(6), send: sender.send, signal: controller.signal, rateLimitDelayMs: () => 60_000 });
  await new Promise((resolve) => setTimeout(resolve, 10));
  controller.abort();
  const attempts = await done;
  assert.equal(sender.calls.length, 3);
  assert.deepEqual(
    attempts.slice(0, 3).map((attempt) => attempt.status),
    ['cancelled', 'cancelled', 'cancelled']
  );
  assert.ok(attempts.every((attempt) => attempt.error === undefined), 'the earlier 429 is not shown after stopping');
});

test('reports streaming progress', async () => {
  const sender = fakeSender([]);
  const snapshots = [];
  await runProbes({ challenges: generateChallenges(6), send: sender.send, onChange: (attempts) => snapshots.push(attempts) });
  assert.ok(snapshots.some((attempts) => attempts.some((attempt) => attempt.status === 'running' && attempt.text.length === 20)));
  assert.equal(snapshots.at(-1).filter((attempt) => attempt.status === 'accepted').length, 3);
});

test('challenges use distinct lengths in ModelTrace range without crypto.randomUUID', () => {
  const saved = crypto.randomUUID;
  try {
    // Plain-HTTP pages have no randomUUID.
    Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true });
    const challenges = generateChallenges(6);
    const lengths = challenges.map((challenge) => challenge.expected_count);
    assert.equal(new Set(lengths).size, 6);
    assert.ok(lengths.every((length) => length >= 292 && length <= 332));
    assert.equal(new Set(challenges.map((challenge) => challenge.id)).size, 6);
    for (const challenge of challenges) assert.ok(challenge.prompt.includes(` ${challenge.expected_count} 个 1 到 355（含端点）的整数。`));
  } finally {
    Object.defineProperty(crypto, 'randomUUID', { value: saved, configurable: true });
  }
});
