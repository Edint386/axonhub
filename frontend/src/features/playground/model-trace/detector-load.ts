import bundleUrl from './data/bundle.json?url';
import detectorUrl from './data/shared_detector.bin.zst?url';
import { decodeDetectorSnapshot, type LoadedDetector } from './detector.ts';

let loading: Promise<LoadedDetector> | undefined;

// The compressed detector is a few megabytes, so it is fetched only when the check view is first opened.
export function loadDetector(): Promise<LoadedDetector> {
  if (loading) return loading;
  loading = Promise.all([
    fetch(bundleUrl).then(async (response) => {
      if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
      return response.json();
    }),
    fetch(detectorUrl).then(async (response) => {
      if (!response.ok) throw new Error(`${response.status} ${response.statusText}`);
      return new Uint8Array(await response.arrayBuffer());
    }),
  ])
    .then(([bundle, compressed]) => decodeDetectorSnapshot(bundle, compressed))
    .catch((error: unknown) => {
      loading = undefined;
      throw error;
    });
  return loading;
}
