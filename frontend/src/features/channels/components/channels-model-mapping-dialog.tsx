import { useEffect, useMemo, useState, useCallback } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { Plus, X, Lightbulb } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form, FormControl, FormField, FormItem } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { TagsAutocompleteInput } from '@/components/ui/tags-autocomplete-input';
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip';
import { useUpdateChannel } from '../data/channels';
import { Channel, ModelMapping } from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';
import { addAliasTarget, hasModelMapping } from '../utils/model-mapping-groups';
import { MODEL_MAPPING_RENAME_ATTR, ModelMappingGraph } from './model-mapping-graph';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

// 扩展 schema 以包含模型映射的校验规则
const createModelMappingFormSchema = (supportedModels: string[]) =>
  z.object({
    extraModelPrefix: z.string().optional(),
    modelMappings: z
      .array(
        z.object({
          from: z.string().min(1, 'Original model is required'),
          to: z.string().min(1, 'Target model is required'),
        })
      )
      .refine(
        (mappings) => {
          // 检查所有目标模型是否在支持的模型列表中
          return mappings.every((m) => supportedModels.includes(m.to));
        },
        {
          message: 'Target model must be in supported models',
        }
      ),
    autoTrimedModelPrefixes: z.array(z.string()).optional(),
    hideOriginalModels: z.boolean().optional(),
    hideMappedModels: z.boolean().optional(),
    lowercaseModelId: z.boolean().optional(),
  });

const extractAliasFromModelPath = (modelPath: string): string => {
  if (!modelPath) {
    return '';
  }
  const segments = modelPath.split('/');
  return segments[segments.length - 1]?.trim() ?? '';
};

const extractAllPrefixes = (models: string[]): string[] => {
  if (!models || models.length === 0) {
    return [];
  }

  const prefixes = new Set<string>();
  models.forEach((model) => {
    const segments = model.split('/');
    for (let i = 1; i < segments.length; i++) {
      const prefix = segments.slice(0, i).join('/');
      if (prefix) {
        prefixes.add(prefix);
      }
    }
  });

  return Array.from(prefixes).sort();
};

