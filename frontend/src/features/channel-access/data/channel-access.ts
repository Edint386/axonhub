import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { graphqlRequest } from '@/gql/graphql';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { useErrorHandler } from '@/hooks/use-error-handler';
import {
  apiKeyChannelCallerAccessListSchema,
  callerAPIKeySummarySchema,
  channelCallerAccessPolicySchema,
  type CallerAPIKeySummary,
  type APIKeyChannelCallerAccess,
  type ChannelCallerAccessPolicy,
  type SetChannelCallerAccessPolicyInput,
} from './schema';

const CHANNEL_CALLER_ACCESS_POLICY_QUERY = `
  query ChannelCallerAccessPolicy($channelID: ID!) {
    channelCallerAccessPolicy(channelID: $channelID) {
      channel { id name type status }
      mode
      members { id name type status projectID projectName }
    }
  }
`;

const API_KEY_CHANNEL_CALLER_ACCESS_QUERY = `
  query APIKeyChannelCallerAccess($apiKeyID: ID!) {
    apiKeyChannelCallerAccess(apiKeyID: $apiKeyID) {
      channel { id name type status }
      mode
      isMember
      allowed
    }
  }
`;

const SET_CHANNEL_CALLER_ACCESS_POLICY_MUTATION = `
  mutation SetChannelCallerAccessPolicy($input: SetChannelCallerAccessPolicyInput!) {
    setChannelCallerAccessPolicy(input: $input) {
      channel { id name type status }
      mode
      members { id name type status projectID projectName }
    }
  }
`;

const CHANNEL_ACCESS_API_KEYS_QUERY = `
  query ChannelAccessAPIKeys {
    channelCallerAccessCandidates {
      id name type status projectID projectName
    }
  }
`;

export function useChannelCallerAccessPolicy(channelID: string, options?: { enabled?: boolean }) {
  const { handleError } = useErrorHandler();
  const { t } = useTranslation();

  return useQuery({
    queryKey: ['channelCallerAccessPolicy', channelID],
    enabled: (options?.enabled ?? true) && Boolean(channelID),
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ channelCallerAccessPolicy: ChannelCallerAccessPolicy }>(CHANNEL_CALLER_ACCESS_POLICY_QUERY, {
          channelID,
        });
        return channelCallerAccessPolicySchema.parse(data.channelCallerAccessPolicy);
      } catch (error) {
        handleError(error, t('channels.callerAccess.messages.loadError'));
        throw error;
      }
    },
  });
}

export function useAPIKeyChannelCallerAccess(apiKeyID: string, options?: { enabled?: boolean }) {
  const { handleError } = useErrorHandler();
  const { t } = useTranslation();

  return useQuery({
    queryKey: ['apiKeyChannelCallerAccess', apiKeyID],
    enabled: (options?.enabled ?? true) && Boolean(apiKeyID),
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ apiKeyChannelCallerAccess: APIKeyChannelCallerAccess[] }>(API_KEY_CHANNEL_CALLER_ACCESS_QUERY, {
          apiKeyID,
        });
        return apiKeyChannelCallerAccessListSchema.parse(data.apiKeyChannelCallerAccess);
      } catch (error) {
        handleError(error, t('apikeys.channelAccess.messages.loadError'));
        throw error;
      }
    },
  });
}

export function useChannelAccessAPIKeys(options?: { enabled?: boolean }) {
  const { handleError } = useErrorHandler();
  const { t } = useTranslation();

  return useQuery({
    queryKey: ['apiKeys', 'channelAccessCandidates'],
    enabled: options?.enabled ?? true,
    queryFn: async () => {
      try {
        const data = await graphqlRequest<{ channelCallerAccessCandidates: CallerAPIKeySummary[] }>(CHANNEL_ACCESS_API_KEYS_QUERY);
        return data.channelCallerAccessCandidates.map((apiKey) => callerAPIKeySummarySchema.parse(apiKey));
      } catch (error) {
        handleError(error, t('channels.callerAccess.messages.loadError'));
        throw error;
      }
    },
  });
}

export function useSetChannelCallerAccessPolicy() {
  const queryClient = useQueryClient();
  const { handleError } = useErrorHandler();
  const { t } = useTranslation();

  return useMutation({
    mutationFn: async (input: SetChannelCallerAccessPolicyInput) => {
      try {
        const data = await graphqlRequest<{ setChannelCallerAccessPolicy: ChannelCallerAccessPolicy }>(
          SET_CHANNEL_CALLER_ACCESS_POLICY_MUTATION,
          { input }
        );
        return channelCallerAccessPolicySchema.parse(data.setChannelCallerAccessPolicy);
      } catch (error) {
        handleError(error, t('channels.callerAccess.messages.saveError'));
        throw error;
      }
    },
    onSuccess: () => {
      toast.success(t('channels.callerAccess.messages.saveSuccess'));
      queryClient.invalidateQueries({ queryKey: ['channelCallerAccessPolicy'] });
      queryClient.invalidateQueries({ queryKey: ['apiKeyChannelCallerAccess'] });
      queryClient.invalidateQueries({ queryKey: ['channels'] });
      queryClient.invalidateQueries({ queryKey: ['channel'] });
      queryClient.invalidateQueries({ queryKey: ['apiKeys'] });
      queryClient.invalidateQueries({ queryKey: ['apiKey'] });
    },
  });
}
