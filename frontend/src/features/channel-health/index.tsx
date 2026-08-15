import { useCallback, useEffect, useMemo, useState } from 'react';
import type { ReactNode } from 'react';
import {
  AlertTriangle,
  CheckCircle2,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Clock3,
  Gauge,
  Loader2,
  Play,
  RefreshCw,
  Settings2,
  SkipForward,
  XCircle,
} from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useDebounce } from '@/hooks/use-debounce';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';
import { Switch } from '@/components/ui/switch';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import {
  type ActiveChannelHealthProbeRun,
  type ActiveHealthProbeModelSetting,
  type ChannelHealthProbeChannel,
  type ChannelHealthProbeHistoryInput,
  type ChannelHealthProbePolicy,
  useChannelHealthProbeHistory,
  useChannelHealthProbeOverview,
  useRunChannelHealthProbe,
  useUpdateChannelHealthProbePolicy,
  useUpdateChannelHealthProbeSettings,
} from './data/channel-health';

const intervalOptions = [1, 5, 10, 15, 30, 60, 120, 360, 720, 1440];
const historyPageSize = 50;

function formatMilliseconds(value?: number | null) {
  if (value == null || !Number.isFinite(value)) {
    return '-';
  }
  if (value < 1000) {
    return `${Math.round(value)} ms`;
  }
  return `${(value / 1000).toFixed(2)} s`;
}

function formatDate(value?: string | null) {
  if (!value) {
    return '-';
  }
  return new Date(value).toLocaleString();
}

function ProbeStatusBadge({ status }: { status?: string | null }) {
  const { t } = useTranslation();
  if (status === 'healthy') {
    return (
      <Badge variant='secondary' className='border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300'>
        <CheckCircle2 className='size-3' />
        {t('channelHealth.status.healthy')}
      </Badge>
    );
  }
  if (status === 'unhealthy') {
    return (
      <Badge variant='destructive'>
        <XCircle className='size-3' />
        {t('channelHealth.status.unhealthy')}
      </Badge>
    );
  }
  if (status === 'skipped') {
    return (
      <Badge variant='outline' className='text-muted-foreground'>
        <SkipForward className='size-3' />
        {t('channelHealth.status.skipped')}
      </Badge>
    );
  }
  return (
    <Badge variant='outline' className='text-muted-foreground'>
      <Clock3 className='size-3' />
      {status === 'pending' ? t('channelHealth.status.pending') : t('channelHealth.status.never')}
    </Badge>
  );
}

function IconAction({
  label,
  children,
  onClick,
  disabled,
}: {
  label: string;
  children: ReactNode;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <Button type='button' variant='ghost' size='icon' className='size-8' aria-label={label} onClick={onClick} disabled={disabled}>
          {children}
        </Button>
      </TooltipTrigger>
      <TooltipContent>{label}</TooltipContent>
    </Tooltip>
  );
}

function effectiveModelSettings(channels: ChannelHealthProbeChannel[], policy: ChannelHealthProbePolicy) {
  const configured = new Map(policy.models.map((model) => [model.modelID, model]));
  const models = new Map<string, ActiveHealthProbeModelSetting>();
  for (const channel of channels) {
    for (const model of channel.models) {
      if (configured.has(model.modelID)) {
        continue;
      }
      models.set(model.modelID, { modelID: model.modelID, enabled: model.enabled, stream: model.stream });
    }
  }
  for (const model of policy.models) {
    models.set(model.modelID, model);
  }
  return [...models.values()].sort((left, right) => left.modelID.localeCompare(right.modelID));
}

