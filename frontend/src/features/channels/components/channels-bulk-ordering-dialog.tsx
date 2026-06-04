import { useState, useEffect, memo, useCallback, useMemo } from 'react';
import { DndContext, closestCenter, KeyboardSensor, PointerSensor, useSensor, useSensors, type DragEndEvent } from '@dnd-kit/core';
import { arrayMove, SortableContext, sortableKeyboardCoordinates, verticalListSortingStrategy } from '@dnd-kit/sortable';
import { useSortable } from '@dnd-kit/sortable';
import { CSS } from '@dnd-kit/utilities';
import { GripVertical, ArrowUpToLine, ArrowDownToLine } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Separator } from '@/components/ui/separator';
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs';
import { useAllChannelSummarys, useBulkUpdateChannelOrdering } from '../data/channels';
import { ChannelSummary, ChannelSummaryConnection } from '../data/schema';

type OrderingMode = 'weight' | 'priority';

interface OrderedChannel {
  channel: ChannelSummary;
  orderingWeight: number;
  priority: number;
}

const MIN_WEIGHT = 0;
const MAX_WEIGHT = 100;
const MIN_GRAPHQL_INT = -2147483648;
const MAX_GRAPHQL_INT = 2147483647;
const PRIORITY_STEP = 10;

const formatWeight = (value: number) => Math.round(value);

const clampWeight = (value: number) => formatWeight(Math.min(MAX_WEIGHT, Math.max(MIN_WEIGHT, value)));

const isValidGraphQLInt = (value: number) => Number.isInteger(value) && value >= MIN_GRAPHQL_INT && value <= MAX_GRAPHQL_INT;

const calculateRelativeWeight = (prev?: number, next?: number) => {
  if (prev == null && next == null) {
    return clampWeight(1);
  }
  if (prev == null) {
    return clampWeight((next ?? 0) + 1);
  }
  if (next == null) {
    return clampWeight(prev - 1);
  }
  if (prev === next) {
    return clampWeight(prev);
  }
  return clampWeight(Math.floor((prev + next) / 2));
};

const sortChannelsByMode = (items: OrderedChannel[], mode: OrderingMode) => {
  const newItems = [...items];
  newItems.sort((a, b) => {
    if (mode === 'priority') {
      return b.priority - a.priority || b.orderingWeight - a.orderingWeight;
    }

    return b.orderingWeight - a.orderingWeight || b.priority - a.priority;
  });

  return newItems;
};

const normalizePrioritiesByOrder = (items: OrderedChannel[]) => {
  const maxPriority = Math.max(...items.map((item) => item.priority), 0);
  const priorityLevels = new Set(items.map((item) => item.priority));
  const startPriority = Math.max(maxPriority, priorityLevels.size * PRIORITY_STEP);
  const normalizedByOriginalPriority = new Map<number, number>();

  return items.map((item) => {
    let nextPriority = normalizedByOriginalPriority.get(item.priority);
    if (nextPriority === undefined) {
      nextPriority = startPriority - normalizedByOriginalPriority.size * PRIORITY_STEP;
      normalizedByOriginalPriority.set(item.priority, nextPriority);
    }

    return {
      ...item,
      priority: nextPriority,
    };
  });
};

const createOrderedChannels = (channelsData: ChannelSummaryConnection | undefined, mode: OrderingMode) => {
  if (!channelsData?.edges) {
    return [];
  }

  const channels = channelsData.edges.map((edge, index) => ({
    channel: edge.node,
    orderingWeight: clampWeight(edge.node.orderingWeight ?? channelsData.edges.length - index),
    priority: edge.node.priority ?? 0,
  }));

  return sortChannelsByMode(channels, mode);
};

interface ChannelOrderingItemProps {
  channel: ChannelSummary;
  orderingWeight: number;
  priority: number;
  mode: OrderingMode;
  index: number;
  total: number;
  onMoveToTop: (index: number) => void;
  onMoveToBottom: (index: number) => void;
  onWeightChange: (id: string, weight: number) => void;
  onPriorityChange: (id: string, priority: number) => void;
}

