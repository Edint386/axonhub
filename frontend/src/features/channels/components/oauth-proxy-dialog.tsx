'use client';

import { useEffect, useRef, useState } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { IconPlayerPlay } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Form } from '@/components/ui/form';
import { useProxyPresets } from '@/features/system/data/system';
import { useSelectedProjectId } from '@/stores/projectStore';
import { codexOAuthTestProxy } from '../data/codex';
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
  const selectedProjectId = useSelectedProjectId();
  const testGenerationRef = useRef(0);
  const [isTesting, setIsTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ success: boolean; latencyMs?: number; message?: string } | null>(null);
  const form = useForm<ProxyConfig>({
    resolver: zodResolver(proxyConfigSchema),
    defaultValues: value,
  });

  useEffect(() => {
    testGenerationRef.current += 1;
    setIsTesting(false);
    setTestResult(null);

    if (!open) return;

    form.reset(value);
  }, [form, open, value]);

  useEffect(() => {
    const subscription = form.watch(() => {
      testGenerationRef.current += 1;
      setIsTesting(false);
      setTestResult(null);
    });
    return () => subscription.unsubscribe();
  }, [form]);

  const handleApply = (nextValue: ProxyConfig) => {
    onApply(normalizeProxyConfig(nextValue));
    onOpenChange(false);
  };

  const handleTest = form.handleSubmit(async (nextValue) => {
    const generation = testGenerationRef.current + 1;
    testGenerationRef.current = generation;
    setIsTesting(true);
    setTestResult(null);

    try {
      const headers = selectedProjectId ? { 'X-Project-ID': selectedProjectId } : undefined;
      const result = await codexOAuthTestProxy({ proxy: normalizeProxyConfig(nextValue) }, headers);

      if (generation !== testGenerationRef.current) return;

      setTestResult({ success: result.success, latencyMs: result.latencyMs });
      if (result.success) {
        toast.success(t('channels.dialogs.proxy.testSuccess'));
      } else {
        toast.error(t('channels.dialogs.proxy.testFailed'));
      }
    } catch (error) {
      if (generation !== testGenerationRef.current) return;

      const message = error instanceof Error ? error.message : t('channels.dialogs.proxy.testFailed');
      setTestResult({ success: false, message });
      toast.error(t('channels.dialogs.proxy.testFailed'));
    } finally {
      if (generation === testGenerationRef.current) setIsTesting(false);
    }
  });

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

        {testResult && (
          <div
            className={`rounded-md border p-3 text-sm ${testResult.success ? 'border-green-200 text-green-800' : 'border-red-200 text-red-800'}`}
            data-testid='oauth-proxy-test-result'
          >
            <p>{testResult.success ? t('channels.dialogs.proxy.testSuccess') : t('channels.dialogs.proxy.testFailed')}</p>
            {testResult.success && testResult.latencyMs != null && (
              <p>
                {t('channels.dialogs.proxy.latency')}: {(testResult.latencyMs / 1000).toFixed(2)}s
              </p>
            )}
            {!testResult.success && testResult.message && <p>{testResult.message}</p>}
          </div>
        )}

        <DialogFooter className='flex justify-between'>
          <Button type='button' variant='outline' onClick={handleTest} disabled={isTesting} data-testid='oauth-proxy-test-button'>
            <IconPlayerPlay className='mr-2 h-4 w-4' />
            {isTesting ? t('channels.dialogs.proxy.testing') : t('channels.dialogs.proxy.test')}
          </Button>
          <div className='flex gap-2'>
            <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
              {t('common.buttons.cancel')}
            </Button>
            <Button type='submit' form='oauth-proxy-form' data-testid='oauth-proxy-apply-button' disabled={isTesting}>
              {t('channels.dialogs.oauth.proxy.apply')}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
