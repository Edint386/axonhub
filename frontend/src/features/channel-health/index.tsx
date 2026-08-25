import { useEffect, useMemo, useState } from 'react';
import { Link } from '@tanstack/react-router';
import { AlertTriangle, ExternalLink, Eye, RefreshCw, Search } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { useDebounce } from '@/hooks/use-debounce';
import { usePermissions } from '@/hooks/usePermissions';
import { Alert, AlertDescription } from '@/components/ui/alert';
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

function useLastUpdated(dataUpdatedAt: number) {
  const { t } = useTranslation();
  const [, setTick] = useState(0);
  useEffect(() => {
    const timer = setInterval(() => setTick((value) => value + 1), 5000);
    return () => clearInterval(timer);
  }, []);
  if (!dataUpdatedAt) {
    return '';
  }
  const seconds = Math.max(0, Math.floor((Date.now() - dataUpdatedAt) / 1000));
  if (seconds < 10) {
    return t('channelHealth.lastUpdated.now');
  }
  if (seconds < 60) {
    return t('channelHealth.lastUpdated.seconds', { seconds });
  }
  return t('channelHealth.lastUpdated.minutes', { minutes: Math.floor(seconds / 60) });
}

function GradeLegend() {
  const { t } = useTranslation();
  const items: { key: ChannelGrade | 'skipped'; color: string }[] = [
    { key: 'health', color: 'var(--grade-health)' },
    { key: 'fluent', color: 'var(--grade-fluent)' },
    { key: 'degraded', color: 'var(--grade-degraded)' },
    { key: 'abnormal', color: 'var(--grade-abnormal)' },
    { key: 'error', color: 'var(--grade-error)' },
    { key: 'skipped', color: 'var(--muted)' },
  ];
  return (
    <div className='text-muted-foreground flex flex-wrap items-center gap-x-4 gap-y-1 text-xs'>
      {items.map((item) => (
        <span key={item.key} className='inline-flex items-center gap-1.5'>
          <span className='size-2 rounded-[3px]' style={{ background: item.color }} />
          {t(`channelHealth.status.${item.key}`)}
        </span>
      ))}
    </div>
  );
}

export default function ChannelHealthPage() {
  const { t } = useTranslation();
  const { channelPermissions } = usePermissions();
  const overview = useChannelHealthProbeOverview();
  const policy = overview.data?.policy;
  const thresholdMs = policy?.acceptableLatencyMs ?? 60_000;
  const canWrite = channelPermissions.canWrite;
  const channels = useMemo(() => overview.data?.channels ?? [], [overview.data?.channels]);
  const modelSettings = useMemo(
    () =>
      effectiveModelSettings(
        policy ?? { enabled: false, acceptableLatencyMs: 60_000, extraChannels: 1, p95LookbackHours: 24, availableModels: [], models: [] }
      ),
    [policy]
  );

  const probeActions = useProbeActions();
  const [detailChannelID, setDetailChannelID] = useState<string | null>(null);
  const [settingsOpen, setSettingsOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [statusFilter, setStatusFilter] = useState<KpiFilter>('all');
  const [modelFilter, setModelFilter] = useState('all');
  const [probeEnabledFilter, setProbeEnabledFilter] = useState<'enabled' | 'all' | 'disabled'>('enabled');
  const debouncedQuery = useDebounce(query, 300);
  const lastUpdatedText = useLastUpdated(overview.dataUpdatedAt);

  const detailChannel = useMemo(
    () => channels.find((channel) => channel.channelID === detailChannelID) ?? null,
    [channels, detailChannelID]
  );
  const problemCounts = useMemo(() => {
    let abnormal = 0;
    let error = 0;
    for (const channel of channels) {
      const grade = gradeOfChannel(channel, thresholdMs);
      if (grade === 'abnormal') {
        abnormal++;
      } else if (grade === 'error') {
        error++;
      }
    }
    return { abnormal, error };
  }, [channels, thresholdMs]);

  const filteredChannels = useMemo(() => {
    const keyword = debouncedQuery.trim().toLowerCase();
    return channels.filter((channel) => {
      const grade = gradeOfChannel(channel, thresholdMs);
      if (probeEnabledFilter === 'enabled' && !channel.enabled) {
        return false;
      }
      if (probeEnabledFilter === 'disabled' && channel.enabled) {
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
  }, [channels, thresholdMs, probeEnabledFilter, statusFilter, modelFilter, debouncedQuery]);

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
            {lastUpdatedText ? <span className='text-muted-foreground text-xs'>{lastUpdatedText}</span> : null}
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
              thresholdMs={thresholdMs}
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
                    value={probeEnabledFilter}
                    onValueChange={(value) => setProbeEnabledFilter(value as 'enabled' | 'all' | 'disabled')}
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

                <GradeLegend />

                <ChannelMatrixTable
                  channels={filteredChannels}
                  thresholdMs={thresholdMs}
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
        thresholdMs={thresholdMs}
        onProbeAll={(channel) => void probeActions.probeChannel(channel)}
        probing={detailChannel != null && probeActions.isChannelProbing(detailChannel.channelID)}
        canWrite={canWrite}
      />
      {policy ? <ProbeSettingsSheet open={settingsOpen} onOpenChange={setSettingsOpen} policy={policy} /> : null}
    </div>
  );
}
