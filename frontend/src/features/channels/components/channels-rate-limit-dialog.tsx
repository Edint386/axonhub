'use client';

import { useEffect } from 'react';
import { z } from 'zod';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { format } from 'date-fns';
import { zhCN, enUS } from 'date-fns/locale';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { AlertTriangle } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form, FormField, FormItem, FormLabel, FormMessage, FormControl, FormDescription } from '@/components/ui/form';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import { Switch } from '@/components/ui/switch';
import { useChannelQuotaUsage, useUpdateChannel } from '../data/channels';
import type { Channel, ChannelQuotaPeriod } from '../data/schema';
import { mergeChannelSettingsForUpdate } from '../utils/merge';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentRow: Channel;
}

const numericField = z
  .union([z.number().int().nonnegative(), z.literal('')])
  .optional()
  .nullable();

const quotaIntField = z
  .union([z.number().int().positive(), z.literal('')])
  .optional()
  .nullable();

const quotaCostField = z
  .union([z.number().nonnegative(), z.literal('')])
  .optional()
  .nullable();

const rateLimitFormSchema = z
  .object({
    rpm: numericField,
    tpm: numericField,
    maxConcurrent: numericField,
    queueSize: numericField,
    queueTimeoutMs: numericField,
    quotaEnabled: z.boolean(),
    quotaRequests: quotaIntField,
    quotaTotalTokens: quotaIntField,
    quotaCost: quotaCostField,
    quotaPeriodType: z.enum(['all_time', 'past_duration', 'calendar_duration']),
    quotaPastDurationValue: quotaIntField,
    quotaPastDurationUnit: z.enum(['minute', 'hour', 'day']),
    quotaCalendarDurationUnit: z.enum(['day', 'month']),
  })
  .superRefine((values, ctx) => {
    const queueSize = values.queueSize;
    const maxConcurrent = values.maxConcurrent;

    if (typeof queueSize === 'number' && queueSize > 0) {
      if (typeof maxConcurrent !== 'number' || maxConcurrent <= 0) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          path: ['queueSize'],
          message: 'queueRequiresMaxConcurrent',
        });
      }
    }

    if (!values.quotaEnabled) {
      return;
    }

    const requests = normalize(values.quotaRequests);
    const totalTokens = normalize(values.quotaTotalTokens);
    const cost = normalize(values.quotaCost);

    if (requests == null && totalTokens == null && cost == null) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['quotaRequests'],
        message: 'quotaAtLeastOneLimit',
      });
    }

    if (values.quotaPeriodType === 'past_duration' && typeof values.quotaPastDurationValue !== 'number') {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ['quotaPastDurationValue'],
        message: 'quotaPastDurationRequired',
      });
    }
  });

type RateLimitFormValues = z.infer<typeof rateLimitFormSchema>;
type RateLimitNumericField = 'rpm' | 'tpm' | 'maxConcurrent' | 'queueSize' | 'queueTimeoutMs';

const emptyDefaults: RateLimitFormValues = {
  rpm: '',
  tpm: '',
  maxConcurrent: '',
  queueSize: '',
  queueTimeoutMs: '',
  quotaEnabled: false,
  quotaRequests: '',
  quotaTotalTokens: '',
  quotaCost: '',
  quotaPeriodType: 'all_time',
  quotaPastDurationValue: 1,
  quotaPastDurationUnit: 'day',
  quotaCalendarDurationUnit: 'day',
};

function valuesFromChannel(currentRow: Channel): RateLimitFormValues {
  const quota = currentRow.settings?.quota;

  return {
    rpm: currentRow.settings?.rateLimit?.rpm ?? '',
    tpm: currentRow.settings?.rateLimit?.tpm ?? '',
    maxConcurrent: currentRow.settings?.rateLimit?.maxConcurrent ?? '',
    queueSize: currentRow.settings?.rateLimit?.queueSize ?? '',
    queueTimeoutMs: currentRow.settings?.rateLimit?.queueTimeoutMs ?? '',
    quotaEnabled: quota != null,
    quotaRequests: quota?.requests ?? '',
    quotaTotalTokens: quota?.totalTokens ?? '',
    quotaCost: quota?.cost ?? '',
    quotaPeriodType: quota?.period?.type ?? 'all_time',
    quotaPastDurationValue: quota?.period?.pastDuration?.value ?? 1,
    quotaPastDurationUnit: quota?.period?.pastDuration?.unit ?? 'day',
    quotaCalendarDurationUnit: quota?.period?.calendarDuration?.unit ?? 'day',
  };
}

