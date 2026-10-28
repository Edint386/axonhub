// Sends one ModelTrace challenge through the playground chat endpoint, the same route, auth headers
// and channel pinning the playground chat uses, and returns the answer text.

export const PLAYGROUND_CHAT_API = '/admin/playground/chat';

export interface ProbeTarget {
  projectId?: string | null;
  // Set to pin the request to one channel (the playground's "channel" model source).
  channelId?: string;
  model: string;
}

export interface SendProbeOptions {
  prompt: string;
  target: ProbeTarget;
  getToken: () => Promise<string | null | undefined>;
  signal?: AbortSignal;
  onText?: (text: string) => void;
  fetchImpl?: typeof fetch;
}

// A request the backend rejected; `status` is its HTTP status.
export class ProbeRequestError extends Error {
  status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'ProbeRequestError';
    this.status = status;
  }
}

function randomId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(8));
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
}

// ModelTrace's API tester sends only the user prompt: no system prompt, temperature or max tokens.
// Anything else shifts the number distribution away from what the bank was built from.
export function buildProbeRequestBody(prompt: string, model: string) {
  return {
    id: `model-trace-${randomId()}`,
    messages: [{ id: randomId(), role: 'user', parts: [{ type: 'text', text: prompt }] }],
    trigger: 'submit-message',
    model,
  };
}

export async function readErrorMessage(response: Response): Promise<string> {
  let message = response.statusText || 'Request failed';
  try {
    const body = await response.json();
    const upstream = body?.error?.message || body?.message;
    if (upstream) message = upstream;
  } catch {
    // keep the status text
  }
  return `${response.status} ${message}`;
}

// Handles one SSE data payload: the text delta to append and whether it is the finish event.
// Throws for an error event.
export function handleStreamPayload(data: string): { delta: string; finished: boolean } {
  const none = { delta: '', finished: false };
  if (!data || data === '[DONE]') return none;
  let event: any;
  try {
    event = JSON.parse(data);
  } catch {
    return none;
  }
  if (event?.type === 'text-delta' && typeof event.delta === 'string') return { delta: event.delta, finished: false };
  if (event?.type === 'finish') return { delta: '', finished: true };
  if (event?.type === 'error') throw new Error(event.errorText || 'Stream error');
  if (!event?.type && event?.error) {
    const error = event.error;
    throw new Error(typeof error === 'string' ? error : error.message || JSON.stringify(error));
  }
  return none;
}

export interface ProbeAnswer {
  text: string;
  // Whether the AI SDK "finish" event arrived. The backend emits it only when the upstream reported a
  // finish reason, so a stream that just stops (dropped upstream, relay writing [DONE] after a
  // failure, or a relay that never sends finish_reason) may hold a cut-off answer.
  finished: boolean;
}

export async function readTextStream(body: ReadableStream<Uint8Array>, onText?: (text: string) => void): Promise<ProbeAnswer> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  let text = '';
  let finished = false;

  const consume = (block: string) => {
    const data = block
      .split(/\r?\n/)
      .filter((line) => line.startsWith('data:'))
      .map((line) => line.slice(5).replace(/^ /, ''))
      .join('\n');
    const event = handleStreamPayload(data);
    finished ||= event.finished;
    if (event.delta) {
      text += event.delta;
      onText?.(text);
    }
  };

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const blocks = buffer.split(/\r?\n\r?\n/);
      buffer = blocks.pop() ?? '';
      blocks.forEach(consume);
    }
    buffer += decoder.decode();
    if (buffer.trim()) consume(buffer);
  } finally {
    reader.releaseLock();
  }
  return { text, finished };
}

export async function sendPlaygroundProbe({ prompt, target, getToken, signal, onText, fetchImpl = fetch }: SendProbeOptions): Promise<ProbeAnswer> {
  const token = await getToken();
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'X-Project-ID': target.projectId || '',
  };
  if (token) headers.Authorization = `Bearer ${token}`;
  if (target.channelId) headers['X-Channel-ID'] = target.channelId;

  const response = await fetchImpl(PLAYGROUND_CHAT_API, {
    method: 'POST',
    credentials: 'include',
    headers,
    body: JSON.stringify(buildProbeRequestBody(prompt, target.model)),
    signal,
  });
  if (!response.ok) throw new ProbeRequestError(await readErrorMessage(response), response.status);
  if (!response.body) throw new Error('Empty response');
  return readTextStream(response.body, onText);
}
