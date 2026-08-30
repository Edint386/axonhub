import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';

const srcRoot = join(import.meta.dirname, '..', '..', '..');

function read(relativePath) {
  return readFileSync(join(srcRoot, relativePath), 'utf8');
}

test('Channel ACL uses one shared backend policy from both management pages', () => {
  const schema = read('features/channel-access/data/schema.ts');
  const data = read('features/channel-access/data/channel-access.ts');
  const policyDialog = read('features/channel-access/components/channel-access-policy-dialog.tsx');
  const apiKeyDrawer = read('features/channel-access/components/api-key-channel-access-drawer.tsx');
  const channelContext = read('features/channels/context/channels-context.tsx');
  const channelActions = read('features/channels/components/channels-columns.tsx');
  const apiKeyContext = read('features/apikeys/context/apikeys-context.tsx');
  const apiKeyActions = read('features/apikeys/components/data-table-row-actions.tsx');

  assert.match(schema, /z\.enum\(\['public', 'allowlist', 'denylist'\]\)/);
  assert.match(data, /channelCallerAccessPolicy\(channelID: \$channelID\)/);
  assert.match(data, /apiKeyChannelCallerAccess\(apiKeyID: \$apiKeyID\)/);
  assert.match(schema, /memberAPIKeyIDs/);
  assert.doesNotMatch(schema, /expectedRevision|revision/);
  assert.match(data, /invalidateQueries\(\{ queryKey: \['channels'\]/);
  assert.match(data, /invalidateQueries\(\{ queryKey: \['apiKeys'\]/);

  assert.match(policyDialog, /hasSystemScope\('read_channels'\) && hasSystemScope\('read_api_keys'\)/);
  assert.match(policyDialog, /hasSystemScope\('write_channels'\) && hasSystemScope\('write_api_keys'\)/);
  assert.match(data, /channelCallerAccessCandidates/);
  assert.match(data, /projectID projectName/);
  assert.match(data, /queryKey: \['apiKeys', 'channelAccessCandidates'\]/);
  assert.doesNotMatch(data, /CHANNEL_ACCESS_POLICY_CONFLICT|markServerErrorHandled/);
  assert.match(policyDialog, /setMode\('allowlist'\)[\s\S]*new Set\(\[apiKeyID\]\)/);
  assert.match(apiKeyDrawer, /entry\.allowed/);
  assert.match(apiKeyDrawer, /entry\.isMember/);
  assert.match(apiKeyDrawer, /<ChannelAccessPolicyDialog/);

  assert.match(channelContext, /'callerAccess'/);
  assert.match(channelActions, /setOpen\('callerAccess'\)/);
  assert.match(apiKeyContext, /'channelAccess'/);
  assert.match(apiKeyActions, /openDialog\('channelAccess', apiKey\)/);
});

test('Channel ACL text is localized in both actual locale directories', () => {
  for (const locale of ['en', 'zh-CN']) {
    const channels = JSON.parse(read(`locales/${locale}/channels.json`));
    const apiKeys = JSON.parse(read(`locales/${locale}/apikeys.json`));

    assert.ok(channels['channels.actions.callerAccess']);
    assert.ok(channels['channels.callerAccess.modes.public.label']);
    assert.ok(channels['channels.callerAccess.modes.allowlist.label']);
    assert.ok(channels['channels.callerAccess.modes.denylist.label']);
    assert.ok(channels['channels.callerAccess.actions.exclusive']);
    assert.ok(apiKeys['apikeys.actions.channelAccess']);
    assert.ok(apiKeys['apikeys.channelAccess.allowed']);
    assert.ok(apiKeys['apikeys.channelAccess.denied']);
  }
});
