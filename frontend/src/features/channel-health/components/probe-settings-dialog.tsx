import { useEffect, useMemo, useState } from 'react';
import { DndContext, closestCenter, KeyboardSensor, PointerSensor, useSensor, useSensors, type DragEndEvent } from '@dnd-kit/core';
import { arrayMove, SortableContext, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { GripVertical, Loader2, Settings2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Switch } from '@/components/ui/switch';
import {
  useUpdateChannelHealthProbePolicy,
  type ActiveHealthProbeModelSetting,
  type ChannelHealthProbePolicy,
} from '../data/channel-health';

function SortableModelRow({
  model,
  disabled,
  onUpdate,
}: {
  model: ActiveHealthProbeModelSetting;
  disabled: boolean;
  onUpdate: (patch: Partial<ActiveHealthProbeModelSetting>) => void;
}) {
  const { t } = useTranslation();
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: model.modelID });

  return (
    <div
      ref={setNodeRef}
      style={{ transform: CSS.Transform.toString(transform), transition }}
      className={cn('bg-background flex items-center gap-2 rounded-lg border px-2 py-2', isDragging && 'z-10 shadow-lg')}
    >
      <button
        type='button'
        {...attributes}
        {...listeners}
        disabled={disabled}
        className='text-muted-foreground hover:text-foreground cursor-grab touch-none p-1 active:cursor-grabbing disabled:cursor-not-allowed disabled:opacity-50'
        aria-label={t('channelHealth.settings.dragModel', { model: model.modelID })}
      >
        <GripVertical className='size-4' />
      </button>
      <span className='min-w-0 flex-1 truncate font-mono text-xs'>{model.modelID}</span>
      <span className='flex w-10 justify-center'>
        <Switch
          checked={model.enabled}
          onCheckedChange={(checked) => onUpdate({ enabled: checked })}
          disabled={disabled}
          aria-label={t('channelHealth.settings.enableModel', { model: model.modelID })}
        />
      </span>
      <span className='flex w-10 justify-center'></span>
    </div>
  );
}

/**
 * Union of the saved model list (in saved order, first) followed by every
 * remaining available model appended. A model not yet saved defaults to
 * probe=OFF — probing costs money, so a newly-appeared upstream
 * model must not start spending on its own.
 */
function syncedModels(policy: ChannelHealthProbePolicy): ActiveHealthProbeModelSetting[] {
  const appended = policy.availableModels.filter((modelID) => !policy.models.some((model) => model.modelID === modelID));
  return [...policy.models, ...appended.map((modelID) => ({ modelID, enabled: false }))];
}

/**
 * Probe policy editor.
 *
 * A centered Dialog rather than a side Sheet: across this app a Sheet is a
 * read-only viewer (request body, trace, test history) while every settings
 * surface that WRITES configuration is a Dialog — see the channels and models
 * settings dialogs this one is modelled on.
 */
