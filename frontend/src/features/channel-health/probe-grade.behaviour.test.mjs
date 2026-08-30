import assert from 'node:assert/strict';
import test from 'node:test';

// The sibling suite asserts on the SOURCE TEXT of this module, which cannot catch a
// broken boundary: flipping `<=` to `<` leaves every one of those regexes matching.
// Node runs TypeScript directly, so this suite calls the real functions instead.
const {
  CHANNEL_GRADE_ORDER,
  FLUENT_LATENCY_MAX_MS,
  HEALTH_LATENCY_MAX_MS,
  channelGateAvgMs,
  channelLatestFirst,
  firstTokenMsOf,
  gradeOfChannel,
  gradeOfRun,
  primaryModelOf,
} = await import('./probe-grade.ts');

const MINUTE = 60_000;

/** A probe run inside the gate window unless `agoMs` says otherwise. */
function run(overrides = {}) {
  return {
    id: 'run-1',
    modelID: 'gpt-4',
    status: 'healthy',
    stream: true,
    ttftMs: 500,
    ttfbMs: null,
    startedAt: new Date(Date.now() - (overrides.agoMs ?? MINUTE)).toISOString(),
    ...overrides,
  };
}

function channel(overrides = {}) {
  const { model = {}, runs = [run()], ...rest } = overrides;
  return {
    channelID: 1,
    channelName: 'c1',
    enabled: true,
    probeEnabled: true,
    primaryModelID: 'gpt-4',
    models: [{ modelID: 'gpt-4', enabled: true, gateAvgMs: 1_000, gateSampleCount: 6, latestRun: runs[runs.length - 1] ?? null, ...model }],
    recentRuns: runs,
    ...rest,
  };
}

test('gradeOfRun band boundaries are inclusive at the top of each band', () => {
  assert.equal(gradeOfRun(run({ ttftMs: HEALTH_LATENCY_MAX_MS })), 'health');
  assert.equal(gradeOfRun(run({ ttftMs: HEALTH_LATENCY_MAX_MS + 1 })), 'fluent');
  assert.equal(gradeOfRun(run({ ttftMs: FLUENT_LATENCY_MAX_MS })), 'fluent');
  assert.equal(gradeOfRun(run({ ttftMs: FLUENT_LATENCY_MAX_MS + 1 })), 'degraded');
});

test('gradeOfRun reports run state before it reports latency', () => {
  assert.equal(gradeOfRun(run({ status: 'skipped' })), 'skipped');
  assert.equal(gradeOfRun(run({ status: 'pending' })), 'pending');
  assert.equal(gradeOfRun(run({ status: 'unhealthy', ttftMs: 10 })), 'error');
});

test('an unmeasured run is unknown rather than fast', () => {
  assert.equal(gradeOfRun(run({ ttftMs: null, ttfbMs: null })), 'unknown');
  assert.equal(gradeOfRun(run({ ttftMs: Number.NaN, ttfbMs: null })), 'unknown');
  assert.equal(gradeOfRun(run({ ttftMs: 0, ttfbMs: null })), 'health');
});

test('firstTokenMsOf picks TTFT for streaming and TTFB otherwise', () => {
  assert.equal(firstTokenMsOf(run({ stream: true, ttftMs: 111, ttfbMs: 222 })), 111);
  assert.equal(firstTokenMsOf(run({ stream: false, ttftMs: 111, ttfbMs: 222 })), 222);
  assert.equal(firstTokenMsOf(run({ stream: true, ttftMs: null, ttfbMs: 222 })), 222);
  assert.equal(firstTokenMsOf(null), null);
  assert.equal(firstTokenMsOf(undefined), null);
});

test('gradeOfChannel grades speed on the gate mean, at the band edges', () => {
  assert.equal(gradeOfChannel(channel({ model: { gateAvgMs: HEALTH_LATENCY_MAX_MS } }), 30 * MINUTE), 'health');
  assert.equal(gradeOfChannel(channel({ model: { gateAvgMs: HEALTH_LATENCY_MAX_MS + 1 } }), 30 * MINUTE), 'fluent');
  assert.equal(gradeOfChannel(channel({ model: { gateAvgMs: FLUENT_LATENCY_MAX_MS } }), 30 * MINUTE), 'fluent');
  assert.equal(gradeOfChannel(channel({ model: { gateAvgMs: FLUENT_LATENCY_MAX_MS + 1 } }), 30 * MINUTE), 'degraded');
});

