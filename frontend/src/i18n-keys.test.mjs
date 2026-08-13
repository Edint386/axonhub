import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const frontendRoot = join(import.meta.dirname, '..');

function loadCatalog(locale) {
  const directory = join(import.meta.dirname, 'locales', locale);
  return Object.assign(
    {},
    ...readdirSync(directory)
      .filter((filename) => filename.endsWith('.json'))
      .map((filename) => JSON.parse(readFileSync(join(directory, filename), 'utf8')))
  );
}

test('all static translation calls resolve in English and Chinese', () => {
  assert.doesNotThrow(() => {
    execFileSync(process.execPath, ['scripts/audit-i18n-keys.mjs', '.'], {
      cwd: frontendRoot,
      stdio: 'pipe',
    });
  });
});

test('locale catalogs stay aligned except for language-specific plural forms', () => {
  const en = loadCatalog('en');
  const zh = loadCatalog('zh-CN');
  const isEnglishPlural = (key) => /_(one|other)$/.test(key) && key.replace(/_(one|other)$/, '') in zh;
  const isChinesePluralBase = (key) => `${key}_one` in en && `${key}_other` in en;

  assert.deepEqual(Object.keys(en).filter((key) => !(key in zh) && !isEnglishPlural(key)), []);
  assert.deepEqual(Object.keys(zh).filter((key) => !(key in en) && !isChinesePluralBase(key)), []);
});

test('indirect channel quota keys resolve in both locales', () => {
  const requiredKeys = [
    'channels.dialogs.rateLimit.quota.requests.placeholder',
    'channels.dialogs.rateLimit.quota.totalTokens.placeholder',
    'channels.dialogs.rateLimit.quota.cost.placeholder',
  ];

  for (const locale of ['en', 'zh-CN']) {
    const catalog = loadCatalog(locale);
    for (const key of requiredKeys) assert.equal(typeof catalog[key], 'string', `${locale} is missing ${key}`);
  }
});
