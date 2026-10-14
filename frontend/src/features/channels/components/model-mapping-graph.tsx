import { useEffect, useMemo, useRef, useState } from 'react';
import type { KeyboardEvent } from 'react';
import { Plus, X } from 'lucide-react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { cn } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { Input } from '@/components/ui/input';
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select';
import type { ModelMapping } from '../data/schema';
import {
  addAliasTarget,
  connectorLayout,
  groupModelMappings,
  hasModelMapping,
  removeMappingAt,
  removeModelAlias,
  renameModelAlias,
  replaceMappingTarget,
} from '../utils/model-mapping-groups';

/**
 * Marks the alias rename input. The dialog checks it in onEscapeKeyDown: Radix
 * handles Escape on the document in the capture phase, before the input's own
 * key handler, so only the dialog can keep Escape from closing it mid-rename.
 */
export const MODEL_MAPPING_RENAME_ATTR = 'data-model-mapping-rename';

interface Props {
  mappings: ModelMapping[];
  supportedModels: string[];
  onChange: (mappings: ModelMapping[]) => void;
}

type FocusRequest = { kind: 'alias'; alias: string } | { kind: 'target'; index: number } | null;

const isEditKey = (event: KeyboardEvent) => event.key === 'Enter' || event.key === ' ' || event.key === 'F2';

