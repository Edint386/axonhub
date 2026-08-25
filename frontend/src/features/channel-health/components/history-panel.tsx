import { useEffect, useMemo, useState } from 'react';
import { ChevronLeft, ChevronRight, Loader2, RotateCcw, SearchX } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { formatDuration } from '@/utils/format-duration';
import { useDebounce } from '@/hooks/use-debounce';
import { Button } from '@/components/ui/button';
import { CopyButton } from '@/components/ui/copy-button';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import {
  useChannelHealthProbeHistory,
  type ActiveChannelHealthProbeRun,
  type ChannelHealthProbeChannel,
  type ChannelHealthProbeHistoryInput,
} from '../data/channel-health';
import { ProbeStatusChip } from './probe-status-chip';

const historyPageSize = 50;

function formatCheckedAt(value?: string | null) {
  if (!value) {
    return '-';
  }
  return new Date(value).toLocaleString();
}

function HistoryRow({ run, channels }: { run: ActiveChannelHealthProbeRun; channels: ChannelHealthProbeChannel[] }) {
  const { t } = useTranslation();
  const channelName = channels.find((channel) => channel.channelID === run.channelID)?.channelName ?? run.channelID;
  const firstToken = run.stream ? (run.ttftMs ?? run.ttfbMs) : (run.ttfbMs ?? run.ttftMs);
  return (
    <TableRow>
      <TableCell className='text-muted-foreground text-xs whitespace-nowrap'>{formatCheckedAt(run.completedAt ?? run.startedAt)}</TableCell>
      <TableCell className='max-w-44 truncate'>{channelName}</TableCell>
      <TableCell className='max-w-52 truncate font-mono text-xs'>{run.modelID}</TableCell>
      <TableCell>
        <span className='bg-secondary text-secondary-foreground rounded px-1.5 py-0.5 text-[10.5px]'>
          {run.source === 'scheduled' ? t('channelHealth.source.scheduled') : t('channelHealth.source.manual')}
        </span>
      </TableCell>
      <TableCell>
        <ProbeStatusChip status={run.status} />
      </TableCell>
      <TableCell className='font-mono text-xs tabular-nums'>
        {firstToken != null ? formatDuration(firstToken) : '-'}
        <span className='text-muted-foreground ml-1 text-[10px]'>{run.stream ? 'TTFT' : 'TTFB'}</span>
      </TableCell>
      <TableCell className='font-mono text-xs tabular-nums'>{run.totalMs != null ? formatDuration(run.totalMs) : '-'}</TableCell>
      <TableCell>
        {run.errorMessage ? (
          <div className='flex max-w-80 items-center gap-1'>
            <span className='text-destructive truncate text-xs' title={run.errorMessage}>
              {run.errorMessage}
            </span>
            <CopyButton content={run.errorMessage} />
          </div>
        ) : (
          <span className='text-muted-foreground text-xs'>-</span>
        )}
      </TableCell>
    </TableRow>
  );
}

export function HistoryPanel({ channels }: { channels: ChannelHealthProbeChannel[] }) {
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
  const { data, isLoading, error, refetch, isFetching } = useChannelHealthProbeHistory(input);
  const totalCount = data?.totalCount ?? 0;
  const totalPages = Math.max(1, Math.ceil(totalCount / historyPageSize));
  const hasPrevious = page > 0;
  const hasNext = (page + 1) * historyPageSize < totalCount;

  return (
    <div className='space-y-4'>
      <div className='grid gap-2 md:grid-cols-4'>
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
      <div className='bg-card overflow-hidden rounded-xl border'>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t('channelHealth.columns.checkedAt')}</TableHead>
              <TableHead>{t('channelHealth.columns.channel')}</TableHead>
              <TableHead>{t('channelHealth.columns.model')}</TableHead>
              <TableHead>{t('channelHealth.columns.source')}</TableHead>
              <TableHead>{t('channelHealth.columns.status')}</TableHead>
              <TableHead>{t('channelHealth.columns.firstToken')}</TableHead>
              <TableHead>{t('channelHealth.columns.total')}</TableHead>
              <TableHead>{t('channelHealth.columns.error')}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading ? (
              <TableRow>
                <TableCell colSpan={8}>
                  <div className='flex justify-center py-8'>
                    <Loader2 className='size-5 animate-spin' />
                  </div>
                </TableCell>
              </TableRow>
            ) : error ? (
              <TableRow>
                <TableCell colSpan={8}>
                  <div className='flex items-center justify-center gap-2 py-8'>
                    <span className='text-destructive text-sm'>{error.message}</span>
                    <Button variant='outline' size='sm' onClick={() => void refetch()} disabled={isFetching}>
                      <RotateCcw className='size-3.5' />
                      {t('common.buttons.retry')}
                    </Button>
                  </div>
                </TableCell>
              </TableRow>
            ) : data?.items.length ? (
              data.items.map((run) => <HistoryRow key={run.id} run={run} channels={channels} />)
            ) : (
              <TableRow>
                <TableCell colSpan={8}>
                  <div className='text-muted-foreground flex flex-col items-center gap-2 py-10 text-sm'>
                    <SearchX className='size-6 opacity-40' />
                    {t('channelHealth.emptyHistory')}
                  </div>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
        <div className='flex items-center justify-between border-t px-4 py-2.5'>
          <div className='text-muted-foreground text-xs'>
            {t('channelHealth.historyCount', { count: totalCount })}
            <span className='ml-2'>{t('channelHealth.pageIndicator', { page: page + 1, total: totalPages })}</span>
          </div>
          <div className='flex items-center gap-1'>
            <Button variant='ghost' size='icon-sm' onClick={() => setPage((value) => Math.max(0, value - 1))} disabled={!hasPrevious}>
              <ChevronLeft className='size-4' />
            </Button>
            <Button variant='ghost' size='icon-sm' onClick={() => setPage((value) => value + 1)} disabled={!hasNext}>
              <ChevronRight className='size-4' />
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}
