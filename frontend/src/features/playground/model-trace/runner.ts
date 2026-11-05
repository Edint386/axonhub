import type { Challenge } from './challenges.ts';
import { minimumNumbers, parseNumbers } from './fingerprint-core.ts';
import type { ProbeAnswer } from './transport.ts';

export type ProbeStatus = 'waiting' | 'running' | 'throttled' | 'accepted' | 'rejected' | 'failed' | 'cancelled';

export interface ProbeAttempt {
  index: number;
  challenge: Challenge;
  status: ProbeStatus;
  text: string;
  parsedNumbers: number;
  minimumNumbers: number;
  // How often this challenge was re-sent after a rate limit.
  rateLimitRetries: number;
  // The answer arrived without the stream's finish event but was long enough to count.
  unfinished?: boolean;
  // The stream was cancelled because the answer ran well past the asked-for count (a model that
  // keeps listing numbers is cut off to save tokens).
  stoppedEarly?: boolean;
  error?: string;
  errorCode?: string;
}

export interface RunProbesOptions {
  challenges: Challenge[];
  send: (prompt: string, options: { signal?: AbortSignal; onText: (text: string) => void; shouldStop?: () => boolean }) => Promise<ProbeAnswer>;
  // Answers needed for a full-strength result (ModelTrace calibrates for 1, 2 or 3).
  target?: number;
  concurrency?: number;
  signal?: AbortSignal;
  onChange?: (attempts: ProbeAttempt[]) => void;
  maxRateLimitRetries?: number;
  rateLimitDelayMs?: (retry: number) => number;
}

const RATE_LIMIT_STATUSES = new Set([408, 429]);
export const MAX_RATE_LIMIT_RETRIES = 3;
// An answer without the finish event counts only if it has nearly all the numbers asked for;
// shorter ones are likely cut off. Some relays never send a finish reason at all.
export const UNFINISHED_MIN_SHARE = 0.9;
// A stream is cut once the answer runs this far past the asked-for count. Most models write exactly
// the asked-for count and stop, so cutting at the count itself would cut nearly every normal answer
// at its last number; the margin leaves those alone and still stops a model that keeps going.
// Kept in percent so the threshold is computed exactly (330 * 1.1 is 363.00000000000006 in floats).
export const STOP_OVERSHOOT_PERCENT = 110;

// ModelTrace's own backoff: 1 s per attempt plus up to 0.5 s of jitter.
const defaultRateLimitDelay = (retry: number) => 1000 * retry + Math.random() * 500;

function errorStatus(error: unknown): number | undefined {
  const status = (error as { status?: unknown } | null)?.status;
  return typeof status === 'number' ? status : undefined;
}

function wait(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(done, ms);
    function done() {
      clearTimeout(timer);
      signal?.removeEventListener('abort', done);
      resolve();
    }
    signal?.addEventListener('abort', done, { once: true });
  });
}

// Drops the last number of a stream that was cancelled mid-answer: the cut can land inside a
// number, and a fragment ("14" cut to "1") would corrupt the sequence. One number more or less
// does not matter for the fingerprint, so the trailing one is simply removed.
export function trimTrailingNumber(text: string): string {
  return text.replace(/\d+([^\d]*)$/, '$1');
}

