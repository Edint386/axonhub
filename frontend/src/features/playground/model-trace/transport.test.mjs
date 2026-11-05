import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import { PLAYGROUND_CHAT_API, readTextStream, sendPlaygroundProbe } from './transport.ts';

function streamOf(...chunks) {
  const encoder = new TextEncoder();
  return new ReadableStream({
    start(controller) {
      for (const chunk of chunks) controller.enqueue(encoder.encode(chunk));
      controller.close();
    },
  });
}

const sse = (event) => `data: ${JSON.stringify(event)}\n\n`;

test('collects text deltas across split chunks and ignores reasoning', async () => {
  const body = sse({ type: 'start' }) + sse({ type: 'reasoning-delta', id: 'r', delta: 'thinking 999' }) + sse({ type: 'text-delta', id: 't', delta: '12, 3' });
  const updates = [];
  const { text, finished } = await readTextStream(
    streamOf(body.slice(0, 50), body.slice(50), sse({ type: 'text-delta', id: 't', delta: '4, 5' }), sse({ type: 'finish' }), 'data: [DONE]\n\n'),
    (value) => updates.push(value)
  );
  assert.equal(text, '12, 34, 5');
  assert.equal(finished, true);
  assert.deepEqual(updates, ['12, 3', '12, 34, 5']);
});

test('handles CRLF framing and a final event without a trailing blank line', async () => {
  const { text } = await readTextStream(
    streamOf('data: {"type":"text-delta","id":"t","delta":"7"}\r\n\r\n', 'data: {"type":"text-delta","id":"t","delta":"8"}\r\n\r\n', 'data: {"type":"finish"}')
  );
  assert.equal(text, '78');
});

test('reads the backend playground stream to the end', async () => {
  // The backend's golden AI SDK output (llm/transformer/aisdk tests), also replayed by upstream's playground transport test.
  const fixture = readFileSync(join(import.meta.dirname, '../../../../../llm/transformer/aisdk/testdata/aisdk-strop.stream.jsonl'), 'utf8');
  const events = fixture.trim().split('\n').map((line) => JSON.parse(line).Data);
  const expected = events.filter((event) => event !== '[DONE]').map((event) => JSON.parse(event))
    .filter((event) => event.type === 'text-delta').map((event) => event.delta).join('');
  assert.ok(expected.length > 0);
  const answer = await readTextStream(streamOf(...events.map((event) => `data: ${event}\n\n`)));
  assert.deepEqual(answer, { text: expected, finished: true });
});

test('reports a stream that ends without the finish event', async () => {
  // e.g. the upstream dropped mid-answer and the backend still closed the stream with [DONE]
  const answer = await readTextStream(streamOf(sse({ type: 'text-delta', id: 't', delta: '1, 2, 3' }), 'data: [DONE]\n\n'));
  assert.deepEqual(answer, { text: '1, 2, 3', finished: false });
});

test('stream errors become request failures', async () => {
  await assert.rejects(readTextStream(streamOf(sse({ type: 'error', errorText: 'quota exhausted' }))), /quota exhausted/);
  await assert.rejects(readTextStream(streamOf('event: error\n' + sse({ error: { message: 'upstream 529' } }))), /upstream 529/);
});

// A stream that hands out one chunk per read, so a cancelled read is visible as "not pulled".
function pullStreamOf(...chunks) {
  const encoder = new TextEncoder();
  const state = { pulled: 0, cancelled: false };
  const body = new ReadableStream({
    pull(controller) {
      if (state.pulled < chunks.length) controller.enqueue(encoder.encode(chunks[state.pulled++]));
      else controller.close();
    },
    cancel() {
      state.cancelled = true;
    },
  });
  return { body, state };
}

const numberList = (from, to) => Array.from({ length: to - from }, (_, index) => from + index).join(', ');

