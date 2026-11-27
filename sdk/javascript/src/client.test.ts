import { describe, expect, it, vi } from 'vitest';
import { LoopSandbox } from './client.js';
import {
  ConflictError,
  ForbiddenError,
  LoopSandboxError,
  NotFoundError,
  PayloadTooLargeError,
  RateLimitError,
  ServerError,
  TimeoutError,
  UnauthorizedError,
  ValidationError,
} from './errors.js';
import type { FetchLike } from './http.js';

const API_KEY = 'lsb_secret_key_do_not_leak';
const BASE = 'https://sandbox.test';

interface Call {
  url: string;
  init: RequestInit;
}

// jsonResponse builds a fetch Response with a JSON body.
function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

const sandboxBody = {
  id: 'sbx_1',
  tenant_id: 'tenant-a',
  image: 'busybox:latest',
  state: 'running',
  created_at: '2026-07-16T00:00:00Z',
  expires_at: '2026-07-16T00:05:00Z',
};

// scriptedFetch returns the next queued Response per call and records the calls.
function scriptedFetch(responses: (Response | (() => Response))[]): { fetch: FetchLike; calls: Call[] } {
  const calls: Call[] = [];
  let i = 0;
  const fetch: FetchLike = async (url, init = {}) => {
    calls.push({ url, init });
    const r = responses[Math.min(i, responses.length - 1)]!;
    i++;
    return typeof r === 'function' ? r() : r;
  };
  return { fetch, calls };
}

function newClient(fetch: FetchLike, extra: Record<string, unknown> = {}): LoopSandbox {
  return new LoopSandbox({ baseUrl: BASE, apiKey: API_KEY, fetch, retryBaseMs: 0, ...extra });
}

describe('LoopSandbox client', () => {
  it('requires baseUrl and apiKey', () => {
    expect(() => new LoopSandbox({ baseUrl: '', apiKey: 'x' })).toThrow(/baseUrl/);
    expect(() => new LoopSandbox({ baseUrl: BASE, apiKey: '' })).toThrow(/apiKey/);
  });

  it('creates a sandbox with Bearer auth and maps snake_case → camelCase', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, sandboxBody)]);
    const client = newClient(fetch);

    const sb = await client.sandboxes.create({ image: 'busybox:latest', ttlSeconds: 300 });

    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes`);
    expect(calls[0]!.init.method).toBe('POST');
    const headers = calls[0]!.init.headers as Record<string, string>;
    expect(headers['Authorization']).toBe(`Bearer ${API_KEY}`);
    expect(JSON.parse(calls[0]!.init.body as string)).toEqual({ image: 'busybox:latest', ttl_seconds: 300 });
    expect(sb.id).toBe('sbx_1');
    expect(sb.info.tenantId).toBe('tenant-a');
    expect(sb.info.expiresAt).toBe('2026-07-16T00:05:00Z');
  });
});

describe('retry policy', () => {
  it('retries an idempotent create (Idempotency-Key) on 503 and sends the key', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(503, { error: { code: 'internal_error', message: 'boom' } }),
      jsonResponse(201, sandboxBody),
    ]);
    const client = newClient(fetch);

    const sb = await client.sandboxes.create({ idempotencyKey: 'idem-1' });

    expect(calls).toHaveLength(2); // one retry
    expect((calls[0]!.init.headers as Record<string, string>)['Idempotency-Key']).toBe('idem-1');
    expect(sb.id).toBe('sbx_1');
  });

  it('does NOT retry a bare create (no key) on 503', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(503, { error: { code: 'internal_error', message: 'boom' } }),
    ]);
    const client = newClient(fetch);

    await expect(client.sandboxes.create({})).rejects.toBeInstanceOf(ServerError);
    expect(calls).toHaveLength(1); // no retry
  });

  it('does NOT retry a command execution (non-idempotent) on 503', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(201, sandboxBody), // create
      jsonResponse(503, { error: { code: 'internal_error', message: 'boom' } }), // exec 503
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});
    await expect(sb.commands.run(['echo', 'hi'])).rejects.toBeInstanceOf(ServerError);
    expect(calls).toHaveLength(2); // create + one exec attempt, no retry on the exec
  });
});

describe('command execution', () => {
  it('returns a non-zero exit as a normal result (US-02), not an error', async () => {
    const { fetch } = scriptedFetch([
      jsonResponse(201, sandboxBody),
      jsonResponse(200, { exit_code: 3, stdout: '', stderr: 'nope' }),
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});
    const out = await sb.commands.run(['false']);
    expect(out.exitCode).toBe(3);
    expect(out.stderr).toBe('nope');
  });
});

describe('files API', () => {
  it('builds the file route without a double slash and lists with is_dir mapping', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(201, sandboxBody),
      new Response(null, { status: 204 }), // write
      new Response('hello', { status: 200 }), // read
      jsonResponse(200, { files: [{ name: 'hi.txt', size: 5, is_dir: false }] }), // list
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});

    await sb.files.write('/workspace/hi.txt', 'hello');
    expect(calls[1]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/files/workspace/hi.txt`);
    expect(calls[1]!.init.method).toBe('PUT');

    const content = await sb.files.read('/workspace/hi.txt');
    expect(content).toBe('hello');

    const entries = await sb.files.list('/workspace');
    expect(entries).toEqual([{ name: 'hi.txt', size: 5, isDir: false }]);
  });
});

