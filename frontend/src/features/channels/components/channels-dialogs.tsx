import { useEffect } from 'react';
import { Loader2 } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/button';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { useChannels } from '../context/channels-context';
import { useChannelDetails } from '../data/channels';
import { ChannelsActionDialog } from './channels-action-dialog';
import { ChannelsAPIKeyManagementDialog } from './channels-api-key-management-dialog';
import { ChannelsArchiveDialog } from './channels-archive-dialog';
import { ChannelsAvailabilityDialog } from './channels-availability-dialog';
import { ChannelsBulkApplyTemplateDialog } from './channels-bulk-apply-template-dialog';
import { ChannelsBulkArchiveDialog } from './channels-bulk-archive-dialog';
import { ChannelsBulkAutoDisableDialog } from './channels-bulk-auto-disable-dialog';
import { ChannelsBulkClearTemplateDialog } from './channels-bulk-clear-template-dialog';
import { ChannelsBulkManageTagsDialog } from './channels-bulk-manage-tags-dialog';
import { ChannelsBulkDeleteDialog } from './channels-bulk-delete-dialog';
import { ChannelsBulkDisableDialog } from './channels-bulk-disable-dialog';
import { ChannelsBulkEnableDialog } from './channels-bulk-enable-dialog';
import { ChannelsBulkImportDialog } from './channels-bulk-import-dialog';
import { ChannelsBulkOrderingDialog } from './channels-bulk-ordering-dialog';
import { ChannelsBulkTestDialog } from './channels-bulk-test-dialog';
import { ChannelsDeleteDialog } from './channels-delete-dialog';
import { ChannelsDisabledAPIKeysDialog } from './channels-disabled-api-keys-dialog';
import { ChannelsEndpointsDialog } from './channels-endpoints-dialog';
import { ChannelsErrorResolvedDialog } from './channels-error-resolved-dialog';
import { ChannelsModelMappingDialog } from './channels-model-mapping-dialog';
import { ChannelsModelPriceDialog } from './channels-model-price-dialog';
import { ChannelsOverrideDialog } from './channels-override-dialog';
import { ChannelsProxyDialog } from './channels-proxy-dialog';
import { ChannelsRateLimitDialog } from './channels-rate-limit-dialog';
import { ChannelsStatusDialog } from './channels-status-dialog';
import { ChannelsSystemSettingsDialog } from './channels-system-settings-dialog';
import { ChannelsTestDialog } from './channels-test-dialog';
import { ChannelsTestHistoryDrawer } from './channels-test-history-drawer';
import { ChannelsCodexTurnStateDialog } from './channels-codex-turn-state-dialog';
import { ChannelsTransformOptionsDialog } from './channels-transform-options-dialog';

interface ChannelDetailsLoadDialogProps {
  open: boolean;
  loading: boolean;
  onRetry: () => void;
  onClose: () => void;
}