test('stops reading and cancels the stream when shouldStop fires', async () => {
  const { body, state } = pullStreamOf(
    sse({ type: 'text-delta', id: 't', delta: numberList(1, 101) }),
    sse({ type: 'text-delta', id: 't', delta: ', ' + numberList(101, 201) }),
    // The trailing block is complete JSON but lacks its blank line, so it is still buffered when the
    // stream stops; it must not be flushed into the answer.
    sse({ type: 'text-delta', id: 't', delta: ', ' + numberList(201, 301) }) +
      `data: ${JSON.stringify({ type: 'text-delta', id: 't', delta: ', 9999' })}\n`,
    sse({ type: 'text-delta', id: 't', delta: ', ' + numberList(301, 401) }),
  );
  let seen = 0;
  const answer = await readTextStream(
    body,
    (text) => {
      seen = text.split(',').length;
    },
    () => seen >= 250,
  );
  assert.equal(answer.finished, false);
  assert.equal(answer.text.split(',').length, 300, 'all complete blocks, no more');
  assert.ok(answer.text.endsWith('300'), 'the partial trailing block is dropped');
  assert.ok(state.cancelled, 'the stream is cancelled');
  assert.ok(state.pulled <= 3, `only ${state.pulled} chunks were read`);
});

test('does not ask shouldStop once the finish event arrived', async () => {
  const calls = [];
  const answer = await readTextStream(
    streamOf(
      sse({ type: 'text-delta', id: 't', delta: '1, 2, 3' }),
      sse({ type: 'finish' }) + sse({ type: 'text-delta', id: 't', delta: ', 4' }),
    ),
    undefined,
    () => {
      calls.push(true);
      return false;
    },
  );
  assert.deepEqual(answer, { text: '1, 2, 3, 4', finished: true });
  assert.equal(calls.length, 1, 'asked only before the finish event');
});

function fakeFetch(response) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init });
    return response;
  };
  return { fetchImpl, calls };
}

test('posts only the prompt and model to the playground endpoint, pinned to the channel', async () => {
  const { fetchImpl, calls } = fakeFetch(new Response(streamOf(sse({ type: 'text-delta', id: 't', delta: '1, 2' }), sse({ type: 'finish' })), { status: 200 }));
  const { text } = await sendPlaygroundProbe({
    prompt: 'write numbers',
    target: { projectId: 'project-1', channelId: 'channel-9', model: 'claude-opus-4-7' },
    getToken: async () => 'token-abc',
    fetchImpl,
  });
  assert.equal(text, '1, 2');
  assert.equal(calls[0].url, PLAYGROUND_CHAT_API);
  assert.equal(PLAYGROUND_CHAT_API, '/admin/playground/chat');
  const headers = calls[0].init.headers;
  assert.equal(headers.Authorization, 'Bearer token-abc');
  assert.equal(headers['X-Project-ID'], 'project-1');
  assert.equal(headers['X-Channel-ID'], 'channel-9');
  const body = JSON.parse(calls[0].init.body);
  assert.equal(body.model, 'claude-opus-4-7');
  assert.deepEqual(body.messages[0].parts, [{ type: 'text', text: 'write numbers' }]);
  assert.equal(body.messages.length, 1);
  for (const key of ['system', 'temperature', 'max_tokens']) assert.ok(!(key in body), `${key} must not be sent`);
});

test('model-name routing sends no channel header', async () => {
  const { fetchImpl, calls } = fakeFetch(new Response(streamOf(sse({ type: 'finish' })), { status: 200 }));
  await sendPlaygroundProbe({ prompt: 'p', target: { projectId: 'project-1', model: 'gpt-5.5' }, getToken: async () => null, fetchImpl });
  assert.ok(!('X-Channel-ID' in calls[0].init.headers));
  assert.ok(!('Authorization' in calls[0].init.headers));
});

test('HTTP errors surface the upstream message', async () => {
  const { fetchImpl } = fakeFetch(new Response(JSON.stringify({ error: { code: 429, message: 'rate limited' } }), { status: 429, statusText: 'Too Many Requests' }));
  await assert.rejects(
    sendPlaygroundProbe({ prompt: 'p', target: { model: 'm' }, getToken: async () => 't', fetchImpl }),
    (error) => error.status === 429 && /429 rate limited/.test(error.message)
  );
});