describe('typed errors', () => {
  it('maps status codes to the right typed error', async () => {
    const cases: [number, string, new (...a: never[]) => LoopSandboxError][] = [
      [400, 'invalid_request', ValidationError],
      [401, 'unauthorized', UnauthorizedError],
      [403, 'forbidden', ForbiddenError],
      [404, 'not_found', NotFoundError],
      [409, 'conflict', ConflictError],
      [413, 'payload_too_large', PayloadTooLargeError],
      [429, 'rate_limited', RateLimitError],
      [504, 'timeout', TimeoutError],
      [500, 'internal_error', ServerError],
      [418, 'teapot', LoopSandboxError], // unmapped 4xx → base error
    ];
    for (const [status, code, Ctor] of cases) {
      // Factory form → a fresh Response per (possibly retried) attempt.
      const { fetch } = scriptedFetch([
        () => jsonResponse(status, { error: { code, message: 'x' } }, { 'X-Request-ID': 'req-9' }),
      ]);
      const client = newClient(fetch);
      const err = await client.sandboxes.get('sbx_x').catch((e: unknown) => e);
      expect(err, `status ${status}`).toBeInstanceOf(Ctor);
      expect((err as LoopSandboxError).status).toBe(status);
      expect((err as LoopSandboxError).requestId).toBe('req-9');
    }
  });

  it('NEVER leaks the API key in a thrown error', async () => {
    const { fetch } = scriptedFetch([
      jsonResponse(500, { error: { code: 'internal_error', message: 'server boom' } }),
    ]);
    const client = newClient(fetch);
    const err = (await client.sandboxes.get('sbx_x').catch((e: unknown) => e)) as Error;
    const serialized = `${err.message} ${JSON.stringify(err)} ${err.stack ?? ''}`;
    expect(serialized).not.toContain(API_KEY);
    expect(serialized).not.toContain('secret');
  });
});

describe('AbortSignal', () => {
  it('rejects with TimeoutError when the signal is already aborted', async () => {
    const fetch = vi.fn(async (_url: string, init?: RequestInit) => {
      if (init?.signal?.aborted) {
        const e = new Error('aborted');
        e.name = 'AbortError';
        throw e;
      }
      return jsonResponse(201, sandboxBody);
    }) as unknown as FetchLike;
    const client = newClient(fetch);
    const controller = new AbortController();
    controller.abort();
    await expect(client.sandboxes.create({ signal: controller.signal })).rejects.toBeInstanceOf(TimeoutError);
  });
});

describe('templates', () => {
  it('lists the server catalog', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(200, {
        templates: [
          { name: 'busybox', image: 'busybox:latest', description: 'x' },
          { name: 'python', image: 'python:3.12-slim', description: 'y' },
        ],
      }),
    ]);
    const client = newClient(fetch);
    const list = await client.templates.list();
    expect(calls[0]!.url).toBe(`${BASE}/v1/templates`);
    expect(list.map((t) => t.name)).toEqual(['busybox', 'python']);
  });
});

