import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, CheckCircle2, Info, Loader2, Play, Square, XCircle } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useSelectedProjectId } from '@/stores/projectStore';
import { ensureFreshAccessToken } from '@/lib/auth-session';
import { cn } from '@/lib/utils';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Progress } from '@/components/ui/progress';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { useAllChannelSummarys } from '@/features/channels/data/channels';
import type { PlaygroundModelSource } from '../selection';
import { generateChallenges } from './challenges';
import { analyzeGlobalOutputs, type FingerprintBank, type FingerprintResult } from './fingerprint-core';
import { findBankModel, judgeClaim, type ClaimVerdict } from './model-match';
import { MAX_RATE_LIMIT_RETRIES, runProbes, type ProbeAttempt, type ProbeStatus } from './runner';
import { sendPlaygroundProbe } from './transport';
import bankUrl from './unified_bank.json?url';

export type PlaygroundMode = 'chat' | 'trace';

// Up to 3 answers are scored; failed or too-short answers are replaced, 6 requests at most.
const PARALLEL_REQUESTS = 3;
const TARGET_ANSWERS = 3;
const MAX_REQUESTS = 6;
const HISTORY_LIMIT = 10;
const LOW_CONFIDENCE = 0.6;
const TOP_CANDIDATES = 5;

let bankPromise: Promise<FingerprintBank> | undefined;

// The bank is ~550 KB, so it is fetched only when the check view is first opened.
function loadBank(): Promise<FingerprintBank> {
  bankPromise ??= fetch(bankUrl)
    .then(async (response) => {
      if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
      return (await response.json()) as FingerprintBank;
    })
    .catch((error: unknown) => {
      bankPromise = undefined;
      throw error;
    });
  return bankPromise;
}

interface TraceTarget {
  modelSource: PlaygroundModelSource;
  channelId?: string;
  channelName?: string;
  model: string;
  // The upstream model the channel maps `model` to, when it differs.
  actualModel?: string;
}

interface TraceRun {
  id: number;
  startedAt: Date;
  target: TraceTarget;
  attempts: ProbeAttempt[];
  status: 'running' | 'done' | 'cancelled';
  result?: FingerprintResult;
  verdict?: ClaimVerdict;
  noValidAnswers?: boolean;
}

function percent(value: number): string {
  return `${(value * 100).toFixed(1)}%`;
}

function channelLabel(target: TraceTarget, gatewayLabel: string): string {
  return target.modelSource === 'channel' ? (target.channelName ?? target.channelId ?? '') : gatewayLabel;
}

// The model the request really asks the upstream for: the channel's mapped model when it has one.
function claimedModel(target: TraceTarget): string {
  return target.actualModel ?? target.model;
}

function bankFamilies(bank: FingerprintBank): string {
  return [...new Set(bank.models.map((model) => model.family_name || model.family || model.id))].join(' / ');
}

export function PlaygroundModeSwitch({ value, onValueChange }: { value: PlaygroundMode; onValueChange: (mode: PlaygroundMode) => void }) {
  const { t } = useTranslation();

  return (
    <Tabs value={value} onValueChange={(next) => onValueChange(next as PlaygroundMode)} className='mt-3'>
      <TabsList className='grid w-full grid-cols-2'>
        <TabsTrigger value='chat'>{t('modelTrace.mode.chat')}</TabsTrigger>
        <TabsTrigger value='trace'>{t('modelTrace.mode.trace')}</TabsTrigger>
      </TabsList>
    </Tabs>
  );
}

const statusBadgeVariant: Record<ProbeStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  waiting: 'outline',
  running: 'secondary',
  throttled: 'secondary',
  accepted: 'default',
  rejected: 'destructive',
  failed: 'destructive',
  cancelled: 'outline',
};

