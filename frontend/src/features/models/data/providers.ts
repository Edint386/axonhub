import { useQuery } from '@tanstack/react-query';
import providersDataRaw from './providers.json';
import { providersDataSchema, type ProvidersData } from './providers.schema';

const PROVIDERS_URL = 'https://raw.githubusercontent.com/ThinkInAIXYZ/PublicProviderConf/refs/heads/dev/dist/all.json';
const DEVELOPERS_URL =
  'https://raw.githubusercontent.com/looplj/axonhub/refs/heads/unstable/frontend/src/features/models/data/providers.json';

const EMPTY_PROVIDERS_DATA: ProvidersData = { providers: {} };

/**
 * Validate a providers catalog WITHOUT ever throwing.
 *
 * Every source feeding these hooks is untrusted input: two are fetched at runtime
 * from repositories we do not control, and the bundled copy is rewritten by an
 * automated upstream sync. A throwing `.parse` on any of them is therefore a
 * liability rather than a safeguard -- and when it threw from `placeholderData` it
 * threw during RENDER, which the root error boundary turns into a full-page 500. That
 * is how a single boolean `experimental` in one DeepSeek entry took down both the
 * channels and models pages at once.
 *
 * A catalog that fails validation degrades to an empty one: the pages then render
 * without provider metadata instead of not rendering at all.
 */
function parseProvidersData(data: unknown, source: string): ProvidersData {
  const result = providersDataSchema.safeParse(data);
  if (result.success) {
    return result.data;
  }

  console.error(`Invalid providers data from ${source}; falling back to an empty catalog`, result.error.issues);

  return EMPTY_PROVIDERS_DATA;
}

/**
 * The bundled catalog, validated ONCE at module load.
 *
 * This was previously re-parsed on every render through `placeholderData`, which both
 * re-validated the whole catalog needlessly and put the throw directly in the render
 * path.
 */
const BUNDLED_PROVIDERS_DATA = parseProvidersData(providersDataRaw, 'the bundled providers.json');

/**
 * Fetch a remote catalog, falling back to the bundled copy when the network fails OR
 * when the remote payload does not match the schema. A remote shape change must not
 * be able to leave the caller with nothing.
 */
async function fetchProvidersData(url: string, label: string): Promise<ProvidersData> {
  try {
    const response = await fetch(url);
    if (!response.ok) {
      throw new Error(`Failed to fetch ${label}: ${response.status}`);
    }

    const result = providersDataSchema.safeParse(await response.json());
    if (result.success) {
      return result.data;
    }

    console.error(`Remote ${label} failed validation, falling back to the bundled copy`, result.error.issues);
  } catch (error) {
    console.error(`Failed to fetch remote ${label}, falling back to the bundled copy:`, error);
  }

  return BUNDLED_PROVIDERS_DATA;
}

export function useProvidersData() {
  return useQuery<ProvidersData>({
    queryKey: ['providers-data'],
    queryFn: () => fetchProvidersData(PROVIDERS_URL, 'providers data'),
    staleTime: 1000 * 60 * 60 * 24, // 1 day
    placeholderData: BUNDLED_PROVIDERS_DATA,
  });
}

export function useDevelopersData() {
  return useQuery<ProvidersData>({
    queryKey: ['developers-data'],
    queryFn: () => fetchProvidersData(DEVELOPERS_URL, 'developers data'),
    staleTime: 1000 * 60 * 60 * 24, // 1 day
    placeholderData: BUNDLED_PROVIDERS_DATA,
  });
}
