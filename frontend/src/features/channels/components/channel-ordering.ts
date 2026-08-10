export type OrderingMode = 'weight' | 'priority';

export type OrderingMoveResult<T> = { ok: true; items: T[] } | { ok: false; reason: 'weight-capacity'; items: T[] };

export const MIN_WEIGHT = 0;
export const MAX_WEIGHT = 100;

const MIN_GRAPHQL_INT = -2147483648;
const MAX_GRAPHQL_INT = 2147483647;
const PRIORITY_STEP = 10;
const WEIGHT_CAPACITY = MAX_WEIGHT - MIN_WEIGHT + 1;

export const clampWeight = (value: number) => Math.round(Math.min(MAX_WEIGHT, Math.max(MIN_WEIGHT, value)));

export const isValidGraphQLInt = (value: number) => Number.isInteger(value) && value >= MIN_GRAPHQL_INT && value <= MAX_GRAPHQL_INT;

export const sortChannelsByMode = <T extends { orderingWeight: number; priority: number }>(items: T[], mode: OrderingMode) => {
  const sortedItems = [...items];
  sortedItems.sort((a, b) => {
    if (mode === 'priority') {
      return b.priority - a.priority || b.orderingWeight - a.orderingWeight;
    }

    return b.orderingWeight - a.orderingWeight || b.priority - a.priority;
  });

  return sortedItems;
};

const rebalancePriorities = <T extends { priority: number }>(items: T[]) => {
  let maxPriority = 0;
  for (const item of items) {
    maxPriority = Math.max(maxPriority, item.priority);
  }

  const lowestValidStart = MIN_GRAPHQL_INT + Math.max(items.length - 1, 0) * PRIORITY_STEP;
  const preferredStart = Math.max(maxPriority, items.length * PRIORITY_STEP);
  const startPriority = Math.min(MAX_GRAPHQL_INT, Math.max(lowestValidStart, preferredStart));

  return items.map((item, index) => ({
    ...item,
    priority: startPriority - index * PRIORITY_STEP,
  }));
};

const rebalanceWeights = <T extends { orderingWeight: number }>(items: T[]): OrderingMoveResult<T> => {
  if (items.length > WEIGHT_CAPACITY) {
    return { ok: false, reason: 'weight-capacity', items };
  }

  if (items.length === 0) {
    return { ok: true, items: [] };
  }

  if (items.length === 1) {
    return { ok: true, items: [{ ...items[0], orderingWeight: MAX_WEIGHT }] };
  }

  const range = MAX_WEIGHT - MIN_WEIGHT;
  return {
    ok: true,
    items: items.map((item, index) => ({
      ...item,
      orderingWeight: Math.round(MAX_WEIGHT - (index * range) / (items.length - 1)),
    })),
  };
};

const findLocalWeight = <T extends { orderingWeight: number }>(items: T[], movedIndex: number) => {
  const previousWeight = items[movedIndex - 1]?.orderingWeight;
  const nextWeight = items[movedIndex + 1]?.orderingWeight;

  if (previousWeight == null && nextWeight == null) {
    return MAX_WEIGHT;
  }
  if (previousWeight == null) {
    return nextWeight < MAX_WEIGHT ? nextWeight + 1 : undefined;
  }
  if (nextWeight == null) {
    return previousWeight > MIN_WEIGHT ? previousWeight - 1 : undefined;
  }
  if (previousWeight - nextWeight < 2) {
    return undefined;
  }

  return Math.floor((previousWeight + nextWeight) / 2);
};

export function applyOrderingMove<T extends { orderingWeight: number; priority: number }>(
  items: T[],
  movedIndex: number,
  mode: OrderingMode
): OrderingMoveResult<T> {
  if (mode === 'priority') {
    return { ok: true, items: rebalancePriorities(items) };
  }

  const localWeight = findLocalWeight(items, movedIndex);
  if (localWeight == null) {
    return rebalanceWeights(items);
  }

  const movedItems = [...items];
  movedItems[movedIndex] = {
    ...movedItems[movedIndex],
    orderingWeight: localWeight,
  };
  return { ok: true, items: movedItems };
}