function ProbeCard({ attempt }: { attempt: ProbeAttempt }) {
  const { t } = useTranslation();
  const preview = attempt.text.length > 240 ? `…${attempt.text.slice(-240)}` : attempt.text;

  return (
    <div className='border-border flex min-w-0 flex-col gap-2 rounded-xl border p-3'>
      <div className='flex items-center justify-between gap-2'>
        <span className='text-sm font-medium'>
          {t('modelTrace.probe.title', { index: attempt.index + 1 })}
          {attempt.index >= PARALLEL_REQUESTS && <span className='text-muted-foreground ml-1 text-xs'>({t('modelTrace.probe.retry')})</span>}
        </span>
        <Badge variant={statusBadgeVariant[attempt.status]} className='gap-1'>
          {(attempt.status === 'running' || attempt.status === 'throttled') && <Loader2 className='size-3 animate-spin' />}
          {t(`modelTrace.probe.status.${attempt.status}`)}
        </Badge>
      </div>
      <p className='text-muted-foreground text-xs'>
        {t('modelTrace.probe.numbers', {
          parsed: attempt.parsedNumbers,
          minimum: attempt.minimumNumbers,
          expected: attempt.challenge.expected_count,
        })}
      </p>
      {preview && <p className='text-muted-foreground line-clamp-3 font-mono text-[11px] break-all'>{preview}</p>}
      {attempt.unfinished && <p className='text-muted-foreground text-xs'>{t('modelTrace.probe.unfinished')}</p>}
      {attempt.error && attempt.status !== 'cancelled' && (
        <p className={cn('text-xs wrap-anywhere', attempt.status === 'throttled' ? 'text-muted-foreground' : 'text-destructive')}>
          {attempt.errorCode === 'incomplete' ? t('modelTrace.errors.incomplete') : attempt.error}
        </p>
      )}
    </div>
  );
}

// `mapped`: the claim is the channel's mapped upstream model rather than the name picked in the sidebar.
function VerdictBanner({ verdict, result, mapped }: { verdict: ClaimVerdict; result: FingerprintResult; mapped: boolean }) {
  const { t } = useTranslation();
  const top = { model: result.prediction_name, probability: percent(result.probability) };
  const subject = mapped ? t('modelTrace.verdict.subjectMapped') : t('modelTrace.verdict.subjectSelected');
  const familyName = (family?: string) =>
    result.family_probabilities.find((item) => item.family === (family || 'models'))?.display_name || family || '';

  const view = (() => {
    switch (verdict.kind) {
      case 'match':
        return result.probability < LOW_CONFIDENCE
          ? { tone: 'warn', Icon: AlertTriangle, text: t('modelTrace.verdict.matchUnsure', { ...top, subject }) }
          : { tone: 'good', Icon: CheckCircle2, text: t('modelTrace.verdict.match', { ...top, subject }) };
      case 'same-family':
        return result.probability < LOW_CONFIDENCE
          ? {
              tone: 'info',
              Icon: Info,
              text: t('modelTrace.verdict.sameFamilyUnsure', { ...top, subject, family: result.family_prediction_name }),
            }
          : {
              tone: 'warn',
              Icon: AlertTriangle,
              text: t('modelTrace.verdict.sameFamily', { ...top, subject, claimed: verdict.claimed.display_name }),
            };
      case 'different-family':
        return {
          tone: 'bad',
          Icon: XCircle,
          text: t('modelTrace.verdict.differentFamily', {
            ...top,
            subject,
            claimed: verdict.claimed.display_name,
            claimedFamily: familyName(verdict.claimed.family),
            family: result.family_prediction_name,
          }),
        };
      case 'uncertain':
        return { tone: 'info', Icon: Info, text: t('modelTrace.verdict.uncertain', top) };
      default:
        return { tone: 'info', Icon: Info, text: t('modelTrace.verdict.notCovered', top) };
    }
  })();

  return (
    <div
      className={cn(
        'flex items-start gap-3 rounded-xl border p-4 text-sm',
        view.tone === 'good' && 'border-emerald-500/40 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300',
        view.tone === 'warn' && 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-300',
        view.tone === 'bad' && 'border-destructive/40 bg-destructive/10 text-destructive',
        view.tone === 'info' && 'border-border bg-muted/50'
      )}
    >
      <view.Icon className='mt-0.5 size-5 shrink-0' />
      <p className='font-medium'>{view.text}</p>
    </div>
  );
}

