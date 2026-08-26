import { Fragment, useMemo, useState } from 'react';
import { ChevronRight, Loader2, Play } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { formatDuration } from '@/utils/format-duration';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useUpdateChannelHealthProbeSettings, type ChannelHealthProbeChannel } from '../data/channel-health';
import {
  CHANNEL_GRADE_ORDER,
  channelLatestFirst,
  channelP50,
  channelP95,
  formatMultiplier,
  gradeOfChannel,
  gradeOfRun,
  primaryModelOf,
} from '../probe-grade';
import { useProbeActions } from '../use-probe-actions';
import { ProbeRecentStrip } from './probe-recent-strip';
import { ProbeStatusChip } from './probe-status-chip';

type SortKey = 'name' | 'mult' | 'status' | 'first';

function SortableHead({
  label,
  sortKey,
  current,
  asc,
  onSort,
  className,
}: {
  label: string;
  sortKey: SortKey;
  current: SortKey;
  asc: boolean;
  onSort: (key: SortKey) => void;
  className?: string;
}) {
  return (
    <TableHead
      className={cn(
        'text-muted-foreground cursor-pointer border-0 px-4 py-3 text-xs font-semibold tracking-wider uppercase select-none',
        className
      )}
      onClick={() => onSort(sortKey)}
    >
      {label}
      {current === sortKey ? <span className='ml-1 text-[10px] opacity-70'>{asc ? '▲' : '▼'}</span> : null}
    </TableHead>
  );
}

const HEAD_CELL = 'text-muted-foreground border-0 px-4 py-3 text-xs font-semibold tracking-wider uppercase';
const BODY_CELL = 'border-0 bg-inherit px-4 py-3';

