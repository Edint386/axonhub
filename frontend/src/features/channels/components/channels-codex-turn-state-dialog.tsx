'use client';

import { useEffect } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form, FormField, FormItem, FormLabel, FormMessage, FormControl, FormDescription } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { useUpdateChannel } from '../data/channels';
import { Channel } from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

const formSchema = z
  .object({
    enabled: z.boolean(),
    plan: z.enum(['pro', 'team']),
    models: z.string(),
    harvestProxyURL: z.string(),
    ttlMinutes: z.number().int().min(1).max(60),
    refreshBeforeMinutes: z.number().int().min(0).max(59),
    maxAttempts: z.number().int().min(1).max(32),
    cooldownSeconds: z.number().int().min(30).max(3600),
    strict: z.boolean(),
  })
  .superRefine((values, ctx) => {
    if (values.enabled && values.refreshBeforeMinutes >= values.ttlMinutes) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['refreshBeforeMinutes'],
        message: 'refreshBeforeMinutes',
      });
    }
    const models = values.models
      .split(/[\n,]/)
      .map((item) => item.trim())
      .filter(Boolean);
    if (values.enabled && models.length === 0) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['models'],
        message: 'modelsRequired',
      });
    }
  });

type FormValues = z.infer<typeof formSchema>;

function valuesFromChannel(currentRow: Channel): FormValues {
  const current = currentRow.settings?.codexTurnState;
  return {
    enabled: current?.enabled ?? false,
    plan: current?.plan === 'team' ? 'team' : 'pro',
    models: (current?.models ?? ['gpt-6-astra']).join('\n'),
    harvestProxyURL: current?.harvestProxyURL ?? '',
    ttlMinutes: current?.ttlMinutes ?? 60,
    refreshBeforeMinutes: current?.refreshBeforeMinutes ?? 10,
    maxAttempts: current?.maxAttempts ?? 8,
    cooldownSeconds: current?.cooldownSeconds ?? 300,
    strict: current?.strict ?? true,
  };
}

export function ChannelsCodexTurnStateDialog({ open, onOpenChange, currentRow }: Props) {
  const { t } = useTranslation();
  const updateChannel = useUpdateChannel();

  const form = useForm<FormValues>({
    resolver: zodResolver(formSchema),
    defaultValues: valuesFromChannel(currentRow),
    mode: 'onChange',
  });

  useEffect(() => {
    if (open) {
      form.reset(valuesFromChannel(currentRow));
    }
  }, [open, currentRow, form]);

  const onSubmit = async (values: FormValues) => {
    try {
      const models = values.models
        .split(/[\n,]/)
        .map((item) => item.trim())
        .filter(Boolean);
      const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
        codexTurnState: values.enabled
          ? {
              enabled: true,
              plan: values.plan,
              models,
              harvestProxyURL: values.harvestProxyURL.trim() || null,
              ttlMinutes: values.ttlMinutes,
              refreshBeforeMinutes: values.refreshBeforeMinutes,
              maxAttempts: values.maxAttempts,
              cooldownSeconds: values.cooldownSeconds,
              strict: values.strict,
            }
          : { enabled: false, plan: values.plan, models, harvestProxyURL: values.harvestProxyURL.trim() || null, ttlMinutes: values.ttlMinutes, refreshBeforeMinutes: values.refreshBeforeMinutes, maxAttempts: values.maxAttempts, cooldownSeconds: values.cooldownSeconds, strict: values.strict },
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

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='max-h-[90vh] overflow-y-auto sm:max-w-2xl'>
        <DialogHeader className='text-left'>
          <DialogTitle>{t('channels.dialogs.codexTurnState.title')}</DialogTitle>
          <DialogDescription>{t('channels.dialogs.codexTurnState.description', { name: currentRow.name })}</DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form onSubmit={form.handleSubmit(onSubmit)} className='space-y-4'>
            <Card>
              <CardHeader>
                <CardTitle className='text-lg'>{t('channels.dialogs.codexTurnState.config.title')}</CardTitle>
                <CardDescription>{t('channels.dialogs.codexTurnState.config.description')}</CardDescription>
              </CardHeader>
              <CardContent className='space-y-4'>
                <FormField
                  control={form.control}
                  name='enabled'
                  render={({ field }) => (
                    <FormItem className='flex items-center justify-between rounded-lg border p-3'>
                      <div className='space-y-0.5'>
                        <FormLabel>{t('channels.dialogs.codexTurnState.fields.enabled.label')}</FormLabel>
                        <FormDescription>{t('channels.dialogs.codexTurnState.fields.enabled.description')}</FormDescription>
                      </div>
                      <FormControl>
                        <Switch checked={field.value} onCheckedChange={field.onChange} />
                      </FormControl>
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='plan'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.codexTurnState.fields.plan.label')}</FormLabel>
                      <Select value={field.value} onValueChange={field.onChange}>
                        <FormControl>
                          <SelectTrigger>
                            <SelectValue />
                          </SelectTrigger>
                        </FormControl>
                        <SelectContent>
                          <SelectItem value='pro'>{t('channels.dialogs.codexTurnState.fields.plan.pro')}</SelectItem>
                          <SelectItem value='team'>{t('channels.dialogs.codexTurnState.fields.plan.team')}</SelectItem>
                        </SelectContent>
                      </Select>
                      <FormDescription>{t('channels.dialogs.codexTurnState.fields.plan.description')}</FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='models'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.codexTurnState.fields.models.label')}</FormLabel>
                      <FormControl>
                        <Input placeholder='gpt-6-astra' {...field} />
                      </FormControl>
                      <FormDescription>{t('channels.dialogs.codexTurnState.fields.models.description')}</FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='harvestProxyURL'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('channels.dialogs.codexTurnState.fields.harvestProxyURL.label')}</FormLabel>
                      <FormControl>
                        <Input placeholder='socks5h://user-sid-{sid}:pass@host:1080' {...field} />
                      </FormControl>
                      <FormDescription>{t('channels.dialogs.codexTurnState.fields.harvestProxyURL.description')}</FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='strict'
                  render={({ field }) => (
                    <FormItem className='flex items-center justify-between rounded-lg border p-3'>
                      <div className='space-y-0.5'>
                        <FormLabel>{t('channels.dialogs.codexTurnState.fields.strict.label')}</FormLabel>
                        <FormDescription>{t('channels.dialogs.codexTurnState.fields.strict.description')}</FormDescription>
                      </div>
                      <FormControl>
                        <Switch checked={field.value} onCheckedChange={field.onChange} />
                      </FormControl>
                    </FormItem>
                  )}
                />
              </CardContent>
            </Card>
            <DialogFooter>
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
  );
}