function normalize(value: number | '' | null | undefined): number | null {
  return value === '' || value == null ? null : value;
}

function quotaPeriodLabel(period: ChannelQuotaPeriod | null | undefined, t: (key: string) => string) {
  if (!period) return '-';

  const unitLabel = (unit: string) => {
    switch (unit) {
      case 'minute':
        return t('channels.dialogs.rateLimit.quota.unitMinute');
      case 'hour':
        return t('channels.dialogs.rateLimit.quota.unitHour');
      case 'day':
        return t('channels.dialogs.rateLimit.quota.unitDay');
      case 'month':
        return t('channels.dialogs.rateLimit.quota.unitMonth');
      default:
        return unit;
    }
  };

  switch (period.type) {
    case 'all_time':
      return t('channels.dialogs.rateLimit.quota.periodAllTime');
    case 'past_duration': {
      const value = period.pastDuration?.value;
      const unit = period.pastDuration?.unit;
      const suffix = value && unit ? ` (${value} ${unitLabel(unit)})` : '';
      return `${t('channels.dialogs.rateLimit.quota.periodPastDuration')}${suffix}`;
    }
    case 'calendar_duration': {
      const unit = period.calendarDuration?.unit;
      const suffix = unit ? ` (${unitLabel(unit)})` : '';
      return `${t('channels.dialogs.rateLimit.quota.periodCalendarDuration')}${suffix}`;
    }
    default:
      return period.type;
  }
}

function formatLimit(value: number | '' | null | undefined) {
  return typeof value === 'number' ? value.toLocaleString() : '∞';
}

function buildQuotaPeriod(values: RateLimitFormValues): ChannelQuotaPeriod {
  if (values.quotaPeriodType === 'past_duration') {
    return {
      type: 'past_duration',
      pastDuration: {
        value: typeof values.quotaPastDurationValue === 'number' ? values.quotaPastDurationValue : 1,
        unit: values.quotaPastDurationUnit,
      },
      calendarDuration: null,
    };
  }

  if (values.quotaPeriodType === 'calendar_duration') {
    return {
      type: 'calendar_duration',
      pastDuration: null,
      calendarDuration: {
        unit: values.quotaCalendarDurationUnit,
      },
    };
  }

  return {
    type: 'all_time',
    pastDuration: null,
    calendarDuration: null,
  };
}