test('a gate mean the ceiling cannot act on grades unknown, never fast', () => {
  // The backend withholds gateAvgMs below its sample floor, so "not enough samples"
  // must not be rendered as a fast channel.
  assert.equal(gradeOfChannel(channel({ model: { gateAvgMs: null, gateSampleCount: 2 } }), 30 * MINUTE), 'unknown');
  assert.equal(channelGateAvgMs(channel({ model: { gateAvgMs: null } })), null);
});

test('failures outrank speed, and are graded however few samples there are', () => {
  const failing = [run({ id: 'a', status: 'unhealthy' }), run({ id: 'b', status: 'unhealthy' })];
  assert.equal(gradeOfChannel(channel({ runs: failing }), 30 * MINUTE), 'error');

  // 1 of 3 failed -> 67% success -> abnormal.
  const mostlyFailing = [run({ id: 'a' }), run({ id: 'b', status: 'unhealthy' }), run({ id: 'c', status: 'unhealthy' })];
  assert.equal(gradeOfChannel(channel({ runs: mostlyFailing }), 30 * MINUTE), 'abnormal');

  // 1 of 5 failed -> 80% success -> degraded, even though the gate mean is fast.
  const oneFail = [run({ id: 'a' }), run({ id: 'b' }), run({ id: 'c' }), run({ id: 'd' }), run({ id: 'e', status: 'unhealthy' })];
  assert.equal(gradeOfChannel(channel({ runs: oneFail, model: { gateAvgMs: 100 } }), 30 * MINUTE), 'degraded');
});

test('gradeOfChannel reports configuration state before any measurement', () => {
  assert.equal(gradeOfChannel(channel({ enabled: false }), 30 * MINUTE), 'disabled');
  assert.equal(gradeOfChannel(channel({ models: [], primaryModelID: null }), 30 * MINUTE), 'unconfigured');
  assert.equal(gradeOfChannel(channel({ probeEnabled: false }), 30 * MINUTE), 'skipped');
});

test('an empty window is unknown, but a never-probed channel is never', () => {
  assert.equal(gradeOfChannel(channel({ runs: [] }), 30 * MINUTE), 'never');
  // Probed, but every run is older than the window.
  assert.equal(gradeOfChannel(channel({ runs: [run({ agoMs: 90 * MINUTE })] }), 30 * MINUTE), 'unknown');
});

test('a pending probe is reported as pending', () => {
  assert.equal(gradeOfChannel(channel({ runs: [run({ id: 'a' }), run({ id: 'b', status: 'pending' })] }), 30 * MINUTE), 'pending');
});

test('an unparseable timestamp keeps the sample instead of dropping it', () => {
  const bad = run({ startedAt: 'not-a-date' });
  assert.equal(gradeOfChannel(channel({ runs: [bad] }), 30 * MINUTE), 'health');
});

test('channelLatestFirst returns null when the window holds no healthy probe', () => {
  assert.equal(channelLatestFirst(channel({ runs: [run({ agoMs: 90 * MINUTE })] }), 30 * MINUTE), null);
  assert.equal(channelLatestFirst(channel({ runs: [run({ status: 'unhealthy' })] }), 30 * MINUTE), null);
  assert.equal(channelLatestFirst(channel({ runs: [run({ ttftMs: 777 })] }), 30 * MINUTE), 777);
});

test('primaryModelOf falls back to the first enabled model', () => {
  assert.equal(primaryModelOf(channel({ primaryModelID: 'missing' }))?.modelID, 'gpt-4');
  assert.equal(primaryModelOf(channel({ models: [{ modelID: 'gpt-4', enabled: false }] })), null);
});

test('problem grades sort ahead of healthy ones', () => {
  assert.ok(CHANNEL_GRADE_ORDER.error < CHANNEL_GRADE_ORDER.abnormal);
  assert.ok(CHANNEL_GRADE_ORDER.abnormal < CHANNEL_GRADE_ORDER.degraded);
  assert.ok(CHANNEL_GRADE_ORDER.degraded < CHANNEL_GRADE_ORDER.unknown);
  assert.ok(CHANNEL_GRADE_ORDER.unknown < CHANNEL_GRADE_ORDER.health);
});
