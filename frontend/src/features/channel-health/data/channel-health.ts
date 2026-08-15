import { z } from 'zod';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
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
  firstTokenMs: z.number().nullable().optional(),
  p95Ms: z.number().nullable().optional(),
  lastProbedAt: z.string().nullable().optional(),
  sampleCount: z.number(),
  latestRun: activeChannelHealthProbeRunSchema.nullable().optional(),
});

const channelHealthProbeChannelSchema = z.object({
  channelID: z.string(),
  channelName: z.string(),
  channelStatus: z.string(),
  priority: z.number(),
  enabled: z.boolean(),
  intervalMinutes: z.number(),
  models: z.array(channelHealthProbeModelOverviewSchema),
});

const channelHealthProbePolicySchema = z.object({
  enabled: z.boolean(),
  acceptableLatencyMs: z.number(),
  extraChannels: z.number(),
  apiKeyMaxFirstTokenLatencyMs: z.number().nullable().optional(),
  models: z.array(z.object({ modelID: z.string(), enabled: z.boolean(), stream: z.boolean() })),
});

const channelHealthProbeOverviewSchema = z.object({
  channels: z.array(channelHealthProbeChannelSchema),
  policy: channelHealthProbePolicySchema,
});

const channelHealthProbeHistoryPageSchema = z.object({
  items: z.array(activeChannelHealthProbeRunSchema),
  totalCount: z.number(),
});

export type ActiveChannelHealthProbeRun = z.infer<typeof activeChannelHealthProbeRunSchema>;
export type ChannelHealthProbeModelOverview = z.infer<typeof channelHealthProbeModelOverviewSchema>;
export type ChannelHealthProbeChannel = z.infer<typeof channelHealthProbeChannelSchema>;
export type ChannelHealthProbePolicy = z.infer<typeof channelHealthProbePolicySchema>;
export type ChannelHealthProbeOverview = z.infer<typeof channelHealthProbeOverviewSchema>;
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

export interface UpdateChannelHealthProbePolicyInput {
  enabled: boolean;
  acceptableLatencyMs: number;
  extraChannels: number;
  models: ActiveHealthProbeModelSetting[];
}

export interface ActiveHealthProbeModelSetting {
  modelID: string;
  enabled: boolean;
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
      priority
      enabled
      intervalMinutes
      models {
        modelID
        enabled
        stream
        firstTokenMs
        p95Ms
        lastProbedAt
        sampleCount
        latestRun {
          ${CHANNEL_HEALTH_PROBE_RUN_FIELDS}
        }
      }
    }
    channelHealthProbePolicy {
      enabled
      acceptableLatencyMs
      extraChannels
      apiKeyMaxFirstTokenLatencyMs
      models {
        modelID
        enabled
        stream
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
      priority
      enabled
      intervalMinutes
      models {
        modelID
        enabled
        stream
        firstTokenMs
        p95Ms
        lastProbedAt
        sampleCount
        latestRun {
          ${CHANNEL_HEALTH_PROBE_RUN_FIELDS}
        }
      }
    }
  }
`;

const UPDATE_CHANNEL_HEALTH_PROBE_POLICY_MUTATION = `
  mutation UpdateChannelHealthProbePolicy($input: UpdateChannelHealthProbePolicyInput!) {
    updateChannelHealthProbePolicy(input: $input) {
      enabled
      acceptableLatencyMs
      extraChannels
      apiKeyMaxFirstTokenLatencyMs
      models {
        modelID
        enabled
        stream
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
      const data = await graphqlRequest<{
        channelHealthProbeOverview: ChannelHealthProbeChannel[];
        channelHealthProbePolicy: ChannelHealthProbePolicy;
      }>(CHANNEL_HEALTH_PROBE_OVERVIEW_QUERY);
      return channelHealthProbeOverviewSchema.parse({
        channels: data.channelHealthProbeOverview,
        policy: data.channelHealthProbePolicy,
      });
    },
    refetchInterval: 15_000,
    refetchIntervalInBackground: false,
  });
}

export function useUpdateChannelHealthProbePolicy() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: async (input: UpdateChannelHealthProbePolicyInput) => {
      const data = await graphqlRequest<{ updateChannelHealthProbePolicy: ChannelHealthProbePolicy }>(
        UPDATE_CHANNEL_HEALTH_PROBE_POLICY_MUTATION,
        { input }
      );
      return channelHealthProbePolicySchema.parse(data.updateChannelHealthProbePolicy);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-overview'] });
    },
  });
}

export function useChannelHealthProbeHistory(input: ChannelHealthProbeHistoryInput) {
  return useQuery({
    queryKey: ['channel-health-probe-history', input],
    queryFn: async () => {
      const data = await graphqlRequest<{ channelHealthProbeHistory: ChannelHealthProbeHistoryPage }>(CHANNEL_HEALTH_PROBE_HISTORY_QUERY, {
        input,
      });
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
      const data = await graphqlRequest<{ runChannelHealthProbe: ActiveChannelHealthProbeRun }>(RUN_CHANNEL_HEALTH_PROBE_MUTATION, {
        input,
      });
      return activeChannelHealthProbeRunSchema.parse(data.runChannelHealthProbe);
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-overview'] });
      void queryClient.invalidateQueries({ queryKey: ['channel-health-probe-history'] });
    },
  });
}
