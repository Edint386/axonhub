import type { ActiveChannelHealthProbeRun, ChannelHealthProbeChannel } from './data/channel-health';

/**
 * Channel-level grades aggregated from the recent probe strip:
 *   health   全部成功且 P95 ≤ 阈值的 50%（快且稳）
 *   fluent   全部成功且 P95 ≤ 阈值（达标顺滑）
 *   degraded 成功率 ≥ 80%，但 P95 超阈值或最近一次失败（能用但不稳定/变慢）
 *   abnormal 成功率 < 80%（频繁失败）
 *   error    近期探测全部失败（渠道不可用）
 * plus auxiliary states: pending / skipped / never.
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

export function median(values: number[]): number | null {
  if (values.length === 0) {
    return null;
  }
  const sorted = [...values].sort((a, b) => a - b);
  const mid = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[mid] : (sorted[mid - 1] + sorted[mid]) / 2;
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

function primaryRunsOf(channel: ChannelHealthProbeChannel): ActiveChannelHealthProbeRun[] {
  const model = primaryModelOf(channel);
  if (!model) {
    return [];
  }
  return (channel.recentRuns ?? []).filter((run) => run.modelID === model.modelID);
}

/** P50 of successful recent probes (skipped runs excluded), for display/sorting only. */
export function channelP50(channel: ChannelHealthProbeChannel): number | null {
  const values = primaryRunsOf(channel)
    .filter((run) => run.status === 'healthy')
    .map(firstTokenMsOf)
    .filter((value): value is number => value != null);
  return median(values);
}

/**
 * First-token latency of the latest actual probe. Returns null when that probe
 * has no first-token metric, so the UI renders "-" instead of passing an older
 * run's latency off as the latest one.
 */
export function channelLatestFirst(channel: ChannelHealthProbeChannel): number | null {
  const probes = primaryRunsOf(channel).filter((run) => run.status === 'healthy' || run.status === 'unhealthy');
  const latest = probes[probes.length - 1];
  return latest?.status === 'healthy' ? firstTokenMsOf(latest) : null;
}

/** P95 for the channel's primary model. */
export function channelP95(channel: ChannelHealthProbeChannel): number | null {
  return primaryModelOf(channel)?.p95Ms ?? null;
}

/** Aggregate the channel-level grade from the recent probe strip. */
export function gradeOfChannel(channel: ChannelHealthProbeChannel): ChannelGrade {
  if (!channel.enabled) {
    return 'disabled';
  }
  if (primaryModelOf(channel) == null) {
    return 'unconfigured';
  }
  const runs = primaryRunsOf(channel);
  if (runs.length === 0) {
    return 'never';
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
  const latestProbe = probes[probes.length - 1];
  if (firstTokenMsOf(latestProbe) == null) {
    return 'unknown';
  }
  const p95 = channelP95(channel);
  if (p95 != null && p95 > FLUENT_LATENCY_MAX_MS) {
    return 'degraded';
  }
  if (p95 != null && p95 > HEALTH_LATENCY_MAX_MS) {
    return 'fluent';
  }
  return 'health';
}

/** Multiplier of 1 means "not configured" and renders as "-". */
export function formatMultiplier(value: number): string {
  return value === 1 ? '-' : String(value);
}
