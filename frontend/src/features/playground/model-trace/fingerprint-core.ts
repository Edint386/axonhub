// Adapter around lm-detector's browser-safe scorer (analyzeSharedOutputs).
// The live ranker is shared-detector-v2; the old Hellinger / Ordered-block port is not used.

import { parseNumbers } from './vendor/lm-detector/fingerprint-core.js';
import { analyzeSharedOutputs, type SharedDetector } from './vendor/lm-detector/shared-detector.ts';
import type { Bank, BankModel, Output } from './vendor/lm-detector/types.ts';

export { parseNumbers };
export type { Bank as FingerprintBank, BankModel as FingerprintModel, Output as FingerprintOutput };

export function minimumNumbers(expectedCount: number): number {
  return expectedCount ? Math.max(80, Math.ceil(expectedCount * 0.55)) : 80;
}

export interface ModelResult {
  model: string;
  display_name: string;
  probability: number | null;
  score: number;
  family: string;
  family_name: string;
}

export interface OutputDiagnostic {
  index: number;
  parsed_numbers: number;
  minimum_numbers: number;
  accepted: boolean;
}

export interface FingerprintResult {
  prediction: string;
  prediction_name: string;
  probability: number | null;
  used_outputs: number;
  results: ModelResult[];
  diagnostics: OutputDiagnostic[];
  family_prediction: string;
  family_prediction_name: string;
  family_probability: number;
  family_probabilities: Array<{ family: string; display_name: string; probability: number }>;
  method: string;
  decision?: string;
}

export function analyzeFingerprint(outputs: Output[], bank: Bank, detector: SharedDetector): FingerprintResult {
  const analysis = analyzeSharedOutputs(outputs, bank, detector, { allowPartial: true });
  if (!analysis.used_outputs) {
    throw new Error('No usable answers: every answer was refused or too short.');
  }
  const familyOrder: string[] = [];
  const familyNames = new Map<string, string>();
  const familyProbabilities = new Map<string, number>();
  const hasProbabilities = analysis.results.length > 0 && analysis.results.every((item) => item.probability != null);
  for (const item of analysis.results) {
    if (!familyNames.has(item.family)) {
      familyOrder.push(item.family);
      familyNames.set(item.family, item.family_name || item.family);
    }
    if (hasProbabilities) {
      familyProbabilities.set(item.family, (familyProbabilities.get(item.family) ?? 0) + (item.probability ?? 0));
    }
  }
  if (!familyOrder.length && analysis.family_prediction) {
    familyOrder.push(analysis.family_prediction);
    familyNames.set(analysis.family_prediction, analysis.family_prediction_name || analysis.family_prediction);
  }
  const winningFamily = analysis.family_prediction || familyOrder[0] || '';
  const familyProbability = hasProbabilities ? (familyProbabilities.get(winningFamily) ?? 0) : 0;
  return {
    prediction: analysis.prediction,
    prediction_name: analysis.prediction_name,
    probability: analysis.probability ?? null,
    used_outputs: analysis.used_outputs,
    results: analysis.results.map((item) => ({
      model: item.model,
      display_name: item.display_name,
      probability: item.probability,
      score: item.score,
      family: item.family,
      family_name: item.family_name,
    })),
    diagnostics: analysis.diagnostics.map((item) => ({
      index: item.index,
      parsed_numbers: item.parsed_numbers,
      minimum_numbers: item.minimum_numbers,
      accepted: item.accepted,
    })),
    family_prediction: winningFamily,
    family_prediction_name: analysis.family_prediction_name || familyNames.get(winningFamily) || winningFamily,
    family_probability: familyProbability,
    family_probabilities: familyOrder.map((family) => ({
      family,
      display_name: familyNames.get(family) || family,
      probability: familyProbabilities.get(family) ?? 0,
    })),
    method: analysis.method,
    decision: analysis.decision,
  };
}
