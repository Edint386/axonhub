import { AlertTriangle, CheckCircle2, CircleDashed, Clock3, Loader2, PowerOff, SkipForward, TrendingDown, XCircle, Zap } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';

const CHIP_STYLE: Record<string, string> = {
  health: 'bg-[var(--grade-health-bg)] text-[var(--grade-health)]',
  fluent: 'bg-[var(--grade-fluent-bg)] text-[var(--grade-fluent)]',
  degraded: 'bg-[var(--grade-degraded-bg)] text-[var(--grade-degraded)]',
  abnormal: 'bg-[var(--grade-abnormal-bg)] text-[var(--grade-abnormal)] font-semibold',
  error: 'bg-destructive text-destructive-foreground font-semibold',
  unknown: 'bg-muted text-muted-foreground',
  healthy: 'bg-[var(--grade-health-bg)] text-[var(--grade-health)]',
  unhealthy: 'bg-destructive text-destructive-foreground font-semibold',
  pending: 'bg-primary/10 text-primary',
  skipped: 'bg-muted text-muted-foreground',
  never: 'text-muted-foreground border border-dashed border-border',
  unconfigured: 'text-muted-foreground border border-dashed border-border',
  disabled: 'bg-muted text-muted-foreground',
};

function StatusIcon({ status }: { status: string }) {
  const className = 'size-3';
  switch (status) {
    case 'health':
    case 'healthy':
      return <CheckCircle2 className={className} />;
    case 'fluent':
      return <Zap className={className} />;
    case 'degraded':
      return <TrendingDown className={className} />;
    case 'unknown':
      return <Clock3 className={className} />;
    case 'abnormal':
      return <AlertTriangle className={className} />;
    case 'error':
    case 'unhealthy':
      return <XCircle className={className} />;
    case 'pending':
      return <Loader2 className={cn(className, 'animate-spin')} />;
    case 'skipped':
      return <SkipForward className={className} />;
    case 'disabled':
      return <PowerOff className={className} />;
    case 'unconfigured':
      return <CircleDashed className={className} />;
    default:
      return <Clock3 className={className} />;
  }
}

/**
 * Unified status chip for channel grades (health/fluent/degraded/abnormal/error)
 * and per-run probe statuses (healthy/unhealthy/pending/skipped/never).
 */
export function ProbeStatusChip({ status, className }: { status: string; className?: string }) {
  const { t } = useTranslation();
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1 rounded-full px-2.5 py-0.5 text-xs font-medium whitespace-nowrap',
        CHIP_STYLE[status] ?? CHIP_STYLE.never,
        className
      )}
    >
      <StatusIcon status={status} />
      {t(`channelHealth.status.${status}`)}
    </span>
  );
}
