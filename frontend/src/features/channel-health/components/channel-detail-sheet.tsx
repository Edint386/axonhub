import { useEffect, useMemo, useState } from 'react';
import { useNavigate } from '@tanstack/react-router';
import { AlertTriangle, Clock3, ExternalLink, Play, TrendingDown } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Area, AreaChart, CartesianGrid, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { formatDuration } from '@/utils/format-duration';
import { Button } from '@/components/ui/button';
import { CopyButton } from '@/components/ui/copy-button';
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet';
import { Tooltip as UITooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { useUpdateChannelHealthProbeSettings, type ChannelHealthProbeChannel } from '../data/channel-health';
import { firstTokenMsOf, formatMultiplier, gradeOfRun } from '../probe-grade';
import { ProbeStatusChip } from './probe-status-chip';

const BAR_COLOR: Record<string, string> = {
  health: 'bg-[var(--grade-health)]',
  fluent: 'bg-[var(--grade-fluent)]',
  degraded: 'bg-[var(--grade-degraded)]',
  error: 'bg-[var(--grade-error)]',
  unknown: 'bg-muted-foreground/50',
  skipped: 'bg-muted',
  pending: 'bg-primary animate-pulse',
};

export function ChannelDetailSheet({
  channel,
  open,
  onOpenChange,
  thresholdMs,
  onProbeAll,
  probing,
  canWrite,
}: {
  channel: ChannelHealthProbeChannel | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  thresholdMs: number;
  onProbeAll: (channel: ChannelHealthProbeChannel) => void;
  probing: boolean;
  canWrite: boolean;
}) {
  const { t } = useTranslation();
  const navigate = useNavigate();
  const updateSettings = useUpdateChannelHealthProbeSettings();
  const [intervalMinutes, setIntervalMinutes] = useState(String(channel?.intervalMinutes ?? 5));

  useEffect(() => {
    if (channel) {
      setIntervalMinutes(String(channel.intervalMinutes));
    }
  }, [channel]);

  const parsedIntervalMinutes = Number(intervalMinutes);
  const intervalValid = Number.isInteger(parsedIntervalMinutes) && parsedIntervalMinutes >= 1 && parsedIntervalMinutes <= 24 * 60;

  const saveInterval = () => {
    if (!channel || !intervalValid || updateSettings.isPending) {
      return;
    }
    updateSettings.mutate(
      { channelID: channel.channelID, intervalMinutes: parsedIntervalMinutes },
      {
        onSuccess: () => toast.success(t('channelHealth.detail.intervalSaved')),
        onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updateFailed')),
      }
    );
  };

  const chartData = useMemo(() => {
    if (!channel) {
      return [];
    }
    return (channel.recentRuns ?? [])
      .map((run, index) => ({ index, firstToken: run.status === 'healthy' ? firstTokenMsOf(run) : null }))
      .filter((point) => point.firstToken != null);
  }, [channel]);

  if (!channel) {
    return null;
  }
  const latestErrorRun = [...(channel.recentRuns ?? [])].reverse().find((run) => run.status === 'unhealthy');
  const runs = channel.recentRuns ?? [];

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent className='flex w-full flex-col sm:max-w-xl'>
        <SheetHeader>
          <SheetTitle>{channel.channelName}</SheetTitle>
          <SheetDescription>
            {t('channelHealth.priority', { priority: channel.priority })} ·{' '}
            {t('channelHealth.detail.multiplier', { value: formatMultiplier(channel.modelPriceMultiplier) })} ·{' '}
            {t('channelHealth.intervalSummary', { minutes: channel.intervalMinutes })}
          </SheetDescription>
        </SheetHeader>
        <div className='flex-1 space-y-6 overflow-y-auto px-4 pb-4'>
          <section>
            <h3 className='mb-2 flex items-center gap-1.5 text-sm font-semibold'>
              <Clock3 className='text-muted-foreground size-3.5' />
              {t('channelHealth.detail.timeline', { count: runs.length })}
            </h3>
            {runs.length > 0 ? (
              <div className='flex h-16 items-end gap-[3px]'>
                {runs.map((run) => {
                  const grade = gradeOfRun(run, thresholdMs);
                  const first = firstTokenMsOf(run);
                  return (
                    <UITooltip key={run.id}>
                      <TooltipTrigger asChild>
                        <div className={cn('flex-1 cursor-help rounded-t', BAR_COLOR[grade], grade === 'skipped' && 'h-[40%]')} />
                      </TooltipTrigger>
                      <TooltipContent>
                        <div className='space-y-1 text-xs'>
                          <div>{run.modelID}</div>
                          <div>{new Date(run.completedAt ?? run.startedAt).toLocaleString()}</div>
                          <div>{t(`channelHealth.status.${grade}`)}</div>
                          {first != null ? <div>{formatDuration(first)}</div> : null}
                        </div>
                      </TooltipContent>
                    </UITooltip>
                  );
                })}
              </div>
            ) : (
              <div className='text-muted-foreground rounded-lg border border-dashed py-6 text-center text-xs'>
                {t('channelHealth.status.never')}
              </div>
            )}
          </section>

          <section className='rounded-lg border p-3'>
            <div className='flex flex-wrap items-start justify-between gap-3'>
              <div>
                <h3 className='text-sm font-semibold'>{t('channelHealth.detail.intervalTitle')}</h3>
              </div>
              <div className='flex items-center gap-2'>
                <input
                  aria-label={t('channelHealth.detail.intervalTitle')}
                  type='number'
                  min='1'
                  max='1440'
                  step='1'
                  value={intervalMinutes}
                  onChange={(event) => setIntervalMinutes(event.target.value)}
                  disabled={!canWrite || updateSettings.isPending}
                  className='border-input bg-background h-8 w-24 rounded-md border px-2 text-right text-sm tabular-nums'
                />
                <span className='text-muted-foreground text-xs'>{t('channelHealth.detail.intervalUnit')}</span>
                <Button
                  type='button'
                  size='sm'
                  variant='outline'
                  onClick={saveInterval}
                  disabled={!canWrite || !intervalValid || updateSettings.isPending || parsedIntervalMinutes === channel.intervalMinutes}
                >
                  {t('channelHealth.detail.intervalSave')}
                </Button>
              </div>
            </div>
            {!intervalValid ? <p className='text-destructive mt-2 text-xs'>{t('channelHealth.detail.intervalInvalid')}</p> : null}
          </section>

          {chartData.length > 1 ? (
            <section>
              <h3 className='mb-2 flex items-center gap-1.5 text-sm font-semibold'>
                <TrendingDown className='text-muted-foreground size-3.5' />
                {t('channelHealth.detail.trend')}
              </h3>
              <div className='rounded-lg border p-2'>
                <ResponsiveContainer width='100%' height={160}>
                  <AreaChart data={chartData} margin={{ top: 8, right: 8, bottom: 0, left: 8 }}>
                    <CartesianGrid strokeDasharray='3 3' vertical={false} />
                    <XAxis dataKey='index' hide />
                    <YAxis
                      width={48}
                      tickFormatter={(value: number) => formatDuration(value)}
                      tick={{ fontSize: 11 }}
                      tickLine={false}
                      axisLine={false}
                    />
                    <Tooltip
                      formatter={(value) => [value != null ? formatDuration(Number(value)) : '-', t('channelHealth.columns.firstToken')]}
                      labelFormatter={() => ''}
                    />
                    <ReferenceLine
                      y={thresholdMs}
                      stroke='var(--destructive)'
                      strokeDasharray='5 4'
                      label={{
                        value: t('channelHealth.detail.thresholdLine'),
                        fill: 'var(--destructive)',
                        fontSize: 10,
                        position: 'insideTopRight',
                      }}
                    />
                    <Area
                      type='monotone'
                      dataKey='firstToken'
                      stroke='var(--chart-1)'
                      fill='var(--chart-1)'
                      fillOpacity={0.12}
                      strokeWidth={2}
                      dot={(props) => {
                        const { cx, cy, payload } = props;
                        const over = payload.firstToken != null && payload.firstToken > thresholdMs;
                        return (
                          <circle
                            key={payload.index}
                            cx={cx}
                            cy={cy}
                            r={over ? 3.5 : 2}
                            fill={over ? 'var(--destructive)' : 'var(--chart-1)'}
                          />
                        );
                      }}
                    />
                  </AreaChart>
                </ResponsiveContainer>
              </div>
            </section>
          ) : null}

          <section>
            <h3 className='mb-2 text-sm font-semibold'>{t('channelHealth.detail.modelCompare')}</h3>
            <div className='overflow-hidden rounded-lg border'>
              <table className='w-full text-sm'>
                <tbody>
                  {channel.models.map((model) => (
                    <tr key={model.modelID} className='border-b last:border-b-0'>
                      <td className='px-3 py-2'>
                        <span className='font-mono text-xs'>{model.modelID}</span>
                      </td>
                      <td className='px-3 py-2'>
                        <ProbeStatusChip status={model.enabled ? (model.latestRun?.status ?? 'never') : 'disabled'} />
                      </td>
                      <td className='px-3 py-2 font-mono text-xs tabular-nums'>
                        {model.firstTokenMs != null ? formatDuration(model.firstTokenMs) : '-'}{' '}
                        <span className='text-muted-foreground text-[10.5px]'>{t('channelHealth.detail.latestLabel')}</span>
                      </td>
                      <td className='px-3 py-2 font-mono text-xs tabular-nums'>
                        {model.p95Ms != null ? formatDuration(model.p95Ms) : '-'}{' '}
                        <span className='text-muted-foreground text-[10.5px]'>P95 · n={model.sampleCount}</span>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          {latestErrorRun?.errorMessage ? (
            <section>
              <h3 className='mb-2 flex items-center gap-1.5 text-sm font-semibold'>
                <AlertTriangle className='text-destructive size-3.5' />
                {t('channelHealth.detail.recentError')}
              </h3>
              <div className='border-destructive/30 bg-destructive/10 rounded-lg border px-3 py-2.5'>
                <div className='flex items-center justify-between gap-2'>
                  <span className='text-destructive text-xs font-semibold'>
                    {t('channelHealth.detail.errorModel', { model: latestErrorRun.modelID })}
                  </span>
                  <CopyButton content={latestErrorRun.errorMessage} />
                </div>
                <p className='mt-1.5 font-mono text-xs leading-relaxed break-all'>{latestErrorRun.errorMessage}</p>
                <p className='text-muted-foreground mt-1.5 text-[11px]'>
                  {t('channelHealth.detail.errorTime', {
                    time: new Date(latestErrorRun.completedAt ?? latestErrorRun.startedAt).toLocaleString(),
                  })}
                </p>
              </div>
            </section>
          ) : null}
        </div>
        <div className='flex gap-2 border-t px-4 py-3'>
          <Button
            disabled={!canWrite || !channel.enabled || channel.models.every((model) => !model.enabled) || probing}
            onClick={() => onProbeAll(channel)}
          >
            <Play className='size-3.5' />
            {t('channelHealth.detail.probeAll')}
          </Button>
          <Button
            variant='outline'
            onClick={() => {
              onOpenChange(false);
              void navigate({ to: '/channels' });
            }}
          >
            <ExternalLink className='size-3.5' />
            {t('channelHealth.detail.viewRuntimeHealth')}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}
