import { decompress } from 'fzstd';
import { sha256Hex } from './sha256.ts';
import { decodeDetector } from './vendor/lm-detector/detector-codec.ts';
import { supportsSharedDetector, type SharedDetector } from './vendor/lm-detector/shared-detector.ts';
import type { Bank, BankModel } from './vendor/lm-detector/types.ts';

export const DATA_SCHEMA = 'lmfpd-data-v1';
export const DETECTOR_SCHEMA = 'shared-detector-v2';

export interface DetectorBundle {
  schema: string;
  detector_schema: string;
  built_at: string;
  models: BankModel[];
  files: { 'shared_detector.bin': { path?: string; bytes?: number; sha256: string } };
  source?: { repo?: string; commit?: string };
}

export interface LoadedDetector {
  bank: Bank;
  detector: SharedDetector;
  bundle: DetectorBundle;
}

// Builds a scoring bank + detector from the self-hosted snapshot (bundle metadata + zstd detector).
// Does not fetch from lm.ikale.io.
// The checksum uses sha256Hex, which falls back to JavaScript when crypto.subtle is missing
// on plain-HTTP pages.
export async function decodeDetectorSnapshot(bundle: DetectorBundle, compressed: Uint8Array): Promise<LoadedDetector> {
  if (bundle.schema !== DATA_SCHEMA || bundle.detector_schema !== DETECTOR_SCHEMA) {
    throw new Error(`Unsupported detector data schema ${bundle.schema}/${bundle.detector_schema}`);
  }
  const bytes = decompress(compressed);
  const expected = bundle.files['shared_detector.bin']?.sha256;
  if (!expected) throw new Error('Detector snapshot is missing its checksum');
  if ((await sha256Hex(bytes)) !== expected) throw new Error('Detector snapshot does not match its checksum');
  const detector = decodeDetector(bytes);
  const bank: Bank = {
    schema: bundle.schema,
    built_at: bundle.built_at,
    models: bundle.models,
    detector_schema: bundle.detector_schema,
  };
  if (!supportsSharedDetector(bank, detector)) {
    throw new Error('The reference bank does not match its detector');
  }
  return { bank, detector, bundle };
}
