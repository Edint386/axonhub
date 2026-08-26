import { useEffect, useMemo, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { AlertTriangle, ExternalLink, Eye, RefreshCw, Search, Settings2, Sparkles } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { useDebounce } from '@/hooks/use-debounce';
import { usePermissions } from '@/hooks/usePermissions';
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Skeleton } from '@/components/ui/skeleton';
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { Header } from '@/components/layout/header';
import { Main } from '@/components/layout/main';
import { ChannelDetailSheet } from './components/channel-detail-sheet';
import { ChannelMatrixTable } from './components/channel-matrix-table';
import { HistoryPanel } from './components/history-panel';
import { KpiCards, type KpiFilter } from './components/kpi-cards';
import { PolicySummaryBar } from './components/policy-summary-bar';
import { ProbeSettingsSheet } from './components/probe-settings-sheet';
import { useChannelHealthProbeOverview, type ChannelHealthProbePolicy } from './data/channel-health';
import { OTHER_GRADES, PROBLEM_GRADES, gradeOfChannel, type ChannelGrade } from './probe-grade';
import { useProbeActions } from './use-probe-actions';

const GRADE_ORDER: ChannelGrade[] = ['health', 'fluent', 'degraded', 'abnormal', 'error', 'unknown'];

function effectiveModelSettings(policy: ChannelHealthProbePolicy) {
  return policy.models.map((model) => ({ ...model }));
}

function LastUpdated({ dataUpdatedAt }: { dataUpdatedAt: number }) {
  const { t } = useTranslation();
  const [, setTick] = useState(0);
  useEffect(() => {
    const timer = setInterval(() => setTick((value) => value + 1), 5000);
    return () => clearInterval(timer);
  }, []);
  if (!dataUpdatedAt) {
    return null;
  }
  const seconds = Math.max(0, Math.floor((Date.now() - dataUpdatedAt) / 1000));
  const text =
    seconds < 10
      ? t('channelHealth.lastUpdated.now')
      : seconds < 60
        ? t('channelHealth.lastUpdated.seconds', { seconds })
        : t('channelHealth.lastUpdated.minutes', { minutes: Math.floor(seconds / 60) });
  return <span className='text-muted-foreground text-xs'>{text}</span>;
}

