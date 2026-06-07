import { useQueries, useQuery } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { channelQuotaUsageSchema } from '@/features/channels/data/schema';
import type { ChannelQuota, ChannelQuotaUsage } from '@/features/channels/data/schema';

const CHECK_PROVIDER_QUOTAS_QUERY = `
  mutation CheckProviderQuotas {
    checkProviderQuotas
  }
`;

const PROVIDER_QUOTA_STATUSES_QUERY = `
  query ProviderQuotaStatuses($input: QueryChannelInput!) {
    queryChannels(input: $input) {
      edges {
        node {
          id
          name
          type
          settings {
            quota {
              requests
              totalTokens
              cost
              period {
                type
                pastDuration {
                  value
                  unit
                }
                calendarDuration {
                  unit
                }
              }
            }
          }
          providerQuotaStatus {
            status
            nextResetAt
            ready
            quotaData
            providerType
          }
        }
      }
    }
  }
`;

const CHANNEL_QUOTA_USAGE_QUERY = `
  query ProviderQuotaBadgeChannelQuotaUsage($channelID: ID!) {
    channelQuotaUsage(channelID: $channelID) {
      channelID
      quota {
        requests
        totalTokens
        cost
        period {
          type
          pastDuration {
            value
            unit
          }
          calendarDuration {
            unit
          }
        }
      }
      window {
        start
        end
      }
      usage {
        requestCount
        totalTokens
        totalCost
      }
    }
  }
`;

export async function checkProviderQuotas() {
  return graphqlRequest(CHECK_PROVIDER_QUOTAS_QUERY);
}

type ProviderQuotaDataCommon = {
  plan_type?: string;
  error?: string;
}

export type ProviderClaudeQuotaData = ProviderQuotaDataCommon & {
  windows?: {
    '5h'?: { utilization?: number; reset?: number; status?: string };
    '7d'?: { utilization?: number; reset?: number; status?: string };
    overage?: { utilization?: number; reset?: number; status?: string };
  };
  representative_claim?: string;
}

export type ProviderCodexQuotaData = ProviderQuotaDataCommon & {
  rate_limit?: {
    primary_window?: {
      used_percent?: number;
      reset_at?: number;
      reset_after_seconds?: number;
      limit_window_seconds?: number;
    };
    secondary_window?: {
      used_percent?: number;
      reset_at?: number;
      reset_after_seconds?: number;
      limit_window_seconds?: number;
    };
  };
}

export type CopilotQuotaSnapshot = {
  entitlement?: number;
  has_quota?: boolean;
  overage_count?: number;
  overage_permitted?: boolean;
  percent_remaining?: number;
  quota_id?: string;
  quota_remaining?: number;
  quota_reset_at?: number;
  remaining?: number;
  timestamp_utc?: string;
  unlimited?: boolean;
};

export type ProviderGitHubCopilotQuotaData = ProviderQuotaDataCommon & {
  limited_user_quotas?: Record<string, number | undefined>;
  quota_snapshots?: Record<string, CopilotQuotaSnapshot | undefined>;
  total_quotas?: Record<string, number | undefined>;
}

export type NanoGPTQuotaWindow = {
  used?: number;
  remaining?: number;
  percentUsed?: number;
  resetAt?: number;
}

export type ProviderNanoGPTQuotaData = ProviderQuotaDataCommon & {
  state?: string;
  active?: boolean;
  allowOverage?: boolean;
  limits?: {
    weeklyInputTokens?: number;
    dailyImages?: number;
    dailyInputTokens?: number;
  };
  windows?: {
    weeklyInputTokens?: NanoGPTQuotaWindow | null;
    dailyImages?: NanoGPTQuotaWindow | null;
    dailyInputTokens?: NanoGPTQuotaWindow | null;
  };
  period?: { currentPeriodEnd?: string };
}

export type ProviderWaferQuotaData = ProviderQuotaDataCommon & {
  current_period_used_percent?: number | null;
  remaining_included_requests?: number | null;
  included_request_limit?: number | null;
  overage_request_count?: number | null;
  window_start?: string | null;
  window_end?: string | null;
  plan_tier?: string | null;
}