export function ChannelsModelMappingDialog({ open, onOpenChange, currentRow }: Props) {
  const { t } = useTranslation();
  const updateChannel = useUpdateChannel();

  const [modelMappings, setModelMappings] = useState<ModelMapping[]>(currentRow.settings?.modelMappings || []);
  const [newMapping, setNewMapping] = useState({ from: '', to: '' });

  const prefixSuggestions = useMemo(() => extractAllPrefixes(currentRow.supportedModels), [currentRow.supportedModels]);

  const modelMappingFormSchema = createModelMappingFormSchema(currentRow.supportedModels);

  const form = useForm<z.infer<typeof modelMappingFormSchema>>({
    resolver: zodResolver(modelMappingFormSchema),
    defaultValues: {
      extraModelPrefix: currentRow.settings?.extraModelPrefix || '',
      modelMappings: currentRow.settings?.modelMappings || [],
      autoTrimedModelPrefixes: currentRow.settings?.autoTrimedModelPrefixes || [],
      hideOriginalModels: currentRow.settings?.hideOriginalModels || false,
      hideMappedModels: currentRow.settings?.hideMappedModels || false,
      lowercaseModelId: currentRow.settings?.lowercaseModelId || false,
    },
  });

  const handleAutoExtractAllPrefixes = useCallback(() => {
    if (prefixSuggestions.length === 0) {
      toast.warning(
        t('channels.dialogs.settings.autoTrimedModelPrefixes.noPrefixesFound', {
          defaultValue: 'No prefixes found in supported models',
        })
      );
      return;
    }

    const currentPrefixes = form.getValues('autoTrimedModelPrefixes') || [];
    const currentPrefixesSet = new Set(currentPrefixes);
    const newPrefixes = prefixSuggestions.filter((prefix) => !currentPrefixesSet.has(prefix));

    if (newPrefixes.length === 0) {
      toast.warning(
        t('channels.dialogs.settings.autoTrimedModelPrefixes.allPrefixesAlreadyAdded', {
          defaultValue: 'All prefixes have already been added',
        })
      );
      return;
    }

    form.setValue('autoTrimedModelPrefixes', [...currentPrefixes, ...newPrefixes]);
    toast.success(
      t('channels.dialogs.settings.autoTrimedModelPrefixes.prefixesAdded', {
        count: newPrefixes.length,
        defaultValue: `${newPrefixes.length} prefix(es) added successfully`,
      })
    );
  }, [form, prefixSuggestions, t]);

  const handleClearAllPrefixes = useCallback(() => {
    const currentPrefixes = form.getValues('autoTrimedModelPrefixes') || [];
    if (currentPrefixes.length === 0) {
      toast.warning(
        t('channels.dialogs.settings.autoTrimedModelPrefixes.noPrefixesToClear', {
          defaultValue: 'No prefixes to clear',
        })
      );
      return;
    }

    form.setValue('autoTrimedModelPrefixes', []);
    toast.success(
      t('channels.dialogs.settings.autoTrimedModelPrefixes.prefixesCleared', {
        count: currentPrefixes.length,
        defaultValue: `${currentPrefixes.length} prefix(es) cleared successfully`,
      })
    );
  }, [form, t]);

  const sanitizeMapping = (mapping: ModelMapping): ModelMapping => ({
    from: mapping.from.trim(),
    to: mapping.to.trim(),
  });

  useEffect(() => {
    const nextExtraModelPrefix = currentRow.settings?.extraModelPrefix || '';
    const nextMappings = currentRow.settings?.modelMappings || [];
    setModelMappings(nextMappings);
    setNewMapping({ from: '', to: '' });
    form.reset({
      extraModelPrefix: nextExtraModelPrefix,
      modelMappings: nextMappings,
      autoTrimedModelPrefixes: currentRow.settings?.autoTrimedModelPrefixes || [],
      hideOriginalModels: currentRow.settings?.hideOriginalModels || false,
      hideMappedModels: currentRow.settings?.hideMappedModels || false,
      lowercaseModelId: currentRow.settings?.lowercaseModelId || false,
    });
  }, [currentRow, open, form]);

  const aliasSuggestion = useMemo(() => {
    const modelPath = newMapping.to;
    if (!modelPath) {
      return '';
    }

    return extractAliasFromModelPath(modelPath);
  }, [newMapping.to]);

  const applyAliasSuggestion = () => {
    if (!aliasSuggestion) {
      return;
    }
    setNewMapping((prev) => ({ ...prev, from: aliasSuggestion }));
  };

  const onSubmit = async (values: z.infer<typeof modelMappingFormSchema>) => {
    // 检查是否有未添加的映射
    if (newMapping.from.trim() || newMapping.to.trim()) {
      toast.warning(t('channels.messages.pendingMappingWarning'));
      return;
    }

    try {
      const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
        extraModelPrefix: values.extraModelPrefix,
        modelMappings: values.modelMappings,
        autoTrimedModelPrefixes: values.autoTrimedModelPrefixes || [],
        hideOriginalModels: values.hideOriginalModels,
        hideMappedModels: values.hideMappedModels,
        lowercaseModelId: values.lowercaseModelId,
      });

      await updateChannel.mutateAsync({
        id: currentRow.id,
        input: {
          settings: nextSettings,
        },
      });
      toast.success(t('channels.messages.updateSuccess'));
      onOpenChange(false);
    } catch (_error) {
      toast.error(t('common.errors.internalServerError'));
    }
  };

  const updateMappings = (updatedMappings: ModelMapping[]) => {
    setModelMappings(updatedMappings);
    form.setValue('modelMappings', updatedMappings, {
      shouldValidate: true,
      shouldDirty: true,
    });
  };

  const addMapping = () => {
    if (!newMapping.from.trim() || !newMapping.to.trim()) {
      return;
    }

    const sanitizedMapping = sanitizeMapping(newMapping);
    if (hasModelMapping(modelMappings, sanitizedMapping.from, sanitizedMapping.to)) {
      toast.warning(
        t('channels.dialogs.settings.modelMapping.duplicateMapping', {
          defaultValue: 'This alias already maps to that model',
        })
      );
      // The pair already exists, so clear it rather than leave it blocking Save.
      setNewMapping({ from: '', to: '' });
      return;
    }

    // An existing alias gains another target next to its current ones.
    updateMappings(addAliasTarget(modelMappings, sanitizedMapping.from, sanitizedMapping.to));
    setNewMapping({ from: '', to: '' });
  };

  return (
    <TooltipProvider>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent
          className='max-h-[90vh] overflow-y-auto sm:max-w-[800px]'
          onEscapeKeyDown={(event) => {
            // Escape while renaming an alias cancels the rename, not the dialog.
            if ((event.target as HTMLElement | null)?.closest?.(`[${MODEL_MAPPING_RENAME_ATTR}]`)) {
              event.preventDefault();
            }
          }}
        >
          <DialogHeader>
            <DialogTitle>{t('channels.dialogs.settings.modelMapping.title')}</DialogTitle>
            <DialogDescription>{t('channels.dialogs.settings.modelMapping.description', { name: currentRow.name })}</DialogDescription>
          </DialogHeader>

          <Form {...form}>
            <form onSubmit={form.handleSubmit(onSubmit)}>
              <div className='space-y-6'>
              <Card>
                <CardHeader>
                  <CardTitle className='text-lg'>{t('channels.dialogs.settings.modelMapping.hideOriginalModels.label')}</CardTitle>
                  <CardDescription>{t('channels.dialogs.settings.modelMapping.hideOriginalModels.description')}</CardDescription>
                </CardHeader>
                <CardContent>
                  <div className='flex items-start gap-3'>
                    <Checkbox
                      id='hideOriginalModels'
                      checked={form.watch('hideOriginalModels') || false}
                      onCheckedChange={(checked) => form.setValue('hideOriginalModels', checked === true)}
                    />
                    <label
                      htmlFor='hideOriginalModels'
                      className='cursor-pointer text-sm leading-none font-medium peer-disabled:cursor-not-allowed peer-disabled:opacity-70'
                    >
                      {t('channels.dialogs.settings.modelMapping.hideOriginalModels.label')}
                    </label>
                  </div>
                </CardContent>
              </Card>
              <Card>
                <CardHeader>
                  <CardTitle className='text-lg'>{t('channels.dialogs.settings.extraModelPrefix.title')}</CardTitle>
                  <CardDescription>{t('channels.dialogs.settings.extraModelPrefix.description')}</CardDescription>
                </CardHeader>
                <CardContent>
                  <Input
                    placeholder={t('channels.dialogs.settings.extraModelPrefix.placeholder')}
                    value={form.watch('extraModelPrefix') || ''}
                    onChange={(e) => form.setValue('extraModelPrefix', e.target.value)}
                  />
                  {form.formState.errors.extraModelPrefix?.message && (
                    <p className='text-destructive mt-2 text-sm'>{form.formState.errors.extraModelPrefix.message.toString()}</p>
                  )}
                </CardContent>
              </Card>

              <Card>
                <CardHeader>
                  <CardTitle className='text-lg'>{t('channels.dialogs.settings.autoTrimedModelPrefixes.title')}</CardTitle>
                  <CardDescription>{t('channels.dialogs.settings.autoTrimedModelPrefixes.description')}</CardDescription>
                </CardHeader>
                <CardContent>
                  <div className='space-y-2'>
                    {/* 添加新前缀 */}
                    <FormField
                      control={form.control}
                      name='autoTrimedModelPrefixes'
                      render={({ field }) => (
                        <FormItem>
                          <FormControl>
                            <TagsAutocompleteInput
                              value={field.value || []}
                              onChange={field.onChange}
                              placeholder={t('channels.dialogs.settings.autoTrimedModelPrefixes.placeholder')}
                              suggestions={prefixSuggestions}
                              className='h-auto min-h-9 py-1'
                            />
                          </FormControl>
                        </FormItem>
                      )}
                    />

                    {/* 自动提取所有前缀 */}
                    {prefixSuggestions.length > 0 && (
                      <div className='flex items-center gap-2 pt-2'>
                        <Button type='button' variant='outline' size='sm' onClick={handleAutoExtractAllPrefixes} className='text-xs'>
                          {t('channels.dialogs.settings.autoTrimedModelPrefixes.autoExtractAll', {
                            defaultValue: 'Auto-extract all prefixes',
                          })}
                        </Button>
                        <Button type='button' variant='outline' size='sm' onClick={handleClearAllPrefixes} className='text-xs'>
                          {t('channels.dialogs.settings.autoTrimedModelPrefixes.clearAll', {
                            defaultValue: 'Clear all',
                          })}
                        </Button>
                        <span className='text-muted-foreground text-xs'>
                          {t('channels.dialogs.settings.autoTrimedModelPrefixes.prefixesDetected', {
                            count: prefixSuggestions.length,
                            defaultValue: `Detected ${prefixSuggestions.length} prefix(es)`,
                          })}
                        </span>
                      </div>
                    )}
                  </div>
                </CardContent>
              </Card>

              <Card>
                <CardHeader>
                  <CardTitle className='text-lg'>{t('channels.dialogs.settings.lowercaseModelId.title')}</CardTitle>
                  <CardDescription>{t('channels.dialogs.settings.lowercaseModelId.description')}</CardDescription>
                </CardHeader>
                <CardContent>
                  <div className='flex items-start gap-3'>
                    <Checkbox
                      id='lowercaseModelId'
                      checked={form.watch('lowercaseModelId') || false}
                      onCheckedChange={(checked) => form.setValue('lowercaseModelId', checked === true)}
                    />
                    <label
                      htmlFor='lowercaseModelId'
                      className='cursor-pointer text-sm leading-none font-medium peer-disabled:cursor-not-allowed peer-disabled:opacity-70'
                    >
                      {t('channels.dialogs.settings.lowercaseModelId.label')}
                    </label>
                  </div>
                </CardContent>
              </Card>

              <Card>
                <CardHeader>
                  <CardTitle className='text-lg'>{t('channels.dialogs.settings.modelMapping.title')}</CardTitle>
                  <CardDescription>{t('channels.dialogs.settings.modelMapping.description')}</CardDescription>
                </CardHeader>
                <CardContent className='space-y-4'>
                  <div className='flex flex-wrap items-center gap-6 pb-2'>
                    <div className='flex items-center gap-2'>
                      <Checkbox
                        id='hideMappedModels'
                        checked={form.watch('hideMappedModels') || false}
                        onCheckedChange={(checked) => form.setValue('hideMappedModels', checked === true)}
                      />
                      <label
                        htmlFor='hideMappedModels'
                        className='cursor-pointer text-sm font-medium leading-none peer-disabled:cursor-not-allowed peer-disabled:opacity-70'
                      >
                        {t('channels.dialogs.settings.modelMapping.hideMappedModels.label')}
                      </label>
                    </div>
                  </div>

                  <div className='grid grid-cols-1 gap-2 md:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)_auto_auto] md:items-center'>
                    <div className='flex min-w-0 gap-2'>
                      <Input
                        placeholder={t('channels.dialogs.settings.modelMapping.originalModel')}
                        value={newMapping.from}
                        onChange={(e) => setNewMapping({ ...newMapping, from: e.target.value })}
                        className='min-w-0 flex-1'
                      />
                      {aliasSuggestion && newMapping.to && (
                        <Tooltip>
                          <TooltipTrigger asChild>
                            <Button
                              type='button'
                              variant='outline'
                              size='sm'
                              onClick={applyAliasSuggestion}
                              disabled={!aliasSuggestion || newMapping.from.trim() === aliasSuggestion}
                              aria-label={t('channels.dialogs.settings.modelMapping.useSuggestion', {
                                alias: aliasSuggestion,
                                defaultValue: `Use ${aliasSuggestion}`,
                              })}
                            >
                              <Lightbulb className='h-4 w-4' />
                            </Button>
                          </TooltipTrigger>
                          <TooltipContent>
                            <p>
                              {t('channels.dialogs.settings.modelMapping.useSuggestion', {
                                alias: aliasSuggestion,
                                defaultValue: `Use ${aliasSuggestion}`,
                              })}
                            </p>
                          </TooltipContent>
                        </Tooltip>
                      )}
                    </div>
                    <span className='text-muted-foreground hidden justify-center md:flex'>→</span>
                    <Select value={newMapping.to} onValueChange={(value) => setNewMapping({ ...newMapping, to: value })}>
                      <SelectTrigger className='min-w-0 w-full'>
                        <SelectValue placeholder={t('channels.dialogs.settings.modelMapping.targetModel')} />
                      </SelectTrigger>
                      <SelectContent>
                        {currentRow.supportedModels.map((model) => (
                          <SelectItem key={model} value={model}>
                            {model}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                    <Button
                      type='button'
                      onClick={addMapping}
                      size='sm'
                      disabled={!newMapping.from.trim() || !newMapping.to.trim()}
                      data-testid='add-model-mapping-button'
                      aria-label={t('channels.dialogs.settings.modelMapping.addMappingButton', {
                        defaultValue: 'Add mapping',
                      })}
                    >
                      <Plus size={16} />
                    </Button>
                    {(newMapping.from.trim() || newMapping.to.trim()) && (
                      <Button type='button' variant='outline' size='sm' onClick={() => setNewMapping({ from: '', to: '' })}>
                        <X size={16} />
                      </Button>
                    )}
                  </div>

                  {/* 显示表单错误 */}
                  {form.formState.errors.modelMappings?.message && (
                    <p className='text-destructive text-sm'>{form.formState.errors.modelMappings.message.toString()}</p>
                  )}

                  {modelMappings.length === 0 ? (
                    <p className='text-muted-foreground py-4 text-center text-sm'>
                      {t('channels.dialogs.settings.modelMapping.noMappings')}
                    </p>
                  ) : (
                    <ModelMappingGraph mappings={modelMappings} supportedModels={currentRow.supportedModels} onChange={updateMappings} />
                  )}
                </CardContent>
              </Card>
            </div>

              <DialogFooter className='mt-6'>
                <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
                  {t('common.buttons.cancel')}
                </Button>
                <Button type='submit' disabled={updateChannel.isPending}>
                  {updateChannel.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
                </Button>
              </DialogFooter>
            </form>
          </Form>
        </DialogContent>
      </Dialog>
    </TooltipProvider>
  );
}