describe('retry exhaustion & network errors', () => {
  it('throws ServerError after exhausting retries on a persistent 503 (maxRetries+1 attempts)', async () => {
    const { fetch, calls } = scriptedFetch([
      () => jsonResponse(503, { error: { code: 'internal_error', message: 'down' } }),
    ]);
    const client = newClient(fetch, { maxRetries: 2 });
    await expect(client.sandboxes.get('sbx_x')).rejects.toBeInstanceOf(ServerError);
    expect(calls).toHaveLength(3); // 1 + 2 retries
  });

  it('surfaces a status-0 network error when fetch itself rejects (non-abort)', async () => {
    const fetch = (async () => {
      throw new Error('ECONNREFUSED');
    }) as unknown as FetchLike;
    const client = newClient(fetch, { maxRetries: 1 });
    const err = (await client.sandboxes.get('sbx_x').catch((e: unknown) => e)) as LoopSandboxError;
    expect(err).toBeInstanceOf(LoopSandboxError);
    expect(err.status).toBe(0);
    expect(err.code).toBe('network');
  });

  it('retries a keyed create on a 409 idempotency_in_progress (in-flight reservation)', async () => {
    const { fetch, calls } = scriptedFetch([
      () => jsonResponse(409, { error: { code: 'idempotency_in_progress', message: 'in progress' } }),
      () => jsonResponse(201, sandboxBody),
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({ idempotencyKey: 'k1' });
    expect(sb.id).toBe('sbx_1');
    expect(calls).toHaveLength(2); // retried through the in-progress conflict
  });

  it('does NOT retry a 409 payload conflict (distinct code)', async () => {
    const { fetch, calls } = scriptedFetch([
      () => jsonResponse(409, { error: { code: 'conflict', message: 'different payload' } }),
    ]);
    const client = newClient(fetch);
    await expect(client.sandboxes.create({ idempotencyKey: 'k1' })).rejects.toBeInstanceOf(ConflictError);
    expect(calls).toHaveLength(1);
  });
});

describe('files negative & edge cases', () => {
  it('read of a missing file raises NotFoundError', async () => {
    const { fetch } = scriptedFetch([
      jsonResponse(201, sandboxBody),
      () => jsonResponse(404, { error: { code: 'not_found', message: 'no such file' } }),
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});
    await expect(sb.files.read('/workspace/nope.txt')).rejects.toBeInstanceOf(NotFoundError);
  });

  it('list of an empty directory returns []', async () => {
    const { fetch } = scriptedFetch([
      jsonResponse(201, sandboxBody),
      jsonResponse(200, { files: [] }),
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});
    expect(await sb.files.list('/workspace')).toEqual([]);
  });

  it('readBytes returns raw bytes (binary-safe)', async () => {
    const bytes = new Uint8Array([0x00, 0xff, 0x10, 0x80]);
    const { fetch } = scriptedFetch([
      jsonResponse(201, sandboxBody),
      new Response(bytes, { status: 200 }),
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});
    const got = await sb.files.readBytes('/workspace/blob.bin');
    expect(Array.from(got)).toEqual([0x00, 0xff, 0x10, 0x80]);
  });
});

describe('exec option passthrough', () => {
  it('serializes timeoutMs → timeout_ms and env/workdir', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(201, sandboxBody),
      jsonResponse(200, { exit_code: 0, stdout: '', stderr: '' }),
    ]);
    const client = newClient(fetch);
    const sb = await client.sandboxes.create({});
    await sb.commands.run(['echo', 'hi'], { timeoutMs: 5000, env: { A: '1' }, workdir: '/workspace' });
    const body = JSON.parse(calls[1]!.init.body as string);
    expect(body).toEqual({ cmd: ['echo', 'hi'], env: { A: '1' }, workdir: '/workspace', timeout_ms: 5000 });
  });
});

describe('AbortSignal mid-flight', () => {
  it('aborts during backoff between retries → TimeoutError', async () => {
    const controller = new AbortController();
    let n = 0;
    const fetch = (async () => {
      n++;
      queueMicrotask(() => controller.abort());
      return jsonResponse(503, { error: { code: 'internal_error', message: 'x' } });
    }) as unknown as FetchLike;
    const client = newClient(fetch, { maxRetries: 3, retryBaseMs: 50 });
    await expect(client.sandboxes.get('sbx_x', controller.signal)).rejects.toBeInstanceOf(TimeoutError);
    expect(n).toBe(1); // aborted before the retry fired
  });
});
