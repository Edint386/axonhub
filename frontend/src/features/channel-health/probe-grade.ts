import type { ActiveChannelHealthProbeRun, ChannelHealthProbeChannel } from './data/channel-health';

/**
 * Channel-level grades over the gate window.
 *
 * Failures are graded first, then speed. Speed reads the SAME number the routing
 * ceiling reads -- the mean over the gate window, with the ceiling's own sample floor
 * already applied -- and compares it against fixed bands rather than against any
 * configured threshold:
 *   error    窗口内探测全部失败（渠道不可用）
 *   abnormal 成功率 < 80%（频繁失败）
 *   degraded 有失败但成功率 ≥ 80%，或全部成功而 gate 均值 > 30s
 *   fluent   全部成功且 gate 均值 ≤ 30s（可用）
 *   health   全部成功且 gate 均值 ≤ 10s（快）
 *   unknown  全部成功但窗口样本不足，上限也读不到值（未判定）
 * plus auxiliary states: pending / skipped / never / disabled / unconfigured.
 *
 * The bands are deliberately NOT the routing threshold: that threshold is per API key,
 * so grading by it would make one channel render differently for each key, and a
 * channel no key restricts would have nothing to grade against at all.
 */
export type ChannelGrade =
  | 'health'
  | 'fluent'
  | 'degraded'
  | 'abnormal'
  | 'error'
  | 'unknown'
  | 'pending'
  | 'skipped'
  | 'never'
  | 'unconfigured'
  | 'disabled';

export type ProbeBarStatus = 'health' | 'fluent' | 'degraded' | 'error' | 'unknown' | 'skipped' | 'pending';

export const CHANNEL_GRADE_ORDER: Record<ChannelGrade, number> = {
  error: 0,
  abnormal: 1,
  degraded: 2,
  unknown: 3,
  pending: 4,
  never: 5,
  unconfigured: 6,
  skipped: 7,
  fluent: 8,
  health: 9,
  disabled: 10,
};

/** Grades that count as problem channels for the alert and status filter. */
export const PROBLEM_GRADES: ChannelGrade[] = ['abnormal', 'error'];

/** Auxiliary states grouped under the "other" KPI card / filter option. */
export const OTHER_GRADES: ChannelGrade[] = ['unknown', 'pending', 'skipped', 'never', 'unconfigured', 'disabled'];

/** First-token latency of a run: TTFT for streaming, TTFB otherwise (mirrors backend rule). */
export function firstTokenMsOf(run: ActiveChannelHealthProbeRun | null | undefined): number | null {
  if (!run) {
    return null;
  }
  const value = run.stream ? (run.ttftMs ?? run.ttfbMs) : (run.ttfbMs ?? run.ttftMs);
  return value == null || !Number.isFinite(value) ? null : value;
}

/**
 * Built-in latency bands for health grading, in milliseconds.
 *
 * These are deliberately constants and NOT derived from the policy's latency
 * setting. That setting is a routing ceiling the operator chooses for business
 * reasons -- with it at 600s every channel that answered at all graded as healthy,
 * so the grade carried no information. "Is this channel in good shape" is a
 * question the system answers on its own; "may this key use it" is the operator's
 * to answer. Keeping them as one number conflated the two.
 *
 * One band set covers both streaming and non-streaming: the metric differs (TTFT
 * vs TTFB) but these bounds are loose enough to be meaningful for either.
 */
export const HEALTH_LATENCY_MAX_MS = 10_000;
export const FLUENT_LATENCY_MAX_MS = 30_000;

/** Grade a single probe run by its latency against the built-in bands. */
export function gradeOfRun(run: ActiveChannelHealthProbeRun): ProbeBarStatus {
  if (run.status === 'skipped') {
    return 'skipped';
  }
  if (run.status === 'pending') {
    return 'pending';
  }
  if (run.status !== 'healthy') {
    return 'error';
  }
  const first = firstTokenMsOf(run);
  if (first == null) {
    return 'unknown';
  }
  if (first <= HEALTH_LATENCY_MAX_MS) {
    return 'health';
  }
  if (first <= FLUENT_LATENCY_MAX_MS) {
    return 'fluent';
  }
  return 'degraded';
}

export function primaryModelOf(channel: ChannelHealthProbeChannel): ChannelHealthProbeChannel['models'][number] | null {
  if (channel.primaryModelID) {
    const configuredPrimary = channel.models.find((model) => model.modelID === channel.primaryModelID && model.enabled);
    if (configuredPrimary) {
      return configuredPrimary;
    }
  }
  return channel.models.find((model) => model.enabled) ?? null;
}

/**
 * Runs for the channel's primary model, optionally bounded to a recency window.
 *
 * The window is not cosmetic. `recentRuns` is "the newest 15 rows" with no time
 * bound at all, and a channel the priority chain mostly skips accumulates few real
 * samples among those rows -- so an unbounded figure can be computed from probes
 * hours old and still be read as the channel's CURRENT speed, with nothing on screen
 * saying so. Bounding by the routing gate's own window makes the displayed number
 * answer the same question the gate answers, and an empty window renders "-" rather
 * than a stale value.
 *
 * Omit windowMs for the recent-probe strip, which is explicitly a history view. The
 * grade is NOT a history view and always takes the window.
 */