const ChannelOrderingItemComponent = memo(function ChannelOrderingItemComponent({
  channel,
  orderingWeight,
  priority,
  mode,
  index,
  total,
  onMoveToTop,
  onMoveToBottom,
  onWeightChange,
  onPriorityChange,
}: ChannelOrderingItemProps) {
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({ id: channel.id });
  const { t } = useTranslation();
  const currentValue = mode === 'priority' ? priority : orderingWeight;
  const [localValue, setLocalValue] = useState(currentValue.toString());

  useEffect(() => {
    setLocalValue(currentValue.toString());
  }, [currentValue]);

  const handleValueBlur = () => {
    const trimmedValue = localValue.trim();
    if (trimmedValue === '') {
      setLocalValue(currentValue.toString());
      return;
    }

    if (mode === 'priority') {
      if (!/^[+-]?\d+$/.test(trimmedValue)) {
        toast.error(t('channels.dialogs.fields.priority.integer'));
        setLocalValue(currentValue.toString());
        return;
      }

      const value = Number(trimmedValue);
      if (!isValidGraphQLInt(value)) {
        toast.error(t('channels.dialogs.fields.priority.integer'));
        setLocalValue(currentValue.toString());
        return;
      }

      if (value !== priority) {
        onPriorityChange(channel.id, value);
      }
      return;
    }

    const value = Number(trimmedValue);
    if (!Number.isNaN(value) && value !== orderingWeight) {
      onWeightChange(channel.id, value);
    } else {
      setLocalValue(currentValue.toString());
    }
  };

  const getTypeDisplayName = (type: string) => {
    const typeKey = `channels.types.${type}` as const;
    return t(typeKey, type);
  };

  const getStatusColor = (status: string) => {
    switch (status) {
      case 'enabled':
        return 'bg-emerald-50 text-emerald-700 border-emerald-200 dark:bg-emerald-950 dark:text-emerald-400 dark:border-emerald-800';
      case 'disabled':
        return 'bg-gray-50 text-gray-600 border-gray-200 dark:bg-gray-900 dark:text-gray-400 dark:border-gray-700';
      case 'archived':
        return 'bg-amber-50 text-amber-700 border-amber-200 dark:bg-amber-950 dark:text-amber-400 dark:border-amber-800';
      default:
        return 'bg-gray-50 text-gray-600 border-gray-200 dark:bg-gray-900 dark:text-gray-400 dark:border-gray-700';
    }
  };

  const getTypeColor = (type: string) => {
    const colors = {
      openai: 'bg-blue-50 text-blue-700 border-blue-200 dark:bg-blue-950 dark:text-blue-400',
      anthropic: 'bg-purple-50 text-purple-700 border-purple-200 dark:bg-purple-950 dark:text-purple-400',
      deepseek: 'bg-indigo-50 text-indigo-700 border-indigo-200 dark:bg-indigo-950 dark:text-indigo-400',
      doubao: 'bg-orange-50 text-orange-700 border-orange-200 dark:bg-orange-950 dark:text-orange-400',
      kimi: 'bg-pink-50 text-pink-700 border-pink-200 dark:bg-pink-950 dark:text-pink-400',
    };
    return colors[type as keyof typeof colors] || 'bg-gray-50 text-gray-700 border-gray-200 dark:bg-gray-900 dark:text-gray-400';
  };

  const style = {
    transform: CSS.Transform.toString(transform),
    transition,
    opacity: isDragging ? 0.5 : 1,
  };

  return (
    <div
      ref={setNodeRef}
      style={style}
      className={`group bg-card flex items-center gap-2 rounded-md border p-1 hover:shadow-sm ${
        isDragging ? 'ring-primary/20 relative z-50 shadow-xl ring-2' : 'hover:border-primary/20'
      }`}
    >
      <div
        className='text-muted-foreground hover:text-foreground flex min-w-[40px] cursor-grab items-center gap-1 px-1 active:cursor-grabbing'
        {...attributes}
        {...listeners}
      >
        <GripVertical className='h-3.5 w-3.5' />
        <span className='w-[20px] text-center font-mono text-[10px]'>{index + 1}</span>
      </div>

      <div className='flex min-w-0 flex-1 items-center gap-2'>
        <div className='flex min-w-0 items-center gap-1.5'>
          <span className='truncate text-sm font-medium'>{channel.name}</span>
          <div className='flex flex-shrink-0 gap-1'>
            <Badge variant='outline' className={`h-3.5 px-1 text-[10px] font-normal ${getTypeColor(channel.type)}`}>
              {getTypeDisplayName(channel.type)}
            </Badge>
            <Badge variant='outline' className={`h-3.5 px-1 text-[10px] font-normal ${getStatusColor(channel.status)}`}>
              {t(`channels.status.${channel.status}`)}
            </Badge>
          </div>
        </div>

        <div className='hidden flex-1 items-center gap-2 sm:flex'>
          <div className='bg-border h-3 w-[1px]' />
          <span className='text-muted-foreground truncate font-mono text-[10px] opacity-70'>{channel.baseURL}</span>
        </div>
      </div>

      <div className='flex items-center gap-1 pr-1'>
        <div className='bg-muted/30 flex items-center gap-1.5 rounded px-1.5 py-0.5'>
          <span className='text-muted-foreground text-[10px]'>
            {t(mode === 'priority' ? 'channels.dialogs.bulkOrdering.priority' : 'channels.dialogs.bulkOrdering.orderingWeight')}
          </span>
          <Input
            type='number'
            inputMode={mode === 'priority' ? 'numeric' : 'decimal'}
            step={mode === 'priority' ? '1' : 'any'}
            min={mode === 'weight' ? MIN_WEIGHT : undefined}
            max={mode === 'weight' ? MAX_WEIGHT : undefined}
            className='h-6 w-16 px-1 text-center text-xs'
            value={localValue}
            onChange={(e) => setLocalValue(e.target.value)}
            onBlur={handleValueBlur}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.currentTarget.blur();
              }
            }}
            onClick={(e) => e.stopPropagation()}
            onPointerDown={(e) => e.stopPropagation()}
          />
        </div>

        <div className='flex items-center gap-0.5'>
          <Button
            variant='ghost'
            size='icon'
            className='text-muted-foreground hover:text-foreground h-6 w-6'
            onClick={() => onMoveToTop(index)}
            disabled={index === 0}
            title={t('common.moveToTop', 'Move to top')}
          >
            <ArrowUpToLine className='h-3.5 w-3.5' />
          </Button>
          <Button
            variant='ghost'
            size='icon'
            className='text-muted-foreground hover:text-foreground h-6 w-6'
            onClick={() => onMoveToBottom(index)}
            disabled={index === total - 1}
            title={t('common.moveToBottom', 'Move to bottom')}
          >
            <ArrowDownToLine className='h-3.5 w-3.5' />
          </Button>
        </div>
      </div>
    </div>
  );
});

interface ChannelsBulkOrderingDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

export function ChannelsBulkOrderingDialog({ open, onOpenChange }: ChannelsBulkOrderingDialogProps) {
  const { t } = useTranslation();
  const [mode, setMode] = useState<OrderingMode>('priority');
  const { data: channelsData, isLoading } = useAllChannelSummarys(undefined, { enabled: open });
  const bulkUpdateMutation = useBulkUpdateChannelOrdering();
  const [orderedChannels, setOrderedChannels] = useState<OrderedChannel[]>([]);
  const [dirtyModes, setDirtyModes] = useState<Record<OrderingMode, boolean>>({ weight: false, priority: false });

  const hasChanges = dirtyModes.weight || dirtyModes.priority;

  const duplicatePriorityCount = useMemo(() => {
    const counts = new Map<number, number>();
    for (const item of orderedChannels) {
      counts.set(item.priority, (counts.get(item.priority) ?? 0) + 1);
    }

    return Array.from(counts.values()).filter((count) => count > 1).length;
  }, [orderedChannels]);

  useEffect(() => {
    if (!open) {
      return;
    }

    setMode('priority');
    setOrderedChannels(createOrderedChannels(channelsData, 'priority'));
    setDirtyModes({ weight: false, priority: false });
  }, [channelsData, open]);

  const markDirty = useCallback((dirtyMode: OrderingMode) => {
    setDirtyModes((prev) => ({ ...prev, [dirtyMode]: true }));
  }, []);

  const handleModeChange = useCallback((value: string) => {
    const nextMode = value as OrderingMode;
    setMode(nextMode);
    setOrderedChannels((items) => sortChannelsByMode(items, nextMode));
  }, []);

  const sensors = useSensors(
    useSensor(PointerSensor),
    useSensor(KeyboardSensor, {
      coordinateGetter: sortableKeyboardCoordinates,
    })
  );

  const handleDragEnd = useCallback(
    (event: DragEndEvent) => {
      const { active, over } = event;

      if (!over || active.id === over.id) {
        return;
      }

      setOrderedChannels((items) => {
        const oldIndex = items.findIndex((item) => item.channel.id === active.id);
        const newIndex = items.findIndex((item) => item.channel.id === over.id);

        if (oldIndex === -1 || newIndex === -1) {
          return items;
        }

        const newItems = arrayMove(items, oldIndex, newIndex);
        markDirty(mode);

        if (mode === 'priority') {
          return normalizePrioritiesByOrder(newItems);
        }

        const prevWeight = newItems[newIndex - 1]?.orderingWeight;
        const nextWeight = newItems[newIndex + 1]?.orderingWeight;

        newItems[newIndex] = {
          ...newItems[newIndex],
          orderingWeight: calculateRelativeWeight(prevWeight, nextWeight),
        };

        return newItems;
      });
    },
    [markDirty, mode]
  );

  const handleWeightChange = useCallback(
    (id: string, weight: number) => {
      const normalizedWeight = clampWeight(weight);
      setOrderedChannels((items) => {
        const newItems = items.map((item) => (item.channel.id === id ? { ...item, orderingWeight: normalizedWeight } : item));
        markDirty('weight');
        return sortChannelsByMode(newItems, mode);
      });
    },
    [markDirty, mode]
  );

  const handlePriorityChange = useCallback(
    (id: string, priority: number) => {
      setOrderedChannels((items) => {
        const newItems = items.map((item) => (item.channel.id === id ? { ...item, priority } : item));
        markDirty('priority');
        return sortChannelsByMode(newItems, mode);
      });
    },
    [markDirty, mode]
  );

  const handleMoveToTop = useCallback(
    (index: number) => {
      setOrderedChannels((items) => {
        if (!items.length || index === 0) {
          return items;
        }

        const newItems = arrayMove(items, index, 0);
        markDirty(mode);

        if (mode === 'priority') {
          return normalizePrioritiesByOrder(newItems);
        }

        const nextWeight = newItems[1]?.orderingWeight;
        newItems[0] = {
          ...newItems[0],
          orderingWeight: calculateRelativeWeight(undefined, nextWeight),
        };

        return newItems;
      });
    },
    [markDirty, mode]
  );

  const handleMoveToBottom = useCallback(
    (index: number) => {
      setOrderedChannels((items) => {
        if (!items.length || index === items.length - 1) {
          return items;
        }

        const targetIndex = items.length - 1;
        const newItems = arrayMove(items, index, targetIndex);
        markDirty(mode);

        if (mode === 'priority') {
          return normalizePrioritiesByOrder(newItems);
        }

        const prevWeight = newItems[targetIndex - 1]?.orderingWeight;
        newItems[targetIndex] = {
          ...newItems[targetIndex],
          orderingWeight: calculateRelativeWeight(prevWeight, undefined),
        };

        return newItems;
      });
    },
    [markDirty, mode]
  );

  const handleSave = async () => {
    try {
      const updates = orderedChannels.map((item) => ({
        id: item.channel.id,
        ...(dirtyModes.weight ? { orderingWeight: item.orderingWeight } : {}),
        ...(dirtyModes.priority ? { priority: item.priority } : {}),
      }));

      await bulkUpdateMutation.mutateAsync({
        channels: updates,
      });

      setDirtyModes({ weight: false, priority: false });
      onOpenChange(false);
    } catch (_error) {
      // Error is handled by the mutation hook
    }
  };

  const resetOrderingState = useCallback(() => {
    setOrderedChannels(createOrderedChannels(channelsData, mode));
    setDirtyModes({ weight: false, priority: false });
  }, [channelsData, mode]);

  const handleCancel = () => {
    resetOrderingState();
    onOpenChange(false);
  };

  const handleOpenChange = (nextOpen: boolean) => {
    if (!nextOpen) {
      resetOrderingState();
    }

    onOpenChange(nextOpen);
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent className='flex max-h-[90vh] flex-col sm:max-w-5xl'>
        <DialogHeader className='flex-shrink-0 text-left'>
          <DialogTitle className='flex items-center gap-2'>
            <GripVertical className='text-muted-foreground h-5 w-5' />
            {t('channels.dialogs.bulkOrdering.title')}
          </DialogTitle>
          <DialogDescription className='text-muted-foreground text-sm'>
            {t(mode === 'priority' ? 'channels.dialogs.bulkOrdering.priorityDescription' : 'channels.dialogs.bulkOrdering.description')}
          </DialogDescription>
        </DialogHeader>

        <div className='flex flex-shrink-0 items-center justify-between gap-3'>
          <Tabs value={mode} onValueChange={handleModeChange}>
            <TabsList>
              <TabsTrigger value='priority'>{t('channels.dialogs.bulkOrdering.priorityMode')}</TabsTrigger>
              <TabsTrigger value='weight'>{t('channels.dialogs.bulkOrdering.weightMode')}</TabsTrigger>
            </TabsList>
          </Tabs>
        </div>

        <Separator className='flex-shrink-0' />

        <div className='-mr-4 h-[40rem] w-full flex-1 overflow-y-auto py-1 pr-4'>
          {isLoading ? (
            <div className='flex items-center justify-center py-12'>
              <div className='flex flex-col items-center gap-3'>
                <div className='border-primary h-8 w-8 animate-spin rounded-full border-b-2'></div>
                <div className='text-muted-foreground text-sm'>{t('common.loading', 'Loading')}...</div>
              </div>
            </div>
          ) : orderedChannels.length === 0 ? (
            <div className='flex items-center justify-center py-12'>
              <div className='flex flex-col items-center gap-3 text-center'>
                <GripVertical className='text-muted-foreground/30 h-12 w-12' />
                <div className='text-muted-foreground text-sm'>{t('channels.dialogs.bulkOrdering.noChannels')}</div>
              </div>
            </div>
          ) : (
            <div className='flex h-full flex-col gap-4 p-0.5'>
              <div className='flex items-center justify-between px-1 py-2'>
                <div className='text-muted-foreground flex flex-wrap items-center gap-3 text-sm'>
                  <span>
                    {t(mode === 'priority' ? 'channels.dialogs.bulkOrdering.priorityDragHint' : 'channels.dialogs.bulkOrdering.dragHint')}
                  </span>
                  <Badge variant='secondary' className='font-mono'>
                    {t('channels.dialogs.bulkOrdering.channelCount', {
                      count: orderedChannels.length,
                    })}
                  </Badge>
                  {mode === 'priority' && duplicatePriorityCount > 0 && (
                    <Badge variant='outline' className='border-sky-200 bg-sky-50 text-sky-600 dark:border-sky-800 dark:bg-sky-950 dark:text-sky-400'>
                      {t('channels.dialogs.bulkOrdering.duplicatePriorityHint')}
                    </Badge>
                  )}
                  {hasChanges && (
                    <Badge
                      variant='outline'
                      className='border-amber-200 bg-amber-50 text-amber-600 dark:border-amber-800 dark:bg-amber-950 dark:text-amber-400'
                    >
                      {t('common.unsavedChanges', 'Unsaved changes')}
                    </Badge>
                  )}
                </div>
              </div>

              <DndContext sensors={sensors} collisionDetection={closestCenter} onDragEnd={handleDragEnd}>
                <SortableContext items={orderedChannels.map((item) => item.channel.id)} strategy={verticalListSortingStrategy}>
                  <div className='flex-1 space-y-1'>
                    {orderedChannels.map((item, index) => (
                      <ChannelOrderingItemComponent
                        key={item.channel.id}
                        channel={item.channel}
                        orderingWeight={item.orderingWeight}
                        priority={item.priority}
                        mode={mode}
                        index={index}
                        total={orderedChannels.length}
                        onMoveToTop={handleMoveToTop}
                        onMoveToBottom={handleMoveToBottom}
                        onWeightChange={handleWeightChange}
                        onPriorityChange={handlePriorityChange}
                      />
                    ))}
                  </div>
                </SortableContext>
              </DndContext>
            </div>
          )}
        </div>

        <DialogFooter className='flex-shrink-0'>
          <div className='flex w-full items-center justify-between'>
            <div className='text-muted-foreground text-xs'>
              {hasChanges && (
                <span className='flex items-center gap-1'>
                  <div className='h-2 w-2 rounded-full bg-amber-500'></div>
                  {t('common.unsavedChanges', 'You have unsaved changes')}
                </span>
              )}
            </div>
            <div className='flex items-center gap-2'>
              <Button variant='outline' onClick={handleCancel}>
                {t('common.buttons.cancel')}
              </Button>
              <Button onClick={handleSave} disabled={!hasChanges || bulkUpdateMutation.isPending} className='min-w-[120px]'>
                {bulkUpdateMutation.isPending ? (
                  <div className='flex items-center gap-2'>
                    <div className='h-4 w-4 animate-spin rounded-full border-b-2 border-white'></div>
                    {t('common.buttons.saving')}
                  </div>
                ) : (
                  t('channels.dialogs.bulkOrdering.saveButton')
                )}
              </Button>
            </div>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
