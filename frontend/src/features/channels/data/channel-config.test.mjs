import assert from 'node:assert/strict';
import test from 'node:test';
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

const dataDir = import.meta.dirname;
const srcRoot = join(dataDir, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

function parseLocale(locale) {
  return JSON.parse(read(`locales/${locale}/channels.json`));
}

test('Cline is available as a channel type in frontend schemas and configs', () => {
  const schema = read('features/channels/data/schema.ts');
  const channelsConfig = read('features/channels/data/config_channels.ts');
  const providersConfig = read('features/channels/data/config_providers.ts');

  assert.match(schema, /channelTypeSchema[\s\S]*'cline'/, 'channelTypeSchema should accept cline');
  assert.match(channelsConfig, /cline:\s*{[\s\S]*channelType:\s*'cline'/, 'CHANNEL_CONFIGS should define cline');
  assert.match(channelsConfig, /cline:\s*{[\s\S]*baseURL:\s*'https:\/\/api\.cline\.bot\/api\/v1'/, 'Cline should use the documented API base URL');
  assert.match(channelsConfig, /cline:\s*{[\s\S]*apiFormat:\s*OPENAI_CHAT_COMPLETIONS/, 'Cline should use OpenAI Chat Completions in the UI');
  assert.match(channelsConfig, /CHANNEL_TYPE_TO_PROVIDER[\s\S]*cline:\s*'cline'/, 'Cline should map to the Cline provider');
  assert.match(providersConfig, /cline:\s*{[\s\S]*channelTypes:\s*\[\s*'cline'\s*\]/, 'PROVIDER_CONFIGS should expose a Cline provider');
});

test('Qiniu exposes OpenAI and Anthropic channel variants after AtlasCloud', () => {
  const schema = read('features/channels/data/schema.ts');
  const channelsConfig = read('features/channels/data/config_channels.ts');
  const providersConfig = read('features/channels/data/config_providers.ts');

  assert.match(schema, /channelTypeSchema[\s\S]*'qiniu'[\s\S]*'qiniu_anthropic'/);
  assert.match(channelsConfig, /qiniu:\s*{[\s\S]*baseURL:\s*'https:\/\/api\.qnaigc\.com\/v1'[\s\S]*apiFormat:\s*OPENAI_CHAT_COMPLETIONS/);
  assert.match(channelsConfig, /qiniu_anthropic:\s*{[\s\S]*baseURL:\s*'https:\/\/api\.qnaigc\.com'[\s\S]*apiFormat:\s*ANTHROPIC_MESSAGES/);
  assert.match(providersConfig, /qiniu:\s*{[\s\S]*channelTypes:\s*\[\s*'qiniu_anthropic',\s*'qiniu'\s*\]/);
  assert.ok(channelsConfig.indexOf('atlascloud:') < channelsConfig.indexOf('qiniu:'));
  assert.ok(providersConfig.indexOf('atlascloud:') < providersConfig.indexOf('qiniu:'));
});

test('Fenno exposes a third-party Codex channel', () => {
  const schema = read('features/channels/data/schema.ts');
  const channelsConfig = read('features/channels/data/config_channels.ts');
  const providersConfig = read('features/channels/data/config_providers.ts');

  assert.match(schema, /channelTypeSchema[\s\S]*'fenno'/);
  assert.match(channelsConfig, /fenno:\s*{[\s\S]*baseURL:\s*'https:\/\/api\.fenno\.ai'[\s\S]*apiFormat:\s*OPENAI_RESPONSES[\s\S]*icon:\s*FennoIcon/);
  assert.match(channelsConfig, /fenno:\s*{[\s\S]*color:\s*'bg-\[#EEF2FF\] text-\[#3155C6\] border-\[#C7D2FE\]'/);
  assert.match(providersConfig, /fenno:\s*{[\s\S]*icon:\s*FennoIcon[\s\S]*channelTypes:\s*\[\s*'fenno'\s*\]/);
  const fennoIcon = read('features/channels/components/fenno-icon.tsx');
  assert.match(fennoIcon, /@\/assets\/fenno-icon\.webp/);
  assert.doesNotMatch(fennoIcon, /https?:\/\//);
  assert.ok(existsSync(join(srcRoot, 'assets/fenno-icon.webp')));
  assert.ok(channelsConfig.indexOf('qiniu_anthropic:') < channelsConfig.indexOf('fenno:'));
  assert.ok(providersConfig.indexOf('qiniu:') < providersConfig.indexOf('fenno:'));
});


test('Cline has localized channel and provider labels', () => {
  for (const locale of ['en', 'zh-CN']) {
    const messages = parseLocale(locale);

    assert.equal(messages['channels.types.cline'], 'Cline');
    assert.equal(messages['channels.providers.cline'], 'Cline');
  }
});

test('xAI subscription is exposed as an OAuth Responses channel', () => {
  const schema = read('features/channels/data/schema.ts');
  const channelsConfig = read('features/channels/data/config_channels.ts');
  const providersConfig = read('features/channels/data/config_providers.ts');
  const channelColumns = read('features/channels/components/channels-columns.tsx');

  assert.match(schema, /channelTypeSchema[\s\S]*'xai_subscription'/);
  assert.equal((schema.match(/data\.type === 'xai_subscription'/g) ?? []).length, 1, 'create schema should validate xAI OAuth credentials');
  assert.match(schema, /effectiveType === 'xai_subscription'/, 'update schema should validate xAI OAuth credentials');
  assert.match(
    schema,
    /requiresJSON\s*=\s*isCopilot\s*\|\|\s*type\s*===\s*'xai_subscription'[\s\S]*if\s*\(requiresJSON\s*&&\s*!apiKey\.trim\(\)\.startsWith\('\{'\)\)/,
    'xAI subscription should reject a plain API key before the generic JSON early return'
  );
  assert.match(
    channelsConfig,
    /xai_subscription:\s*{[\s\S]*baseURL:\s*'https:\/\/cli-chat-proxy\.grok\.com\/v1'[\s\S]*apiFormat:\s*OPENAI_RESPONSES/
  );
  assert.match(providersConfig, /xai_subscription:\s*{[\s\S]*channelTypes:\s*\[\s*'xai_subscription'\s*\]/);
  assert.match(
    channelColumns,
    /channel\.type !== 'xai_subscription'\s*&&\s*\([\s\S]*setOpen\('endpoints'\)/,
    'xAI subscription channels should not expose an endpoint editor that the server rejects'
  );
});

test('channel proxy connection reuse setting is submitted, echoed, and localized', () => {
  const schema = read('features/channels/data/schema.ts');
  const channelsData = read('features/channels/data/channels.ts');
  const proxyDialog = read('features/channels/components/channels-proxy-dialog.tsx');
  const proxyConfig = read('features/channels/data/proxy-config.ts');

  assert.match(
    schema,
    /proxyConfigSchema[\s\S]*disableConnectionReuse:\s*z\.boolean\(\)\.optional\(\)/,
    'ProxyConfig schema should accept disableConnectionReuse'
  );

  const proxySelections = channelsData.match(/proxy\s*\{[\s\S]*?\}/g) ?? [];
  assert.equal(proxySelections.length, 5, 'all five channel proxy selections should be covered by this assertion');
  for (const selection of proxySelections) {
    assert.match(selection, /disableConnectionReuse/, 'channel proxy queries should echo disableConnectionReuse');
  }
  assert.match(channelsData, /proxy\?:\s*ProxyConfig;/, 'channel test input should use the shared ProxyConfig type');

  assert.match(proxyDialog, /export function ProxyConfigFields/, 'proxy fields should be reusable outside the saved-channel dialog');
  assert.match(proxyDialog, /name='disableConnectionReuse'/, 'proxy dialog should render the connection reuse switch');
  const submitSection = proxyDialog.slice(proxyDialog.indexOf('const onSubmit'), proxyDialog.indexOf('const handleTest'));
  const handleTestStart = proxyDialog.indexOf('const handleTest');
  const testSection = proxyDialog.slice(handleTestStart, proxyDialog.indexOf('\n  return (', handleTestStart));
  assert.match(submitSection, /normalizeProxyConfig\(values\)/, 'channel save payload should use the shared proxy normalizer');
  assert.match(testSection, /normalizeProxyConfig\(values\)/, 'channel test payload should use the shared proxy normalizer');
  assert.match(
    proxyConfig,
    /normalizeProxyConfig[\s\S]*disableConnectionReuse:\s*values\.disableConnectionReuse/,
    'the shared normalizer should preserve disableConnectionReuse for URL proxies'
  );
  const presetPayload = submitSection.match(/saveProxyPreset\.mutate\(\{[\s\S]*?\}\);/)?.[0] ?? '';
  assert.doesNotMatch(presetPayload, /disableConnectionReuse/, 'proxy presets should remain address and credential only');
  assert.match(
    proxyDialog,
    /channels\.dialogs\.proxy\.fields\.disableConnectionReuse\.description/,
    'proxy dialog should render the explanatory text below the option'
  );

  const en = parseLocale('en');
  assert.equal(en['channels.dialogs.proxy.fields.disableConnectionReuse.label'], 'Use a new proxy connection for every request');
  assert.equal(
    en['channels.dialogs.proxy.fields.disableConnectionReuse.description'],
    'Enable this for proxy pools such as Resin that rotate nodes per connection. Each request will create a new proxy connection, increasing CONNECT and TLS handshake overhead.'
  );

  const zh = parseLocale('zh-CN');
  assert.equal(zh['channels.dialogs.proxy.fields.disableConnectionReuse.label'], '每次请求使用新的代理连接');
  assert.equal(
    zh['channels.dialogs.proxy.fields.disableConnectionReuse.description'],
    '适用于 Resin 等按连接切换节点的代理池。开启后每个请求都会重新建立代理连接，并增加 CONNECT 与 TLS 握手开销。'
  );
});

test('Codex OAuth configures the same proxy used after the channel is saved', () => {
  const actionDialog = read('features/channels/components/channels-action-dialog.tsx');
  const oauthProxyDialog = read('features/channels/components/oauth-proxy-dialog.tsx');
  const oauthHook = read('features/channels/hooks/use-oauth-flow.ts');

  assert.match(actionDialog, /data-testid='codex-oauth-proxy-button'/, 'Codex OAuth should expose a pre-login proxy button');
  assert.match(
    actionDialog,
    /!isEdit && !\(isCodexType && authMode === 'official'\)/,
    'official Codex login should use the proxy button instead of rendering a second inline proxy editor'
  );
  assert.match(actionDialog, /<OAuthProxyDialog[\s\S]*value=\{proxyConfig\}[\s\S]*onApply=\{handleOAuthProxyApply\}/);
  assert.equal(
    (actionDialog.match(/proxy:\s*proxyConfig/g) ?? []).length,
    2,
    'create and update settings should both persist the exact OAuth proxy config'
  );
  assert.match(
    actionDialog,
    /if \(open\) return;[\s\S]*setProxyType[\s\S]*setProxyDisableConnectionReuse[\s\S]*setOAuthProxyDialogOpen\(false\)/,
    'closing the channel dialog should reset draft OAuth proxy state'
  );
  assert.match(
    oauthProxyDialog,
    /<ProxyConfigFields form=\{form\} proxyPresets=\{proxyPresets\}/,
    'OAuth should reuse the channel proxy fields'
  );
  assert.match(
    oauthHook,
    /if \(proxyConfig\) \{\s*exchangeInput\.proxy = proxyConfig;/,
    'OAuth exchange should send every explicit proxy mode'
  );
  assert.match(oauthHook, /proxyConfigSchema\.safeParse\(proxyConfig\)/, 'OAuth should reject malformed proxy config before starting or exchanging');

  const en = parseLocale('en');
  assert.equal(en['channels.dialogs.oauth.proxy.button'], 'Set Proxy');
  assert.match(en['channels.dialogs.oauth.proxy.description'], /token exchange/);
  assert.match(en['channels.dialogs.oauth.proxy.current'], /Current server route/);
  assert.match(en['channels.dialogs.oauth.errors.proxyInvalid'], /valid proxy/);

  const zh = parseLocale('zh-CN');
  assert.equal(zh['channels.dialogs.oauth.proxy.button'], '设置代理');
  assert.match(zh['channels.dialogs.oauth.proxy.description'], /交换令牌/);
  assert.match(zh['channels.dialogs.oauth.proxy.current'], /当前服务端线路/);
  assert.match(zh['channels.dialogs.oauth.errors.proxyInvalid'], /有效代理/);
});
