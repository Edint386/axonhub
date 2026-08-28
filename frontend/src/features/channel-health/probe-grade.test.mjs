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
const settings = read('components/probe-settings-dialog.tsx');
const matrix = read('components/channel-matrix-table.tsx');
const detail = read('components/channel-detail-sheet.tsx');
const data = read('data/channel-health.ts');
const kpi = read('components/kpi-cards.tsx');

test('primary model metrics use the ordered enabled model', () => {
  assert.match(grade, /channel\.primaryModelID/);
  assert.match(grade, /configuredPrimary[\s\S]*return configuredPrimary/);
  assert.match(grade, /return channel\.models\.find\(\(model\) => model\.enabled\)/);
  assert.match(grade, /primaryModelOf\(channel\)\?\.p95Ms/);
});

test('an unmeasured window stays unknown and failures stay degraded or worse', () => {
  // No usable gate figure means the ceiling itself reads unknown, so the chip must too
  // -- an empty window is what a stopped process looks like, not a healthy channel.
  assert.match(grade, /if \(gateAvgMs == null\) \{[\s\S]*return 'unknown'/);
  assert.match(grade, /if \(probes\.length === 0\) \{[\s\S]*return 'skipped'/);
  assert.match(grade, /if \(fails > 0\) \{[\s\S]*return 'degraded'/);
  assert.match(grade, /if \(rate === 0\) \{[\s\S]*return 'error'/);

  // The sample floor is for LATENCY only, and that is a claim about ORDER, not about
  // the branches existing: consulting the gate figure before the failure checks would
  // hide a real outage behind "not enough samples" while every regex above still
  // matched. Assert on POSITION inside the grade function's own body, and against any
  // spelling of the read -- pinning one variable name is dodged by renaming it.
  const gradeBody = grade.slice(grade.indexOf('export function gradeOfChannel'));
  const at = (needle) => {
    const index = gradeBody.indexOf(needle);
    assert.notStrictEqual(index, -1, `guard needle vanished from gradeOfChannel: ${needle}`);

    return index;
  };
  const firstGateRead = at('channelGateAvgMs(');
  assert.ok(at("return 'error'") < firstGateRead, 'total failure must be graded before the gate figure is read');
  assert.ok(at("return 'abnormal'") < firstGateRead, 'a low success rate must be graded before the gate figure is read');
  assert.ok(at("return 'degraded'") < firstGateRead, 'any failure must be graded before the gate figure is read');

  // A channel opted out of probing writes no rows at all, so it must not be pooled
  // with channels whose scheduler stopped.
  assert.ok(at('if (!channel.probeEnabled)') < at('primaryRunsOf(channel, windowMs)'));
  // "Never probed" is decided from the unbounded list; keying it off the parameter made
  // the grade unreachable because every production caller passes a window.
  assert.match(gradeBody, /primaryRunsOf\(channel\)\.length === 0 \? 'never' : 'unknown'/);
});

test('problem status is implemented in the status dropdown filter', () => {
  assert.match(page, /statusFilter === 'problem'[\s\S]*PROBLEM_GRADES\.includes\(grade\)/);
  assert.doesNotMatch(page, /filters\.badOnly/);
});

test('the settings surface is a centered dialog, not a side sheet', () => {
  // Across this app a Sheet is a read-only viewer (request body, trace, test
  // history) and every settings surface that WRITES config is a Dialog. This one
  // was the sole exception; the guard keeps it from drifting back.
  assert.match(settings, /from '@\/components\/ui\/dialog'/);
  assert.doesNotMatch(settings, /components\/ui\/sheet/);
  assert.doesNotMatch(settings, /<Sheet/);
  assert.match(settings, /<DialogContent[\s\S]*sm:max-w-\[720px\]/);
  assert.match(settings, /<DialogFooter/);
});

test('alert banners align their icon with a single-line row', () => {
  // The Alert primitive pins its icon to the top of the grid, which reads as
  // misaligned when the row height comes from a button beside one line of text.
  // Split rather than match a tag: the class itself contains '>' ([&>svg]), so a
  // [^>]*> tag regex truncates before reaching it.
  const banners = page.split("<Alert variant='destructive'").slice(1);
  assert.ok(banners.length > 0, 'expected at least one destructive alert');
  for (const banner of banners) {
    const openingTag = banner.slice(0, 80);
    assert.match(openingTag, /items-center/);
    assert.match(openingTag, /\[&>svg\]:translate-y-0/);
  }
});

test('the routing gate window is a setting of its own, not the dashboard P95 window', () => {
  // Sharing one window was the original defect: it made a gate meant to answer "is
  // this channel fast right now" average over a whole day. Both fields must exist
  // independently, and the dialog must send the gate window on save.
  assert.match(data, /gateWindowMinutes: z\.number\(\)/);
  assert.match(data, /p95LookbackHours: z\.number\(\)/);
  assert.match(settings, /gateWindowMinutes: parsedGateWindowMinutes/);
  assert.match(settings, /id='probe-gate-window'/);
  // The window is only useful once it can hold the ceiling's 3-sample minimum, so the
  // dialog warns on interval x 3 rather than silently letting the gate go inert.
  assert.match(settings, /parsedIntervalMinutes \* 3/);
});

test('the displayed first-token figure IS the gate figure, not a second opinion', () => {
  // A locally recomputed mean would only be *close* to the ceiling's. The page reads
  // gateAvgMs, which the backend produced with the ceiling's own window, scope and
  // sample floor, so there is one number rather than two that happen to agree.
  assert.match(grade, /export function channelGateAvgMs\(channel: ChannelHealthProbeChannel\)/);
  assert.match(grade, /primaryModelOf\(channel\)\?\.gateAvgMs/);
  // The median is gone: it disagreed with the mean the ceiling compares against.
  assert.doesNotMatch(grade, /channelP50/);
  assert.doesNotMatch(grade, /export function median/);

  // The "latest" sub-line is still a probe-record figure, so it still needs bounding.
  assert.match(grade, /function primaryRunsOf\(channel: ChannelHealthProbeChannel, windowMs\?: number\)/);
  assert.match(grade, /const cutoff = Date\.now\(\) - windowMs/);
  assert.match(grade, /export function channelLatestFirst\(channel: ChannelHealthProbeChannel, windowMs\?: number\)/);

  // The table must display the gate figure AND sort by it, or the column would order
  // itself by a number that is nowhere on screen.
  assert.match(matrix, /const gateWindowMs = gateWindowMinutes \* 60_000/);
  assert.match(matrix, /const gateAvgMs = channelGateAvgMs\(channel\)/);
  assert.match(matrix, /channelLatestFirst\(channel, gateWindowMs\)/);
  assert.match(matrix, /channelGateAvgMs\(a\) \?\? Number\.MAX_VALUE/);
  assert.match(matrix, /firstTokenWindowed/);
  assert.match(matrix, /\{gateAvgMs != null \? formatDuration\(gateAvgMs\) : '-'\}/);

  // The field has to be REQUESTED, not merely declared in the zod schema: a selection
  // set missing it yields undefined at runtime with no error. Count occurrences so
  // deleting it from the query documents cannot be masked by the schema line.
  for (const field of ['gateAvgMs', 'gateSampleCount']) {
    assert.ok(
      (data.match(new RegExp(field, 'g')) ?? []).length >= 3,
      `${field} must appear in the zod schema AND both selection sets`
    );
  }

  // And the page's window must come FROM the policy. A hard-coded 30 here with a
  // policy-driven table splits the page in two the moment the operator changes it.
  assert.match(page, /const gateWindowMinutes = policy\?\.gateWindowMinutes \?\? 30/);
});

test('the status chip reads the same window as the figure beside it', () => {
  // Both halves of the grade used the wrong range before: the success rate read "the
  // newest 15 rows" (a count, not a window) and the latency tier read the dashboard's
  // 24-hour P95. That is why a recovering channel's number came back in minutes while
  // its chip stayed degraded for an hour or more.
  assert.match(grade, /export function gradeOfChannel\(channel: ChannelHealthProbeChannel, windowMs\?: number\)/);
  assert.match(grade, /const runs = primaryRunsOf\(channel, windowMs\)/);
  assert.match(grade, /const gateAvgMs = channelGateAvgMs\(channel\)/);
  // Insufficient data is unknown, never healthy.
  assert.match(grade, /if \(gateAvgMs == null\) \{\s*return 'unknown'/);
  // The latency tier must no longer consult the 24-hour percentile. Assert on the
  // grade function's own body, so a revert spelled any other way still fails.
  const gradeBody = grade.slice(grade.indexOf('export function gradeOfChannel'));
  assert.doesNotMatch(gradeBody, /channelP95/);

  // EVERY consumer passes the window, or the chips, the KPI counts and the filters each
  // describe a different period. Asserting the absence of a one-argument call covers
  // all call sites at once -- a per-file `match` would pass while one of the page's two
  // or the matrix's three was reverted.
  for (const [name, src] of [
    ['index.tsx', page],
    ['channel-matrix-table.tsx', matrix],
    ['kpi-cards.tsx', kpi],
  ]) {
    assert.doesNotMatch(src, /gradeOfChannel\([^,)]+\)/, `${name} has an unwindowed gradeOfChannel call`);
  }

  // The model chip beside the channel chip must respect the window too, or a row shows
  // a green model against a channel reading "no metric" and a first-token cell of "-".
  assert.match(matrix, /latestRunInWindow/);
  assert.match(matrix, /latestRunStartedAt >= Date\.now\(\) - gateWindowMs/);

  // One unit at every boundary: minutes in, milliseconds derived locally. Two siblings
  // taking one concept in two units are both `number`, so a mis-wire is silent.
  assert.match(kpi, /gateWindowMinutes: number/);
  assert.match(kpi, /const gateWindowMs = gateWindowMinutes \* 60_000/);
  assert.doesNotMatch(page, /gateWindowMs=\{/);
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
  assert.match(matrix, /gradeOfRun\(primaryModel\.latestRun\)/);
  assert.match(matrix, /gradeOfRun\(model\.latestRun\)/);
  assert.doesNotMatch(matrix, /latestRun\?\.status/);
  // The detail sheet must grade its model-compare rows the same way — the
  // omission of this check is exactly why the raw-status regression slipped in.
  assert.match(detail, /gradeOfRun\(model\.latestRun\)/);
  assert.doesNotMatch(detail, /latestRun\?\.status/);
});

test('latest first-token latency never falls back to an older run', () => {
  assert.match(grade, /const latest = probes\[probes\.length - 1\]/);
  assert.match(grade, /latest\?\.status === 'healthy' \? firstTokenMsOf\(latest\) : null/);
  assert.doesNotMatch(grade, /for \(let i = runs\.length - 1/);
});

test('health grading uses built-in bands, not the operator-set routing ceiling', () => {
  // With the ceiling at 600s every answering channel graded healthy, so the grade
  // carried no information. Grading must not read that setting at all.
  assert.match(grade, /HEALTH_LATENCY_MAX_MS = 10_000/);
  assert.match(grade, /FLUENT_LATENCY_MAX_MS = 30_000/);
  assert.doesNotMatch(grade, /thresholdMs/);
  assert.match(grade, /export function gradeOfRun\(run: ActiveChannelHealthProbeRun\)/);
  assert.match(grade, /export function gradeOfChannel\(channel: ChannelHealthProbeChannel, windowMs\?: number\)/);

  // The first-token column colours off the same bands so the number agrees with
  // the chip beside it.
  assert.match(matrix, /gateAvgMs > FLUENT_LATENCY_MAX_MS/);
  assert.match(matrix, /gateAvgMs > HEALTH_LATENCY_MAX_MS/);
  assert.doesNotMatch(matrix, /thresholdMs/);

  // The ceiling survives only as the detail chart's reference line, renamed so it
  // stops reading as a health criterion. It is the EFFECTIVE routing ceiling: the
  // stricter of the global fallback and the tightest enabled API key ceiling, so
  // the line lands inside the plotted range instead of wrecking the y-axis scale.
  assert.match(page, /routingCeilingMs = useMemo/);
  assert.match(page, /fallbackMs = policy\?\.acceptableLatencyMs \?\? 60_000/);
  assert.match(page, /apiKeyCeilingMs = policy\?\.apiKeyMaxFirstTokenLatencyMs/);
  assert.match(page, /return Math\.min\(fallbackMs, apiKeyCeilingMs\)/);
});
