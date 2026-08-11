import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { z } from 'zod';
import { graphqlRequest } from '@/gql/graphql';

const activeChannelHealthProbeRunSchema = z.object({
  id: z.string(),
  channelID: z.string(),
  modelID: z.string(),
  source: z.string(),
  status: z.string(),
  stream: z.boolean(),
  ttfbMs: z.number().nullable().optional(),
  ttftMs: z.number().nullable().optional(),
  totalMs: z.number(),
  errorMessage: z.string().nullable().optional(),
  startedAt: z.string(),
  completedAt: z.string().nullable().optional(),
  createdAt: z.string(),
});

const channelHealthProbeModelOverviewSchema = z.object({
  modelID: z.string(),
  enabled: z.boolean(),
  stream: z.boolean(),
  latestRun: activeChannelHealthProbeRunSchema.nullable().optional(),
});

const channelHealthProbeChannelSchema = z.object({
  channelID: z.string(),
  channelName: z.string(),
  channelStatus: z.string(),
  enabled: z.boolean(),
  intervalMinutes: z.number(),
  models: z.array(channelHealthProbeModelOverviewSchema),
});

const channelHealthProbeHistoryPageSchema = z.object({
  items: z.array(activeChannelHealthProbeRunSchema),
  totalCount: z.number(),
});

export type ActiveChannelHealthProbeRun = z.infer<typeof activeChannelHealthProbeRunSchema>;
export type ChannelHealthProbeModelOverview = z.infer<typeof channelHealthProbeModelOverviewSchema>;
export type ChannelHealthProbeChannel = z.infer<typeof channelHealthProbeChannelSchema>;
export type ChannelHealthProbeHistoryPage = z.infer<typeof channelHealthProbeHistoryPageSchema>;

export interface ChannelHealthProbeModelInput {
  modelID: string;
  enabled: boolean;
  stream: boolean;
}

export interface UpdateChannelHealthProbeSettingsInput {
  channelID: string;
  enabled: boolean;
  intervalMinutes: number;
  models: ChannelHealthProbeModelInput[];
}

export interface RunChannelHealthProbeInput {
  channelID: string;
  modelID: string;
  stream: boolean;
}

export interface ChannelHealthProbeHistoryInput {
  channelID?: string;
  modelID?: string;
  status?: string;
  source?: string;
  offset: number;
  limit: number;
}

const CHANNEL_HEALTH_PROBE_RUN_FIELDS = `
  id
  channelID
  modelID
  source
  status
  stream
  ttfbMs
  ttftMs
  totalMs
  errorMessage
  startedAt
  completedAt
  createdAt
`;

const CHANNEL_HEALTH_PROBE_OVERVIEW_QUERY = `
  query ChannelHealthProbeOverview {
    channelHealthProbeOverview {
      channelID
      channelName
      channelStatus
      enabled
      intervalMinutes
      models {
        modelID
        enabled
        stream
        latestRun {
          ${CHANNEL_HEALTH_PROBE_RUN_FIELDS}
        }
      }
    }
  }
`;

const CHANNEL_HEALTH_PROBE_HISTORY_QUERY = `
  query ChannelHealthProbeHistory($input: ChannelHealthProbeHistoryInput!) {
    channelHealthProbeHistory(input: $input) {
      totalCount
      items {
        ${CHANNEL_HEALTH_PROBE_RUN_FIELDS}
      }
    }
  }
`;

const UPDATE_CHANNEL_HEALTH_PROBE_SETTINGS_MUTATION = `
  mutation UpdateChannelHealthProbeSettings($input: UpdateChannelHealthProbeSettingsInput!) {
    updateChannelHealthProbeSettings(input: $input) {
      channelID
      channelName
      channelStatus
      enabled
      intervalMinutes
      models {
        modelID
        enabled
        stream
        latestRun {
          ${CHANNEL_HEALTH_PROBE_RUN_FIELDS}
        }
      }
    }
  }
`;

const RUN_CHANNEL_HEALTH_PROBE_MUTATION = `
  mutation RunChannelHealthProbe($input: RunChannelHealthProbeInput!) {
    runChannelHealthProbe(input: $input) {
      ${CHANNEL_HEALTH_PROBE_RUN_FIELDS}
    }
  }
`;

export function useChannelHealthProbeOverview() {
  return useQuery({
    queryKey: ['channel-health-probe-overview'],
    queryFn: async () => {
      const data = await graphqlRequest<{ channelHealthProbeOverview: ChannelHealthProbeChannel[] }>(CHANNEL_HEALTH_PROBE_OVERVIEW_QUERY);
      return z.array(channelHealthProbeChannelSchema).parse(data.channelHealthProbeOverview);
    },
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
  });
}

export function useChannelHealthProbeHistory(input: ChannelHealthProbeHistoryInput) {
  return useQuery({
    queryKey: ['channel-health-probe-history', input.channelID, input.modelID, input.status, input.source, input.offset, input.limit],
    queryFn: async () => {
      const data = await graphqlRequest<{ channelHealthProbeHistory: ChannelHealthProbeHistoryPage }>(CHANNEL_HEALTH_PROBE_HISTORY_QUERY, { input });
      return channelHealthProbeHistoryPageSchema.parse(data.channelHealthProbeHistory);
    },
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
  });
}

export function useUpdateChannelHealthProbeSettings() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: UpdateChannelHealthProbeSettingsInput) => {
      const data = await graphqlRequest<{ updateChannelHealthProbeSettings: ChannelHealthProbeChannel }>(
        UPDATE_CHANNEL_HEALTH_PROBE_SETTINGS_MUTATION,
        { input }
      );
      return channelHealthProbeChannelSchema.parse(data.updateChannelHealthProbeSettings);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-overview'] });
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-history'] });
      void queryClient.invalidateQueries({ queryKey: ['channels'] });
    },
  });
}

export function useRunChannelHealthProbe() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: RunChannelHealthProbeInput) => {
      const data = await graphqlRequest<{ runChannelHealthProbe: ActiveChannelHealthProbeRun }>(RUN_CHANNEL_HEALTH_PROBE_MUTATION, { input });
      return activeChannelHealthProbeRunSchema.parse(data.runChannelHealthProbe);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-overview'] });
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-history'] });
    },
  });
}
