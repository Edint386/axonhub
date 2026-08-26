import { formatDistanceToNow } from 'date-fns';
import { enUS, zhCN } from 'date-fns/locale';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { formatDuration } from '@/utils/format-duration';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import type { ActiveChannelHealthProbeRun, ChannelHealthProbeChannel } from '../data/channel-health';
import { firstTokenMsOf, gradeOfRun, type ProbeBarStatus } from '../probe-grade';

const BAR_COLOR: Record<ProbeBarStatus, string> = {
  health: 'bg-[var(--grade-health)]',
  fluent: 'bg-[var(--grade-fluent)]',
  degraded: 'bg-[var(--grade-degraded)]',
  error: 'bg-[var(--grade-error)]',
  unknown: 'bg-muted-foreground/50',
  skipped: 'bg-muted',
  pending: 'bg-primary animate-pulse',
};

function ProbeBar({ run, status, dateLocale }: { run: ActiveChannelHealthProbeRun; status: ProbeBarStatus; dateLocale: typeof zhCN }) {
  const { t } = useTranslation();
  const time = formatDistanceToNow(new Date(run.completedAt ?? run.startedAt), { addSuffix: true, locale: dateLocale });
  const first = firstTokenMsOf(run);
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div
          className={cn(
            'h-9 w-[5px] cursor-help rounded-[2px] opacity-90 transition-opacity hover:opacity-100',
            BAR_COLOR[status],
            status === 'skipped' && 'h-[42%]'
          )}
        />
      </TooltipTrigger>
      <TooltipContent>
        <div className='space-y-1 text-xs'>
          <div>{time}</div>
          <div>{t(`channelHealth.status.${status}`)}</div>
          {first != null ? <div>{formatDuration(first)}</div> : null}
          {run.errorMessage ? <div className='text-destructive max-w-60 truncate'>{run.errorMessage}</div> : null}
        </div>
      </TooltipContent>
    </Tooltip>
  );
}

/**
 * Recent probe strip for a channel — one graded bar per run, oldest first.
 * The bar list answers "近期表现如何" without leaving the matrix.
 */
export function ProbeRecentStrip({ channel, thresholdMs }: { channel: ChannelHealthProbeChannel; thresholdMs: number }) {
  const { t, i18n } = useTranslation();
  const runs = channel.recentRuns ?? [];
  if (runs.length === 0) {
    return <span className='text-muted-foreground text-xs'>{t('channelHealth.status.never')}</span>;
  }
  const dateLocale = i18n.language.startsWith('zh') ? zhCN : enUS;
  return (
    <div className='flex h-9 items-end justify-center gap-[2px]'>
      {runs.map((run) => (
        <ProbeBar key={run.id} run={run} status={gradeOfRun(run, thresholdMs)} dateLocale={dateLocale} />
      ))}
    </div>
  );
}