export type ProviderSyntheticQuotaData = ProviderQuotaDataCommon & {
  weeklyTokenLimit?: { percentRemaining?: number | null; remainingCredits?: string | null; maxCredits?: string | null; nextRegenAt?: string | null } | null;
  rollingFiveHourLimit?: { limited?: boolean | null; remaining?: number | null; max?: number | null; nextTickAt?: string | null; tickPercent?: number | null } | null;
}

export type ProviderNeuralWattQuotaData = ProviderQuotaDataCommon & {
  balance?: { credits_remaining_usd?: number | null; total_credits_usd?: number | null } | null;
  subscription?: { kwh_included?: number | null; kwh_used?: number | null; kwh_remaining?: number | null; in_overage?: boolean | null; status?: string | null; plan?: string | null; kwh_reset_date?: string | null } | null;
}

type ProviderQuotaData =
  | ProviderClaudeQuotaData
  | ProviderCodexQuotaData
  | ProviderGitHubCopilotQuotaData
  | ProviderNanoGPTQuotaData
  | ProviderWaferQuotaData
  | ProviderSyntheticQuotaData
  | ProviderNeuralWattQuotaData
  | (ProviderQuotaDataCommon & Record<string, unknown>);

export type ProviderQuotaChannel = {
  id: string;
  name: string;
  type: string;
  providerType?: string;
  localQuota?: ChannelQuota | null;
  localQuotaUsage?: ChannelQuotaUsage | null;
  localQuotaUsageLoading?: boolean;
  quotaStatus?: {
    status: 'available' | 'warning' | 'exhausted' | 'unknown';
    nextResetAt: string | null;
    ready: boolean;
    quotaData: ProviderQuotaData;
  };
}

export function useProviderQuotaStatuses() {
  const { data } = useQuery({
    queryKey: ['provider-quotas'],
    queryFn: async () => {
      const input = {
        where: {
          statusIn: ['enabled']
        }
      };
      return graphqlRequest<any>(PROVIDER_QUOTA_STATUSES_QUERY, { input });
    },
    refetchInterval: 60000,
    refetchIntervalInBackground: true,
  });

  const channels = data?.queryChannels?.edges?.map((e: any) => e.node) || [];
  const quotaChannels = channels.filter((c: any) => c.providerQuotaStatus != null || c.settings?.quota != null);
  const localQuotaChannels = quotaChannels.filter((c: any) => c.settings?.quota != null);

  const localQuotaUsageQueries = useQueries({
    queries: localQuotaChannels.map((channel: any) => ({
      queryKey: ['channelQuotaUsage', channel.id],
      queryFn: async () => {
        const usageData = await graphqlRequest<{ channelQuotaUsage: ChannelQuotaUsage | null }>(CHANNEL_QUOTA_USAGE_QUERY, {
          channelID: channel.id,
        });
        return channelQuotaUsageSchema.nullable().parse(usageData.channelQuotaUsage);
      },
      enabled: !!channel.id,
      refetchInterval: 60000,
      refetchIntervalInBackground: true,
    })),
  });

  const localQuotaUsageByChannelID = new Map<string, { data: ChannelQuotaUsage | null | undefined; isLoading: boolean }>(
    localQuotaChannels.map((channel: any, index: number) => [
      channel.id,
      {
        data: localQuotaUsageQueries[index]?.data,
        isLoading: localQuotaUsageQueries[index]?.isLoading || localQuotaUsageQueries[index]?.isFetching,
      },
    ] as [string, { data: ChannelQuotaUsage | null | undefined; isLoading: boolean }])
  );

  // Map to standard format - providerQuotaStatus is a single object, not an edge/node structure.
  return quotaChannels.map((channel: any): ProviderQuotaChannel => {
    const quotaStatus = channel.providerQuotaStatus;
    const providerType = quotaStatus?.providerType;
    const localQuotaUsage = localQuotaUsageByChannelID.get(channel.id);
    return {
      id: channel.id,
      name: channel.name,
      type: channel.type,
      ...(channel.type === 'openai' ? { providerType: providerType || undefined } : {}),
      quotaStatus,
      localQuota: channel.settings?.quota ?? null,
      localQuotaUsage: localQuotaUsage?.data,
      localQuotaUsageLoading: localQuotaUsage?.isLoading ?? false,
    };
  });
}
