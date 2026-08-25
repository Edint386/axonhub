import { useEffect, useState } from 'react';
import { DndContext, closestCenter, KeyboardSensor, PointerSensor, useSensor, useSensors, type DragEndEvent } from '@dnd-kit/core';
import { arrayMove, SortableContext, sortableKeyboardCoordinates, useSortable, verticalListSortingStrategy } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { GripVertical, Loader2, Plus, Trash2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { formatDuration } from '@/utils/format-duration';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Sheet, SheetContent, SheetHeader, SheetTitle } from '@/components/ui/sheet';
import { Switch } from '@/components/ui/switch';
import {
  useUpdateChannelHealthProbePolicy,
  type ActiveHealthProbeModelSetting,
  type ChannelHealthProbePolicy,
} from '../data/channel-health';

const LATENCY_PRESETS_SECONDS = [0.5, 1, 2, 5];
const ADD_MODEL_VALUE = '__add_model__';

function SortableModelRow({
  model,
  disabled,
  onUpdate,
  onRemove,
}: {
  model: ActiveHealthProbeModelSetting;
  disabled: boolean;
  onUpdate: (patch: Partial<ActiveHealthProbeModelSetting>) => void;
  onRemove: () => void;
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
      <Switch
        checked={model.enabled}
        onCheckedChange={(checked) => onUpdate({ enabled: checked })}
        disabled={disabled}
        aria-label={t('channelHealth.settings.enableModel', { model: model.modelID })}
      />
      <Switch
        checked={model.stream}
        onCheckedChange={(checked) => onUpdate({ stream: checked })}
        disabled={disabled}
        aria-label={t('channelHealth.settings.streamModel', { model: model.modelID })}
      />
      <Button
        type='button'
        variant='ghost'
        size='icon-sm'
        onClick={onRemove}
        disabled={disabled}
        aria-label={t('channelHealth.settings.removeModel', { model: model.modelID })}
      >
        <Trash2 className='size-3.5' />
      </Button>
    </div>
  );
}

export function ProbeSettingsSheet({
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
  const [latencySeconds, setLatencySeconds] = useState(String(policy.acceptableLatencyMs / 1000));
  const [extraChannels, setExtraChannels] = useState(String(policy.extraChannels));
  const [p95LookbackHours, setP95LookbackHours] = useState(String(policy.p95LookbackHours));
  const [models, setModels] = useState<ActiveHealthProbeModelSetting[]>(policy.models);
  const [modelToAdd, setModelToAdd] = useState(ADD_MODEL_VALUE);

  useEffect(() => {
    if (!open) {
      return;
    }
    setEnabled(policy.enabled);
    setLatencySeconds(String(policy.acceptableLatencyMs / 1000));
    setExtraChannels(String(policy.extraChannels));
    setP95LookbackHours(String(policy.p95LookbackHours));
    setModels(policy.models);
    setModelToAdd(ADD_MODEL_VALUE);
  }, [open, policy]);

  const parsedLatencySeconds = Number(latencySeconds);
  const parsedExtraChannels = Number(extraChannels);
  const parsedP95LookbackHours = Number(p95LookbackHours);
  const acceptableLatencyMs = Math.round(parsedLatencySeconds * 1000);
  const hasDuplicateModels = new Set(models.map((model) => model.modelID)).size !== models.length;
  const isValid =
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

  const handleAddModel = (modelID: string) => {
    if (modelID === ADD_MODEL_VALUE || models.some((model) => model.modelID === modelID)) {
      return;
    }
    setModels((current) => [...current, { modelID, enabled: true, stream: false }]);
    setModelToAdd(ADD_MODEL_VALUE);
  };

  const handleSave = () => {
    if (!isValid) {
      return;
    }
    updatePolicy.mutate(
      { enabled, acceptableLatencyMs, extraChannels: parsedExtraChannels, p95LookbackHours: parsedP95LookbackHours, models },
      {
        onSuccess: () => {
          toast.success(t('channelHealth.messages.policyUpdated'));
          onOpenChange(false);
        },
        onError: (error) => toast.error(error instanceof Error ? error.message : t('channelHealth.messages.updatePolicyFailed')),
      }
    );
  };

  const remainingModels = policy.availableModels.filter((modelID) => !models.some((model) => model.modelID === modelID));

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent aria-describedby={undefined} className='flex w-full flex-col sm:max-w-xl'>
        <SheetHeader>
          <SheetTitle>{t('channelHealth.settings.title')}</SheetTitle>
        </SheetHeader>
        <div className='flex-1 space-y-6 overflow-y-auto px-4 pb-4'>
          <div className='flex items-center justify-between gap-3'>
            <span className='text-sm font-medium'>{t('channelHealth.settings.masterSwitch')}</span>
            <Switch checked={enabled} onCheckedChange={setEnabled} disabled={updatePolicy.isPending} />
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
            <div className='flex gap-1.5'>
              {LATENCY_PRESETS_SECONDS.map((preset) => (
                <button
                  key={preset}
                  type='button'
                  onClick={() => setLatencySeconds(String(preset))}
                  className='text-muted-foreground hover:border-primary hover:text-primary rounded-full border px-2.5 py-0.5 text-xs transition-colors'
                  disabled={updatePolicy.isPending}
                >
                  {formatDuration(preset * 1000)}
                </button>
              ))}
            </div>
          </div>

          <div className='space-y-2'>
            <label htmlFor='probe-extra-channels' className='text-sm font-medium'>
              {t('channelHealth.settings.extraChannels')}
            </label>
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
          </div>

          <div className='space-y-3'>
            <div className='flex items-center justify-between gap-2'>
              <span className='text-sm font-medium'>{t('channelHealth.settings.models')}</span>
              <Select value={modelToAdd} onValueChange={handleAddModel} disabled={updatePolicy.isPending || remainingModels.length === 0}>
                <SelectTrigger className='w-48'>
                  <Plus className='size-3.5' />
                  <SelectValue placeholder={t('channelHealth.settings.addModel')} />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={ADD_MODEL_VALUE} disabled>
                    {t('channelHealth.settings.addModel')}
                  </SelectItem>
                  {remainingModels.map((modelID) => (
                    <SelectItem key={modelID} value={modelID}>
                      {modelID}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            {models.length > 0 ? (
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
                        onRemove={() => setModels((current) => current.filter((item) => item.modelID !== model.modelID))}
                      />
                    ))}
                  </div>
                </SortableContext>
              </DndContext>
            ) : (
              <div className='text-muted-foreground rounded-lg border border-dashed py-8 text-center text-sm'>
                {t('channelHealth.settings.noModels')}
              </div>
            )}
          </div>
        </div>
        <div className='flex gap-2 border-t px-4 py-3'>
          <Button onClick={handleSave} disabled={!isValid || updatePolicy.isPending}>
            {updatePolicy.isPending ? <Loader2 className='size-4 animate-spin' /> : null}
            {t('channelHealth.settings.save')}
          </Button>
          <Button variant='ghost' onClick={() => onOpenChange(false)}>
            {t('common.buttons.cancel')}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}