export function ProbeSettingsDialog({
  open,
  onOpenChange,
  policy,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  policy: ChannelHealthProbePolicy;
}) {
  const { t } = useTranslation();
  const updatePolicy = useUpdateChannelHealthProbePolicy();
  const [enabled, setEnabled] = useState(policy.enabled);
  const [intervalMinutes, setIntervalMinutes] = useState(String(policy.intervalMinutes));
  const [stream, setStream] = useState(policy.stream);
  const [latencySeconds, setLatencySeconds] = useState(String(policy.acceptableLatencyMs / 1000));
  const [extraChannels, setExtraChannels] = useState(String(policy.extraChannels));
  const [p95LookbackHours, setP95LookbackHours] = useState(String(policy.p95LookbackHours));
  const [gateWindowMinutes, setGateWindowMinutes] = useState(String(policy.gateWindowMinutes));
  const [models, setModels] = useState<ActiveHealthProbeModelSetting[]>(() => syncedModels(policy));

  const initialModels = useMemo(() => syncedModels(policy), [policy]);

  useEffect(() => {
    if (!open) {
      return;
    }
    setEnabled(policy.enabled);
    setIntervalMinutes(String(policy.intervalMinutes));
    setStream(policy.stream);
    setLatencySeconds(String(policy.acceptableLatencyMs / 1000));
    setExtraChannels(String(policy.extraChannels));
    setP95LookbackHours(String(policy.p95LookbackHours));
    setGateWindowMinutes(String(policy.gateWindowMinutes));
    setModels(initialModels);
  }, [open, policy, initialModels]);

  const parsedIntervalMinutes = Number(intervalMinutes);
  const parsedLatencySeconds = Number(latencySeconds);
  const parsedExtraChannels = Number(extraChannels);
  const parsedP95LookbackHours = Number(p95LookbackHours);
  const parsedGateWindowMinutes = Number(gateWindowMinutes);
  const acceptableLatencyMs = Math.round(parsedLatencySeconds * 1000);
  const hasDuplicateModels = new Set(models.map((model) => model.modelID)).size !== models.length;

  /**
   * A gate window shorter than interval x 3 holds fewer than the ceiling's minimum
   * sample count, so the statistic reads UNKNOWN and the ceiling silently passes
   * every channel. Not an error -- a shorter window is a legitimate choice when real
   * traffic supplies the samples -- so this warns rather than blocking the save.
   */
  const minimumUsefulGateWindow = Number.isInteger(parsedIntervalMinutes) && parsedIntervalMinutes >= 1 ? parsedIntervalMinutes * 3 : 0;
  const gateWindowTooShortForProbes =
    Number.isInteger(parsedGateWindowMinutes) && parsedGateWindowMinutes >= 1 && parsedGateWindowMinutes < minimumUsefulGateWindow;

  const isValid =
    Number.isInteger(parsedIntervalMinutes) &&
    parsedIntervalMinutes >= 1 &&
    parsedIntervalMinutes <= 1440 &&
    Number.isFinite(parsedLatencySeconds) &&
    parsedLatencySeconds > 0 &&
    acceptableLatencyMs >= 1 &&
    acceptableLatencyMs <= 600_000 &&
    Number.isInteger(parsedExtraChannels) &&
    parsedExtraChannels >= 0 &&
    parsedExtraChannels <= 20 &&
    Number.isInteger(parsedP95LookbackHours) &&
    parsedP95LookbackHours >= 1 &&
    parsedP95LookbackHours <= 720 &&
    Number.isInteger(parsedGateWindowMinutes) &&
    parsedGateWindowMinutes >= 1 &&
    parsedGateWindowMinutes <= 1440 &&
    !hasDuplicateModels &&
    (!enabled || models.some((model) => model.enabled));

  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates })
  );

  const handleDragEnd = ({ active, over }: DragEndEvent) => {
    if (!over || active.id === over.id) {
      return;
    }
    setModels((current) => {
      const oldIndex = current.findIndex((model) => model.modelID === active.id);
      const newIndex = current.findIndex((model) => model.modelID === over.id);
      return oldIndex < 0 || newIndex < 0 ? current : arrayMove(current, oldIndex, newIndex);
    });
  };

  const handleSave = () => {
    if (!isValid) {
      return;
    }
    updatePolicy.mutate(
      {
        enabled,
        intervalMinutes: parsedIntervalMinutes,
        stream,
        acceptableLatencyMs,
        extraChannels: parsedExtraChannels,
        p95LookbackHours: parsedP95LookbackHours,
        gateWindowMinutes: parsedGateWindowMinutes,
        models,
      },
      {
        onSuccess: () => {
          toast.success(t('channelHealth.messages.policyUpdated'));
          onOpenChange(false);
        },
        onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updatePolicyFailed')),
      }
    );
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[90vh] w-full max-w-full flex-col overflow-hidden sm:max-w-[720px]'>
        <DialogHeader className='shrink-0'>
          <DialogTitle className='flex items-center gap-2'>
            <Settings2 className='size-5' />
            {t('channelHealth.settings.title')}
          </DialogTitle>
          <DialogDescription>{t('channelHealth.settings.description')}</DialogDescription>
        </DialogHeader>

        <div className='min-h-0 flex-1 space-y-6 overflow-y-auto pr-1'>
          <div className='flex items-center justify-between gap-3'>
            <span className='text-sm font-medium'>{t('channelHealth.settings.masterSwitch')}</span>
            <Switch checked={enabled} onCheckedChange={setEnabled} disabled={updatePolicy.isPending} />
          </div>

          <div className='flex items-center justify-between gap-3'>
            <span className='text-sm font-medium'>{t('channelHealth.settings.stream')}</span>
            <Switch checked={stream} onCheckedChange={setStream} disabled={updatePolicy.isPending} />
          </div>

          <div className='space-y-2'>
            <label htmlFor='probe-interval' className='text-sm font-medium'>
              {t('channelHealth.settings.interval')}
            </label>
            <div className='flex items-center gap-2'>
              <Input
                id='probe-interval'
                type='number'
                min='1'
                max='1440'
                step='1'
                className='w-36'
                value={intervalMinutes}
                onChange={(event) => setIntervalMinutes(event.target.value)}
                disabled={updatePolicy.isPending}
              />
              <span className='text-muted-foreground text-sm'>{t('channelHealth.settings.intervalUnit')}</span>
            </div>
            <p className='text-muted-foreground text-xs'>{t('channelHealth.settings.intervalDescription')}</p>
          </div>

          <div className='space-y-2'>
            <label htmlFor='probe-gate-window' className='text-sm font-medium'>
              {t('channelHealth.settings.gateWindow')}
            </label>
            <div className='flex items-center gap-2'>
              <Input
                id='probe-gate-window'
                type='number'
                min='1'
                max='1440'
                step='1'
                className='w-36'
                value={gateWindowMinutes}
                onChange={(event) => setGateWindowMinutes(event.target.value)}
                disabled={updatePolicy.isPending}
              />
              <span className='text-muted-foreground text-sm'>{t('channelHealth.settings.gateWindowUnit')}</span>
            </div>
            <p className='text-muted-foreground text-xs'>{t('channelHealth.settings.gateWindowDescription')}</p>
            {gateWindowTooShortForProbes ? (
              <p className='text-[var(--grade-degraded)] text-xs'>
                {t('channelHealth.settings.gateWindowTooShort', {
                  minutes: minimumUsefulGateWindow,
                  interval: parsedIntervalMinutes,
                })}
              </p>
            ) : null}
          </div>

          <div className='space-y-2'>
            <label htmlFor='probe-p95-lookback' className='text-sm font-medium'>
              {t('channelHealth.settings.p95Lookback')}
            </label>
            <div className='flex items-center gap-2'>
              <Input
                id='probe-p95-lookback'
                type='number'
                min='1'
                max='720'
                step='1'
                className='w-36'
                value={p95LookbackHours}
                onChange={(event) => setP95LookbackHours(event.target.value)}
                disabled={updatePolicy.isPending}
              />
              <span className='text-muted-foreground text-sm'>{t('channelHealth.settings.p95LookbackUnit')}</span>
            </div>
          </div>

          <div className='space-y-2'>
            <label htmlFor='probe-latency' className='text-sm font-medium'>
              {t('channelHealth.settings.acceptableLatency')}
            </label>
            <div className='flex items-center gap-2'>
              <Input
                id='probe-latency'
                type='number'
                min='0.001'
                max='600'
                step='0.5'
                className='w-36'
                value={latencySeconds}
                onChange={(event) => setLatencySeconds(event.target.value)}
                disabled={updatePolicy.isPending}
              />
              <span className='text-muted-foreground text-sm'>{t('channelHealth.settings.latencyUnit')}</span>
            </div>
          </div>

          <div className='space-y-2'>
            <label htmlFor='probe-extra-channels' className='text-sm font-medium'>
              {t('channelHealth.settings.extraChannels')}
            </label>
            <div className='flex items-center gap-2'>
              <Input
                id='probe-extra-channels'
                type='number'
                min='0'
                max='20'
                step='1'
                className='w-36'
                value={extraChannels}
                onChange={(event) => setExtraChannels(event.target.value)}
                disabled={updatePolicy.isPending}
              />
              <span className='text-muted-foreground text-sm'>{t('channelHealth.settings.extraChannelsUnit')}</span>
            </div>
          </div>

          <div className='space-y-3'>
            <span className='text-sm font-medium'>{t('channelHealth.settings.models')}</span>
            {models.length > 0 ? (
              <>
                <div className='text-muted-foreground flex items-center gap-2 px-2 text-[11px] font-semibold tracking-wider uppercase'>
                  <span className='w-6' />
                  <span className='min-w-0 flex-1'>{t('channelHealth.settings.modelColumn')}</span>
                  <span className='w-10 text-center'>{t('channelHealth.settings.probeColumn')}</span>
                </div>
                <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={handleDragEnd}>
                  <SortableContext items={models.map((model) => model.modelID)} strategy={verticalListSortingStrategy}>
                    <div className='space-y-2'>
                      {models.map((model) => (
                        <SortableModelRow
                          key={model.modelID}
                          model={model}
                          disabled={updatePolicy.isPending}
                          onUpdate={(patch) =>
                            setModels((current) => current.map((item) => (item.modelID === model.modelID ? { ...item, ...patch } : item)))
                          }
                        />
                      ))}
                    </div>
                  </SortableContext>
                </DndContext>
              </>
            ) : (
              <div className='text-muted-foreground rounded-lg border border-dashed py-8 text-center text-sm'>
                {t('channelHealth.settings.noModels')}
              </div>
            )}
          </div>
        </div>

        <DialogFooter className='shrink-0'>
          <Button variant='outline' onClick={() => onOpenChange(false)} disabled={updatePolicy.isPending}>
            {t('common.buttons.cancel')}
          </Button>
          <Button onClick={handleSave} disabled={!isValid || updatePolicy.isPending}>
            {updatePolicy.isPending ? <Loader2 className='size-4 animate-spin' /> : null}
            {t('channelHealth.settings.save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