function primaryRunsOf(channel: ChannelHealthProbeChannel, windowMs?: number): ActiveChannelHealthProbeRun[] {
  const model = primaryModelOf(channel);
  if (!model) {
    return [];
  }

  const runs = (channel.recentRuns ?? []).filter((run) => run.modelID === model.modelID);
  if (windowMs == null || !Number.isFinite(windowMs) || windowMs <= 0) {
    return runs;
  }

  const cutoff = Date.now() - windowMs;

  return runs.filter((run) => {
    const startedAt = Date.parse(run.startedAt);
    // An unparseable timestamp must not silently drop a sample; keep it and let the
    // value show rather than claiming there is no measurement.
    return !Number.isFinite(startedAt) || startedAt >= cutoff;
  });
}

/**
 * The number the routing ceiling judges this channel's primary model by.
 *
 * This is what the page displays, rather than a figure computed here. A locally
 * recomputed mean would only be *close* to the ceiling's; this is the same value,
 * because the backend already applied the ceiling's window, scope and sample floor.
 * Null means the ceiling reads UNKNOWN and would keep this channel whatever the limit
 * is -- so it must render as "no measurement", never as fast.
 */
export function channelGateAvgMs(channel: ChannelHealthProbeChannel): number | null {
  return primaryModelOf(channel)?.gateAvgMs ?? null;
}

/** How many samples the gate window held, even when it was too few to act on. */
export function channelGateSampleCount(channel: ChannelHealthProbeChannel): number {
  return primaryModelOf(channel)?.gateSampleCount ?? 0;
}

/**
 * First-token latency of the latest actual probe inside the window. Returns null when
 * that probe has no first-token metric, or when the window holds no probe at all, so
 * the UI renders "-" instead of passing an older run's latency off as the latest one.
 */
export function channelLatestFirst(channel: ChannelHealthProbeChannel, windowMs?: number): number | null {
  const probes = primaryRunsOf(channel, windowMs).filter((run) => run.status === 'healthy' || run.status === 'unhealthy');
  const latest = probes[probes.length - 1];

  return latest?.status === 'healthy' ? firstTokenMsOf(latest) : null;
}

/** P95 for the channel's primary model. */
export function channelP95(channel: ChannelHealthProbeChannel): number | null {
  return primaryModelOf(channel)?.p95Ms ?? null;
}

/**
 * Aggregate the channel-level grade, bounded to the routing gate's own window.
 *
 * Both halves of this grade used the wrong time range before. The success rate read
 * "the newest 15 rows", which is a COUNT and not a window: a channel the priority
 * chain mostly skips holds few real samples among those rows, so the verdict could be
 * built from probes hours old with nothing on screen saying so. The latency tier read
 * the dashboard's 24-hour P95, so a channel that slowed down last night and has since
 * recovered kept reading "degraded" for up to a day after the number beside it had
 * already recovered.
 *
 * Now both read the same window as the big figure and as the ceiling, so a row
 * recovers as one thing instead of the number recovering in minutes and the chip an
 * hour later.
 *
 * Insufficient data is UNKNOWN, never healthy: an empty window means nothing was
 * measured, which is exactly what a stopped process looks like.
 *
 * The sample floor applies to LATENCY only. A failed probe is direct evidence of a
 * problem and is graded however few there are -- withholding "error" because only two
 * probes ran would hide a real outage behind "unknown".
 */
export function gradeOfChannel(channel: ChannelHealthProbeChannel, windowMs?: number): ChannelGrade {
  if (!channel.enabled) {
    return 'disabled';
  }
  if (primaryModelOf(channel) == null) {
    return 'unconfigured';
  }
  // A channel opted out of scheduled probing has no rows written for it at all -- not
  // even skipped ones -- so once its old rows age out its window is empty forever.
  // Reporting that as "no metric" would pool a deliberate opt-out with channels whose
  // scheduler has actually stopped, which is the one thing this window exists to make
  // visible. The page's own filter already honours probeEnabled; the grade is its twin.
  if (!channel.probeEnabled) {
    return 'skipped';
  }
  const runs = primaryRunsOf(channel, windowMs);
  if (runs.length === 0) {
    // Decide "never probed" from the unbounded list, not from whether a window was
    // passed: every production caller passes one, so keying off the parameter made
    // 'never' unreachable and reported a never-probed channel as "no metric".
    return primaryRunsOf(channel).length === 0 ? 'never' : 'unknown';
  }
  if (runs[runs.length - 1].status === 'pending') {
    return 'pending';
  }
  const probes = runs.filter((run) => run.status === 'healthy' || run.status === 'unhealthy');
  if (probes.length === 0) {
    return 'skipped';
  }
  const fails = probes.filter((run) => run.status !== 'healthy').length;
  const rate = (probes.length - fails) / probes.length;
  if (rate === 0) {
    return 'error';
  }
  if (rate < 0.8) {
    return 'abnormal';
  }
  if (fails > 0) {
    return 'degraded';
  }
  // Every probe in the window succeeded, so the grade now turns on speed -- and the
  // number it turns on must be the one the ceiling uses, not a second opinion.
  const gateAvgMs = channelGateAvgMs(channel);
  if (gateAvgMs == null) {
    return 'unknown';
  }
  if (gateAvgMs > FLUENT_LATENCY_MAX_MS) {
    return 'degraded';
  }
  if (gateAvgMs > HEALTH_LATENCY_MAX_MS) {
    return 'fluent';
  }

  return 'health';
}

/** Multiplier of 1 means "not configured" and renders as "-". */
export function formatMultiplier(value: number): string {
  return value === 1 ? '-' : String(value);
}
