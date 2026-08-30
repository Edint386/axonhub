import { z } from 'zod';
import { apiKeyStatusSchema, apiKeyTypeSchema } from '@/features/apikeys/data/schema';
import { channelStatusSchema, channelTypeSchema } from '@/features/channels/data/schema';

export const channelCallerAccessModeSchema = z.enum(['public', 'allowlist', 'denylist']);
export type ChannelCallerAccessMode = z.infer<typeof channelCallerAccessModeSchema>;

export const callerAPIKeySummarySchema = z.object({
  id: z.string(),
  name: z.string(),
  type: apiKeyTypeSchema,
  status: apiKeyStatusSchema,
  projectID: z.string(),
  projectName: z.string(),
});
export type CallerAPIKeySummary = z.infer<typeof callerAPIKeySummarySchema>;

export const callerAccessChannelSummarySchema = z.object({
  id: z.string(),
  name: z.string(),
  type: channelTypeSchema,
  status: channelStatusSchema,
});
export type CallerAccessChannelSummary = z.infer<typeof callerAccessChannelSummarySchema>;

export const channelCallerAccessPolicySchema = z.object({
  channel: callerAccessChannelSummarySchema,
  mode: channelCallerAccessModeSchema,
  members: z.array(callerAPIKeySummarySchema),
});
export type ChannelCallerAccessPolicy = z.infer<typeof channelCallerAccessPolicySchema>;

export const apiKeyChannelCallerAccessSchema = z.object({
  channel: callerAccessChannelSummarySchema,
  mode: channelCallerAccessModeSchema,
  isMember: z.boolean(),
  allowed: z.boolean(),
});
export const apiKeyChannelCallerAccessListSchema = z.array(apiKeyChannelCallerAccessSchema);
export type APIKeyChannelCallerAccess = z.infer<typeof apiKeyChannelCallerAccessSchema>;

export type SetChannelCallerAccessPolicyInput = {
  channelID: string;
  mode: ChannelCallerAccessMode;
  memberAPIKeyIDs: string[];
};
