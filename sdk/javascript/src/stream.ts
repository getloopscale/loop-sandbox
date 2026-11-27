import { LoopSandboxError } from './errors.js';

/** One event of a streamed command or code cell. Output arrives as "stdout"
 *  and "stderr" chunks; "done" is the last event and carries the exit code. */
export type OutputStreamEvent =
  | { type: 'stdout' | 'stderr'; data: string }
  | {
      type: 'done';
      exitCode: number;
      /** Sent by a streamed code cell: the traceback of a cell that raised. */
      stderr?: string;
    };

interface WireStreamData {
  data?: string;
  exit_code?: number;
  stderr?: string;
  error?: string;
  code?: string;
}

/** readOutputStream decodes the Server-Sent Events of a streamed execution.
 *  It throws when the server reports an "error" event, and when the stream ends
 *  before "done", so a cut connection is never mistaken for a finished command. */
export async function* readOutputStream(res: Response): AsyncGenerator<OutputStreamEvent, void, void> {
  if (!res.body) throw new LoopSandboxError('the stream response has no body', 0, 'stream_incomplete');
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  try {
    for (;;) {
      const { done, value } = await reader.read();
      buffer += done ? decoder.decode() : decoder.decode(value, { stream: true });
      let end = buffer.indexOf('\n\n');
      while (end !== -1) {
        const event = parseEvent(buffer.slice(0, end));
        buffer = buffer.slice(end + 2);
        if (event) {
          yield event;
          if (event.type === 'done') return;
        }
        end = buffer.indexOf('\n\n');
      }
      if (done) break;
    }
  } finally {
    await reader.cancel().catch(() => undefined); // the consumer may stop early
  }
  throw new LoopSandboxError('the stream ended before the done event', 0, 'stream_incomplete');
}

function parseEvent(block: string): OutputStreamEvent | undefined {
  let name = '';
  const dataLines: string[] = [];
  for (const line of block.split('\n')) {
    if (line.startsWith('event:')) name = line.slice('event:'.length).trim();
    else if (line.startsWith('data:')) dataLines.push(line.slice('data:'.length).trim());
  }
  if (name === '') return undefined; // a comment or keep-alive block
  let payload: WireStreamData;
  try {
    payload = JSON.parse(dataLines.join('\n')) as WireStreamData;
  } catch {
    throw new LoopSandboxError(`malformed data in stream event "${name}"`, 0, 'stream_malformed');
  }
  switch (name) {
    case 'stdout':
    case 'stderr':
      return { type: name, data: payload.data ?? '' };
    case 'done':
      if (typeof payload.exit_code !== 'number') {
        throw new LoopSandboxError('the done event of the stream has no exit code', 0, 'stream_malformed');
      }
      return payload.stderr === undefined
        ? { type: 'done', exitCode: payload.exit_code }
        : { type: 'done', exitCode: payload.exit_code, stderr: payload.stderr };
    case 'error':
      throw new LoopSandboxError(payload.error ?? 'the stream failed', 0, payload.code ?? 'stream_error');
    default:
      return undefined; // an event type this SDK version does not know
  }
}