export function ChannelMatrixTable({
  channels,
  thresholdMs,
  canWrite,
  onOpenDetail,
  probeActions,
}: {
  channels: ChannelHealthProbeChannel[];
  thresholdMs: number;
  canWrite: boolean;
  onOpenDetail: (channel: ChannelHealthProbeChannel) => void;
  probeActions: ReturnType<typeof useProbeActions>;
}) {
  const { t } = useTranslation();
  const [sortKey, setSortKey] = useState<SortKey>('status');
  const [sortAsc, setSortAsc] = useState(true);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const { probing, probeChannel, probeModel } = probeActions;
  const updateSettings = useUpdateChannelHealthProbeSettings();

  const sorted = useMemo(() => {
    const comparators: Record<SortKey, (a: ChannelHealthProbeChannel, b: ChannelHealthProbeChannel) => number> = {
      name: (a, b) => a.channelName.localeCompare(b.channelName, 'zh'),
      mult: (a, b) => a.modelPriceMultiplier - b.modelPriceMultiplier,
      status: (a, b) => CHANNEL_GRADE_ORDER[gradeOfChannel(a, thresholdMs)] - CHANNEL_GRADE_ORDER[gradeOfChannel(b, thresholdMs)],
      first: (a, b) => (channelP50(a) ?? Number.MAX_VALUE) - (channelP50(b) ?? Number.MAX_VALUE),
    };
    const list = [...channels].sort((a, b) => {
      const result = comparators[sortKey](a, b);
      return sortAsc ? result : -result;
    });
    return list;
  }, [channels, sortKey, sortAsc, thresholdMs]);

  const handleSort = (key: SortKey) => {
    if (sortKey === key) {
      setSortAsc((value) => !value);
    } else {
      setSortKey(key);
      setSortAsc(key === 'name' || key === 'mult');
    }
  };

  const toggleExpanded = (channelID: string) => {
    setExpanded((current) => {
      const next = new Set(current);
      if (next.has(channelID)) {
        next.delete(channelID);
      } else {
        next.add(channelID);
      }
      return next;
    });
  };

  const toggleProbeEnabled = (channel: ChannelHealthProbeChannel, probeEnabled: boolean) => {
    updateSettings.mutate(
      { channelID: channel.channelID, intervalMinutes: channel.intervalMinutes, probeEnabled },
      {
        onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updateFailed')),
      }
    );
  };

  return (
    <div className='relative overflow-auto rounded-2xl border border-[var(--table-border)]'>
      <Table className='border-separate border-spacing-0 bg-[var(--table-background)]'>
        <colgroup>
          <col className='w-8' />
          <col className='w-[220px]' />
          <col className='w-20' />
          <col className='w-24' />
          <col className='w-40' />
          <col className='w-52' />
          <col className='w-32' />
          <col />
          <col className='w-28' />
        </colgroup>
        <TableHeader className='sticky top-0 z-20 bg-[var(--table-header)] shadow-sm'>
          <TableRow className='border-0'>
            <TableHead className={cn(HEAD_CELL, 'w-8')} />
            <SortableHead label={t('channelHealth.columns.channel')} sortKey='name' current={sortKey} asc={sortAsc} onSort={handleSort} />
            <TableHead className={HEAD_CELL}>{t('channelHealth.columns.probeEnabled')}</TableHead>
            <SortableHead
              label={t('channelHealth.columns.multiplier')}
              sortKey='mult'
              current={sortKey}
              asc={sortAsc}
              onSort={handleSort}
            />
            <SortableHead label={t('channelHealth.columns.status')} sortKey='status' current={sortKey} asc={sortAsc} onSort={handleSort} />
            <TableHead className={HEAD_CELL}>{t('channelHealth.columns.modelHealth')}</TableHead>
            <SortableHead
              label={t('channelHealth.columns.firstToken')}
              sortKey='first'
              current={sortKey}
              asc={sortAsc}
              onSort={handleSort}
            />
            <TableHead className={HEAD_CELL}>{t('channelHealth.columns.recentProbes')}</TableHead>
            <TableHead className={cn(HEAD_CELL, 'text-right')}>{t('channelHealth.columns.actions')}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody className='!bg-[var(--table-background)]'>
          {sorted.map((channel) => {
            const grade = gradeOfChannel(channel, thresholdMs);
            const primaryModel = primaryModelOf(channel);
            const latest = channelLatestFirst(channel);
            const p50 = channelP50(channel);
            const p95 = channelP95(channel);
            const isProbing = probeActions.isChannelProbing(channel.channelID);
            const isExpanded = expanded.has(channel.channelID);
            return (
              <Fragment key={channel.channelID}>
                <TableRow
                  className={cn('table-row-hover cursor-pointer border-0 !bg-[var(--table-background)]', isExpanded && '!bg-accent/50')}
                  onClick={() => onOpenDetail(channel)}
                >
                  <TableCell className={BODY_CELL} onClick={(event) => event.stopPropagation()}>
                    <button
                      type='button'
                      onClick={() => toggleExpanded(channel.channelID)}
                      className='text-muted-foreground hover:text-foreground transition-colors'
                      aria-label={t('channelHealth.actions.expand')}
                    >
                      <ChevronRight className={cn('size-4 transition-transform', isExpanded && 'rotate-90')} />
                    </button>
                  </TableCell>
                  <TableCell className={BODY_CELL}>
                    <div className='truncate font-medium'>{channel.channelName}</div>
                    {channel.enabled ? null : (
                      <div className='mt-0.5 text-xs text-[var(--grade-degraded)]'>{t('channelHealth.channelDisabled')}</div>
                    )}
                  </TableCell>
                  <TableCell className={BODY_CELL} onClick={(event) => event.stopPropagation()}>
                    <Switch
                      checked={channel.probeEnabled}
                      onCheckedChange={(checked) => toggleProbeEnabled(channel, checked)}
                      disabled={!canWrite || updateSettings.isPending}
                      aria-label={t('channelHealth.columns.probeEnabled')}
                    />
                  </TableCell>
                  <TableCell
                    className={cn(BODY_CELL, 'font-mono text-xs tabular-nums', channel.modelPriceMultiplier === 1 && 'text-muted-foreground')}
                  >
                    {formatMultiplier(channel.modelPriceMultiplier)}
                  </TableCell>
                  <TableCell className={BODY_CELL}>
                    <ProbeStatusChip status={grade} />
                  </TableCell>
                  <TableCell className={BODY_CELL}>
                    <div className='space-y-1'>
                      <span className='block max-w-40 truncate font-mono text-[11px]'>{primaryModel?.modelID ?? '-'}</span>
                      <ProbeStatusChip status={primaryModel?.latestRun ? gradeOfRun(primaryModel.latestRun, thresholdMs) : grade} />
                    </div>
                  </TableCell>
                  <TableCell className={BODY_CELL}>
                    <span
                      className={cn(
                        'text-xs tabular-nums',
                        p50 != null && p50 > thresholdMs * 2 && 'text-destructive font-bold',
                        p50 != null && p50 > thresholdMs && p50 <= thresholdMs * 2 && 'font-semibold text-[var(--grade-degraded)]'
                      )}
                    >
                      {p50 != null ? formatDuration(p50) : '-'}
                      <span className='text-muted-foreground block text-[10.5px]'>
                        {t('channelHealth.firstTokenDetail', {
                          latest: latest != null ? formatDuration(latest) : '-',
                          p95: p95 != null ? formatDuration(p95) : '-',
                        })}
                      </span>
                    </span>
                  </TableCell>
                  <TableCell className={BODY_CELL}>
                    <ProbeRecentStrip channel={channel} thresholdMs={thresholdMs} />
                  </TableCell>
                  <TableCell className={cn(BODY_CELL, 'text-right')} onClick={(event) => event.stopPropagation()}>
                    <Button
                      variant='outline'
                      size='sm'
                      disabled={!canWrite || !channel.enabled || !channel.models.some((model) => model.enabled) || isProbing}
                      onClick={() => void probeChannel(channel)}
                    >
                      {isProbing ? <Loader2 className='size-3.5 animate-spin' /> : <Play className='size-3.5' />}
                      {isProbing ? t('channelHealth.actions.probing') : t('channelHealth.actions.probe')}
                    </Button>
                  </TableCell>
                </TableRow>
                {isExpanded ? (
                  <TableRow key={`${channel.channelID}-sub`} className='border-0 bg-muted/30 hover:bg-muted/30'>
                    <TableCell colSpan={9} className='border-0 p-0'>
                      <div className='px-14 pt-1 pb-3'>
                        <Table>
                          <TableBody>
                            {channel.models.map((model) => {
                              const modelKey = `${channel.channelID}:${model.modelID}`;
                              const modelProbing = probing.has(modelKey);
                              return (
                                <TableRow key={model.modelID} className='border-b-0'>
                                  <TableCell className='w-[26%]'>
                                    <span className='font-mono text-xs'>{model.modelID}</span>
                                    <span className='text-muted-foreground ml-1.5 text-[10.5px]'>
                                      {model.stream ? t('channelHealth.stream.on') : t('channelHealth.stream.off')}
                                    </span>
                                  </TableCell>
                                  <TableCell className='w-[14%]'>
                                    <ProbeStatusChip
                                      status={
                                        model.enabled ? (model.latestRun ? gradeOfRun(model.latestRun, thresholdMs) : 'never') : 'disabled'
                                      }
                                    />
                                  </TableCell>
                                  <TableCell className='w-[14%] font-mono text-xs tabular-nums'>
                                    {model.firstTokenMs != null ? formatDuration(model.firstTokenMs) : '-'}
                                  </TableCell>
                                  <TableCell className='w-[20%] font-mono text-xs tabular-nums'>
                                    {model.p95Ms != null ? `${formatDuration(model.p95Ms)} · n=${model.sampleCount}` : '-'}
                                  </TableCell>
                                  <TableCell className='text-right'>
                                    <Button
                                      variant='ghost'
                                      size='sm'
                                      disabled={!canWrite || !channel.enabled || !model.enabled || modelProbing}
                                      onClick={() => void probeModel(channel, model.modelID, model.stream)}
                                    >
                                      {modelProbing ? <Loader2 className='size-3.5 animate-spin' /> : <Play className='size-3.5' />}
                                      {modelProbing ? t('channelHealth.actions.probing') : t('channelHealth.actions.probe')}
                                    </Button>
                                  </TableCell>
                                </TableRow>
                              );
                            })}
                          </TableBody>
                        </Table>
                      </div>
                    </TableCell>
                  </TableRow>
                ) : null}
              </Fragment>
            );
          })}
          {sorted.length === 0 ? (
            <TableRow className='border-0'>
              <TableCell colSpan={9} className='text-muted-foreground border-0 py-12 text-center text-sm'>
                {t('channelHealth.emptyMatrix')}
              </TableCell>
            </TableRow>
          ) : null}
        </TableBody>
      </Table>
    </div>
  );
}
