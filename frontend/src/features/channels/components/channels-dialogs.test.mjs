import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { runInNewContext } from 'node:vm';
import ts from 'typescript';

const source = readFileSync(new URL('./channels-dialogs.tsx', import.meta.url), 'utf8');
const { outputText } = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2023, jsx: ts.JsxEmit.ReactJSX },
});

// Exercise the real dialog coordinator with controlled query/context states.
// Child forms stay shallow so these regressions need no browser or API server.
function harness(open = 'edit') {
  let effects = [];
  let queryEnabled;
  let retries = 0;
  const context = {
    open,
    currentRow: { id: 'channel-1', name: 'Example', type: 'codex' },
    selectedChannels: [],
    setOpen: (value) => { context.open = value; },
    setCurrentRow: (value) => { context.currentRow = value; },
  };
  const query = {
    data: undefined,
    isLoading: false,
    isFetching: false,
    isError: true,
    refetch: () => { retries += 1; },
  };
  const jsx = (type, props, key) => ({ type, props, key });
  const exports = {};
  runInNewContext(outputText, {
    exports,
    require: (name) => {
      if (name === 'react') return { useEffect: (effect) => effects.push(effect) };
      if (name === 'react/jsx-runtime') return { jsx, jsxs: jsx, Fragment: 'Fragment' };
      if (name === '../context/channels-context') return { useChannels: () => context };
      if (name === '../data/channels') return {
        useChannelDetails: (_id, options) => {
          queryEnabled = options.enabled;
          return query;
        },
      };
      // Component imports become named leaf nodes in the rendered tree.
      return new Proxy({}, { get: (_target, key) => key });
    },
  });

  function render() {
    effects = [];
    const nodes = [];
    function visit(node) {
      if (Array.isArray(node)) return node.forEach(visit);
      if (!node || typeof node !== 'object') return;
      nodes.push(node);
      visit(node.props?.children);
    }
    visit(exports.ChannelsDialogs());
    return {
      get: (name) => nodes.find((node) => (node.type.name ?? node.type) === name),
      edit: nodes.find((node) => node.type === 'ChannelsActionDialog' && node.props.open && node.props.currentRow),
    };
  }
  return { context, query, render, flushEffects: () => effects.forEach((effect) => effect()),
    get queryEnabled() { return queryEnabled; }, get retries() { return retries; } };
}

test('failed detail loading never opens an editor with a partial row; retry and close remain available', () => {
  const app = harness();
  const view = app.render();
  assert.equal(view.edit, undefined);
  const failure = view.get('ChannelDetailsLoadDialog');
  assert.equal(failure.props.open, true);
  assert.equal(failure.props.loading, false);
  assert.equal(app.queryEnabled, true);
  failure.props.onRetry();
  assert.equal(app.retries, 1);
  failure.props.onClose();
  assert.equal(app.context.open, null);
  assert.equal(app.context.currentRow, null);
});

test('retry success publishes the full snapshot before mounting forms that read context', () => {
  const app = harness('keyManagement');
  app.query.isError = false;
  app.query.isFetching = true;
  assert.equal(app.render().get('ChannelDetailsLoadDialog').props.loading, true);

  const full = { ...app.context.currentRow, manualModels: ['saved-model'], credentials: { apiKeys: ['test-key'] } };
  app.query.data = full;
  app.query.isFetching = false;
  const beforeSync = app.render();
  assert.equal(beforeSync.get('ChannelsAPIKeyManagementDialog'), undefined);
  assert.equal(beforeSync.get('ChannelDetailsLoadDialog').props.loading, true);
  app.flushEffects();
  const ready = app.render();
  assert.equal(app.context.currentRow, full);
  assert.equal(ready.get('ChannelsAPIKeyManagementDialog').props.open, true);
  assert.equal(ready.get('ChannelDetailsLoadDialog').props.open, false);

  app.context.open = 'edit';
  assert.equal(app.render().edit.props.currentRow, full);
});

test('details from another channel cannot unlock the selected channel editor', () => {
  const app = harness();
  app.query.data = { id: 'channel-2', manualModels: ['other-model'] };
  app.query.isError = false;
  app.render();
  app.flushEffects();
  assert.equal(app.context.currentRow.id, 'channel-1');
  assert.equal(app.render().edit, undefined);
});

test('a retained row does not block add, system settings, or bulk dialogs', () => {
  for (const open of ['add', 'channelSettings', 'bulkImport', 'bulkOrdering', 'bulkApplyTemplate']) {
    const app = harness(open);
    const view = app.render();
    assert.equal(app.queryEnabled, false, open);
    assert.equal(view.get('ChannelDetailsLoadDialog').props.open, false, open);
    if (open === 'add') assert.equal(view.get('ChannelsActionDialog').props.open, true);
  }
});
