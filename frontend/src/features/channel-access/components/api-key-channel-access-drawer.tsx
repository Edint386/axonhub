import { useEffect, useMemo, useState } from 'react';
import { IconEdit, IconLoader2, IconSearch, IconShieldCheck, IconShieldX } from '@tabler/icons-react';
import { useTranslation } from 'react-i18next';
import { usePermissions } from '@/hooks/usePermissions';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { ScrollArea } from '@/components/ui/scroll-area';
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from '@/components/ui/sheet';
import type { ApiKey } from '@/features/apikeys/data/schema';
import { useAPIKeyChannelCallerAccess } from '../data/channel-access';
import type { APIKeyChannelCallerAccess } from '../data/schema';
import { ChannelAccessPolicyDialog } from './channel-access-policy-dialog';

interface APIKeyChannelAccessDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  apiKey: Pick<ApiKey, 'id' | 'name'> | null;
}

export function APIKeyChannelAccessDrawer({ open, onOpenChange, apiKey }: APIKeyChannelAccessDrawerProps) {
  const { t } = useTranslation();
  const { hasSystemScope } = usePermissions();
  const canRead = hasSystemScope('read_channels') && hasSystemScope('read_api_keys');
  const canWrite = hasSystemScope('write_channels') && hasSystemScope('write_api_keys');
  const accessQuery = useAPIKeyChannelCallerAccess(apiKey?.id ?? '', { enabled: open && canRead });
  const [search, setSearch] = useState('');
  const [editingAccess, setEditingAccess] = useState<APIKeyChannelCallerAccess | null>(null);

  useEffect(() => {
    setSearch('');
    setEditingAccess(null);
  }, [open, apiKey?.id]);

  const filteredAccess = useMemo(() => {
    const normalizedSearch = search.trim().toLocaleLowerCase();
    const entries = accessQuery.data ?? [];
    if (!normalizedSearch) return entries;
    return entries.filter((entry) => entry.channel.name.toLocaleLowerCase().includes(normalizedSearch));
  }, [accessQuery.data, search]);

  if (!canRead) return null;

  return (
    <>
      <Sheet open={open} onOpenChange={onOpenChange}>
        <SheetContent className='w-full gap-0 p-0 sm:max-w-2xl'>
          <SheetHeader className='border-b p-5 pr-12'>
            <SheetTitle>{t('apikeys.channelAccess.title', { name: apiKey?.name ?? '' })}</SheetTitle>
            <SheetDescription>{t('apikeys.channelAccess.description')}</SheetDescription>
          </SheetHeader>

          <div className='border-b p-4'>
            <div className='relative'>
              <IconSearch className='text-muted-foreground absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2' />
              <Input
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder={t('apikeys.channelAccess.searchPlaceholder')}
                className='pl-9'
              />
            </div>
          </div>

          <ScrollArea className='min-h-0 flex-1'>
            {accessQuery.isLoading ? (
              <div className='text-muted-foreground flex min-h-60 items-center justify-center gap-2 text-sm'>
                <IconLoader2 className='h-4 w-4 animate-spin' />
                {t('apikeys.channelAccess.loading')}
              </div>
            ) : filteredAccess.length === 0 ? (
              <div className='text-muted-foreground p-10 text-center text-sm'>{t('apikeys.channelAccess.empty')}</div>
            ) : (
              <div className='divide-y'>
                {filteredAccess.map((entry) => (
                  <div key={entry.channel.id} className='flex items-center gap-3 p-4'>
                    {entry.allowed ? (
                      <IconShieldCheck className='h-5 w-5 shrink-0 text-green-600' />
                    ) : (
                      <IconShieldX className='text-destructive h-5 w-5 shrink-0' />
                    )}
                    <div className='min-w-0 flex-1'>
                      <div className='flex flex-wrap items-center gap-2'>
                        <span className='truncate font-medium'>{entry.channel.name}</span>
                        <Badge variant='outline'>{t(`channels.callerAccess.modes.${entry.mode}.label`)}</Badge>
                        <Badge variant={entry.allowed ? 'default' : 'destructive'}>
                          {t(entry.allowed ? 'apikeys.channelAccess.allowed' : 'apikeys.channelAccess.denied')}
                        </Badge>
                      </div>
                      <div className='text-muted-foreground mt-1 text-xs'>
                        {t(`apikeys.channelAccess.reasons.${entry.mode}.${entry.isMember ? 'member' : 'nonMember'}`)}
                      </div>
                    </div>
                    {canWrite && (
                      <Button type='button' variant='outline' size='sm' onClick={() => setEditingAccess(entry)}>
                        <IconEdit className='mr-2 h-4 w-4' />
                        {t('common.actions.edit')}
                      </Button>
                    )}
                  </div>
                ))}
              </div>
            )}
          </ScrollArea>
        </SheetContent>
      </Sheet>

      {editingAccess && apiKey && (
        <ChannelAccessPolicyDialog
          open={Boolean(editingAccess)}
          onOpenChange={(nextOpen) => !nextOpen && setEditingAccess(null)}
          channelID={editingAccess.channel.id}
          channelName={editingAccess.channel.name}
          focusedAPIKey={apiKey}
        />
      )}
    </>
  );
}
