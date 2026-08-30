import { useEffect, useMemo, useState } from 'react';
import { IconKey, IconLoader2, IconSearch, IconShieldLock } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Checkbox } from '@/components/ui/checkbox';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import { Label } from '@/components/ui/label';
import { RadioGroup, RadioGroupItem } from '@/components/ui/radio-group';
import { ScrollArea } from '@/components/ui/scroll-area';
import type { ApiKey } from '@/features/apikeys/data/schema';
import { useChannelAccessAPIKeys, useChannelCallerAccessPolicy, useSetChannelCallerAccessPolicy } from '../data/channel-access';
import type { CallerAPIKeySummary, ChannelCallerAccessMode } from '../data/schema';

type FocusedAPIKey = Pick<ApiKey, 'id' | 'name'>;

interface ChannelAccessPolicyDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  channelID: string;
  channelName?: string;
  focusedAPIKey?: FocusedAPIKey;
}

const modes: ChannelCallerAccessMode[] = ['public', 'allowlist', 'denylist'];

export function ChannelAccessPolicyDialog({ open, onOpenChange, channelID, channelName, focusedAPIKey }: ChannelAccessPolicyDialogProps) {
  const { t } = useTranslation();
  const { hasSystemScope } = usePermissions();
  const canRead = hasSystemScope('read_channels') && hasSystemScope('read_api_keys');
  const canWrite = hasSystemScope('write_channels') && hasSystemScope('write_api_keys');
  const policyQuery = useChannelCallerAccessPolicy(channelID, { enabled: open && canRead });
  const apiKeysQuery = useChannelAccessAPIKeys({ enabled: open && canRead });
  const updatePolicy = useSetChannelCallerAccessPolicy();
  const [mode, setMode] = useState<ChannelCallerAccessMode>('public');
  const [memberIDs, setMemberIDs] = useState<Set<string>>(new Set());
  const [search, setSearch] = useState('');

  useEffect(() => {
    if (!open || !policyQuery.data) return;
    setMode(policyQuery.data.mode);
    setMemberIDs(new Set(policyQuery.data.members.map((member) => member.id)));
    setSearch('');
  }, [open, policyQuery.data]);

  const availableAPIKeys = useMemo(() => {
    const byID = new Map<string, CallerAPIKeySummary>();
    for (const member of policyQuery.data?.members ?? []) byID.set(member.id, member);
    for (const apiKey of apiKeysQuery.data ?? []) {
      byID.set(apiKey.id, apiKey);
    }
    return Array.from(byID.values()).sort((a, b) => a.name.localeCompare(b.name));
  }, [apiKeysQuery.data, policyQuery.data?.members]);

  const filteredAPIKeys = useMemo(() => {
    const normalizedSearch = search.trim().toLocaleLowerCase();
    if (!normalizedSearch) return availableAPIKeys;
    return availableAPIKeys.filter((apiKey) => apiKey.name.toLocaleLowerCase().includes(normalizedSearch));
  }, [availableAPIKeys, search]);

  const toggleMember = (apiKeyID: string, checked: boolean) => {
    setMemberIDs((current) => {
      const next = new Set(current);
      if (checked) next.add(apiKeyID);
      else next.delete(apiKeyID);
      return next;
    });
  };

  const setExclusive = (apiKeyID: string) => {
    setMode('allowlist');
    setMemberIDs(new Set([apiKeyID]));
  };

  const handleSave = async () => {
    if (!policyQuery.data || !canWrite) return;
    await updatePolicy.mutateAsync({
      channelID,
      mode,
      memberAPIKeyIDs: mode === 'public' ? [] : Array.from(memberIDs),
    });
    onOpenChange(false);
  };

  const effectiveChannelName = policyQuery.data?.channel.name ?? channelName ?? '';
  const isLoading = policyQuery.isLoading || apiKeysQuery.isLoading;

  if (!canRead) return null;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className='flex max-h-[90vh] flex-col overflow-hidden sm:max-w-3xl'>
        <DialogHeader>
          <DialogTitle className='flex items-center gap-2'>
            <IconShieldLock className='h-5 w-5' />
            {t('channels.callerAccess.title', { name: effectiveChannelName })}
          </DialogTitle>
          <DialogDescription>{t('channels.callerAccess.description')}</DialogDescription>
        </DialogHeader>

        {isLoading ? (
          <div className='text-muted-foreground flex min-h-60 items-center justify-center gap-2 text-sm'>
            <IconLoader2 className='h-4 w-4 animate-spin' />
            {t('channels.callerAccess.loading')}
          </div>
        ) : policyQuery.data ? (
          <div className='flex min-h-0 flex-1 flex-col gap-5 overflow-hidden'>
            <RadioGroup
              value={mode}
              onValueChange={(value) => setMode(value as ChannelCallerAccessMode)}
              className='grid gap-3 md:grid-cols-3'
              disabled={!canWrite}
            >
              {modes.map((item) => (
                <Label
                  key={item}
                  htmlFor={`caller-access-mode-${item}`}
                  className='hover:bg-muted/50 flex cursor-pointer items-start gap-3 rounded-lg border p-3'
                >
                  <RadioGroupItem id={`caller-access-mode-${item}`} value={item} className='mt-0.5' />
                  <span className='space-y-1'>
                    <span className='block font-medium'>{t(`channels.callerAccess.modes.${item}.label`)}</span>
                    <span className='text-muted-foreground block text-xs leading-relaxed font-normal'>
                      {t(`channels.callerAccess.modes.${item}.description`)}
                    </span>
                  </span>
                </Label>
              ))}
            </RadioGroup>

            {mode === 'public' ? (
              <div className='bg-muted/40 rounded-lg border p-4 text-sm'>{t('channels.callerAccess.publicHint')}</div>
            ) : (
              <div className='flex min-h-0 flex-1 flex-col gap-3'>
                <div className='flex flex-wrap items-center justify-between gap-2'>
                  <div>
                    <div className='font-medium'>{t('channels.callerAccess.members.title')}</div>
                    <div className='text-muted-foreground text-xs'>
                      {t('channels.callerAccess.members.selectedCount', { count: memberIDs.size })}
                    </div>
                  </div>
                  {focusedAPIKey && canWrite && (
                    <Button type='button' variant='secondary' size='sm' onClick={() => setExclusive(focusedAPIKey.id)}>
                      <IconKey className='mr-2 h-4 w-4' />
                      {t('channels.callerAccess.actions.exclusiveTo', { name: focusedAPIKey.name })}
                    </Button>
                  )}
                </div>

                <div className='relative'>
                  <IconSearch className='text-muted-foreground absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2' />
                  <Input
                    value={search}
                    onChange={(event) => setSearch(event.target.value)}
                    placeholder={t('channels.callerAccess.members.searchPlaceholder')}
                    className='pl-9'
                  />
                </div>

                <ScrollArea className='min-h-0 flex-1 rounded-md border'>
                  <div className='divide-y'>
                    {filteredAPIKeys.length === 0 ? (
                      <div className='text-muted-foreground p-8 text-center text-sm'>{t('channels.callerAccess.members.empty')}</div>
                    ) : (
                      filteredAPIKeys.map((apiKey) => {
                        const checked = memberIDs.has(apiKey.id);
                        return (
                          <div key={apiKey.id} className='flex items-center gap-3 p-3'>
                            <Checkbox
                              id={`caller-access-key-${apiKey.id}`}
                              checked={checked}
                              onCheckedChange={(value) => toggleMember(apiKey.id, value === true)}
                              disabled={!canWrite}
                            />
                            <Label htmlFor={`caller-access-key-${apiKey.id}`} className='min-w-0 flex-1 cursor-pointer'>
                              <span className='truncate'>{apiKey.name}</span>
                              {apiKey.projectName && (
                                <span className='text-muted-foreground mt-0.5 block truncate text-xs'>{apiKey.projectName}</span>
                              )}
                            </Label>
                            <Badge variant='outline'>{t(`apikeys.status.${apiKey.status}`)}</Badge>
                            {canWrite && (
                              <Button type='button' variant='ghost' size='sm' onClick={() => setExclusive(apiKey.id)}>
                                {t('channels.callerAccess.actions.exclusive')}
                              </Button>
                            )}
                          </div>
                        );
                      })
                    )}
                  </div>
                </ScrollArea>

                {mode === 'allowlist' && memberIDs.size === 0 && (
                  <div className='border-destructive/40 bg-destructive/5 text-destructive rounded-md border px-3 py-2 text-sm'>
                    {t('channels.callerAccess.members.emptyAllowlistWarning')}
                  </div>
                )}
              </div>
            )}
          </div>
        ) : null}

        <DialogFooter>
          <Button type='button' variant='outline' onClick={() => onOpenChange(false)}>
            {t('common.buttons.cancel')}
          </Button>
          {canWrite && (
            <Button type='button' onClick={handleSave} disabled={!policyQuery.data || updatePolicy.isPending}>
              {updatePolicy.isPending && <IconLoader2 className='mr-2 h-4 w-4 animate-spin' />}
              {t('common.buttons.saveChanges')}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