function GlobalModelControlPanel({
  channels,
  policy,
  modelSettings,
  canWrite,
}: {
  channels: ChannelHealthProbeChannel[];
  policy: ChannelHealthProbePolicy;
  modelSettings: ActiveHealthProbeModelSetting[];
  canWrite: boolean;
}) {
  const { t } = useTranslation();
  const updatePolicy = useUpdateChannelHealthProbePolicy();

  const saveModels = useCallback(
    (models: ActiveHealthProbeModelSetting[]) => {
      updatePolicy.mutate(
        {
          enabled: policy.enabled,
          acceptableLatencyMs: policy.acceptableLatencyMs,
          extraChannels: policy.extraChannels,
          models,
        },
        {
          onSuccess: () => toast.success(t('channelHealth.messages.modelsUpdated')),
          onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updatePolicyFailed')),
        }
      );
    },
    [policy.acceptableLatencyMs, policy.enabled, policy.extraChannels, t, updatePolicy]
  );

  const updateModel = (modelID: string, patch: Partial<ActiveHealthProbeModelSetting>) => {
    saveModels(modelSettings.map((model) => (model.modelID === modelID ? { ...model, ...patch } : model)));
  };

  return (
    <Card>
      <CardHeader className='gap-1 pb-3'>
        <div className='flex items-start justify-between gap-4'>
          <div>
            <CardTitle className='flex items-center gap-2 text-base'>
              <Gauge className='size-4' />
              {t('channelHealth.models.title')}
            </CardTitle>
            <CardDescription>{t('channelHealth.models.description')}</CardDescription>
          </div>
          <Badge variant='outline'>
            {t('channelHealth.models.enabledCount', {
              enabled: modelSettings.filter((model) => model.enabled).length,
              total: modelSettings.length,
            })}
          </Badge>
        </div>
      </CardHeader>
      <CardContent className='space-y-2'>
        {modelSettings.length === 0 ? (
          <div className='text-muted-foreground rounded-lg border border-dashed py-8 text-center text-sm'>
            {t('channelHealth.models.empty')}
          </div>
        ) : (
          modelSettings.map((model) => {
            const channelCount = channels.filter((channel) => channel.models.some((item) => item.modelID === model.modelID)).length;
            return (
              <div
                key={model.modelID}
                className='flex flex-col gap-3 rounded-lg border px-3 py-3 sm:flex-row sm:items-center sm:justify-between'
              >
                <div className='min-w-0'>
                  <div className='truncate font-mono text-sm'>{model.modelID}</div>
                  <div className='text-muted-foreground mt-1 text-xs'>
                    {t('channelHealth.models.channelCount', { count: channelCount })}
                  </div>
                </div>
                <div className='flex items-center gap-4'>
                  <label className='flex items-center gap-2 text-xs'>
                    <span className='text-muted-foreground'>{t('channelHealth.models.probe')}</span>
                    <Switch
                      checked={model.enabled}
                      onCheckedChange={(enabled) => updateModel(model.modelID, { enabled })}
                      disabled={!canWrite || updatePolicy.isPending}
                      aria-label={`${t('channelHealth.models.probe')} ${model.modelID}`}
                    />
                  </label>
                  <label className='flex items-center gap-2 text-xs'>
                    <span className='text-muted-foreground'>{t('channelHealth.columns.stream')}</span>
                    <Switch
                      checked={model.stream}
                      onCheckedChange={(stream) => updateModel(model.modelID, { stream })}
                      disabled={!canWrite || updatePolicy.isPending}
                      aria-label={`${t('channelHealth.columns.stream')} ${model.modelID}`}
                    />
                  </label>
                </div>
              </div>
            );
          })
        )}
      </CardContent>
    </Card>
  );
}