function ChannelDetailsLoadDialog({ open, loading, onRetry, onClose }: ChannelDetailsLoadDialogProps) {
  const { t } = useTranslation();

  return (
    <Dialog open={open} onOpenChange={(isOpen) => !isOpen && onClose()}>
      <DialogContent className='sm:max-w-[420px]'>
        <DialogHeader>
          <DialogTitle>{t('channels.title')}</DialogTitle>
          <DialogDescription>{loading ? t('common.loading') : t('common.errors.loadFailed')}</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant='outline' onClick={onClose}>
            {t('common.buttons.close')}
          </Button>
          <Button onClick={onRetry} disabled={loading}>
            {loading && <Loader2 className='mr-2 h-4 w-4 animate-spin' />}
            {t('common.buttons.retry')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function ChannelsDialogs() {
  const { open, setOpen, currentRow: partialCurrentRow, setCurrentRow, selectedChannels } = useChannels();
  // Only dialogs that operate on a selected channel need the detail snapshot.
  // Keep the delayed row cleanup from making the add, settings, or bulk dialogs
  // look like a failed channel-detail request.
  const rowDialogOpen =
    open != null && open !== 'add' && open !== 'settings' && open !== 'channelSettings' && !open.startsWith('bulk');
  const detailsQuery = useChannelDetails(partialCurrentRow?.id, {
    enabled: Boolean(partialCurrentRow && rowDialogOpen),
  });

  useEffect(() => {
    if (detailsQuery.data && partialCurrentRow?.id === detailsQuery.data.id && partialCurrentRow !== detailsQuery.data) {
      setCurrentRow(detailsQuery.data);
    }
  }, [detailsQuery.data, partialCurrentRow, setCurrentRow]);

  // Never open a channel form with the partial list row, even if details fail:
  // saving it could clear omitted fields. Context-based dialogs also need the
  // effect above to publish the full snapshot before they initialize.
  const hasFullDetails = partialCurrentRow != null && detailsQuery.data === partialCurrentRow;
  const currentRow = partialCurrentRow && (!open || hasFullDetails) ? partialCurrentRow : null;
  const detailsDialogOpen = Boolean(partialCurrentRow && rowDialogOpen && !currentRow);
  const detailsLoading = detailsQuery.isFetching || (!detailsQuery.isError && !hasFullDetails);
  return (
    <>
      <ChannelDetailsLoadDialog
        open={detailsDialogOpen}
        loading={detailsLoading}
        onRetry={() => void detailsQuery.refetch()}
        onClose={() => {
          setOpen(null);
          setCurrentRow(null);
        }}
      />

      <ChannelsSystemSettingsDialog />

      <ChannelsActionDialog key='channel-add' open={open === 'add'} onOpenChange={(isOpen) => setOpen(isOpen ? 'add' : null)} />

      <ChannelsBulkArchiveDialog />

      <ChannelsBulkDisableDialog />

      <ChannelsBulkEnableDialog />

      <ChannelsBulkTestDialog />

      <ChannelsBulkDeleteDialog />

      <ChannelsBulkManageTagsDialog />

      <ChannelsBulkApplyTemplateDialog
        open={open === 'bulkApplyTemplate'}
        onOpenChange={(isOpen) => setOpen(isOpen ? 'bulkApplyTemplate' : null)}
        selectedChannels={selectedChannels}
      />

      <ChannelsBulkAutoDisableDialog
        open={open === 'bulkAutoDisable'}
        onOpenChange={(isOpen) => setOpen(isOpen ? 'bulkAutoDisable' : null)}
        selectedChannels={selectedChannels}
      />

      <ChannelsBulkClearTemplateDialog />

      <ChannelsBulkImportDialog isOpen={open === 'bulkImport'} onClose={() => setOpen(null)} />

      <ChannelsBulkOrderingDialog open={open === 'bulkOrdering'} onOpenChange={(isOpen) => setOpen(isOpen ? 'bulkOrdering' : null)} />

      {currentRow && (
        <>
          <ChannelsActionDialog
            key={`channel-edit-${currentRow.id}`}
            open={open === 'edit'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('edit');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsActionDialog
            key={`channel-duplicate-${currentRow.id}`}
            open={open === 'duplicate'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('duplicate');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            duplicateFromRow={currentRow}
          />

          <ChannelsActionDialog
            key={`channel-view-models-${currentRow.id}`}
            open={open === 'viewModels'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('viewModels');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
            showModelsPanel={true}
          />

          <ChannelsDeleteDialog
            key={`channel-delete-${currentRow.id}`}
            open={open === 'delete'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          {/* <ChannelsSettingsDialog
            key={`channel-settings-${currentRow.id}`}
            open={open === 'settings'}
            onOpenChange={() => {
              setOpen('settings')
              setTimeout(() => {
                setCurrentRow(null)
              }, 500)
            }}
            currentRow={currentRow}
          /> */}

          <ChannelsModelMappingDialog
            key={`channel-model-mapping-${currentRow.id}`}
            open={open === 'modelMapping'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('modelMapping');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsModelPriceDialog />

          <ChannelsOverrideDialog
            key={`channel-overrides-${currentRow.id}`}
            open={open === 'overrides'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsProxyDialog
            key={`channel-proxy-${currentRow.id}`}
            open={open === 'proxy'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsStatusDialog
            key={`channel-status-${currentRow.id}`}
            open={open === 'status'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('status');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsArchiveDialog
            key={`channel-archive-${currentRow.id}`}
            open={open === 'archive'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('archive');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsTestDialog
            key={`channel-test-${currentRow.id}`}
            open={open === 'test'}
            onOpenChange={(isOpen: boolean) => {
              if (isOpen) {
                setOpen('test');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            channel={currentRow}
          />

          <ChannelsTestHistoryDrawer
            key={`channel-test-history-${currentRow.id}`}
            open={open === 'testHistory'}
            onOpenChange={(isOpen) => {
              if (isOpen) {
                setOpen('testHistory');
              } else {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            channel={currentRow}
          />

          <ChannelsErrorResolvedDialog
            key={`channel-error-resolved-${currentRow.id}`}
            open={open === 'errorResolved'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
          />

          <ChannelsTransformOptionsDialog
            key={`channel-transform-options-${currentRow.id}`}
            open={open === 'transformOptions'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsRateLimitDialog
            key={`channel-rate-limit-${currentRow.id}`}
            open={open === 'rateLimit'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          {(currentRow.type === 'codex' || currentRow.type === 'fenno') && (
            <ChannelsCodexTurnStateDialog
              key={`channel-codex-turn-state-${currentRow.id}`}
              open={open === 'codexTurnState'}
              onOpenChange={(isOpen) => {
                if (!isOpen) {
                  setOpen(null);
                  setTimeout(() => {
                    setCurrentRow(null);
                  }, 500);
                }
              }}
              currentRow={currentRow}
            />
          )}

          <ChannelsEndpointsDialog
            key={`channel-endpoints-${currentRow.id}`}
            open={open === 'endpoints'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            channel={currentRow}
          />

          <ChannelsDisabledAPIKeysDialog
            key={`channel-disabled-api-keys-${currentRow.id}`}
            open={open === 'disabledAPIKeys'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
          />

          <ChannelsAvailabilityDialog
            key={`channel-availability-${currentRow.id}`}
            open={open === 'availability'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
            currentRow={currentRow}
          />

          <ChannelsAPIKeyManagementDialog
            key={`channel-key-management-${currentRow.id}`}
            open={open === 'keyManagement'}
            onOpenChange={(isOpen) => {
              if (!isOpen) {
                setOpen(null);
                setTimeout(() => {
                  setCurrentRow(null);
                }, 500);
              }
            }}
          />
        </>
      )}
    </>
  );
}