function ProbabilityRow({ label, value, highlight }: { label: string; value: number; highlight?: boolean }) {
  return (
    <div className='grid grid-cols-[minmax(0,10rem)_1fr_3.5rem] items-center gap-3 text-xs'>
      <span className={cn('truncate', highlight && 'font-semibold')} title={label}>
        {label}
      </span>
      <div className='bg-muted h-2 overflow-hidden rounded-full'>
        <div className={cn('h-full rounded-full', highlight ? 'bg-primary' : 'bg-primary/40')} style={{ width: `${Math.max(value * 100, 0.5)}%` }} />
      </div>
      <span className='text-right tabular-nums'>{percent(value)}</span>
    </div>
  );
}

function ResultView({ run, bank }: { run: TraceRun; bank?: FingerprintBank }) {
  const { t } = useTranslation();
  const [showAll, setShowAll] = useState(false);
  const { result, verdict } = run;
  if (!result || !verdict) return null;
  const candidates = showAll ? result.results : result.results.slice(0, TOP_CANDIDATES);

  return (
    <div className='space-y-4'>
      <VerdictBanner verdict={verdict} result={result} mapped={!!run.target.actualModel} />
      {result.probability < LOW_CONFIDENCE && verdict.kind !== 'match' && verdict.kind !== 'same-family' && (
        <p className='text-muted-foreground text-xs'>{t('modelTrace.result.lowConfidence')}</p>
      )}
      {result.used_outputs < TARGET_ANSWERS && (
        <p className='text-muted-foreground text-xs'>{t('modelTrace.result.partial', { used: result.used_outputs, target: TARGET_ANSWERS })}</p>
      )}

      <div className='space-y-2'>
        <h4 className='text-xs font-semibold'>{t('modelTrace.result.family')}</h4>
        {result.family_probabilities.map((family) => (
          <ProbabilityRow
            key={family.family}
            label={family.display_name}
            value={family.probability}
            highlight={family.family === result.family_prediction}
          />
        ))}
      </div>

      <div className='space-y-2'>
        <h4 className='text-xs font-semibold'>{t('modelTrace.result.candidates')}</h4>
        {candidates.map((item) => (
          <ProbabilityRow key={item.model} label={item.display_name} value={item.probability} highlight={item.model === result.prediction} />
        ))}
        {result.results.length > TOP_CANDIDATES && (
          <Button variant='link' size='sm' className='h-auto px-0 text-xs' onClick={() => setShowAll((value) => !value)}>
            {showAll ? t('modelTrace.result.showLess') : t('modelTrace.result.showAll', { count: result.results.length })}
          </Button>
        )}
      </div>

      <p className='text-muted-foreground text-xs leading-relaxed'>
        {t('modelTrace.result.footer', {
          used: result.used_outputs,
          accuracy: percent(result.calibration.cv_accuracy),
          date: bank?.built_at?.slice(0, 10) ?? '-',
        })}
      </p>
    </div>
  );
}

interface ModelTracePanelProps {
  hidden?: boolean;
  modelSource: PlaygroundModelSource;
  channelId: string;
  model: string;
}