export default function ChannelHealthPage() {
  const { t } = useTranslation();
  const { channelPermissions } = usePermissions();
  const overview = useChannelHealthProbeOverview();
  const policy = overview.data?.policy;
  // The routing ceiling, drawn as a reference line in the detail chart. Health
  // grading no longer reads it -- that uses the built-in bands in probe-grade.
  // policy.acceptableLatencyMs is only the fallback; the ceiling actually in force
  // is the stricter of it and the tightest ceiling across enabled API keys.
  const routingCeilingMs = useMemo(() => {
    const fallbackMs = policy?.acceptableLatencyMs ?? 60_000;
    const apiKeyCeilingMs = policy?.apiKeyMaxFirstTokenLatencyMs;
    if (typeof apiKeyCeilingMs !== 'number' || !Number.isFinite(apiKeyCeilingMs) || apiKeyCeilingMs <= 0) {
      return fallbackMs;
    }
    return Math.min(fallbackMs, apiKeyCeilingMs);
  }, [policy?.acceptableLatencyMs, policy?.apiKeyMaxFirstTokenLatencyMs]);
  const canWrite = channelPermissions.canWrite;
  const channels = useMemo(() => overview.data?.channels ?? [], [overview.data?.channels]);
  const modelSettings = useMemo(
    () =>
      effectiveModelSettings(
        policy ?? {
          enabled: false,
          intervalMinutes: 5,
          stream: false,
          acceptableLatencyMs: 60_000,
          extraChannels: 1,
          p95LookbackHours: 24,
          availableModels: [],
          models: [],
        }
      ),
    [policy]
  );

  const probeActions = useProbeActions();
  const [detailChannelID, setDetailChannelID] = useState<string | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState<KpiFilter>('all');
  const [modelFilter, setModelFilter] = useState('all');
  const [channelStatusFilter, setChannelStatusFilter] = useState<'enabled' | 'all' | 'disabled'>('enabled');
  const debouncedQuery = useDebounce(query, 300);

  const detailChannel = useMemo(
    () => channels.find((channel) => channel.channelID === detailChannelID) ?? null,
    [channels, detailChannelID]
  );
  const problemCounts = useMemo(() => {
    let abnormal = 0;
    let error = 0;
    for (const channel of channels) {
      const grade = gradeOfChannel(channel);
      if (grade === 'abnormal') {
        abnormal++;
      } else if (grade === 'error') {
        error++;
      }
    }
    return { abnormal, error };
  }, [channels]);

  const filteredChannels = useMemo(() => {
    const keyword = debouncedQuery.trim().toLowerCase();
    return channels.filter((channel) => {
      const grade = gradeOfChannel(channel);
      // On this page "enabled" means "actually being probed", which needs BOTH the
      // channel's own status and its per-channel probe opt-in. Without probeEnabled
      // here, a channel you switch off stays on screen under every filter option.
      const probed = channel.enabled && channel.probeEnabled;
      if (channelStatusFilter === 'enabled' && !probed) {
        return false;
      }
      if (channelStatusFilter === 'disabled' && probed) {
        return false;
      }
      if (statusFilter !== 'all') {
        const match =
          statusFilter === 'problem'
            ? PROBLEM_GRADES.includes(grade)
            : statusFilter === 'other'
              ? OTHER_GRADES.includes(grade)
              : grade === statusFilter;
        if (!match) {
          return false;
        }
      }
      if (modelFilter !== 'all' && !channel.models.some((model) => model.modelID === modelFilter)) {
        return false;
      }
      if (
        keyword &&
        !(
          channel.channelName.toLowerCase().includes(keyword) ||
          channel.models.some((model) => model.modelID.toLowerCase().includes(keyword))
        )
      ) {
        return false;
      }
      return true;
    });
  }, [channels, channelStatusFilter, statusFilter, modelFilter, debouncedQuery]);

  const allModels = useMemo(
    () =>
      Array.from(
        new Set([...(policy?.availableModels ?? []), ...channels.flatMap((channel) => channel.models.map((model) => model.modelID))])
      ).sort(),
    [channels, policy?.availableModels]
  );

  const showProblemBanner = () => {
    setStatusFilter('problem');
  };

  return (
    <div className='flex flex-1 flex-col overflow-hidden'>
      <Header fixed>
        <div className='flex flex-1 items-center justify-between gap-4'>
          <div className='min-w-0'>
            <h2 className='truncate text-xl font-bold tracking-tight'>
              {t('channelHealth.title')}
              <span className='text-muted-foreground ml-2 text-sm font-medium'>{t('channelHealth.subtitle')}</span>
            </h2>
            <Link to='/channels' className='text-primary text-sm hover:underline'>
              {t('channelHealth.viewRuntimeHealth')}
              <ExternalLink className='ml-0.5 inline size-3' />
            </Link>
          </div>
          <div className='flex shrink-0 items-center gap-2'>
            {canWrite ? null : (
              <Badge variant='outline' className='gap-1'>
                <Eye className='size-3' />
                {t('channelHealth.readOnly')}
              </Badge>
            )}
            <LastUpdated dataUpdatedAt={overview.dataUpdatedAt} />
            <Button variant='ghost' size='icon-sm' onClick={() => void overview.refetch()} disabled={overview.isFetching}>
              <RefreshCw className={overview.isFetching ? 'size-4 animate-spin' : 'size-4'} />
            </Button>
          </div>
        </div>
      </Header>
      <Main fixed className='overflow-y-auto'>
        {overview.isLoading ? (
          <div className='space-y-3'>
            <div className='grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-3'>
              {Array.from({ length: 6 }).map((_, index) => (
                <Skeleton key={index} className='h-[72px] rounded-xl' />
              ))}
            </div>
            <Skeleton className='h-11 rounded-xl' />
            <Skeleton className='h-96 rounded-xl' />
          </div>
        ) : overview.error || !policy ? (
          <Alert variant='destructive'>
            <AlertTriangle className='size-4' />
            <AlertDescription className='flex items-center gap-3'>
              {overview.error?.message ?? t('channelHealth.messages.loadFailed')}
              <Button variant='outline' size='sm' onClick={() => void overview.refetch()}>
                {t('common.buttons.retry')}
              </Button>
            </AlertDescription>
          </Alert>
        ) : (
          <div className='space-y-4'>
            {!policy.enabled ? (
              <Alert className='border-primary/30 bg-primary/5 [&>svg]:text-primary'>
                <Sparkles className='size-4' />
                <AlertTitle>{t('channelHealth.emptyState.title')}</AlertTitle>
                <AlertDescription className='flex flex-wrap items-center gap-3'>
                  <span>{t('channelHealth.emptyState.description')}</span>
                  <Button size='sm' className='ml-auto' onClick={() => setSettingsOpen(true)} disabled={!canWrite}>
                    <Settings2 className='size-3.5' />
                    {t('channelHealth.emptyState.action')}
                  </Button>
                </AlertDescription>
              </Alert>
            ) : null}

            {problemCounts.abnormal + problemCounts.error > 0 ? (
              <Alert variant='destructive'>
                <AlertTriangle className='size-4' />
                <AlertDescription className='flex flex-wrap items-center gap-2'>
                  <span>
                    {t('channelHealth.alert.title', { count: problemCounts.abnormal + problemCounts.error })}
                    <span className='text-muted-foreground ml-1.5 text-xs'>
                      {t('channelHealth.alert.detail', { abnormal: problemCounts.abnormal, error: problemCounts.error })}
                    </span>
                  </span>
                  <Button variant='outline' size='sm' className='ml-auto' onClick={showProblemBanner}>
                    {t('channelHealth.alert.action')}
                  </Button>
                </AlertDescription>
              </Alert>
            ) : null}

            <KpiCards
              channels={channels}
              active={statusFilter}
              onSelect={(filter) => {
                setStatusFilter(filter);
              }}
            />

            <PolicySummaryBar policy={policy} modelSettings={modelSettings} canWrite={canWrite} onEdit={() => setSettingsOpen(true)} />

            <Tabs defaultValue='matrix'>
              <TabsList>
                <TabsTrigger value='matrix'>{t('channelHealth.tabs.matrix')}</TabsTrigger>
                <TabsTrigger value='history'>{t('channelHealth.tabs.history')}</TabsTrigger>
              </TabsList>
              <TabsContent value='matrix' className='space-y-3 pt-3'>
                <div className='flex flex-wrap items-center gap-2'>
                  <div className='relative w-56'>
                    <Search className='text-muted-foreground absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2' />
                    <Input
                      value={query}
                      onChange={(event) => setQuery(event.target.value)}
                      placeholder={t('channelHealth.filters.search')}
                      className='pl-8'
                    />
                  </div>
                  <Select value={statusFilter} onValueChange={(value) => setStatusFilter(value as KpiFilter)}>
                    <SelectTrigger className='w-44'>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value='all'>{t('channelHealth.filters.allStates')}</SelectItem>
                      {GRADE_ORDER.map((grade) => (
                        <SelectItem key={grade} value={grade}>
                          {t(`channelHealth.status.${grade}`)}
                        </SelectItem>
                      ))}
                      <SelectItem value='problem'>{t('channelHealth.filters.problemStates')}</SelectItem>
                      <SelectItem value='other'>{t('channelHealth.filters.otherStates')}</SelectItem>
                    </SelectContent>
                  </Select>
                  <Select value={modelFilter} onValueChange={setModelFilter}>
                    <SelectTrigger className='w-44'>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value='all'>{t('channelHealth.filters.allModels')}</SelectItem>
                      {allModels.map((model) => (
                        <SelectItem key={model} value={model}>
                          {model}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <Select
                    value={channelStatusFilter}
                    onValueChange={(value) => setChannelStatusFilter(value as 'enabled' | 'all' | 'disabled')}
                  >
                    <SelectTrigger className='w-36'>
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value='enabled'>{t('channelHealth.filters.channelEnabled')}</SelectItem>
                      <SelectItem value='all'>{t('channelHealth.filters.channelAll')}</SelectItem>
                      <SelectItem value='disabled'>{t('channelHealth.filters.channelDisabled')}</SelectItem>
                    </SelectContent>
                  </Select>
                  <span className='text-muted-foreground ml-auto text-xs'>
                    {t('channelHealth.filters.resultCount', { shown: filteredChannels.length, total: channels.length })}
                  </span>
                </div>

                <ChannelMatrixTable
                  channels={filteredChannels}
                  canWrite={canWrite}
                  onOpenDetail={(channel) => setDetailChannelID(channel.channelID)}
                  probeActions={probeActions}
                />
              </TabsContent>
              <TabsContent value='history' className='pt-3'>
                <HistoryPanel channels={channels} />
              </TabsContent>
            </Tabs>
          </div>
        )}
      </Main>

      <ChannelDetailSheet
        channel={detailChannel}
        open={detailChannel != null}
        onOpenChange={(open) => {
          if (!open) {
            setDetailChannelID(null);
          }
        }}
        thresholdMs={routingCeilingMs}
        onProbeAll={(channel) => void probeActions.probeChannel(channel)}
        probing={detailChannel != null && probeActions.isChannelProbing(detailChannel.channelID)}
        canWrite={canWrite}
      />
      {policy ? <ProbeSettingsSheet open={settingsOpen} onOpenChange={setSettingsOpen} policy={policy} /> : null}
    </div>
  );
}
