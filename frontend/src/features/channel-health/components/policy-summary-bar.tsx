import { Activity, Settings2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import type { ActiveHealthProbeModelSetting, ChannelHealthProbePolicy } from '../data/channel-health';
import { formatThresholdSeconds } from '../format-threshold';

export function PolicySummaryBar({
  policy,
  modelSettings,
  canWrite,
  onEdit,
}: {
  policy: ChannelHealthProbePolicy;
  modelSettings: ActiveHealthProbeModelSetting[];
  canWrite: boolean;
  onEdit: () => void;
}) {
  const { t } = useTranslation();
  const enabledModels = modelSettings.filter((model) => model.enabled).length;
  return (
    <div className='bg-card flex flex-wrap items-center gap-x-3 gap-y-2 rounded-xl border px-4 py-2.5 text-sm'>
      <Activity className='text-muted-foreground size-4 shrink-0' />
      <span className='text-muted-foreground'>
        {t('channelHealth.policyBar.probe')}{' '}
        <span className='text-foreground font-medium'>
          {policy.enabled ? t('channelHealth.policyBar.enabled') : t('channelHealth.policyBar.disabled')}
        </span>
      </span>
      <Badge variant='secondary'>{t('channelHealth.policyBar.modelCount', { count: enabledModels })}</Badge>
      <Badge variant='secondary'>{t('channelHealth.policyBar.threshold', { latency: formatThresholdSeconds(policy.acceptableLatencyMs) })}</Badge>
      <Badge variant='secondary'>{t('channelHealth.policyBar.p95Window', { hours: policy.p95LookbackHours })}</Badge>
      <Badge variant='secondary'>{t('channelHealth.policyBar.extraChannels', { count: policy.extraChannels })}</Badge>
      <span className='flex-1' />
      <Button variant='secondary' size='sm' onClick={onEdit} disabled={!canWrite}>
        <Settings2 className='size-3.5' />
        {t('channelHealth.policyBar.edit')}
      </Button>
    </div>
  );
}
