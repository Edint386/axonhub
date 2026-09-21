'use client';

import { useTranslation } from 'react-i18next';
import { Badge } from '@/components/ui/badge';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { cn } from '@/lib/utils';

const PHASE_CLASS: Record<string, string> = {
  disabled: 'text-muted-foreground border-muted-foreground/30',
  pending: 'text-muted-foreground border-muted-foreground/30',
  harvesting: 'text-blue-500 border-blue-500/30',
  ready: 'text-green-500 border-green-500/30',
  cooldown: 'text-amber-500 border-amber-500/30',
  failed: 'text-red-500 border-red-500/30',
};

const DOT_CLASS: Record<string, string> = {
  disabled: 'bg-muted-foreground',
  pending: 'bg-muted-foreground',
  harvesting: 'bg-blue-500 animate-pulse',
  ready: 'bg-green-500',
  cooldown: 'bg-amber-500',
  failed: 'bg-red-500',
};

const STATUS_KEYS: Record<string, string> = {
  disabled: 'channels.dialogs.codexTurnState.status.disabled',
  pending: 'channels.dialogs.codexTurnState.status.pending',
  harvesting: 'channels.dialogs.codexTurnState.status.harvesting',
  ready: 'channels.dialogs.codexTurnState.status.ready',
  cooldown: 'channels.dialogs.codexTurnState.status.cooldown',
  failed: 'channels.dialogs.codexTurnState.status.failed',
};

export function CodexTurnStateIndicator({
  phase,
  compact = false,
  className,
}: {
  phase?: string | null;
  compact?: boolean;
  className?: string;
}) {
  const { t } = useTranslation();
  if (!phase) return null;
  const label = t(STATUS_KEYS[phase] ?? STATUS_KEYS.pending);
  const color = PHASE_CLASS[phase] ?? PHASE_CLASS.pending;
  const dot = DOT_CLASS[phase] ?? DOT_CLASS.pending;

  if (compact) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            className={cn('inline-flex h-2.5 w-2.5 shrink-0 rounded-full', dot, className)}
            aria-label={label}
          />
        </TooltipTrigger>
        <TooltipContent>{label}</TooltipContent>
      </Tooltip>
    );
  }

  return (
    <Badge variant='outline' className={cn(color, className)}>
      <span className={cn('h-1.5 w-1.5 rounded-full', dot)} />
      {label}
    </Badge>
  );
}