// Custom grouped view of the channel's model aliases: each alias appears once and
// is joined by a curve to every upstream model it maps to. Rows are sized in px
// so they match connectorLayout (36px rows, 8px gaps, 56px connector column).
export function ModelMappingGraph({ mappings, supportedModels, onChange }: Props) {
  const { t } = useTranslation();
  const groups = useMemo(() => groupModelMappings(mappings), [mappings]);
  const supported = useMemo(() => new Set(supportedModels), [supportedModels]);

  const containerRef = useRef<HTMLDivElement>(null);
  const [renaming, setRenaming] = useState<{ alias: string; draft: string } | null>(null);
  const [editingTarget, setEditingTarget] = useState<number | null>(null);
  const [addingTo, setAddingTo] = useState<string | null>(null);
  const [hoveredTarget, setHoveredTarget] = useState<number | null>(null);
  const [focusRequest, setFocusRequest] = useState<FocusRequest>(null);

  // Inline editors replace the element that opened them, so keyboard focus is
  // moved back to the matching label once it is rendered again.
  useEffect(() => {
    if (!focusRequest || !containerRef.current) {
      return;
    }
    const selector =
      focusRequest.kind === 'alias'
        ? `[data-mapping-alias="${CSS.escape(focusRequest.alias)}"]`
        : `[data-mapping-target="${focusRequest.index}"]`;
    containerRef.current.querySelector<HTMLElement>(selector)?.focus();
    setFocusRequest(null);
  }, [focusRequest, mappings]);

  const duplicateWarning = () =>
    toast.warning(
      t('channels.dialogs.settings.modelMapping.duplicateMapping', {
        defaultValue: 'This alias already maps to that model',
      })
    );

  const startRename = (alias: string) => setRenaming({ alias, draft: alias });

  const commitRename = (restoreFocus: boolean) => {
    if (!renaming) {
      return;
    }
    const next = renaming.draft.trim();
    const alias = next || renaming.alias;
    if (next && next !== renaming.alias) {
      onChange(renameModelAlias(mappings, renaming.alias, next));
    }
    setRenaming(null);
    if (restoreFocus) {
      setFocusRequest({ kind: 'alias', alias });
    }
  };

  const handleRenameKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      commitRename(true);
    }
    if (event.key === 'Escape' && renaming) {
      event.preventDefault();
      const alias = renaming.alias;
      setRenaming(null);
      setFocusRequest({ kind: 'alias', alias });
    }
  };

  const startEditTarget = (index: number) => {
    setHoveredTarget(null);
    setEditingTarget(index);
  };

  const changeTarget = (index: number, to: string) => {
    const mapping = mappings[index];
    if (!mapping || mapping.to === to) {
      return;
    }
    if (hasModelMapping(mappings, mapping.from, to)) {
      duplicateWarning();
      return;
    }
    onChange(replaceMappingTarget(mappings, index, to));
  };

  const addTarget = (alias: string, to: string) => {
    if (hasModelMapping(mappings, alias, to)) {
      duplicateWarning();
      return;
    }
    onChange(addAliasTarget(mappings, alias, to));
  };

  const editHint = t('channels.dialogs.settings.modelMapping.editHint', { defaultValue: 'Double-click to edit' });
  const invalidHint = t('channels.dialogs.settings.modelMapping.targetInvalid', {
    defaultValue: 'Target model must be in supported models',
  });

  return (
    <div ref={containerRef} className='space-y-3'>
      {groups.map((group) => {
        const layout = connectorLayout(group.targets.length);
        const targetNames = new Set(group.targets.map((target) => target.to));
        const availableModels = supportedModels.filter((model) => !targetNames.has(model));
        const groupHovered = group.targets.some((target) => target.index === hoveredTarget);

        return (
          <div
            // The first entry's position survives a rename, so the group does not
            // remount (and swallow a click) while its alias changes.
            key={group.targets[0].index}
            data-testid='model-mapping-group'
            className='group/mapping bg-card hover:border-primary/30 rounded-lg border p-3 transition-colors'
          >
            {/* Below sm the alias sits above an indented target list; from sm up the
                columns are alias | 56px connector | targets. */}
            <div className='grid grid-cols-1 gap-y-[8px] sm:grid-cols-[minmax(0,1fr)_56px_minmax(0,1.4fr)]'>
              <div className='flex min-w-0 items-center justify-start self-center sm:justify-end'>
                {renaming?.alias === group.alias ? (
                  <Input
                    {...{ [MODEL_MAPPING_RENAME_ATTR]: '' }}
                    value={renaming.draft}
                    onChange={(event) => setRenaming({ alias: group.alias, draft: event.target.value })}
                    onKeyDown={handleRenameKeyDown}
                    onBlur={() => commitRename(false)}
                    onFocus={(event) => event.currentTarget.select()}
                    autoFocus
                    className='h-[36px] rounded-full'
                    aria-label={t('channels.dialogs.settings.modelMapping.originalModel')}
                  />
                ) : (
                  <div className='border-primary/30 bg-primary/10 text-primary flex h-[36px] max-w-full min-w-0 items-center gap-1.5 rounded-full border px-3 text-sm font-medium'>
                    <button
                      type='button'
                      data-mapping-alias={group.alias}
                      onDoubleClick={() => startRename(group.alias)}
                      onKeyDown={(event) => {
                        if (isEditKey(event)) {
                          event.preventDefault();
                          startRename(group.alias);
                        }
                      }}
                      className='min-w-[3ch] cursor-text truncate rounded-sm text-left outline-none focus-visible:underline focus-visible:underline-offset-4'
                      title={`${group.alias}\n${editHint}`}
                    >
                      {group.alias}
                    </button>
                    {group.targets.length > 1 && (
                      <span className='bg-primary/15 shrink-0 rounded-full px-1.5 text-[11px] leading-5 tabular-nums'>
                        {group.targets.length}
                      </span>
                    )}
                    {/* Shown on hover, keyboard focus and touch screens; display:none
                        otherwise so the hidden buttons do not take the name's space. */}
                    <div className='-mr-1.5 hidden shrink-0 items-center group-focus-within/mapping:flex group-hover/mapping:flex pointer-coarse:flex'>
                      {availableModels.length > 0 && (
                        <Button
                          type='button'
                          variant='ghost'
                          size='icon'
                          onClick={() => setAddingTo(group.alias)}
                          className='text-primary/70 hover:text-primary hover:bg-primary/15 h-6 w-6 rounded-full'
                          aria-label={t('channels.dialogs.settings.modelMapping.addTarget', {
                            alias: group.alias,
                            defaultValue: `Add a model to ${group.alias}`,
                          })}
                          title={t('channels.dialogs.settings.modelMapping.addTarget', {
                            alias: group.alias,
                            defaultValue: `Add a model to ${group.alias}`,
                          })}
                        >
                          <Plus className='h-3.5 w-3.5' />
                        </Button>
                      )}
                      <Button
                        type='button'
                        variant='ghost'
                        size='icon'
                        onClick={() => onChange(removeModelAlias(mappings, group.alias))}
                        className='text-primary/70 hover:text-destructive hover:bg-primary/15 h-6 w-6 rounded-full'
                        aria-label={t('channels.dialogs.settings.modelMapping.removeAlias', {
                          alias: group.alias,
                          defaultValue: `Remove alias ${group.alias}`,
                        })}
                        title={t('channels.dialogs.settings.modelMapping.removeAlias', {
                          alias: group.alias,
                          defaultValue: `Remove alias ${group.alias}`,
                        })}
                      >
                        <X className='h-3.5 w-3.5' />
                      </Button>
                    </div>
                  </div>
                )}
              </div>

              <svg
                width={layout.width}
                height={layout.height}
                viewBox={`0 0 ${layout.width} ${layout.height}`}
                className='hidden overflow-visible sm:block'
                aria-hidden='true'
              >
                {layout.paths.map((path, position) => {
                  const active = group.targets[position]?.index === hoveredTarget;
                  return (
                    <path
                      key={path}
                      d={path}
                      fill='none'
                      strokeWidth={active ? 2 : 1.5}
                      strokeLinecap='round'
                      className={cn(
                        'transition-colors',
                        active ? 'stroke-primary' : groupHovered ? 'stroke-primary/50' : 'stroke-muted-foreground/60'
                      )}
                    />
                  );
                })}
                <circle cx={layout.start.x} cy={layout.start.y} r={3.5} className='fill-primary' />
                {layout.ends.map((end, position) => (
                  <circle
                    key={`${end.x}-${end.y}`}
                    cx={end.x}
                    cy={end.y}
                    r={3}
                    strokeWidth={1.5}
                    className={cn(
                      'fill-background transition-colors',
                      group.targets[position]?.index === hoveredTarget ? 'stroke-primary' : 'stroke-muted-foreground/70'
                    )}
                  />
                ))}
              </svg>

              <div className='border-muted-foreground/30 ml-4 flex min-w-0 flex-col gap-[8px] border-l pl-3 sm:ml-0 sm:border-l-0 sm:pl-0'>
                {group.targets.map((target) => {
                  const invalid = !supported.has(target.to);

                  return editingTarget === target.index ? (
                    <Select
                      key={target.index}
                      defaultOpen
                      value={target.to}
                      onValueChange={(value) => changeTarget(target.index, value)}
                      onOpenChange={(open) => {
                        if (!open) {
                          setEditingTarget(null);
                          setFocusRequest({ kind: 'target', index: target.index });
                        }
                      }}
                    >
                      <SelectTrigger className='h-[36px] w-full min-w-0 font-mono text-[13px] data-[size=default]:h-[36px]'>
                        <SelectValue>
                          <span className='min-w-0 truncate'>{target.to}</span>
                        </SelectValue>
                      </SelectTrigger>
                      <SelectContent onCloseAutoFocus={(event) => event.preventDefault()}>
                        {supportedModels.map((model) => (
                          <SelectItem key={model} value={model}>
                            {model}
                          </SelectItem>
                        ))}
                      </SelectContent>
                    </Select>
                  ) : (
                    <div
                      key={target.index}
                      onMouseEnter={() => setHoveredTarget(target.index)}
                      onMouseLeave={() => setHoveredTarget(null)}
                      title={invalid ? invalidHint : undefined}
                      className={cn(
                        'bg-background hover:border-primary/40 flex h-[36px] min-w-0 items-center gap-1 rounded-md border pr-1 pl-3 shadow-xs transition-colors',
                        invalid && 'border-destructive/60 bg-destructive/5'
                      )}
                    >
                      <button
                        type='button'
                        data-mapping-target={target.index}
                        onDoubleClick={() => startEditTarget(target.index)}
                        onKeyDown={(event) => {
                          if (isEditKey(event)) {
                            event.preventDefault();
                            startEditTarget(target.index);
                          }
                        }}
                        className={cn(
                          'min-w-0 flex-1 cursor-text truncate rounded-sm text-left font-mono text-[13px] outline-none focus-visible:underline focus-visible:underline-offset-4',
                          invalid && 'text-destructive'
                        )}
                        title={`${target.to}\n${invalid ? invalidHint : editHint}`}
                      >
                        {target.to}
                      </button>
                      <Button
                        type='button'
                        variant='ghost'
                        size='icon'
                        onClick={() => {
                          setHoveredTarget(null);
                          onChange(removeMappingAt(mappings, target.index));
                        }}
                        className='text-muted-foreground hover:text-destructive h-7 w-7 shrink-0'
                        aria-label={t('channels.dialogs.settings.modelMapping.removeTarget', {
                          model: target.to,
                          defaultValue: `Remove ${target.to}`,
                        })}
                        title={t('channels.dialogs.settings.modelMapping.removeTarget', {
                          model: target.to,
                          defaultValue: `Remove ${target.to}`,
                        })}
                      >
                        <X className='h-3.5 w-3.5' />
                      </Button>
                    </div>
                  );
                })}
              </div>

              {addingTo === group.alias && availableModels.length > 0 && (
                <div className='ml-4 min-w-0 pl-3 sm:col-start-3 sm:ml-0 sm:pl-0'>
                  <Select
                    defaultOpen
                    onValueChange={(value) => addTarget(group.alias, value)}
                    onOpenChange={(open) => {
                      if (!open) {
                        setAddingTo(null);
                        setFocusRequest({ kind: 'alias', alias: group.alias });
                      }
                    }}
                  >
                    <SelectTrigger className='h-[36px] w-full min-w-0 border-dashed font-mono text-[13px] data-[size=default]:h-[36px]'>
                      <SelectValue placeholder={t('channels.dialogs.settings.modelMapping.targetModel')} />
                    </SelectTrigger>
                    <SelectContent onCloseAutoFocus={(event) => event.preventDefault()}>
                      {availableModels.map((model) => (
                        <SelectItem key={model} value={model}>
                          {model}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
              )}
            </div>
          </div>
        );
      })}
    </div>
  );
}
