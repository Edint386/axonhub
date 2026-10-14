// Pure helpers for the grouped model alias view (custom): one alias can map to
// several upstream models, so the dialog shows each alias once with a connector
// to every target. The stored settings stay a flat list of { from, to } entries.

export interface ModelMappingEntry {
  from: string;
  to: string;
}

export interface ModelMappingTarget {
  to: string;
  // Position of this entry in the flat mapping list.
  index: number;
}

export interface ModelMappingGroup {
  alias: string;
  targets: ModelMappingTarget[];
}

/** Groups mappings by alias in first-appearance order, keeping each entry's index. */
export function groupModelMappings(mappings: ModelMappingEntry[]): ModelMappingGroup[] {
  const groups: ModelMappingGroup[] = [];
  const byAlias = new Map<string, ModelMappingGroup>();

  mappings.forEach((mapping, index) => {
    let group = byAlias.get(mapping.from);
    if (!group) {
      group = { alias: mapping.from, targets: [] };
      byAlias.set(mapping.from, group);
      groups.push(group);
    }
    group.targets.push({ to: mapping.to, index });
  });

  return groups;
}

export function hasModelMapping(mappings: ModelMappingEntry[], from: string, to: string): boolean {
  return mappings.some((mapping) => mapping.from === from && mapping.to === to);
}

/** Adds a target to an alias right after the alias's last entry, so a group stays together. */
export function addAliasTarget(mappings: ModelMappingEntry[], alias: string, to: string): ModelMappingEntry[] {
  let lastIndex = -1;
  mappings.forEach((mapping, index) => {
    if (mapping.from === alias) {
      lastIndex = index;
    }
  });

  const entry = { from: alias, to };
  if (lastIndex === -1) {
    return [...mappings, entry];
  }

  return [...mappings.slice(0, lastIndex + 1), entry, ...mappings.slice(lastIndex + 1)];
}

/** Renames every entry of an alias. Renaming onto an existing alias merges the two. */
export function renameModelAlias(mappings: ModelMappingEntry[], from: string, to: string): ModelMappingEntry[] {
  const renamed = mappings.map((mapping) => (mapping.from === from ? { ...mapping, from: to } : mapping));

  // A merge can repeat a pair that the other alias already had; keep the first.
  return renamed.filter((mapping, index) => renamed.findIndex((other) => other.from === mapping.from && other.to === mapping.to) === index);
}

export function removeModelAlias(mappings: ModelMappingEntry[], alias: string): ModelMappingEntry[] {
  return mappings.filter((mapping) => mapping.from !== alias);
}

export function replaceMappingTarget(mappings: ModelMappingEntry[], index: number, to: string): ModelMappingEntry[] {
  return mappings.map((mapping, idx) => (idx === index ? { ...mapping, to } : mapping));
}

export function removeMappingAt(mappings: ModelMappingEntry[], index: number): ModelMappingEntry[] {
  return mappings.filter((_, idx) => idx !== index);
}

// Target rows are a fixed height so the connector can be laid out without
// measuring the DOM: the alias sits at the vertical middle of the target stack.
export const MAPPING_NODE_HEIGHT = 36;
export const MAPPING_NODE_GAP = 8;
// Keep in sync with the px sizes in model-mapping-graph.tsx (h-[36px], gap-[8px]
// and the 56px grid column).
export const MAPPING_CONNECTOR_WIDTH = 56;

export interface ConnectorPoint {
  x: number;
  y: number;
}

export interface ConnectorLayout {
  width: number;
  height: number;
  start: ConnectorPoint;
  ends: ConnectorPoint[];
  paths: string[];
}

/**
 * Lays out the curves from an alias to its targets: one cubic curve per target,
 * leaving the alias horizontally and entering each target horizontally. Points
 * are inset so the endpoint dots are not clipped by the SVG edges.
 */
export function connectorLayout(
  targetCount: number,
  width = MAPPING_CONNECTOR_WIDTH,
  nodeHeight = MAPPING_NODE_HEIGHT,
  gap = MAPPING_NODE_GAP,
  inset = 4
): ConnectorLayout {
  const count = Math.max(targetCount, 1);
  const height = count * nodeHeight + (count - 1) * gap;
  const start = { x: inset, y: height / 2 };
  const ends = Array.from({ length: count }, (_, index) => ({
    x: width - inset,
    y: index * (nodeHeight + gap) + nodeHeight / 2,
  }));
  const bend = width / 2;
  const paths = ends.map((end) => `M ${start.x} ${start.y} C ${bend} ${start.y}, ${bend} ${end.y}, ${end.x} ${end.y}`);

  return { width, height, start, ends, paths };
}
