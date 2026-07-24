import { useQueries, useQuery } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { channelQuotaUsageSchema } from '@/features/channels/data/schema';
import type { ChannelQuota, ChannelQuotaUsage } from '@/features/channels/data/schema';

const CHECK_PROVIDER_QUOTAS_QUERY = `
  mutation CheckProviderQuotas {
    checkProviderQuotas
  }
`;

const RESET_CHANNEL_QUOTA_NOW_MUTATION = `
  mutation ResetChannelQuotaNow($channelID: ID!) {
    resetChannelQuotaNow(channelID: $channelID)
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
          providerQuotaStatus {
            status
            nextResetAt
            ready
            quotaData
            providerType
          }
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
            providerQuota {
              opencodeGo {
                workspaceId
              }
            }
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

export async function resetChannelQuotaNow(channelID: string) {
  return graphqlRequest(RESET_CHANNEL_QUOTA_NOW_MUTATION, { channelID });
}

type ProviderQuotaDataCommon = {
  plan_type?: string;
  error?: string;
};

export type ProviderClaudeQuotaData = ProviderQuotaDataCommon & {
  windows?: {
    '5h'?: { utilization?: number; reset?: number; status?: string };
    '7d'?: { utilization?: number; reset?: number; status?: string };
    overage?: { utilization?: number; reset?: number; status?: string };
  };
  representative_claim?: string;
};

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
};

export type CopilotQuotaSnapshot = {
  entitlement: number;
  has_quota: boolean;
  overage_count: number;
  overage_permitted: boolean;
  percent_remaining: number;
  quota_id: string;
  quota_remaining: number;
  quota_reset_at: number;
  remaining: number;
  timestamp_utc: string;
  unlimited: boolean;
};

export type ProviderGitHubCopilotQuotaData = ProviderQuotaDataCommon & {
  limited_user_quotas?: {
    chat?: number;
    completions?: number;
    [key: string]: number | undefined;
  };
  quota_snapshots?: {
    chat?: CopilotQuotaSnapshot;
    completions?: CopilotQuotaSnapshot;
    premium_interactions?: CopilotQuotaSnapshot;
    premium_models?: CopilotQuotaSnapshot;
    [key: string]: CopilotQuotaSnapshot | undefined;
  };
  total_quotas?: {
    chat?: number;
    completions?: number;
    [key: string]: number | undefined;
  };
};

export type NanoGPTQuotaWindow = {
  used?: number;
  remaining?: number;
  percentUsed?: number;
  resetAt?: number;
};

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
};

export type ProviderWaferQuotaData = ProviderQuotaDataCommon & {
  current_period_used_percent?: number | null;
  remaining_included_requests?: number | null;
  included_request_limit?: number | null;
  overage_request_count?: number | null;
  window_start?: string | null;
  window_end?: string | null;
  plan_tier?: string | null;
};

export type ProviderSyntheticQuotaData = ProviderQuotaDataCommon & {
  weeklyTokenLimit?: {
    percentRemaining?: number | null;
    remainingCredits?: string | null;
    maxCredits?: string | null;
    nextRegenAt?: string | null;
  } | null;
  rollingFiveHourLimit?: {
    limited?: boolean | null;
    remaining?: number | null;
    max?: number | null;
    nextTickAt?: string | null;
    tickPercent?: number | null;
  } | null;
};

export type ProviderNeuralWattQuotaData = ProviderQuotaDataCommon & {
  balance?: { credits_remaining_usd?: number | null; total_credits_usd?: number | null } | null;
  subscription?: {
    kwh_included?: number | null;
    kwh_used?: number | null;
    kwh_remaining?: number | null;
    in_overage?: boolean | null;
    status?: string | null;
    plan?: string | null;
    kwh_reset_date?: string | null;
  } | null;
};

export type ProviderApertisQuotaData = ProviderQuotaDataCommon & {
  is_subscriber?: boolean;
  payg?: {
    account_credits?: number;
    token_used?: number;
    token_total?: number | string;
    token_remaining?: number | string;
    token_is_unlimited?: boolean;
    token_monthly_limit_usd?: number;
    token_monthly_used_usd?: number;
    monthly_reset_day?: number;
  };
  subscription?: {
    plan_type?: string;
    status?: string;
    cycle_quota_limit?: number;
    cycle_quota_used?: number;
    cycle_quota_remaining?: number;
    cycle_start?: string;
    cycle_end?: string;
    payg_fallback_enabled?: boolean;
    payg_spent_usd?: number;
    payg_limit_usd?: number;
  };
};

export type OpenCodeGoQuotaWindow = {
  usage_percent?: number;
  reset_in_seconds?: number;
  reset_time?: string;
  status?: string;
  percent_remaining?: number;
};

export type ProviderOpenCodeGoQuotaData = ProviderQuotaDataCommon & {
  windows?: {
    rolling?: OpenCodeGoQuotaWindow;
    weekly?: OpenCodeGoQuotaWindow;
    monthly?: OpenCodeGoQuotaWindow;
  };
};

export type KimiCodeUsageRow = {
  label: string;
  used: number;
  limit: number;
  resetAt?: string;
  resetAfterSeconds?: number;
};

export type ProviderKimiCodeQuotaData = ProviderQuotaDataCommon & {
  rows?: KimiCodeUsageRow[];
  boosterWallet?: {
    balanceCents: number;
    totalCents: number;
    monthlyChargeLimitEnabled: boolean;
    monthlyChargeLimitCents: number;
    monthlyUsedCents: number;
    currency: string;
  };
};

export type MinimaxModelRow = {
  modelName: string;
  intervalUsedPercent: number;
  intervalTotalPercent: number;
  intervalPercent: number;
  intervalStatus: string;
  intervalResetAt?: string;
  weeklyUsedPercent: number;
  weeklyTotalPercent: number;
  weeklyPercent: number;
  weeklyStatus: string;
  weeklyResetAt?: string;
  weeklyBoostPermille?: number;
};

export type ProviderMinimaxQuotaData = ProviderQuotaDataCommon & {
  rows?: MinimaxModelRow[];
};

export type ZhipuWindowRow = {
  window: string;
  usedPercent: number;
  status: string;
  resetAt?: string;
};

export type ProviderZhipuQuotaData = ProviderQuotaDataCommon & {
  rows?: ZhipuWindowRow[];
  level?: string;
};

export type ClineQuotaWindow = {
  items_count: number;
  used_cost_units: number;
  limit_cost_units: number;
  remaining_cost_units: number;
  credits_used: number;
  usage_ratio?: number;
  usage_percent?: number;
  next_reset_at?: string | null;
};

type ClineBalance = {
  raw_balance?: number | null;
  unit_note?: string;
};

type ClineUsageFetch = {
  pages: number;
  items_seen: number;
  truncated: boolean;
};

type ProviderClinePassQuotaData = ProviderQuotaDataCommon & {
  model_scope: 'cline_pass_only' | 'mixed' | 'unknown';
  status_basis: string;
  pool: 'cline_pass';
  pool_note?: string;
  cost_scale: number;
  balance: ClineBalance;
  windows: {
    last5h: ClineQuotaWindow;
    last7d: ClineQuotaWindow;
    last30d: ClineQuotaWindow;
  };
  usage_fetch: ClineUsageFetch;
};

type ProviderClineDirectQuotaData = ProviderQuotaDataCommon & {
  model_scope: 'direct_only';
  status_basis: string;
  pool: 'direct_credit' | string;
  pool_note?: string;
  balance: ClineBalance;
  cost_scale?: never;
  windows?: never;
  usage_fetch?: never;
};

type ProviderClineErrorQuotaData = ProviderQuotaDataCommon & {
  model_scope?: undefined;
  status_basis?: string;
  pool?: string;
  balance?: ClineBalance;
  cost_scale?: never;
  windows?: never;
  usage_fetch?: never;
};

export type ProviderClineQuotaData = ProviderClinePassQuotaData | ProviderClineDirectQuotaData | ProviderClineErrorQuotaData;

export function isClinePassPoolQuotaData(qd: ProviderClineQuotaData): qd is ProviderClinePassQuotaData {
  return qd.pool === 'cline_pass';
}

export type ProviderQuotaData =
  | ProviderClaudeQuotaData
  | ProviderCodexQuotaData
  | ProviderClineQuotaData
  | ProviderGitHubCopilotQuotaData
  | ProviderNanoGPTQuotaData
  | ProviderOpenCodeGoQuotaData
  | ProviderKimiCodeQuotaData
  | ProviderMinimaxQuotaData
  | ProviderZhipuQuotaData
  | ProviderWaferQuotaData
  | ProviderSyntheticQuotaData
  | ProviderNeuralWattQuotaData
  | ProviderApertisQuotaData
  | (ProviderQuotaDataCommon & Record<string, unknown>);

export type ProviderQuotaStatus = {
  status: 'available' | 'warning' | 'exhausted' | 'unknown';
  nextResetAt: string | null;
  ready: boolean;
  quotaData: ProviderQuotaData;
  providerType?: string | null;
};

export type ProviderQuotaChannel = {
  id: string;
  name: string;
  type: string;
  providerType?: string;
  workspaceId?: string | null;
  localQuota?: ChannelQuota | null;
  localQuotaUsage?: ChannelQuotaUsage | null;
  localQuotaUsageLoading?: boolean;
  quotaStatus?: ProviderQuotaStatus;
};

type ProviderQuotaStatusNode = {
  status: 'available' | 'warning' | 'exhausted' | 'unknown';
  nextResetAt: string | null;
  ready: boolean;
  quotaData: unknown;
  providerType: string;
};

type QueryChannelNode = {
  id: string;
  name: string;
  type: string;
  providerQuotaStatus: ProviderQuotaStatusNode | null;
  settings?: {
    quota?: ChannelQuota | null;
    providerQuota?: {
      opencodeGo?: {
        workspaceId?: string | null;
      } | null;
    } | null;
  } | null;
};

type QueryChannelsResponse = {
  queryChannels: {
    edges: Array<{
      node: QueryChannelNode | null;
    } | null>;
  };
};

function hasQuotaStatusOrLocalQuota(node: QueryChannelNode | null | undefined): node is QueryChannelNode {
  return node != null && (node.providerQuotaStatus != null || node.settings?.quota != null);
}

export function useProviderQuotaStatuses() {
  const query = useQuery({
    queryKey: ['provider-quotas'],
    queryFn: async () => {
      const input = {
        where: {
          statusIn: ['enabled'],
        },
      };
      return graphqlRequest<QueryChannelsResponse>(PROVIDER_QUOTA_STATUSES_QUERY, { input });
    },
    refetchInterval: 60000,
    refetchIntervalInBackground: true,
  });

  const quotaChannels = (query.data?.queryChannels?.edges ?? [])
    .map((edge) => edge?.node ?? null)
    .filter(hasQuotaStatusOrLocalQuota)
    .filter((channel) => {
      // Preserve local quotas even where provider credentials are absent. For
      // provider quota rows, retain upstream's noise filter.
      if (channel.settings?.quota != null) return true;
      const quotaData = channel.providerQuotaStatus?.quotaData as { error?: string } | undefined;
      return quotaData?.error !== 'channel has no credentials';
    });

  const localQuotaChannels = quotaChannels.filter((channel) => channel.settings?.quota != null);
  const localQuotaUsageQueries = useQueries({
    queries: localQuotaChannels.map((channel) => ({
      queryKey: ['channelQuotaUsage', channel.id],
      queryFn: async () => {
        const data = await graphqlRequest<{ channelQuotaUsage: ChannelQuotaUsage | null }>(CHANNEL_QUOTA_USAGE_QUERY, {
          channelID: channel.id,
        });
        return channelQuotaUsageSchema.nullable().parse(data.channelQuotaUsage);
      },
      enabled: !!channel.id,
      refetchInterval: 60000,
      refetchIntervalInBackground: true,
    })),
  });

  const localQuotaUsageByChannelID = new Map<string, { data: ChannelQuotaUsage | null | undefined; isLoading: boolean }>(
    localQuotaChannels.map((channel, index) => [
      channel.id,
      {
        data: localQuotaUsageQueries[index]?.data,
        isLoading: localQuotaUsageQueries[index]?.isLoading || localQuotaUsageQueries[index]?.isFetching,
      },
    ] as [string, { data: ChannelQuotaUsage | null | undefined; isLoading: boolean }])
  );

  const channels = quotaChannels.map((channel): ProviderQuotaChannel => {
    const providerQuotaStatus = channel.providerQuotaStatus;
    const localQuotaUsage = localQuotaUsageByChannelID.get(channel.id);

    return {
      id: channel.id,
      name: channel.name,
      type: channel.type,
      providerType: providerQuotaStatus?.providerType || undefined,
      workspaceId: channel.settings?.providerQuota?.opencodeGo?.workspaceId ?? null,
      quotaStatus: providerQuotaStatus
        ? {
            status: providerQuotaStatus.status,
            nextResetAt: providerQuotaStatus.nextResetAt,
            ready: providerQuotaStatus.ready,
            quotaData: providerQuotaStatus.quotaData as ProviderQuotaData,
            providerType: providerQuotaStatus.providerType,
          }
        : undefined,
      localQuota: channel.settings?.quota ?? null,
      localQuotaUsage: localQuotaUsage?.data,
      localQuotaUsageLoading: localQuotaUsage?.isLoading ?? false,
    };
  });

  return {
    channels,
    isLoading: query.isLoading,
    isError: query.isError,
    error: query.error,
    isFetching: query.isFetching,
  };
}