// Runs ModelTrace (https://github.com/xqy2006/ModelTrace) against the channel and model picked in the
// playground sidebar. Requests go through the playground chat endpoint, so they are routed, logged and
// billed like playground chats.
export function ModelTracePanel({ hidden, modelSource, channelId, model }: ModelTracePanelProps) {
  const { t } = useTranslation();
  const projectId = useSelectedProjectId();
  const { data: channelsData } = useAllChannelSummarys(projectId, { enabled: !hidden });
  const [bank, setBank] = useState<FingerprintBank>();
  const [bankError, setBankError] = useState<string>();
  const [runs, setRuns] = useState<TraceRun[]>([]);
  const [activeRunId, setActiveRunId] = useState<number>();
  const controllerRef = useRef<AbortController | null>(null);
  const nextRunIdRef = useRef(1);

  useEffect(() => {
    if (hidden || bank) return;
    let cancelled = false;
    loadBank()
      .then((loaded) => {
        if (cancelled) return;
        setBank(loaded);
        setBankError(undefined);
      })
      .catch((error: unknown) => {
        if (!cancelled) setBankError(error instanceof Error ? error.message : String(error));
      });
    return () => {
      cancelled = true;
    };
  }, [hidden, bank]);

  useEffect(() => () => controllerRef.current?.abort(), []);

  const target = useMemo<TraceTarget | undefined>(() => {
    if (!model) return undefined;
    if (modelSource === 'model_gateway') return { modelSource, model };
    // Wait for the channel list: without it the channel's model mapping is unknown.
    const channel = channelsData?.edges?.find((edge) => edge.node.id === channelId)?.node;
    if (!channel) return undefined;
    const actualModel = channel.allModelEntries?.find((entry) => entry.requestModel === model)?.actualModel;
    return {
      modelSource,
      channelId,
      channelName: channel.name,
      model,
      actualModel: actualModel && actualModel !== model ? actualModel : undefined,
    };
  }, [channelId, channelsData, model, modelSource]);

  const covered = useMemo(() => (bank && target ? !!findBankModel(claimedModel(target), bank.models) : undefined), [bank, target]);

  const running = runs.some((run) => run.status === 'running');
  const activeRun = runs.find((run) => run.id === activeRunId) ?? runs[0];

  const start = useCallback(async () => {
    if (!target || controllerRef.current) return;
    const controller = new AbortController();
    controllerRef.current = controller;
    let loadedBank: FingerprintBank;
    try {
      loadedBank = bank ?? (await loadBank());
      setBank(loadedBank);
      setBankError(undefined);
    } catch (error) {
      controllerRef.current = null;
      toast.error(t('modelTrace.errors.bank', { message: error instanceof Error ? error.message : String(error) }));
      return;
    }

    const id = nextRunIdRef.current;
    nextRunIdRef.current += 1;
    const runTarget = target;
    setRuns((current) => [{ id, startedAt: new Date(), target: runTarget, attempts: [], status: 'running' as const }, ...current].slice(0, HISTORY_LIMIT));
    setActiveRunId(id);
    const update = (patch: Partial<TraceRun>) => setRuns((current) => current.map((run) => (run.id === id ? { ...run, ...patch } : run)));

    // Streaming answers report every delta; render at most once per frame.
    let pending: ProbeAttempt[] | undefined;
    let frame = 0;
    const onChange = (attempts: ProbeAttempt[]) => {
      pending = attempts;
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        if (pending) update({ attempts: pending });
      });
    };

    const attempts = await runProbes({
      challenges: generateChallenges(MAX_REQUESTS),
      concurrency: PARALLEL_REQUESTS,
      target: TARGET_ANSWERS,
      signal: controller.signal,
      onChange,
      send: (prompt, { signal, onText }) =>
        sendPlaygroundProbe({
          prompt,
          target: { projectId, channelId: runTarget.channelId, model: runTarget.model },
          getToken: ensureFreshAccessToken,
          signal,
          onText,
        }),
    });
    if (frame) cancelAnimationFrame(frame);
    controllerRef.current = null;

    const status = controller.signal.aborted ? ('cancelled' as const) : ('done' as const);
    const outputs = attempts
      .filter((attempt) => attempt.status === 'accepted')
      .map((attempt) => ({ text: attempt.text, expected_count: attempt.challenge.expected_count }));
    if (!outputs.length) {
      update({ attempts, status, noValidAnswers: status === 'done' });
      return;
    }
    const result = analyzeGlobalOutputs(outputs, loadedBank);
    update({ attempts, status, result, verdict: judgeClaim(claimedModel(runTarget), result, loadedBank.models) });
  }, [bank, projectId, t, target]);

  const stop = useCallback(() => controllerRef.current?.abort(), []);

  const startedAttempts = activeRun?.attempts.filter((attempt) => attempt.status !== 'waiting') ?? [];
  const acceptedCount = startedAttempts.filter((attempt) => attempt.status === 'accepted').length;
  const rateLimited = startedAttempts.some((attempt) => attempt.rateLimitRetries > 0);
  const rateLimitedOut = startedAttempts.some((attempt) => attempt.status === 'failed' && attempt.rateLimitRetries >= MAX_RATE_LIMIT_RETRIES);

  return (
    <div className={cn('shadow-soft border-border bg-card flex h-full min-h-0 flex-col rounded-2xl border', hidden && 'hidden')}>
      <div className='flex flex-wrap items-start justify-between gap-3 border-b p-6 pb-4'>
        <div className='min-w-0 space-y-1'>
          <h2 className='text-lg font-semibold'>{t('modelTrace.title')}</h2>
          <p className='text-muted-foreground text-xs leading-relaxed'>{t('modelTrace.description')}</p>
        </div>
        {running ? (
          <Button variant='outline' onClick={stop}>
            <Square className='mr-2 size-4' />
            {t('modelTrace.stop')}
          </Button>
        ) : (
          <Button onClick={start} disabled={!target}>
            <Play className='mr-2 size-4' />
            {t('modelTrace.start')}
          </Button>
        )}
      </div>

      <ScrollArea className='min-h-0 flex-1'>
        <div className='space-y-5 p-6'>
          <div className='bg-muted/40 space-y-1 rounded-xl p-3 text-xs'>
            {target ? (
              <>
                <p>
                  <span className='text-muted-foreground'>
                    {modelSource === 'channel' ? t('modelTrace.target.channel') : t('modelTrace.target.gateway')}
                  </span>
                  {modelSource === 'channel' && <span className='ml-2 font-medium'>{target.channelName ?? target.channelId}</span>}
                </p>
                <p>
                  <span className='text-muted-foreground'>{t('modelTrace.target.model')}</span>
                  <span className='ml-2 font-mono break-all'>{target.model}</span>
                  {target.actualModel && (
                    <span className='text-muted-foreground ml-2'>
                      {t('modelTrace.target.actualModel')} <span className='font-mono break-all'>{target.actualModel}</span>
                    </span>
                  )}
                </p>
              </>
            ) : (
              <p className='text-muted-foreground'>{t('modelTrace.target.none')}</p>
            )}
          </div>

          <div className='space-y-2 text-xs'>
            {modelSource === 'model_gateway' && (
              <p className='flex gap-2 text-amber-700 dark:text-amber-300'>
                <AlertTriangle className='mt-0.5 size-3.5 shrink-0' />
                {t('modelTrace.warnings.gateway')}
              </p>
            )}
            {covered === false && bank && target && (
              <p className='flex gap-2 text-amber-700 dark:text-amber-300'>
                <AlertTriangle className='mt-0.5 size-3.5 shrink-0' />
                <span className='min-w-0 wrap-anywhere'>
                  {t('modelTrace.warnings.notCovered', { model: claimedModel(target), count: bank.models.length, families: bankFamilies(bank) })}
                </span>
              </p>
            )}
            {bankError && <p className='text-destructive'>{t('modelTrace.errors.bank', { message: bankError })}</p>}
            <p className='text-muted-foreground flex gap-2'>
              <Info className='mt-0.5 size-3.5 shrink-0' />
              <span className='min-w-0'>{t('modelTrace.notes.request', { parallel: PARALLEL_REQUESTS, max: MAX_REQUESTS, retries: MAX_RATE_LIMIT_RETRIES })}</span>
            </p>
            <p className='text-muted-foreground flex gap-2'>
              <Info className='mt-0.5 size-3.5 shrink-0' />
              {t('modelTrace.notes.bias')}
            </p>
          </div>

          {activeRun ? (
            <div className='space-y-4'>
              <div className='flex flex-wrap items-center justify-between gap-2'>
                <h3 className='min-w-0 text-sm font-semibold wrap-anywhere'>
                  {t('modelTrace.run.title', {
                    time: activeRun.startedAt.toLocaleTimeString(),
                    target: channelLabel(activeRun.target, t('modelTrace.target.gateway')),
                    model: activeRun.target.model,
                  })}
                </h3>
                {activeRun.status === 'running' && (
                  <span className='text-muted-foreground text-xs'>
                    {t('modelTrace.run.progress', { accepted: acceptedCount, target: TARGET_ANSWERS })}
                  </span>
                )}
                {activeRun.status === 'cancelled' && <span className='text-muted-foreground text-xs'>{t('modelTrace.run.cancelled')}</span>}
              </div>
              {activeRun.status === 'running' && <Progress value={(acceptedCount / TARGET_ANSWERS) * 100} />}
              {rateLimitedOut ? (
                <p className='text-destructive text-xs'>{t('modelTrace.run.rateLimitedOut')}</p>
              ) : (
                rateLimited && <p className='text-muted-foreground text-xs'>{t('modelTrace.run.rateLimited')}</p>
              )}

              {startedAttempts.length > 0 && (
                <div className='grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3'>
                  {startedAttempts.map((attempt) => (
                    <ProbeCard key={attempt.challenge.id} attempt={attempt} />
                  ))}
                </div>
              )}

              {activeRun.noValidAnswers && <p className='text-destructive text-sm'>{t('modelTrace.errors.noValid')}</p>}
              <ResultView key={activeRun.id} run={activeRun} bank={bank} />
            </div>
          ) : (
            <p className='text-muted-foreground py-8 text-center text-sm'>{t('modelTrace.empty')}</p>
          )}

          {runs.length > 1 && (
            <div className='space-y-2 border-t pt-4'>
              <h3 className='text-xs font-semibold'>{t('modelTrace.history.title')}</h3>
              <div className='space-y-1'>
                {runs.map((run) => (
                  <button
                    key={run.id}
                    type='button'
                    onClick={() => setActiveRunId(run.id)}
                    className={cn(
                      'hover:bg-muted flex w-full flex-wrap items-center gap-x-2 rounded-lg px-2 py-1.5 text-left text-xs',
                      run.id === activeRun?.id && 'bg-muted'
                    )}
                  >
                    <span className='text-muted-foreground tabular-nums'>{run.startedAt.toLocaleTimeString()}</span>
                    <span className='font-medium'>{channelLabel(run.target, t('modelTrace.target.gateway'))}</span>
                    <span className='min-w-0 font-mono wrap-anywhere'>{run.target.model}</span>
                    <span className='text-muted-foreground'>→</span>
                    <span>
                      {run.result
                        ? `${run.result.prediction_name} ${percent(run.result.probability)}`
                        : t(`modelTrace.history.status.${run.status}`)}
                    </span>
                  </button>
                ))}
              </div>
            </div>
          )}

          <p className='text-muted-foreground text-[11px]'>
            {t('modelTrace.credit')}{' '}
            <a className='underline' href='https://github.com/xqy2006/ModelTrace' target='_blank' rel='noreferrer'>
              ModelTrace
            </a>{' '}
            (MIT)
          </p>
        </div>
      </ScrollArea>
    </div>
  );
}
