import { useState, useCallback } from 'react';
import { useTranslation } from 'react-i18next';
import { toast } from 'sonner';
import { proxyConfigSchema, type ProxyConfig } from '../data/proxy-config';

export type { ProxyConfig } from '../data/proxy-config';

export interface OAuthStartResult {
  session_id: string;
  auth_url: string;
}

export interface OAuthExchangeInput {
  session_id: string;
  callback_url: string;
  proxy?: ProxyConfig;
}

export interface OAuthExchangeResult {
  credentials: string;
}

export interface OAuthFlowOptions {
  /**
   * Function to call the OAuth start endpoint
   */
  startFn: (headers?: Record<string, string>) => Promise<OAuthStartResult>;

  /**
   * Function to call the OAuth exchange endpoint
   */
  exchangeFn: (input: OAuthExchangeInput, headers?: Record<string, string>) => Promise<OAuthExchangeResult>;

  /**
   * Optional proxy configuration for exchange token request
   */
  proxyConfig?: ProxyConfig;

  /**
   * Callback when credentials are successfully obtained
   */
  onSuccess?: (credentials: string) => void;
}

export interface OAuthFlowState {
  sessionId: string | null;
  authUrl: string | null;
  callbackUrl: string;
  isStarting: boolean;
  isExchanging: boolean;
}

export interface OAuthFlowActions {
  start: () => Promise<void>;
  exchange: () => Promise<void>;
  setCallbackUrl: (url: string) => void;
  reset: () => void;
}

/**
 * A reusable hook for managing OAuth flows (e.g., Codex, Claude Code).
 * This eliminates code duplication for different OAuth providers.
 *
 * @example
 * ```tsx
 * const codexOAuth = useOAuthFlow({
 *   startFn: codexOAuthStart,
 *   exchangeFn: codexOAuthExchange,
 *   onSuccess: (credentials) => form.setValue('credentials.apiKey', credentials),
 * });
 *
 * // Later in your component:
 * <Button onClick={codexOAuth.start} disabled={codexOAuth.isStarting}>
 *   {codexOAuth.isStarting ? 'Starting...' : 'Start OAuth'}
 * </Button>
 * ```
 */
export function useOAuthFlow(options: OAuthFlowOptions): OAuthFlowState & OAuthFlowActions {
  const { startFn, exchangeFn, proxyConfig, onSuccess } = options;
  const { t } = useTranslation();

  const [sessionId, setSessionId] = useState<string | null>(null);
  const [authUrl, setAuthUrl] = useState<string | null>(null);
  const [callbackUrl, setCallbackUrl] = useState('');
  const [isStarting, setIsStarting] = useState(false);
  const [isExchanging, setIsExchanging] = useState(false);

  const hasValidProxyConfig = useCallback(() => {
    if (!proxyConfig || proxyConfigSchema.safeParse(proxyConfig).success) return true;

    toast.error(t('channels.dialogs.oauth.errors.proxyInvalid'));
    return false;
  }, [proxyConfig, t]);

  const start = useCallback(async () => {
    if (!hasValidProxyConfig()) return;

    setIsStarting(true);
    try {
      const result = await startFn();
      setSessionId(result.session_id);
      setAuthUrl(result.auth_url);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    } finally {
      setIsStarting(false);
    }
  }, [hasValidProxyConfig, startFn]);

  const exchange = useCallback(async () => {
    if (!sessionId) {
      toast.error(t('channels.dialogs.oauth.errors.sessionMissing'));
      return;
    }

    if (!callbackUrl.trim()) {
      toast.error(t('channels.dialogs.oauth.errors.callbackUrlRequired'));
      return;
    }

    if (!hasValidProxyConfig()) return;

    setIsExchanging(true);
    try {
      const exchangeInput: OAuthExchangeInput = {
        session_id: sessionId,
        callback_url: callbackUrl.trim(),
      };

      if (proxyConfig) {
        exchangeInput.proxy = proxyConfig;
      }

      const result = await exchangeFn(exchangeInput);

      if (onSuccess) {
        onSuccess(result.credentials);
      }

      toast.success(t('channels.dialogs.oauth.messages.credentialsImported'));
    } catch (error) {
      toast.error(error instanceof Error ? error.message : String(error));
    } finally {
      setIsExchanging(false);
    }
  }, [sessionId, callbackUrl, exchangeFn, onSuccess, t, proxyConfig, hasValidProxyConfig]);

  const reset = useCallback(() => {
    setSessionId(null);
    setAuthUrl(null);
    setCallbackUrl('');
    setIsStarting(false);
    setIsExchanging(false);
  }, []);

  return {
    sessionId,
    authUrl,
    callbackUrl,
    isStarting,
    isExchanging,
    start,
    exchange,
    setCallbackUrl,
    reset,
  };
}