export function ChannelsRateLimitDialog({ open, onOpenChange, currentRow }: Props) {
  const { t, i18n } = useTranslation();
  const updateChannel = useUpdateChannel();
  const locale = i18n.language.startsWith('zh') ? zhCN : enUS;

  const form = useForm<RateLimitFormValues>({
    resolver: zodResolver(rateLimitFormSchema),
    defaultValues: valuesFromChannel(currentRow),
    mode: 'onChange',
  });

  useEffect(() => {
    if (open) {
      form.reset(valuesFromChannel(currentRow));
    }
  }, [open, currentRow, form]);

  // Soft-mode advisory: when the user sets MaxConcurrent without a queue, the
  // limiter only down-ranks the channel in load-balancer scoring. It does not
  // block excess requests unless a queue is configured.
  const watchedMaxConcurrent = form.watch('maxConcurrent');
  const watchedQueueSize = form.watch('queueSize');
  const quotaEnabled = form.watch('quotaEnabled');
  const quotaPeriodType = form.watch('quotaPeriodType');
  const quotaValues = form.watch();
  const showSoftModeHint =
    typeof watchedMaxConcurrent === 'number' &&
    watchedMaxConcurrent > 0 &&
    (typeof watchedQueueSize !== 'number' || watchedQueueSize <= 0);

  const quotaUsageQuery = useChannelQuotaUsage(currentRow.id, {
    enabled: open && !!currentRow.id && (quotaEnabled || currentRow.settings?.quota != null),
    refetchInterval: open ? 10000 : undefined,
  });
  const quotaUsage = quotaUsageQuery.data ?? undefined;
  const quotaUsagePeriod = quotaEnabled ? buildQuotaPeriod(quotaValues) : quotaUsage?.quota?.period;
  const quotaUsageEnd = quotaUsage?.window.end ?? (quotaUsagePeriod?.type !== 'calendar_duration' ? new Date() : null);

  const onSubmit = async (values: RateLimitFormValues) => {
    try {
      const rateLimit = {
        rpm: normalize(values.rpm),
        tpm: normalize(values.tpm),
        maxConcurrent: normalize(values.maxConcurrent),
        queueSize: normalize(values.queueSize),
        queueTimeoutMs: normalize(values.queueTimeoutMs),
      };

      const allEmpty =
        rateLimit.rpm == null &&
        rateLimit.tpm == null &&
        rateLimit.maxConcurrent == null &&
        rateLimit.queueSize == null &&
        rateLimit.queueTimeoutMs == null;

      const quota = values.quotaEnabled
        ? {
            requests: normalize(values.quotaRequests),
            totalTokens: normalize(values.quotaTotalTokens),
            cost: normalize(values.quotaCost),
            period: buildQuotaPeriod(values),
          }
        : null;

      const rateLimitValue = allEmpty ? null : rateLimit;

      const nextSettings = mergeChannelSettingsForUpdate(currentRow.settings, {
        rateLimit: rateLimitValue,
        quota,
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

  const renderNumericField = (name: RateLimitNumericField, labelKey: string, placeholderKey: string, descriptionKey: string) => (
    <FormField
      control={form.control}
      name={name}
      render={({ field }) => (
        <FormItem>
          <FormLabel>{t(labelKey)}</FormLabel>
          <FormControl>
            <Input
              type='number'
              min={0}
              placeholder={t(placeholderKey)}
              value={field.value === '' || field.value == null ? '' : field.value}
              onChange={(e) => {
                const val = e.target.value;
                field.onChange(val === '' ? '' : parseInt(val, 10));
              }}
            />
          </FormControl>
          <FormDescription>{t(descriptionKey)}</FormDescription>
          <FormMessage />
        </FormItem>
      )}
    />
  );

  const renderQuotaLimitField = (name: 'quotaRequests' | 'quotaTotalTokens' | 'quotaCost', labelKey: string, placeholderKey: string) => (
    <FormField
      control={form.control}
      name={name}
      render={({ field, fieldState }) => (
        <FormItem>
          <FormLabel>{t(labelKey)}</FormLabel>
          <FormControl>
            <Input
              type='number'
              min={name === 'quotaCost' ? 0 : 1}
              step={name === 'quotaCost' ? '0.000001' : 1}
              placeholder={t(placeholderKey)}
              value={field.value === '' || field.value == null ? '' : field.value}
              onChange={(e) => {
                const val = e.target.value;
                field.onChange(val === '' ? '' : name === 'quotaCost' ? Number(val) : parseInt(val, 10));
              }}
            />
          </FormControl>
          {fieldState.error?.message === 'quotaAtLeastOneLimit' ? (
            <p className='text-destructive text-sm'>{t('channels.dialogs.rateLimit.quota.errors.atLeastOneLimit')}</p>
          ) : (
            <FormMessage />
          )}
        </FormItem>
      )}
    />
  );

  return (
    <Dialog
      open={open}
      onOpenChange={(state) => {
        if (!state) {
          form.reset(emptyDefaults);
        }
        onOpenChange(state);
      }}
    >
      <DialogContent className='max-h-[90vh] overflow-y-auto sm:max-w-2xl'>
        <DialogHeader className='text-left'>
          <DialogTitle>{t('channels.dialogs.rateLimit.title')}</DialogTitle>
          <DialogDescription>{t('channels.dialogs.rateLimit.description', { name: currentRow.name })}</DialogDescription>
        </DialogHeader>

        <div className='space-y-6'>
          <Form {...form}>
            <form className='space-y-6'>
              <Card>
                <CardHeader>
                  <CardTitle className='text-lg'>{t('channels.dialogs.rateLimit.config.title')}</CardTitle>
                  <CardDescription>{t('channels.dialogs.rateLimit.config.description')}</CardDescription>
                </CardHeader>
                <CardContent className='space-y-4'>
                  {renderNumericField(
                    'rpm',
                    'channels.dialogs.rateLimit.fields.rpm.label',
                    'channels.dialogs.rateLimit.fields.rpm.placeholder',
                    'channels.dialogs.rateLimit.fields.rpm.description'
                  )}

                  {renderNumericField(
                    'tpm',
                    'channels.dialogs.rateLimit.fields.tpm.label',
                    'channels.dialogs.rateLimit.fields.tpm.placeholder',
                    'channels.dialogs.rateLimit.fields.tpm.description'
                  )}

                  {renderNumericField(
                    'maxConcurrent',
                    'channels.dialogs.rateLimit.fields.maxConcurrent.label',
                    'channels.dialogs.rateLimit.fields.maxConcurrent.placeholder',
                    'channels.dialogs.rateLimit.fields.maxConcurrent.description'
                  )}

                  <FormField
                    control={form.control}
                    name='queueSize'
                    render={({ field, fieldState }) => (
                      <FormItem>
                        <FormLabel>{t('channels.dialogs.rateLimit.fields.queueSize.label')}</FormLabel>
                        <FormControl>
                          <Input
                            type='number'
                            min={0}
                            placeholder={t('channels.dialogs.rateLimit.fields.queueSize.placeholder')}
                            value={field.value === '' || field.value == null ? '' : field.value}
                            onChange={(e) => {
                              const val = e.target.value;
                              field.onChange(val === '' ? '' : parseInt(val, 10));
                            }}
                          />
                        </FormControl>
                        <FormDescription>{t('channels.dialogs.rateLimit.fields.queueSize.description')}</FormDescription>
                        {showSoftModeHint && (
                          <div className='mt-1 flex items-start gap-2 rounded-md border border-amber-300 bg-amber-50 p-2 text-xs text-amber-900 dark:border-amber-700/50 dark:bg-amber-950/40 dark:text-amber-200'>
                            <AlertTriangle className='mt-0.5 h-3.5 w-3.5 shrink-0' />
                            <span>{t('channels.dialogs.rateLimit.hints.softModeWarning')}</span>
                          </div>
                        )}
                        {fieldState.error?.message === 'queueRequiresMaxConcurrent' ? (
                          <p className='text-destructive text-sm'>
                            {t('channels.dialogs.rateLimit.errors.queueRequiresMaxConcurrent')}
                          </p>
                        ) : (
                          <FormMessage />
                        )}
                      </FormItem>
                    )}
                  />

                  {renderNumericField(
                    'queueTimeoutMs',
                    'channels.dialogs.rateLimit.fields.queueTimeoutMs.label',
                    'channels.dialogs.rateLimit.fields.queueTimeoutMs.placeholder',
                    'channels.dialogs.rateLimit.fields.queueTimeoutMs.description'
                  )}
                </CardContent>
              </Card>

              <Card>
                <CardHeader>
                  <div className='flex items-start justify-between gap-4'>
                    <div>
                      <CardTitle className='text-lg'>{t('channels.dialogs.rateLimit.quota.title')}</CardTitle>
                      <CardDescription>{t('channels.dialogs.rateLimit.quota.description')}</CardDescription>
                    </div>
                    <FormField
                      control={form.control}
                      name='quotaEnabled'
                      render={({ field }) => (
                        <FormItem className='flex items-center space-y-0 gap-x-2'>
                          <FormLabel className='text-sm'>{t('channels.dialogs.rateLimit.quota.enabled')}</FormLabel>
                          <FormControl>
                            <Switch checked={field.value} onCheckedChange={field.onChange} />
                          </FormControl>
                        </FormItem>
                      )}
                    />
                  </div>
                </CardHeader>

                {quotaEnabled && (
                  <CardContent className='space-y-4'>
                    <div className='grid gap-4 md:grid-cols-3'>
                      {renderQuotaLimitField(
                        'quotaRequests',
                        'channels.dialogs.rateLimit.quota.requests.label',
                        'channels.dialogs.rateLimit.quota.requests.placeholder'
                      )}
                      {renderQuotaLimitField(
                        'quotaTotalTokens',
                        'channels.dialogs.rateLimit.quota.totalTokens.label',
                        'channels.dialogs.rateLimit.quota.totalTokens.placeholder'
                      )}
                      {renderQuotaLimitField(
                        'quotaCost',
                        'channels.dialogs.rateLimit.quota.cost.label',
                        'channels.dialogs.rateLimit.quota.cost.placeholder'
                      )}
                    </div>

                    <div className='grid gap-4 md:grid-cols-3'>
                      <FormField
                        control={form.control}
                        name='quotaPeriodType'
                        render={({ field }) => (
                          <FormItem>
                            <FormLabel>{t('channels.dialogs.rateLimit.quota.periodType')}</FormLabel>
                            <FormControl>
                              <Select
                                value={field.value}
                                onValueChange={(value) => {
                                  field.onChange(value);
                                  if (value === 'past_duration') {
                                    form.setValue('quotaPastDurationValue', 1, { shouldValidate: true });
                                    form.setValue('quotaPastDurationUnit', 'day', { shouldValidate: true });
                                  }
                                  if (value === 'calendar_duration') {
                                    form.setValue('quotaCalendarDurationUnit', 'day', { shouldValidate: true });
                                  }
                                }}
                              >
                                <SelectTrigger>
                                  <SelectValue />
                                </SelectTrigger>
                                <SelectContent>
                                  <SelectItem value='all_time'>{t('channels.dialogs.rateLimit.quota.periodAllTime')}</SelectItem>
                                  <SelectItem value='past_duration'>
                                    {t('channels.dialogs.rateLimit.quota.periodPastDuration')}
                                  </SelectItem>
                                  <SelectItem value='calendar_duration'>
                                    {t('channels.dialogs.rateLimit.quota.periodCalendarDuration')}
                                  </SelectItem>
                                </SelectContent>
                              </Select>
                            </FormControl>
                            <FormMessage />
                          </FormItem>
                        )}
                      />

                      {quotaPeriodType === 'past_duration' && (
                        <>
                          <FormField
                            control={form.control}
                            name='quotaPastDurationValue'
                            render={({ field, fieldState }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.rateLimit.quota.pastDurationValue')}</FormLabel>
                                <FormControl>
                                  <Input
                                    type='number'
                                    min={1}
                                    value={field.value === '' || field.value == null ? '' : field.value}
                                    onChange={(e) => {
                                      const val = e.target.value;
                                      field.onChange(val === '' ? '' : parseInt(val, 10));
                                    }}
                                  />
                                </FormControl>
                                {fieldState.error?.message === 'quotaPastDurationRequired' ? (
                                  <p className='text-destructive text-sm'>
                                    {t('channels.dialogs.rateLimit.quota.errors.pastDurationRequired')}
                                  </p>
                                ) : (
                                  <FormMessage />
                                )}
                              </FormItem>
                            )}
                          />
                          <FormField
                            control={form.control}
                            name='quotaPastDurationUnit'
                            render={({ field }) => (
                              <FormItem>
                                <FormLabel>{t('channels.dialogs.rateLimit.quota.pastDurationUnit')}</FormLabel>
                                <FormControl>
                                  <Select value={field.value} onValueChange={field.onChange}>
                                    <SelectTrigger>
                                      <SelectValue />
                                    </SelectTrigger>
                                    <SelectContent>
                                      <SelectItem value='minute'>{t('channels.dialogs.rateLimit.quota.unitMinute')}</SelectItem>
                                      <SelectItem value='hour'>{t('channels.dialogs.rateLimit.quota.unitHour')}</SelectItem>
                                      <SelectItem value='day'>{t('channels.dialogs.rateLimit.quota.unitDay')}</SelectItem>
                                    </SelectContent>
                                  </Select>
                                </FormControl>
                                <FormMessage />
                              </FormItem>
                            )}
                          />
                        </>
                      )}

                      {quotaPeriodType === 'calendar_duration' && (
                        <FormField
                          control={form.control}
                          name='quotaCalendarDurationUnit'
                          render={({ field }) => (
                            <FormItem>
                              <FormLabel>{t('channels.dialogs.rateLimit.quota.calendarDurationUnit')}</FormLabel>
                              <FormControl>
                                <Select value={field.value} onValueChange={field.onChange}>
                                  <SelectTrigger>
                                    <SelectValue />
                                  </SelectTrigger>
                                  <SelectContent>
                                    <SelectItem value='day'>{t('channels.dialogs.rateLimit.quota.unitDay')}</SelectItem>
                                    <SelectItem value='month'>{t('channels.dialogs.rateLimit.quota.unitMonth')}</SelectItem>
                                  </SelectContent>
                                </Select>
                              </FormControl>
                              <FormMessage />
                            </FormItem>
                          )}
                        />
                      )}
                    </div>

                    {quotaUsage && (
                      <div className='space-y-2 rounded-md border p-3'>
                        <div className='text-xs font-medium'>{t('channels.dialogs.rateLimit.quota.usageTitle')}</div>
                        <div className='text-muted-foreground text-xs'>
                          {t('channels.dialogs.rateLimit.quota.periodType')}: {quotaPeriodLabel(quotaUsagePeriod, t)}
                        </div>
                        <div className='grid gap-3 md:grid-cols-3'>
                          <div>
                            <div className='text-muted-foreground text-xs'>
                              {t('channels.dialogs.rateLimit.quota.requests.label')}
                            </div>
                            <div className='text-sm'>
                              {quotaUsage.usage.requestCount.toLocaleString()}/{formatLimit(quotaValues.quotaRequests)}
                            </div>
                          </div>
                          <div>
                            <div className='text-muted-foreground text-xs'>
                              {t('channels.dialogs.rateLimit.quota.totalTokens.label')}
                            </div>
                            <div className='text-sm'>
                              {quotaUsage.usage.totalTokens.toLocaleString()}/{formatLimit(quotaValues.quotaTotalTokens)}
                            </div>
                          </div>
                          <div>
                            <div className='text-muted-foreground text-xs'>{t('channels.dialogs.rateLimit.quota.cost.label')}</div>
                            <div className='text-sm'>
                              {(quotaUsage.usage.totalCost ?? 0).toLocaleString(undefined, { maximumFractionDigits: 6 })}/
                              {formatLimit(quotaValues.quotaCost)}
                            </div>
                          </div>
                        </div>
                        <div className='text-muted-foreground grid gap-2 text-xs md:grid-cols-2'>
                          <div>
                            {t('common.filters.startTime')}{' '}
                            {quotaUsage.window.start ? format(quotaUsage.window.start, 'PPpp', { locale }) : '-'}
                          </div>
                          <div>
                            {t('common.filters.endTime')} {quotaUsageEnd ? format(quotaUsageEnd, 'PPpp', { locale }) : '-'}
                          </div>
                        </div>
                      </div>
                    )}
                  </CardContent>
                )}
              </Card>
            </form>
          </Form>
        </div>

        <DialogFooter>
          <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
            {t('common.buttons.cancel')}
          </Button>
          <Button
            type='button'
            onClick={form.handleSubmit(onSubmit)}
            disabled={updateChannel.isPending || !form.formState.isValid}
          >
            {updateChannel.isPending ? t('common.buttons.saving') : t('common.buttons.save')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
