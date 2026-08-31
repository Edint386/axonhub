import { z } from 'zod';

export const ProxyType = {
  DISABLED: 'disabled',
  ENVIRONMENT: 'environment',
  URL: 'url',
} as const;

export type ProxyType = (typeof ProxyType)[keyof typeof ProxyType];

export const proxyConfigSchema = z
  .object({
    type: z.enum([ProxyType.DISABLED, ProxyType.ENVIRONMENT, ProxyType.URL]),
    url: z.string().optional(),
    username: z.string().optional(),
    password: z.string().optional(),
    disableConnectionReuse: z.boolean().optional(),
  })
  .refine(
    (data) => {
      if (data.type === ProxyType.URL) {
        return !!data.url && data.url.trim() !== '';
      }
      return true;
    },
    {
      message: 'Proxy URL is required when type is URL',
      path: ['url'],
    }
  );

export type ProxyConfig = z.infer<typeof proxyConfigSchema>;

export function normalizeProxyConfig(values: ProxyConfig): ProxyConfig {
  return {
    type: values.type,
    ...(values.type === ProxyType.URL && {
      url: values.url,
      username: values.username || undefined,
      password: values.password || undefined,
      disableConnectionReuse: values.disableConnectionReuse,
    }),
  };
}
