import type { FingerprintModel, FingerprintResult } from './fingerprint-core.ts';

// Leading provider and region tokens of hosted ids such as "us.anthropic.claude-opus-4-7-v1:0".
const VENDOR_TOKENS = new Set(['anthropic', 'openai']);
const PREFIX_TOKENS = new Set(['us', 'eu', 'apac', 'jp', 'au', 'ca', 'gov', 'global', 'azure']);
// Below this family probability a "different family" verdict would rest on near coin-flip odds.
export const FAMILY_CONFIDENCE = 0.8;
// Suffixes that pick a reasoning effort or mode of the same model, not a different model.
const MODE_SUFFIXES = new Set(['thinking', 'reasoning', 'minimal', 'none', 'low', 'medium', 'high', 'xhigh', 'max', 'fast', 'latest']);

function stripTrailingDecorations(parts: string[]): boolean {
  const last = parts[parts.length - 1];
  const previous = parts[parts.length - 2];
  if (MODE_SUFFIXES.has(last) || last === '1m' || /^v\d+$/.test(last)) {
    parts.pop();
    return true;
  }
  // "-v1:0" version tags of hosted ids
  if (/^\d+$/.test(last) && previous && /^v\d+$/.test(previous)) {
    parts.splice(-2);
    return true;
  }
  return false;
}

// "us.anthropic.Claude-Opus-4.7-20260101-v1:0" -> "claude-opus-4-7", "gpt-5.4-2026-03-05-high" -> "gpt-5-4"
export function normalizeModelName(name: string): string {
  const base = name.trim().toLowerCase().split('/').pop() ?? '';
  let parts = base.split(/[^a-z0-9]+/).filter(Boolean);
  parts = parts.filter((part) => !/^\d{8}$/.test(part));
  parts = parts.filter((_, index) => {
    // drop "2026-03-05" style dates
    const dateAt = (start: number) =>
      /^(19|20)\d{2}$/.test(parts[start] ?? '') && /^\d{2}$/.test(parts[start + 1] ?? '') && /^\d{2}$/.test(parts[start + 2] ?? '');
    return !(dateAt(index) || dateAt(index - 1) || dateAt(index - 2));
  });
  // "us-gov.anthropic.claude-…", "azure-openai-gpt-…": drop everything up to the vendor
  const vendor = parts.slice(0, 3).findIndex((part) => VENDOR_TOKENS.has(part));
  if (vendor >= 0 && vendor < parts.length - 1) parts = parts.slice(vendor + 1);
  while (parts.length > 1 && PREFIX_TOKENS.has(parts[0])) parts.shift();
  while (parts.length > 1 && stripTrailingDecorations(parts));
  return parts.join('-');
}

// The bank model a model name refers to, or undefined when the name is not one of the bank's models.
export function findBankModel(name: string, models: FingerprintModel[]): FingerprintModel | undefined {
  const normalized = normalizeModelName(name);
  if (!normalized) return undefined;
  return models.find((model) => normalizeModelName(model.id) === normalized);
}

export type ClaimVerdict =
  | { kind: 'match'; claimed: FingerprintModel }
  | { kind: 'same-family'; claimed: FingerprintModel }
  | { kind: 'different-family'; claimed: FingerprintModel }
  // The top model and the most likely family disagree, or the family leans away from the claim
  // without FAMILY_CONFIDENCE, so no family can be named.
  | { kind: 'uncertain'; claimed: FingerprintModel }
  // The claimed model is not in the bank. ModelTrace is closed-set: an unseen model (even a sibling
  // version) is forced onto the closest bank model, often of another family, so no verdict is given.
  | { kind: 'not-covered' };

// Compares the model a request claims to reach with ModelTrace's prediction. Pass the model the
// channel actually sends upstream (its mapped model when it has one), not a local alias.
export function judgeClaim(name: string, result: FingerprintResult, models: FingerprintModel[]): ClaimVerdict {
  const claimed = findBankModel(name, models);
  if (!claimed) return { kind: 'not-covered' };
  if (result.results[0]?.family !== result.family_prediction) return { kind: 'uncertain', claimed };
  if (claimed.id === result.prediction) return { kind: 'match', claimed };
  if ((claimed.family || 'models') === result.family_prediction) return { kind: 'same-family', claimed };
  if (result.family_probability < FAMILY_CONFIDENCE) return { kind: 'uncertain', claimed };
  return { kind: 'different-family', claimed };
}
