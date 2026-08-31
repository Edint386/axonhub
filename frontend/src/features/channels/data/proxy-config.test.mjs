import assert from 'node:assert/strict';
import test from 'node:test';
import { normalizeProxyConfig, ProxyType, proxyConfigSchema } from './proxy-config.ts';

test('normalizeProxyConfig keeps only fields relevant to the selected proxy mode', () => {
  assert.deepEqual(
    normalizeProxyConfig({
      type: ProxyType.DISABLED,
      url: 'http://ignored.example:8080',
      username: 'ignored',
      password: 'ignored',
      disableConnectionReuse: true,
    }),
    { type: ProxyType.DISABLED }
  );

  assert.deepEqual(
    normalizeProxyConfig({
      type: ProxyType.ENVIRONMENT,
      url: 'http://ignored.example:8080',
      disableConnectionReuse: true,
    }),
    { type: ProxyType.ENVIRONMENT }
  );

  assert.deepEqual(
    normalizeProxyConfig({
      type: ProxyType.URL,
      url: 'http://proxy.example:8080',
      username: 'proxy-user',
      password: 'proxy-password',
      disableConnectionReuse: true,
    }),
    {
      type: ProxyType.URL,
      url: 'http://proxy.example:8080',
      username: 'proxy-user',
      password: 'proxy-password',
      disableConnectionReuse: true,
    }
  );
});

test('proxyConfigSchema requires a URL only for URL proxy mode', () => {
  assert.equal(proxyConfigSchema.safeParse({ type: ProxyType.DISABLED }).success, true);
  assert.equal(proxyConfigSchema.safeParse({ type: ProxyType.ENVIRONMENT }).success, true);
  assert.equal(proxyConfigSchema.safeParse({ type: ProxyType.URL, url: '' }).success, false);
  assert.equal(proxyConfigSchema.safeParse({ type: ProxyType.URL, url: 'http://proxy.example:8080' }).success, true);
});