// Runs challenges in parallel until `target` answers pass ModelTrace's length check. A failed or
// too-short answer is replaced by the next unused challenge, so at most challenges.length
// challenges are used. A rate-limited request (429/408) is re-sent with the same challenge after a
// backoff instead, and fewer requests run at once from then on; once one challenge is still rate
// limited after maxRateLimitRetries, nothing more is sent. Resolves once nothing is in flight or
// waiting; never rejects.
export function runProbes({
  challenges,
  send,
  target = 3,
  concurrency = 3,
  signal,
  onChange,
  maxRateLimitRetries = MAX_RATE_LIMIT_RETRIES,
  rateLimitDelayMs = defaultRateLimitDelay,
}: RunProbesOptions): Promise<ProbeAttempt[]> {
  const attempts: ProbeAttempt[] = challenges.map((challenge, index) => ({
    index,
    challenge,
    status: 'waiting',
    text: '',
    parsedNumbers: 0,
    minimumNumbers: minimumNumbers(challenge.expected_count),
    rateLimitRetries: 0,
  }));
  const queue = attempts.slice();
  const emit = () => onChange?.(attempts.map((attempt) => ({ ...attempt })));
  let limit = concurrency;
  let running = 0;
  let backingOff = 0;
  let accepted = 0;
  let gaveUp = false;

  return new Promise((resolve) => {
    const launch = () => {
      while (!signal?.aborted && !gaveUp && running < limit && accepted + running + backingOff < target && queue.length) {
        start(queue.shift()!);
      }
      if (running !== 0 || backingOff !== 0) return;
      for (const attempt of queue) {
        if (attempt.status !== 'throttled') continue;
        if (signal?.aborted) cancel(attempt);
        else attempt.status = 'failed';
      }
      resolve(attempts.map((attempt) => ({ ...attempt })));
    };

    // A stopped request is not an error, whatever it failed with before.
    const cancel = (attempt: ProbeAttempt) => {
      attempt.status = 'cancelled';
      attempt.error = undefined;
      attempt.errorCode = undefined;
    };

    const retryLater = (attempt: ProbeAttempt) => {
      attempt.rateLimitRetries += 1;
      attempt.status = 'throttled';
      // Run no more requests at once than the channel is still serving.
      limit = Math.max(1, Math.min(limit, running));
      backingOff += 1;
      wait(rateLimitDelayMs(attempt.rateLimitRetries), signal).then(() => {
        backingOff -= 1;
        if (signal?.aborted) cancel(attempt);
        else if (gaveUp) attempt.status = 'failed';
        else queue.unshift(attempt);
        emit();
        launch();
      });
    };

    const start = (attempt: ProbeAttempt) => {
      running += 1;
      attempt.status = 'running';
      attempt.text = '';
      attempt.parsedNumbers = 0;
      attempt.error = undefined;
      attempt.errorCode = undefined;
      attempt.unfinished = undefined;
      attempt.stoppedEarly = undefined;
      emit();
      const onText = (text: string) => {
        attempt.text = text;
        attempt.parsedNumbers = parseNumbers(text).length;
        emit();
      };
      // Once the answer runs 10% past the asked-for count the stream is cut: some models keep
      // listing numbers far past the challenge's count, burning tokens for nothing.
      const stopAt = Math.ceil((attempt.challenge.expected_count * STOP_OVERSHOOT_PERCENT) / 100);
      let stoppedEarly = false;
      const shouldStop = () => {
        if (attempt.parsedNumbers < stopAt) return false;
        stoppedEarly = true;
        return true;
      };
      let rateLimited = false;
      Promise.resolve()
        .then(() => send(attempt.challenge.prompt, { signal, onText, shouldStop }))
        .then(({ text, finished }) => {
          const finalText = stoppedEarly ? trimTrailingNumber(text) : text;
          attempt.text = finalText;
          attempt.parsedNumbers = parseNumbers(finalText).length;
          if (!finished && attempt.parsedNumbers < Math.ceil(attempt.challenge.expected_count * UNFINISHED_MIN_SHARE)) {
            attempt.status = 'failed';
            attempt.errorCode = 'incomplete';
            attempt.error = 'The answer did not finish normally';
          } else if (attempt.parsedNumbers >= attempt.minimumNumbers) {
            attempt.status = 'accepted';
            attempt.unfinished = (!finished && !stoppedEarly) || undefined;
            attempt.stoppedEarly = stoppedEarly || undefined;
            accepted += 1;
          } else {
            attempt.status = 'rejected';
          }
        })
        .catch((error: unknown) => {
          if (signal?.aborted) {
            cancel(attempt);
            return;
          }
          const status = errorStatus(error);
          const limited = status !== undefined && RATE_LIMIT_STATUSES.has(status);
          rateLimited = limited && attempt.rateLimitRetries < maxRateLimitRetries;
          if (limited && !rateLimited) gaveUp = true;
          attempt.status = 'failed';
          attempt.error = error instanceof Error ? error.message : String(error);
          const code = (error as { code?: unknown } | null)?.code;
          if (typeof code === 'string') attempt.errorCode = code;
        })
        .finally(() => {
          running -= 1;
          if (rateLimited) retryLater(attempt);
          emit();
          launch();
        });
    };

    launch();
  });
}