function PriorityProbePolicyPanel({
  policy,
  canWrite,
  canReadAPIKeys,
}: {
  policy: ChannelHealthProbePolicy;
  canWrite: boolean;
  canReadAPIKeys: boolean;
}) {
  const { t } = useTranslation();
  const updatePolicy = useUpdateChannelHealthProbePolicy();
  const [open, setOpen] = useState(false);
  const [enabled, setEnabled] = useState(policy.enabled);
  const [latencySeconds, setLatencySeconds] = useState(String(policy.acceptableLatencyMs / 1000));
  const [extraChannels, setExtraChannels] = useState(String(policy.extraChannels));

  useEffect(() => {
    setEnabled(policy.enabled);
    setLatencySeconds(String(policy.acceptableLatencyMs / 1000));
    setExtraChannels(String(policy.extraChannels));
  }, [policy.acceptableLatencyMs, policy.enabled, policy.extraChannels]);

  const parsedLatencySeconds = Number(latencySeconds);
  const parsedExtraChannels = Number(extraChannels);
  const acceptableLatencyMs = Math.round(parsedLatencySeconds * 1000);
  const isValid =
    Number.isFinite(parsedLatencySeconds) &&
    parsedLatencySeconds > 0 &&
    acceptableLatencyMs <= 600_000 &&
    Number.isInteger(parsedExtraChannels) &&
    parsedExtraChannels >= 0 &&
    parsedExtraChannels <= 20;
  const exceedsAPIKeyLimit = policy.apiKeyMaxFirstTokenLatencyMs != null && acceptableLatencyMs > policy.apiKeyMaxFirstTokenLatencyMs;

  const handleSave = () => {
    if (!isValid) {
      return;
    }
    updatePolicy.mutate(
      {
        enabled,
        acceptableLatencyMs,
        extraChannels: parsedExtraChannels,
        models: policy.models,
      },
      {
        onSuccess: () => toast.success(t('channelHealth.messages.policyUpdated')),
        onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updatePolicyFailed')),
      }
    );
  };

  return (
    <Collapsible open={open} onOpenChange={setOpen} className='rounded-xl border'>
      <div className='flex items-center justify-between gap-3 px-4 py-3'>
        <div className='flex items-center gap-2'>
          <Settings2 className='text-muted-foreground size-4' />
          <div>
            <div className='text-sm font-medium'>{t('channelHealth.policy.title')}</div>
            <div className='text-muted-foreground text-xs'>{t('channelHealth.policy.description')}</div>
          </div>
        </div>
        <CollapsibleTrigger asChild>
          <Button variant='ghost' size='sm' className='gap-1'>
            {t('channelHealth.policy.configure')}
            <ChevronDown className={open ? 'size-4 rotate-180 transition-transform' : 'size-4 transition-transform'} />
          </Button>
        </CollapsibleTrigger>
      </div>
      <CollapsibleContent className='border-t px-4 py-4'>
        <div className='grid gap-4 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto] md:items-start'>
          <div className='space-y-2'>
            <label htmlFor='channel-health-acceptable-latency' className='text-sm font-medium'>
              {t('channelHealth.policy.acceptableLatency')}
            </label>
            <div className='flex items-center gap-2'>
              <Input
                id='channel-health-acceptable-latency'
                type='number'
                min='0.001'
                max='600'
                step='1'
                value={latencySeconds}
                onChange={(event) => setLatencySeconds(event.target.value)}
                disabled={!canWrite || updatePolicy.isPending}
              />
              <span className='text-muted-foreground shrink-0 text-sm'>{t('channelHealth.seconds')}</span>
            </div>
            <p className='text-muted-foreground text-xs'>{t('channelHealth.policy.acceptableLatencyDescription')}</p>
            {canReadAPIKeys && policy.apiKeyMaxFirstTokenLatencyMs != null ? (
              <p
                className={
                  exceedsAPIKeyLimit
                    ? 'flex items-start gap-1.5 text-xs text-amber-600 dark:text-amber-400'
                    : 'text-muted-foreground text-xs'
                }
              >
                {exceedsAPIKeyLimit ? <AlertTriangle className='mt-0.5 size-3.5 shrink-0' /> : null}
                <span>
                  {exceedsAPIKeyLimit
                    ? t('channelHealth.policy.apiKeyLatencyWarning', { latency: formatMilliseconds(policy.apiKeyMaxFirstTokenLatencyMs) })
                    : t('channelHealth.policy.apiKeyLatencyHint', { latency: formatMilliseconds(policy.apiKeyMaxFirstTokenLatencyMs) })}
                </span>
              </p>
            ) : canReadAPIKeys ? (
              <p className='text-muted-foreground text-xs'>{t('channelHealth.policy.noAPIKeyLatencyHint')}</p>
            ) : null}
          </div>
          <div className='space-y-2'>
            <label htmlFor='channel-health-extra-channels' className='text-sm font-medium'>
              {t('channelHealth.policy.extraChannels')}
            </label>
            <Input
              id='channel-health-extra-channels'
              type='number'
              min='0'
              max='20'
              step='1'
              value={extraChannels}
              onChange={(event) => setExtraChannels(event.target.value)}
              disabled={!canWrite || updatePolicy.isPending}
            />
            <p className='text-muted-foreground text-xs'>{t('channelHealth.policy.extraChannelsDescription')}</p>
          </div>
          <div className='flex items-center gap-3 md:mt-7'>
            <label className='flex items-center gap-2 text-xs'>
              <span className='text-muted-foreground'>{t('channelHealth.policy.enabled')}</span>
              <Switch checked={enabled} onCheckedChange={setEnabled} disabled={!canWrite || updatePolicy.isPending} />
            </label>
            <Button type='button' onClick={handleSave} disabled={!canWrite || !isValid || updatePolicy.isPending}>
              {updatePolicy.isPending ? <Loader2 className='size-4 animate-spin' /> : null}
              {t('channelHealth.actions.savePolicy')}
            </Button>
          </div>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

function ChannelMonitorCard({ channel, canWrite }: { channel: ChannelHealthProbeChannel; canWrite: boolean }) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const runProbe = useRunChannelHealthProbe();
  const updateSettings = useUpdateChannelHealthProbeSettings();
  const enabledModels = channel.models.filter((model) => model.enabled);
  const healthyModels = enabledModels.filter((model) => model.latestRun?.status === 'healthy');
  const latestRun = [...channel.models]
    .map((model) => model.latestRun)
    .filter((run): run is ActiveChannelHealthProbeRun => run != null)
    .sort(
      (left, right) => new Date(right.completedAt ?? right.startedAt).getTime() - new Date(left.completedAt ?? left.startedAt).getTime()
    )[0];

  const handleRun = useCallback(
    (modelID: string, stream: boolean) => {
      runProbe.mutate(
        { channelID: channel.channelID, modelID, stream },
        {
          onSuccess: () => toast.success(t('channelHealth.messages.runStarted')),
          onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.runFailed')),
        }
      );
    },
    [channel.channelID, runProbe, t]
  );

  const handleIntervalChange = (value: string) => {
    updateSettings.mutate(
      {
        channelID: channel.channelID,
        enabled: channel.enabled,
        intervalMinutes: Number(value),
        models: channel.models.map(({ modelID, enabled, stream }) => ({ modelID, enabled, stream })),
      },
      {
        onSuccess: () => toast.success(t('channelHealth.messages.intervalUpdated')),
        onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updateFailed')),
      }
    );
  };

  return (
    <Collapsible open={open} onOpenChange={setOpen} className='bg-card overflow-hidden rounded-xl border shadow-sm'>
      <div className='flex items-center gap-3 px-4 py-4'>
        <CollapsibleTrigger asChild>
          <button type='button' className='hover:bg-muted/30 flex min-w-0 flex-1 items-center gap-4 text-left transition-colors'>
            <div className='bg-primary/10 text-primary flex size-9 shrink-0 items-center justify-center rounded-lg'>
              <Gauge className='size-4' />
            </div>
            <div className='min-w-0 flex-1'>
              <div className='flex flex-wrap items-center gap-2'>
                <span className='truncate font-medium'>{channel.channelName}</span>
                <Badge variant='outline'>{t('channelHealth.priority', { priority: channel.priority })}</Badge>
                <Badge variant={channel.channelStatus === 'enabled' ? 'secondary' : 'outline'}>{channel.channelStatus}</Badge>
              </div>
              <div className='text-muted-foreground mt-1 flex flex-wrap gap-x-3 gap-y-1 text-xs'>
                <span>{t('channelHealth.models.monitorCount', { enabled: enabledModels.length, total: channel.models.length })}</span>
                <span>{t('channelHealth.intervalSummary', { minutes: channel.intervalMinutes })}</span>
                <span>{latestRun ? formatDate(latestRun.completedAt ?? latestRun.startedAt) : t('channelHealth.status.never')}</span>
              </div>
            </div>
            <div className='hidden items-center gap-2 text-xs sm:flex'>
              <Badge variant={enabledModels.length > 0 ? 'secondary' : 'outline'}>
                {healthyModels.length}/{enabledModels.length} {t('channelHealth.status.healthy')}
              </Badge>
            </div>
            <ChevronDown className={open ? 'size-5 rotate-180 transition-transform' : 'size-5 transition-transform'} />
          </button>
        </CollapsibleTrigger>
        <div className='flex shrink-0 items-center gap-2'>
          <span className='text-muted-foreground hidden text-xs md:inline'>{t('channelHealth.interval')}</span>
          <Select
            value={String(channel.intervalMinutes)}
            onValueChange={handleIntervalChange}
            disabled={!canWrite || updateSettings.isPending}
          >
            <SelectTrigger size='sm' className='w-24'>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {intervalOptions.map((minutes) => (
                <SelectItem key={minutes} value={String(minutes)}>
                  {t('channelHealth.minutes', { count: minutes })}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      </div>
      <CollapsibleContent className='border-t'>
        <div className='overflow-x-auto'>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('channelHealth.columns.model')}</TableHead>
                <TableHead>{t('channelHealth.columns.status')}</TableHead>
                <TableHead>{t('channelHealth.columns.firstToken')}</TableHead>
                <TableHead>{t('channelHealth.columns.p95')}</TableHead>
                <TableHead>{t('channelHealth.columns.checkedAt')}</TableHead>
                <TableHead>{t('channelHealth.columns.stream')}</TableHead>
                <TableHead className='w-12 text-right'>{t('channelHealth.columns.actions')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {channel.models.map((model) => (
                <TableRow key={model.modelID}>
                  <TableCell className='max-w-72 truncate font-mono text-xs'>{model.modelID}</TableCell>
                  <TableCell>
                    <div className='flex items-center gap-2'>
                      <ProbeStatusBadge status={model.enabled ? model.latestRun?.status : 'skipped'} />
                      {model.sampleCount > 0 ? <span className='text-muted-foreground text-xs'>n={model.sampleCount}</span> : null}
                    </div>
                  </TableCell>
                  <TableCell className='font-mono text-xs'>{formatMilliseconds(model.firstTokenMs)}</TableCell>
                  <TableCell className='font-mono text-xs'>{formatMilliseconds(model.p95Ms)}</TableCell>
                  <TableCell className='text-muted-foreground text-xs'>{formatDate(model.lastProbedAt)}</TableCell>
                  <TableCell>
                    <Badge variant='outline'>{model.stream ? t('channelHealth.stream.on') : t('channelHealth.stream.off')}</Badge>
                  </TableCell>
                  <TableCell className='text-right'>
                    <IconAction
                      label={t('channelHealth.actions.runNow')}
                      onClick={() => handleRun(model.modelID, model.stream)}
                      disabled={!canWrite || !model.enabled || runProbe.isPending}
                    >
                      {runProbe.isPending ? <Loader2 className='size-4 animate-spin' /> : <Play className='size-4' />}
                    </IconAction>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      </CollapsibleContent>
    </Collapsible>
  );
}

function OverviewSummary({
  channels,
  modelSettings,
}: {
  channels: ChannelHealthProbeChannel[];
  modelSettings: ActiveHealthProbeModelSetting[];
}) {
  const { t } = useTranslation();
  const enabledModels = modelSettings.filter((model) => model.enabled);
  const monitoredRows = channels.flatMap((channel) => channel.models.filter((model) => model.enabled));
  const metrics = [
    [t('channelHealth.summary.enabledModels'), enabledModels.length],
    [t('channelHealth.summary.monitoredChannels'), channels.filter((channel) => channel.channelStatus === 'enabled').length],
    [t('channelHealth.summary.healthy'), monitoredRows.filter((model) => model.latestRun?.status === 'healthy').length],
    [t('channelHealth.summary.unhealthy'), monitoredRows.filter((model) => model.latestRun?.status === 'unhealthy').length],
  ];
  return (
    <div className='grid border-y sm:grid-cols-4'>
      {metrics.map(([label, value]) => (
        <div key={String(label)} className='border-b px-4 py-3 last:border-b-0 sm:border-r sm:last:border-r-0'>
          <div className='text-muted-foreground text-xs'>{label}</div>
          <div className='mt-1 text-lg font-semibold'>{value}</div>
        </div>
      ))}
    </div>
  );
}

function OverviewPanel({
  channels,
  policy,
  isLoading,
  error,
  canWrite,
  canReadAPIKeys,
}: {
  channels?: ChannelHealthProbeChannel[];
  policy?: ChannelHealthProbePolicy;
  isLoading: boolean;
  error: Error | null;
  canWrite: boolean;
  canReadAPIKeys: boolean;
}) {
  const { t } = useTranslation();
  const modelSettings = useMemo(
    () => effectiveModelSettings(channels ?? [], policy ?? { enabled: false, acceptableLatencyMs: 60_000, extraChannels: 1, models: [] }),
    [channels, policy]
  );

  if (isLoading) {
    return (
      <div className='space-y-3'>
        {Array.from({ length: 4 }).map((_, index) => (
          <Skeleton key={index} className='h-24 w-full rounded-xl' />
        ))}
      </div>
    );
  }
  if (error || !policy) {
    return <div className='text-destructive text-sm'>{error?.message ?? t('channelHealth.messages.loadFailed')}</div>;
  }

  return (
    <div className='space-y-4'>
      <GlobalModelControlPanel channels={channels ?? []} policy={policy} modelSettings={modelSettings} canWrite={canWrite} />
      <OverviewSummary channels={channels ?? []} modelSettings={modelSettings} />
      <PriorityProbePolicyPanel policy={policy} canWrite={canWrite} canReadAPIKeys={canReadAPIKeys} />
      {channels?.length ? (
        <div className='space-y-3'>
          <div className='flex items-center justify-between'>
            <div>
              <h3 className='text-sm font-semibold'>{t('channelHealth.channels.title')}</h3>
              <p className='text-muted-foreground text-xs'>{t('channelHealth.channels.description')}</p>
            </div>
            <Badge variant='outline'>{t('channelHealth.channels.sortedByPriority')}</Badge>
          </div>
          {channels.map((channel) => (
            <ChannelMonitorCard key={channel.channelID} channel={channel} canWrite={canWrite} />
          ))}
        </div>
      ) : (
        <div className='text-muted-foreground py-12 text-center text-sm'>{t('channelHealth.emptyOverview')}</div>
      )}
    </div>
  );
}

function HistoryPanel({ channels }: { channels: ChannelHealthProbeChannel[] }) {
  const { t } = useTranslation();
  const [channelID, setChannelID] = useState('all');
  const [modelID, setModelID] = useState('');
  const [status, setStatus] = useState('all');
  const [source, setSource] = useState('all');
  const [page, setPage] = useState(0);
  const debouncedModelID = useDebounce(modelID, 300);
  const models = useMemo(
    () => Array.from(new Set(channels.flatMap((channel) => channel.models.map((model) => model.modelID)))).sort(),
    [channels]
  );

  useEffect(() => {
    setPage(0);
  }, [channelID, debouncedModelID, status, source]);

  const input: ChannelHealthProbeHistoryInput = {
    channelID: channelID === 'all' ? undefined : channelID,
    modelID: debouncedModelID.trim() || undefined,
    status: status === 'all' ? undefined : status,
    source: source === 'all' ? undefined : source,
    offset: page * historyPageSize,
    limit: historyPageSize,
  };
  const { data, isLoading, error } = useChannelHealthProbeHistory(input);
  const hasPrevious = page > 0;
  const hasNext = (page + 1) * historyPageSize < (data?.totalCount ?? 0);

  return (
    <div className='space-y-4'>
      <div className='grid gap-2 border-b pb-4 md:grid-cols-4'>
        <Select value={channelID} onValueChange={setChannelID}>
          <SelectTrigger className='w-full'>
            <SelectValue placeholder={t('channelHealth.filters.channel')} />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value='all'>{t('channelHealth.filters.allChannels')}</SelectItem>
            {channels.map((channel) => (
              <SelectItem key={channel.channelID} value={channel.channelID}>
                {channel.channelName}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          value={modelID}
          onChange={(event) => setModelID(event.target.value)}
          placeholder={t('channelHealth.filters.model')}
          list='channel-health-probe-models'
        />
        <datalist id='channel-health-probe-models'>
          {models.map((model) => (
            <option key={model} value={model} />
          ))}
        </datalist>
        <Select value={status} onValueChange={setStatus}>
          <SelectTrigger className='w-full'>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value='all'>{t('channelHealth.filters.allStatuses')}</SelectItem>
            <SelectItem value='healthy'>{t('channelHealth.status.healthy')}</SelectItem>
            <SelectItem value='unhealthy'>{t('channelHealth.status.unhealthy')}</SelectItem>
            <SelectItem value='pending'>{t('channelHealth.status.pending')}</SelectItem>
            <SelectItem value='skipped'>{t('channelHealth.status.skipped')}</SelectItem>
          </SelectContent>
        </Select>
        <Select value={source} onValueChange={setSource}>
          <SelectTrigger className='w-full'>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value='all'>{t('channelHealth.filters.allSources')}</SelectItem>
            <SelectItem value='scheduled'>{t('channelHealth.source.scheduled')}</SelectItem>
            <SelectItem value='manual'>{t('channelHealth.source.manual')}</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <div className='overflow-x-auto rounded-md border'>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('channelHealth.columns.checkedAt')}</TableHead>
              <TableHead>{t('channelHealth.columns.channel')}</TableHead>
              <TableHead>{t('channelHealth.columns.model')}</TableHead>
              <TableHead>{t('channelHealth.columns.source')}</TableHead>
              <TableHead>{t('channelHealth.columns.status')}</TableHead>
              <TableHead>{t('channelHealth.columns.ttfb')}</TableHead>
              <TableHead>{t('channelHealth.columns.ttft')}</TableHead>
              <TableHead>{t('channelHealth.columns.total')}</TableHead>
              <TableHead>{t('channelHealth.columns.error')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              <TableRow>
                <TableCell colSpan={9}>
                  <div className='flex justify-center py-8'>
                    <Loader2 className='size-5 animate-spin' />
                  </div>
                </TableCell>
              </TableRow>
            ) : error ? (
              <TableRow>
                <TableCell colSpan={9} className='text-destructive'>
                  {error.message}
                </TableCell>
              </TableRow>
            ) : data?.items.length ? (
              data.items.map((run) => <HistoryRow key={run.id} run={run} channels={channels} />)
            ) : (
              <TableRow>
                <TableCell colSpan={9} className='text-muted-foreground py-10 text-center'>
                  {t('channelHealth.emptyHistory')}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      <div className='flex items-center justify-between'>
        <div className='text-muted-foreground text-xs'>{t('channelHealth.historyCount', { count: data?.totalCount ?? 0 })}</div>
        <div className='flex items-center gap-1'>
          <IconAction
            label={t('channelHealth.actions.previousPage')}
            onClick={() => setPage((value) => Math.max(0, value - 1))}
            disabled={!hasPrevious}
          >
            <ChevronLeft className='size-4' />
          </IconAction>
          <IconAction label={t('channelHealth.actions.nextPage')} onClick={() => setPage((value) => value + 1)} disabled={!hasNext}>
            <ChevronRight className='size-4' />
          </IconAction>
        </div>
      </div>
    </div>
  );
}

function HistoryRow({ run, channels }: { run: ActiveChannelHealthProbeRun; channels: ChannelHealthProbeChannel[] }) {
  const { t } = useTranslation();
  const channelName = channels.find((channel) => channel.channelID === run.channelID)?.channelName ?? run.channelID;
  return (
    <TableRow>
      <TableCell className='text-muted-foreground text-xs'>{formatDate(run.completedAt ?? run.startedAt)}</TableCell>
      <TableCell className='max-w-44 truncate'>{channelName}</TableCell>
      <TableCell className='max-w-52 truncate font-mono text-xs'>{run.modelID}</TableCell>
      <TableCell>
        <Badge variant='outline'>
          {run.source === 'scheduled' ? t('channelHealth.source.scheduled') : t('channelHealth.source.manual')}
        </Badge>
      </TableCell>
      <TableCell>
        <ProbeStatusBadge status={run.status} />
      </TableCell>
      <TableCell className='font-mono text-xs'>{formatMilliseconds(run.ttfbMs)}</TableCell>
      <TableCell className='font-mono text-xs'>{formatMilliseconds(run.ttftMs)}</TableCell>
      <TableCell className='font-mono text-xs'>{formatMilliseconds(run.totalMs)}</TableCell>
      <TableCell className='text-destructive max-w-80 truncate text-xs'>{run.errorMessage ?? '-'}</TableCell>
    </TableRow>
  );
}

export default function ChannelHealthPage() {
  const { t } = useTranslation();
  const { channelPermissions, apiKeyPermissions } = usePermissions();
  const overview = useChannelHealthProbeOverview();
  const channels = useMemo(() => [...(overview.data?.channels ?? [])].sort((a, b) => b.priority - a.priority), [overview.data?.channels]);

  return (
    <div className='flex flex-1 flex-col overflow-hidden'>
      <Header fixed>
        <div className='flex flex-1 items-center justify-between gap-4'>
          <div className='min-w-0'>
            <h2 className='truncate text-xl font-bold tracking-tight'>{t('channelHealth.title')}</h2>
            <p className='text-muted-foreground text-sm'>{t('channelHealth.description')}</p>
          </div>
          <IconAction label={t('common.refresh')} onClick={() => void overview.refetch()} disabled={overview.isFetching}>
            <RefreshCw className={overview.isFetching ? 'size-4 animate-spin' : 'size-4'} />
          </IconAction>
        </div>
      </Header>
      <Main fixed className='overflow-y-auto'>
        <Tabs defaultValue='monitoring' className='min-h-0'>
          <TabsList>
            <TabsTrigger value='monitoring'>{t('channelHealth.tabs.monitoring')}</TabsTrigger>
            <TabsTrigger value='history'>{t('channelHealth.tabs.history')}</TabsTrigger>
          </TabsList>
          <TabsContent value='monitoring' className='pt-2'>
            <OverviewPanel
              channels={channels}
              policy={overview.data?.policy}
              isLoading={overview.isLoading}
              error={overview.error}
              canWrite={channelPermissions.canWrite}
              canReadAPIKeys={apiKeyPermissions.canRead}
            />
          </TabsContent>
          <TabsContent value='history' className='pt-2'>
            <HistoryPanel channels={channels} />
          </TabsContent>
        </Tabs>
      </Main>
    </div>
  );
}
