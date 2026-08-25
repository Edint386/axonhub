import { useMemo } from 'react';
import { AlertTriangle, CheckCircle2, Clock3, TrendingDown, XCircle, Zap } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { cn } from '@/lib/utils';
import type { ChannelHealthProbeChannel } from '../data/channel-health';
import { OTHER_GRADES, gradeOfChannel, type ChannelGrade } from '../probe-grade';

export type KpiFilter = ChannelGrade | 'other' | 'problem' | 'all';

const GRADE_DEFS: { key: ChannelGrade; icon: typeof CheckCircle2; color: string }[] = [
  { key: 'health', icon: CheckCircle2, color: 'var(--grade-health)' },
  { key: 'fluent', icon: Zap, color: 'var(--grade-fluent)' },
  { key: 'degraded', icon: TrendingDown, color: 'var(--grade-degraded)' },
  { key: 'abnormal', icon: AlertTriangle, color: 'var(--grade-abnormal)' },
  { key: 'error', icon: XCircle, color: 'var(--grade-error)' },
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
      disabled: 0,
    };
    for (const channel of channels) {
      result[gradeOfChannel(channel, thresholdMs)]++;
    }
    return result;
  }, [channels, thresholdMs]);

  const cards: { key: KpiFilter; label: string; icon: typeof CheckCircle2; color: string; value: number }[] = [
    ...GRADE_DEFS.map((def) => ({
      key: def.key as KpiFilter,
      label: t(`channelHealth.status.${def.key}`),
      icon: def.icon,
      color: def.color,
      value: counts[def.key],
    })),
    {
      key: 'other',
      label: t('channelHealth.status.other'),
      icon: Clock3,
      color: 'var(--muted-foreground)',
      value: OTHER_GRADES.reduce((sum, key) => sum + counts[key], 0),
    },
  ];

  return (
    <div className='grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-3'>
      {cards.map((card) => (
        <button
          key={card.key}
          type='button'
          onClick={() => onSelect(active === card.key ? 'all' : card.key)}
          className={cn(
            'bg-card relative rounded-xl border px-4 py-3 text-left transition-shadow hover:shadow-sm',
            active === card.key && 'border-primary ring-primary ring-1'
          )}
        >
          <span className='absolute inset-y-0 left-0 w-[3px] rounded-l-xl' style={{ background: card.color }} />
          <span className='text-muted-foreground flex items-center gap-1.5 text-xs'>
            <card.icon className='size-3.5' style={{ color: card.color }} />
            {card.label}
          </span>
          <span className='mt-1 flex items-baseline gap-2'>
            <span className={cn('text-2xl font-bold tracking-tight tabular-nums', card.value === 0 && 'text-muted-foreground')}>
              {card.value}
            </span>
            <span className='text-muted-foreground text-xs'>{t('channelHealth.kpi.channelUnit')}</span>
          </span>
        </button>
      ))}
    </div>
  );
}
