'use client';

import { useEffect } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form } from '@/components/ui/form';
import { useProxyPresets } from '@/features/system/data/system';
import { normalizeProxyConfig, proxyConfigSchema, type ProxyConfig } from '../data/proxy-config';
import { ProxyConfigFields } from './channels-proxy-dialog';

interface Props {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  value: ProxyConfig;
  onApply: (value: ProxyConfig) => void;
}

export function OAuthProxyDialog({ open, onOpenChange, value, onApply }: Props) {
  const { t } = useTranslation();
  const { data: proxyPresets = [] } = useProxyPresets();
  const form = useForm<ProxyConfig>({
    resolver: zodResolver(proxyConfigSchema),
    defaultValues: value,
  });

  useEffect(() => {
    if (open) form.reset(value);
  }, [form, open, value]);

  const handleApply = (nextValue: ProxyConfig) => {
    onApply(normalizeProxyConfig(nextValue));
    onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='sm:max-w-2xl' data-testid='oauth-proxy-dialog'>
        <DialogHeader className='text-left'>
          <DialogTitle>{t('channels.dialogs.oauth.proxy.title')}</DialogTitle>
          <DialogDescription>{t('channels.dialogs.oauth.proxy.description')}</DialogDescription>
        </DialogHeader>

        <Card>
          <CardHeader>
            <CardTitle className='text-lg'>{t('channels.dialogs.proxy.config.title')}</CardTitle>
            <CardDescription>{t('channels.dialogs.proxy.config.description')}</CardDescription>
          </CardHeader>
          <CardContent>
            <Form {...form}>
              <form id='oauth-proxy-form' className='space-y-4' onSubmit={form.handleSubmit(handleApply)}>
                <ProxyConfigFields form={form} proxyPresets={proxyPresets} />
              </form>
            </Form>
          </CardContent>
        </Card>

        <DialogFooter>
          <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
            {t('common.buttons.cancel')}
          </Button>
          <Button type='submit' form='oauth-proxy-form' data-testid='oauth-proxy-apply-button'>
            {t('channels.dialogs.oauth.proxy.apply')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
