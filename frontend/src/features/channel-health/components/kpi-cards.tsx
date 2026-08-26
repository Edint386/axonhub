import { useMemo } from 'react';
import { AlertTriangle, CheckCircle2, Clock3, TrendingDown, XCircle, Zap } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import { Card } from '@/components/ui/card';
import type { ChannelHealthProbeChannel } from '../data/channel-health';
import { OTHER_GRADES, gradeOfChannel, type ChannelGrade } from '../probe-grade';

export type KpiFilter = ChannelGrade | 'other' | 'problem' | 'all';

/**
 * Icon-chip tints reuse the same --grade-*-bg tokens as ProbeStatusChip so a grade
 * looks identical on a KPI card and on a status chip, in light and dark themes
 * alike. --grade-error has no -bg token, so error follows the destructive tint.
 */
const GRADE_DEFS: { key: ChannelGrade; icon: typeof CheckCircle2; chip: string }[] = [
  { key: 'health', icon: CheckCircle2, chip: 'bg-[var(--grade-health-bg)] text-[var(--grade-health)]' },
  { key: 'fluent', icon: Zap, chip: 'bg-[var(--grade-fluent-bg)] text-[var(--grade-fluent)]' },
  { key: 'degraded', icon: TrendingDown, chip: 'bg-[var(--grade-degraded-bg)] text-[var(--grade-degraded)]' },
  { key: 'abnormal', icon: AlertTriangle, chip: 'bg-[var(--grade-abnormal-bg)] text-[var(--grade-abnormal)]' },
  { key: 'error', icon: XCircle, chip: 'bg-destructive/10 text-destructive' },
];

export function KpiCards({
  channels,
  thresholdMs,
  active,
  onSelect,
}: {
  channels: ChannelHealthProbeChannel[];
  thresholdMs: number;
  active: KpiFilter;
  onSelect: (filter: KpiFilter) => void;
}) {
  const { t } = useTranslation();
  const counts = useMemo(() => {
    const result: Record<ChannelGrade, number> = {
      health: 0,
      fluent: 0,
      degraded: 0,
      abnormal: 0,
      error: 0,
      unknown: 0,
      pending: 0,
      skipped: 0,
      never: 0,
      unconfigured: 0,
      disabled: 0,
    };
    for (const channel of channels) {
      result[gradeOfChannel(channel, thresholdMs)]++;
    }
    return result;
  }, [channels, thresholdMs]);

  const cards: { key: KpiFilter; label: string; icon: typeof CheckCircle2; chip: string; value: number }[] = [
    ...GRADE_DEFS.map((def) => ({
      key: def.key as KpiFilter,
      label: t(`channelHealth.status.${def.key}`),
      icon: def.icon,
      chip: def.chip,
      value: counts[def.key],
    })),
    {
      key: 'other',
      label: t('channelHealth.status.other'),
      icon: Clock3,
      chip: 'bg-muted text-muted-foreground',
      value: OTHER_GRADES.reduce((sum, key) => sum + counts[key], 0),
    },
  ];

  return (
    <div className='grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-3'>
      {cards.map((card) => {
        const selected = active === card.key;
        return (
          <Card
            key={card.key}
            role='button'
            tabIndex={0}
            onClick={() => onSelect(selected ? 'all' : card.key)}
            onKeyDown={(event) => {
              if (event.key === 'Enter' || event.key === ' ') {
                event.preventDefault();
                onSelect(selected ? 'all' : card.key);
              }
            }}
            className={cn(
              'hover-card cursor-pointer gap-0 px-4 py-3',
              selected && 'border-primary ring-primary ring-1'
            )}
          >
            <span className='text-muted-foreground flex items-center gap-2 text-xs'>
              <span className={cn('rounded-lg p-1.5', card.chip)}>
                <card.icon className='size-3.5' />
              </span>
              {card.label}
            </span>
            <span className='mt-1 flex items-baseline gap-2'>
              <span className={cn('text-2xl font-bold tracking-tight tabular-nums', card.value === 0 && 'text-muted-foreground')}>
                {card.value}
              </span>
              <span className='text-muted-foreground text-xs'>{t('channelHealth.kpi.channelUnit')}</span>
            </span>
          </Card>
        );
      })}
    </div>
  );
}
