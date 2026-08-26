/**
 * Seconds-only formatter for the acceptable-latency threshold.
 *
 * The shared `@/utils/format-duration` switches to minutes at >= 60000ms, so
 * the default 60000 renders as "1.0m" while the settings sheet edits it as
 * "60" seconds. This local formatter keeps the threshold in seconds on every
 * channel-health surface so the two agree. Do NOT reach for format-duration
 * for the threshold badge.
 */
export function formatThresholdSeconds(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) {
    return '0s';
  }
  const seconds = ms / 1000;
  // Trim a trailing ".0" so 60000 -> "60s", but keep "1.5s" for fractional values.
  const text = Number.isInteger(seconds) ? String(seconds) : seconds.toFixed(seconds < 1 ? 3 : 1);
  return `${text}s`;
}
