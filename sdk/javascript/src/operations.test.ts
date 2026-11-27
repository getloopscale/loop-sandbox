import { describe, expect, it, vi } from 'vitest';
import { LoopSandbox } from './client.js';
import {
  ConflictError,
  ForbiddenError,
  LoopSandboxError,
  NotImplementedError,
  QuotaExceededError,
  RateLimitError,
  ServerError,
} from './errors.js';
import type { FetchLike, WebSocketLike } from './http.js';
import type { OutputStreamEvent } from './stream.js';
import type { Sandbox } from './sandboxes.js';

const API_KEY = 'lsb_secret_key_do_not_leak';
const BASE = 'https://sandbox.test';

interface Call {
  url: string;
  init: RequestInit;
}

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  });
}

function noContent(): Response {
  return new Response(null, { status: 204 });
}

function errorResponse(status: number, code: string, headers: Record<string, string> = {}): Response {
  return jsonResponse(status, { error: { code, message: `${code} message` } }, headers);
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

// withSandbox builds a client and a Sandbox handle, then drops the create call
// from the record, so calls[0] is the request under test.
async function withSandbox(
  responses: (Response | (() => Response))[],
  extra: Record<string, unknown> = {},
): Promise<{ sandbox: Sandbox; calls: Call[] }> {
  const { fetch, calls } = scriptedFetch([jsonResponse(201, sandboxBody), ...responses]);
  const sandbox = await newClient(fetch, extra).sandboxes.create();
  calls.length = 0;
  return { sandbox, calls };
}

function sentJSON(call: Call): unknown {
  return JSON.parse(call.init.body as string);
}

function sentHeaders(call: Call): Record<string, string> {
  return call.init.headers as Record<string, string>;
}

describe('sandbox listing', () => {
  it('returns handles and the cursor of the next page', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(200, { sandboxes: [{ ...sandboxBody, metadata: { team: 'search' } }], next_cursor: 'cur_2' }),
    ]);

    const page = await newClient(fetch).sandboxes.list();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes`);
    expect(page.sandboxes.map((s) => s.id)).toEqual(['sbx_1']);
    expect(page.sandboxes[0]!.info.metadata).toEqual({ team: 'search' });
    expect(page.nextCursor).toBe('cur_2');
  });

  it('sends the state, metadata, limit and cursor filters as query parameters', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, { sandboxes: [] })]);

    const page = await newClient(fetch).sandboxes.list({
      state: ['running', 'paused'],
      metadata: { team: 'search', env: 'ci' },
      limit: 10,
      cursor: 'cur_1',
    });

    const query = new URL(calls[0]!.url).searchParams;
    expect(query.get('state')).toBe('running,paused');
    expect(query.getAll('metadata')).toEqual(['team=search', 'env=ci']);
    expect(query.get('limit')).toBe('10');
    expect(query.get('cursor')).toBe('cur_1');
    expect(page.nextCursor).toBeUndefined();
  });
});

describe('sandbox creation options', () => {
  it('sends template, metadata and env in the request body', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, { ...sandboxBody, env: { A: '1' } })]);

    const sandbox = await newClient(fetch).sandboxes.create({
      template: 'tpl_1',
      metadata: { team: 'search' },
      env: { A: '1' },
    });

    expect(sentJSON(calls[0]!)).toEqual({ template: 'tpl_1', metadata: { team: 'search' }, env: { A: '1' } });
    expect(sandbox.info.env).toEqual({ A: '1' });
  });
});

describe('sandbox network', () => {
  it('create sends the rules as allow_out and deny_out and maps them back', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(201, { ...sandboxBody, network: { allow_out: ['pypi.org'], deny_out: ['0.0.0.0/0'] } }),
    ]);

    const sandbox = await newClient(fetch).sandboxes.create({
      network: { allowOut: ['pypi.org'], denyOut: ['0.0.0.0/0'] },
    });

    expect((sentJSON(calls[0]!) as { network: unknown }).network).toEqual({
      allow_out: ['pypi.org'],
      deny_out: ['0.0.0.0/0'],
    });
    expect(sandbox.info.network).toEqual({ allowOut: ['pypi.org'], denyOut: ['0.0.0.0/0'] });
  });

  it('create without rules sends no network field', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, sandboxBody)]);

    const sandbox = await newClient(fetch).sandboxes.create({ image: 'python' });

    expect(sentJSON(calls[0]!)).not.toHaveProperty('network');
    expect(sandbox.info.network).toBeUndefined();
  });

  it('updateNetwork puts both lists and keeps the handle in step', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, { allow_out: ['*.example.org'], deny_out: ['0.0.0.0/0'] }),
      jsonResponse(200, { allow_out: [], deny_out: [] }),
    ]);

    const applied = await sandbox.updateNetwork({ allowOut: ['*.example.org'], denyOut: ['0.0.0.0/0'] });

    expect(calls[0]!.init.method).toBe('PUT');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/network`);
    expect(sentJSON(calls[0]!)).toEqual({ allow_out: ['*.example.org'], deny_out: ['0.0.0.0/0'] });
    expect(applied).toEqual({ allowOut: ['*.example.org'], denyOut: ['0.0.0.0/0'] });
    expect(sandbox.info.network).toEqual(applied);

    await sandbox.updateNetwork({});

    expect(sentJSON(calls[1]!)).toEqual({ allow_out: [], deny_out: [] });
    expect(sandbox.info.network).toBeUndefined();
  });

  it('networkDenials reads the refused connections with an optional limit', async () => {
    const denial = { time: '2026-11-27T10:00:00Z', destination: '93.184.216.34:443', host: 'example.com', reason: 'denied by deny_out' };
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, { denials: [denial, { time: '2026-11-27T10:00:01Z', destination: '10.0.0.1:80', reason: 'private address' }] }),
      jsonResponse(200, { denials: [] }),
    ]);

    const denials = await sandbox.networkDenials();

    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/network/denials`);
    expect(denials).toEqual([denial, { time: '2026-11-27T10:00:01Z', destination: '10.0.0.1:80', reason: 'private address' }]);

    await sandbox.networkDenials(5);

    expect(calls[1]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/network/denials?limit=5`);
  });
});

describe('sandbox lifecycle', () => {
  it('setTimeout posts ttl_seconds and returns the sandbox with its new expiry', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, { ...sandboxBody, expires_at: '2026-07-16T00:10:00Z' }),
    ]);

    const updated = await sandbox.setTimeout(600);

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/timeout`);
    expect(sentJSON(calls[0]!)).toEqual({ ttl_seconds: 600 });
    expect(updated.info.expiresAt).toBe('2026-07-16T00:10:00Z');
  });

  it('fork posts without a body and returns the forked sandbox', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(201, { ...sandboxBody, id: 'sbx_fork' })]);

    const fork = await sandbox.fork();

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/fork`);
    expect(calls[0]!.init.body).toBeUndefined();
    expect(fork.id).toBe('sbx_fork');
  });

  it('forkMany posts the count and returns one handle per fork', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(201, {
        sandboxes: [
          { ...sandboxBody, id: 'sbx_a' },
          { ...sandboxBody, id: 'sbx_b' },
        ],
      }),
    ]);

    const forks = await sandbox.forkMany(2);

    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/fork`);
    expect(sentJSON(calls[0]!)).toEqual({ count: 2 });
    expect(forks.map((f) => f.id)).toEqual(['sbx_a', 'sbx_b']);
  });

  it('fork by id on the collection reaches the same route', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, { ...sandboxBody, id: 'sbx_fork' })]);

    const fork = await newClient(fetch).sandboxes.fork('sbx_9');

    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_9/fork`);
    expect(fork.id).toBe('sbx_fork');
  });

  it('does not retry a failed fork', async () => {
    const { sandbox, calls } = await withSandbox([() => errorResponse(503, 'internal_error')]);

    await expect(sandbox.fork()).rejects.toBeInstanceOf(ServerError);
    expect(calls).toHaveLength(1);
  });

  it('pause posts to the pause route and accepts an empty 200 body', async () => {
    const { sandbox, calls } = await withSandbox([new Response(null, { status: 200 })]);

    await sandbox.pause();

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/pause`);
  });

  it('resume posts to the resume route and returns the running sandbox', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(200, sandboxBody)]);

    const resumed = await sandbox.resume();

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/resume`);
    expect(resumed.info.state).toBe('running');
  });

  it('pausing a sandbox in the wrong state raises ConflictError', async () => {
    const { sandbox } = await withSandbox([errorResponse(409, 'conflict')]);

    await expect(sandbox.pause()).rejects.toBeInstanceOf(ConflictError);
  });
});

describe('sandbox metrics and logs', () => {
  it('metrics maps the resource counters to camelCase', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, {
        cpu_usage_ns: 1500,
        memory_used_bytes: 2048,
        memory_limit_bytes: 4096,
        at: '2026-07-16T00:00:01Z',
      }),
    ]);

    const metrics = await sandbox.metrics();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/metrics`);
    expect(metrics).toEqual({
      cpuUsageNs: 1500,
      memoryUsedBytes: 2048,
      memoryLimitBytes: 4096,
      at: '2026-07-16T00:00:01Z',
    });
  });

  it('logs sends the cursor and wait as query parameters and returns the entries', async () => {
    const entry = { pid: 7, stream: 'stdout', data: 'ready\n', at: '2026-07-16T00:00:01Z' };
    const { sandbox, calls } = await withSandbox([jsonResponse(200, { entries: [entry], next: 6, truncated: false })]);

    const logs = await sandbox.logs({ after: 0, waitMs: 500 });

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/logs?after=0&wait_ms=500`);
    expect(logs).toEqual({ entries: [entry], next: 6, truncated: false });
  });
});

describe('file operations', () => {
  it('info reads the file-info route and maps the fields to camelCase', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, {
        name: 'report.txt',
        path: '/workspace/final/report.txt',
        size: 12,
        is_dir: false,
        mode: '0644',
        modified_at: '2026-07-16T00:00:01Z',
      }),
    ]);

    const info = await sandbox.files.info('/workspace/final/report.txt');

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/file-info/workspace/final/report.txt`);
    expect(info).toEqual({
      name: 'report.txt',
      path: '/workspace/final/report.txt',
      size: 12,
      isDir: false,
      mode: '0644',
      modifiedAt: '2026-07-16T00:00:01Z',
    });
  });

  it('makeDir puts to the directories route', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.files.makeDir('/workspace/out/logs');

    expect(calls[0]!.init.method).toBe('PUT');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/directories/workspace/out/logs`);
  });

  it('move posts from, to and overwrite', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.files.move('/workspace/draft.txt', '/workspace/final/report.txt', { overwrite: true });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/file-moves`);
    expect(sentJSON(calls[0]!)).toEqual({
      from: '/workspace/draft.txt',
      to: '/workspace/final/report.txt',
      overwrite: true,
    });
  });

  it('move onto an existing destination raises ConflictError with the code already_exists', async () => {
    const { sandbox } = await withSandbox([errorResponse(409, 'already_exists')]);

    const err = await sandbox.files.move('/workspace/a', '/workspace/b').catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ConflictError);
    expect((err as ConflictError).code).toBe('already_exists');
  });

  it('artifactUrl reads the artifact-url route and maps expires_in_seconds', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, { url: 'https://storage.test/report.pdf?sig=1', expires_in_seconds: 600 }),
    ]);

    const artifact = await sandbox.files.artifactUrl('/workspace/report.pdf');

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/artifact-url/workspace/report.pdf`);
    expect(artifact).toEqual({ url: 'https://storage.test/report.pdf?sig=1', expiresInSeconds: 600 });
  });

  it('percent-encodes the characters of a file path that a URL would misread', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.files.write('/workspace/my notes/100%#draft?.txt', 'x');

    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/files/workspace/my%20notes/100%25%23draft%3F.txt`);
  });
});

describe('signed file URLs', () => {
  it('createUrl posts path, operation and expiry and maps the answer', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(201, {
        url: 'https://files.test/v1/signed-files/tok_1',
        method: 'PUT',
        expires_at: '2026-07-16T00:02:00Z',
      }),
    ]);

    const signed = await sandbox.files.createUrl('/workspace/in.csv', 'upload', { expiresInSeconds: 120 });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/file-urls`);
    expect(sentJSON(calls[0]!)).toEqual({ path: '/workspace/in.csv', operation: 'upload', expires_in_seconds: 120 });
    expect(signed).toEqual({
      url: 'https://files.test/v1/signed-files/tok_1',
      method: 'PUT',
      expiresAt: '2026-07-16T00:02:00Z',
    });
  });

  it('download fetches a full signed URL as bytes', async () => {
    const { fetch, calls } = scriptedFetch([new Response(new Uint8Array([1, 2, 255]), { status: 200 })]);

    const bytes = await newClient(fetch).signedFiles.download('https://files.test/v1/signed-files/tok_1');

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe('https://files.test/v1/signed-files/tok_1');
    expect(Array.from(bytes)).toEqual([1, 2, 255]);
  });

  it('download never sends the API key to the signed URL', async () => {
    const { fetch, calls } = scriptedFetch([new Response('x', { status: 200 })]);

    await newClient(fetch).signedFiles.download('https://files.test/v1/signed-files/tok_1');

    expect(sentHeaders(calls[0]!)['Authorization']).toBeUndefined();
    expect(JSON.stringify(calls[0]!.init.headers)).not.toContain(API_KEY);
  });

  it('upload puts the content to the route of a bare token, without the API key', async () => {
    const { fetch, calls } = scriptedFetch([noContent()]);

    await newClient(fetch).signedFiles.upload('tok_2', 'a,b\n');

    expect(calls[0]!.init.method).toBe('PUT');
    expect(calls[0]!.url).toBe(`${BASE}/v1/signed-files/tok_2`);
    expect(calls[0]!.init.body).toBe('a,b\n');
    expect(sentHeaders(calls[0]!)['Authorization']).toBeUndefined();
  });

  it('an expired signed URL raises ForbiddenError', async () => {
    const { fetch } = scriptedFetch([errorResponse(403, 'forbidden')]);

    await expect(newClient(fetch).signedFiles.download('tok_old')).rejects.toBeInstanceOf(ForbiddenError);
  });
});

describe('directory watches', () => {
  it('start posts the path and the recursive flag and returns the watch', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(201, { watch_id: 'w_1', path: '/workspace/out', recursive: true }),
    ]);

    const watch = await sandbox.watches.start('/workspace/out', { recursive: true });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/watches`);
    expect(sentJSON(calls[0]!)).toEqual({ path: '/workspace/out', recursive: true });
    expect(watch).toEqual({ watchId: 'w_1', path: '/workspace/out', recursive: true });
  });

  it('events sends the cursor and maps is_dir, leaving the path out of an overflow', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, {
        events: [
          { type: 'create', path: '/workspace/out/a.txt', is_dir: false },
          { type: 'overflow', is_dir: false },
        ],
        next: 9,
        truncated: true,
        closed: false,
      }),
    ]);

    const page = await sandbox.watches.events('w_1', { after: 7, waitMs: 1000 });

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/watches/w_1/events?after=7&wait_ms=1000`);
    expect(page).toEqual({
      events: [
        { type: 'create', path: '/workspace/out/a.txt', isDir: false },
        { type: 'overflow', isDir: false },
      ],
      next: 9,
      truncated: true,
      closed: false,
    });
  });

  it('stop deletes the watch', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.watches.stop('w_1');

    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/watches/w_1`);
  });

  it('a sandbox at its limit of watches raises ConflictError with the code too_many_watches', async () => {
    const { sandbox } = await withSandbox([errorResponse(409, 'too_many_watches')]);

    const err = await sandbox.watches.start('/workspace').catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ConflictError);
    expect((err as ConflictError).code).toBe('too_many_watches');
  });
});

describe('background processes', () => {
  const running = { pid: 7, cmd: ['sleep', '60'], started_at: '2026-07-16T00:00:01Z', running: true, exit_code: null };
  const ended = { ...running, running: false, exit_code: 143 };

  it('start posts cmd, env and workdir and returns the running process', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(201, running)]);

    const proc = await sandbox.processes.start(['sleep', '60'], { env: { A: '1' }, workdir: '/workspace' });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes`);
    expect(sentJSON(calls[0]!)).toEqual({ cmd: ['sleep', '60'], env: { A: '1' }, workdir: '/workspace' });
    expect(proc).toEqual({
      pid: 7,
      cmd: ['sleep', '60'],
      startedAt: '2026-07-16T00:00:01Z',
      running: true,
      exitCode: null,
    });
  });

  it('start refuses an empty command before any request', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(201, running)]);

    await expect(sandbox.processes.start([])).rejects.toThrow(/cmd/);
    expect(calls).toHaveLength(0);
  });

  it('list returns every process with its exit code', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(200, { processes: [ended, running] })]);

    const list = await sandbox.processes.list();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes`);
    expect(list.map((p) => p.exitCode)).toEqual([143, null]);
  });

  it('get reads one process by pid', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(200, ended)]);

    const proc = await sandbox.processes.get(7);

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes/7`);
    expect(proc.running).toBe(false);
    expect(proc.exitCode).toBe(143);
  });

  it('output sends the cursor and returns the text, the next cursor and the process', async () => {
    const { sandbox, calls } = await withSandbox([
      jsonResponse(200, { stdout: 'out', stderr: 'err', next: 6, truncated: false, process: ended }),
    ]);

    const output = await sandbox.processes.output(7, { after: 0, waitMs: 250 });

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes/7/output?after=0&wait_ms=250`);
    expect(output.stdout).toBe('out');
    expect(output.stderr).toBe('err');
    expect(output.next).toBe(6);
    expect(output.truncated).toBe(false);
    expect(output.process.exitCode).toBe(143);
  });

  it('writeStdin posts the raw bytes', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.processes.writeStdin(7, 'hello\n');

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes/7/stdin`);
    expect(calls[0]!.init.body).toBe('hello\n');
    expect(sentHeaders(calls[0]!)['Content-Type']).toBe('application/octet-stream');
  });

  it('writeStdin to a process that ended raises ConflictError with the code process_exited', async () => {
    const { sandbox } = await withSandbox([errorResponse(409, 'process_exited')]);

    const err = await sandbox.processes.writeStdin(7, 'x').catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ConflictError);
    expect((err as ConflictError).code).toBe('process_exited');
  });

  it('closeStdin deletes the stdin of the process', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.processes.closeStdin(7);

    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes/7/stdin`);
  });

  it('signal posts the signal name', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.processes.signal(7, 'TERM');

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes/7/signal`);
    expect(sentJSON(calls[0]!)).toEqual({ signal: 'TERM' });
  });
});

function sseResponse(text: string): Response {
  return new Response(text, { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
}

async function collect(stream: AsyncIterable<OutputStreamEvent>): Promise<OutputStreamEvent[]> {
  const events: OutputStreamEvent[] = [];
  for await (const event of stream) events.push(event);
  return events;
}

describe('streamed output', () => {
  it('commands.stream requests stream=1 and yields the output and the exit code', async () => {
    const { sandbox, calls } = await withSandbox([
      sseResponse(
        'event: stdout\ndata: {"data":"building\\n"}\n\n' +
          'event: stderr\ndata: {"data":"warn\\n"}\n\n' +
          'event: done\ndata: {"exit_code":2}\n\n',
      ),
    ]);

    const events = await collect(sandbox.commands.stream(['make'], { timeoutMs: 1000 }));

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/executions?stream=1`);
    expect(sentJSON(calls[0]!)).toEqual({ cmd: ['make'], timeout_ms: 1000 });
    expect(events).toEqual([
      { type: 'stdout', data: 'building\n' },
      { type: 'stderr', data: 'warn\n' },
      { type: 'done', exitCode: 2 },
    ]);
  });

  it('reassembles an event that arrives split across network chunks', async () => {
    const encoder = new TextEncoder();
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode('event: stdout\ndata: {"da'));
        controller.enqueue(encoder.encode('ta":"hi"}\n\nevent: done\ndata: {"exit_code":0}\n\n'));
        controller.close();
      },
    });
    const { sandbox } = await withSandbox([new Response(body, { status: 200 })]);

    const events = await collect(sandbox.commands.stream(['echo', 'hi']));

    expect(events).toEqual([
      { type: 'stdout', data: 'hi' },
      { type: 'done', exitCode: 0 },
    ]);
  });

  it('throws the server error event instead of ending the stream quietly', async () => {
    const { sandbox } = await withSandbox([
      sseResponse('event: stdout\ndata: {"data":"a"}\n\nevent: error\ndata: {"error":"runtime died","code":"error"}\n\n'),
    ]);

    const err = await collect(sandbox.commands.stream(['x'])).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(LoopSandboxError);
    expect((err as LoopSandboxError).message).toBe('runtime died');
    expect((err as LoopSandboxError).code).toBe('error');
  });

  it('throws when the stream ends before the done event', async () => {
    const { sandbox } = await withSandbox([sseResponse('event: stdout\ndata: {"data":"a"}\n\n')]);

    const err = await collect(sandbox.commands.stream(['x'])).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(LoopSandboxError);
    expect((err as LoopSandboxError).code).toBe('stream_incomplete');
  });

  it('session.executeStream requests stream=1 and keeps the stderr of the done event', async () => {
    const { sandbox, calls } = await withSandbox([
      sseResponse('event: stdout\ndata: {"data":"42\\n"}\n\nevent: done\ndata: {"exit_code":1,"stderr":"ValueError"}\n\n'),
    ]);

    const events = await collect(sandbox.sessions.attach('ses_1').executeStream('print(42)'));

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/sessions/ses_1/execute?stream=1`);
    expect(sentJSON(calls[0]!)).toEqual({ code: 'print(42)' });
    expect(events).toEqual([
      { type: 'stdout', data: '42\n' },
      { type: 'done', exitCode: 1, stderr: 'ValueError' },
    ]);
  });
});

describe('sessions by id', () => {
  it('attach builds a handle without a request and close deletes the session', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.sessions.attach('ses_9').close();

    expect(calls).toHaveLength(1);
    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/sessions/ses_9`);
  });
});

describe('browser', () => {
  it('navigate posts the URL', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.browser.navigate('https://example.com');

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/browser/navigate`);
    expect(sentJSON(calls[0]!)).toEqual({ url: 'https://example.com' });
  });

  it('screenshot returns the PNG bytes', async () => {
    const png = new Uint8Array([0x89, 0x50, 0x4e, 0x47]);
    const { sandbox, calls } = await withSandbox([
      new Response(png, { status: 200, headers: { 'Content-Type': 'image/png' } }),
    ]);

    const bytes = await sandbox.browser.screenshot();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/browser/screenshot`);
    expect(Array.from(bytes)).toEqual([0x89, 0x50, 0x4e, 0x47]);
  });

  it('dom returns the HTML of the page', async () => {
    const { sandbox, calls } = await withSandbox([jsonResponse(200, { dom: '<html></html>' })]);

    const html = await sandbox.browser.dom();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/browser/dom`);
    expect(html).toBe('<html></html>');
  });

  it('input posts the action as given', async () => {
    const { sandbox, calls } = await withSandbox([noContent()]);

    await sandbox.browser.input({ action: 'click', x: 10, y: 20 });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/browser/input`);
    expect(sentJSON(calls[0]!)).toEqual({ action: 'click', x: 10, y: 20 });
  });
});

// FakeSocket stands in for a WebSocket: it records what the SDK sends and lets
// a test fire the events a server would cause.
class FakeSocket implements WebSocketLike {
  static last: FakeSocket | undefined;
  binaryType = 'blob';
  readonly sent: (string | Uint8Array)[] = [];
  closeCode: number | undefined;
  private readonly listeners = new Map<string, ((event: { data?: unknown }) => void)[]>();

  constructor(
    readonly url: string,
    readonly options: { headers: Record<string, string> },
  ) {
    FakeSocket.last = this;
  }

  send(data: string | Uint8Array): void {
    this.sent.push(data);
  }

  close(code?: number): void {
    this.closeCode = code;
    this.emit('close');
  }

  addEventListener(type: string, listener: (event: { data?: unknown }) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }

  emit(type: string, event: { data?: unknown } = {}): void {
    for (const listener of this.listeners.get(type) ?? []) listener(event);
  }
}

async function openTerminal(opts: { rows?: number; cols?: number } = {}) {
  const { sandbox } = await withSandbox([], { webSocket: FakeSocket });
  const opening = sandbox.pty.open(opts);
  const socket = FakeSocket.last!;
  socket.emit('open');
  return { terminal: await opening, socket };
}

describe('terminal', () => {
  it('opens a WebSocket to the pty route with the API key in the Authorization header', async () => {
    const { socket } = await openTerminal();

    expect(socket.url).toBe('wss://sandbox.test/v1/sandboxes/sbx_1/pty');
    expect(socket.options.headers).toEqual({ Authorization: `Bearer ${API_KEY}` });
    expect(socket.binaryType).toBe('arraybuffer');
  });

  it('sends the initial window size as a resize control frame', async () => {
    const { socket } = await openTerminal({ rows: 24, cols: 80 });

    expect(socket.sent).toEqual(['{"type":"resize","rows":24,"cols":80}']);
  });

  it('write sends typed text as a binary frame', async () => {
    const { terminal, socket } = await openTerminal();

    terminal.write('ls\n');

    expect(socket.sent).toHaveLength(1);
    expect(socket.sent[0]).toBeInstanceOf(Uint8Array);
    expect(new TextDecoder().decode(socket.sent[0] as Uint8Array)).toBe('ls\n');
  });

  it('delivers the bytes of the terminal to a data listener', async () => {
    const { terminal, socket } = await openTerminal();
    const chunks: string[] = [];
    terminal.onData((chunk) => chunks.push(new TextDecoder().decode(chunk)));

    socket.emit('message', { data: new TextEncoder().encode('$ ').buffer });

    expect(chunks).toEqual(['$ ']);
  });

  it('keeps output that arrives before the first listener is registered', async () => {
    const { terminal, socket } = await openTerminal();
    socket.emit('message', { data: new TextEncoder().encode('early').buffer });
    const chunks: string[] = [];

    terminal.onData((chunk) => chunks.push(new TextDecoder().decode(chunk)));

    expect(chunks).toEqual(['early']);
  });

  it('close closes the socket and settles the closed promise', async () => {
    const { terminal, socket } = await openTerminal();

    terminal.close();

    await expect(terminal.closed).resolves.toBeUndefined();
    expect(socket.closeCode).toBe(1000);
  });

  it('rejects when the connection is refused before it opens', async () => {
    const { sandbox } = await withSandbox([], { webSocket: FakeSocket });
    const opening = sandbox.pty.open();

    FakeSocket.last!.emit('error');

    const err = await opening.catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LoopSandboxError);
    expect((err as LoopSandboxError).code).toBe('pty_connect_failed');
  });
});

describe('secrets', () => {
  const secretBody = { name: 'API_TOKEN', created_at: '2026-11-20T10:00:00Z', updated_at: '2026-11-20T11:00:00Z' };

  it('put sends the value and returns the secret without it', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, secretBody)]);

    const secret = await newClient(fetch).secrets.put('API_TOKEN', 'v');

    expect(calls[0]!.init.method).toBe('PUT');
    expect(calls[0]!.url).toBe(`${BASE}/v1/secrets/API_TOKEN`);
    expect(sentJSON(calls[0]!)).toEqual({ value: 'v' });
    expect(secret).toEqual({ name: 'API_TOKEN', createdAt: '2026-11-20T10:00:00Z', updatedAt: '2026-11-20T11:00:00Z' });
  });

  it('list returns the secrets of the tenant', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, { secrets: [secretBody] })]);

    const secrets = await newClient(fetch).secrets.list();

    expect(calls[0]!.url).toBe(`${BASE}/v1/secrets`);
    expect(secrets.map((s) => s.name)).toEqual(['API_TOKEN']);
  });

  it('delete removes the secret by name', async () => {
    const { fetch, calls } = scriptedFetch([noContent()]);

    await newClient(fetch).secrets.delete('API_TOKEN');

    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/secrets/API_TOKEN`);
  });

  it('create sends the names of the secrets a sandbox receives', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(201, { id: 'sbx_1', tenant_id: 't', state: 'running', created_at: 'x', secrets: ['API_TOKEN'] }),
    ]);

    const sandbox = await newClient(fetch).sandboxes.create({ secrets: ['API_TOKEN'] });

    expect(sentJSON(calls[0]!)).toEqual({ secrets: ['API_TOKEN'] });
    expect(sandbox.info.secrets).toEqual(['API_TOKEN']);
  });
});

describe('automatic pause', () => {
  it('create sends auto_pause_seconds and reads it back', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(201, { id: 'sbx_1', tenant_id: 't', state: 'running', created_at: 'x', auto_pause_seconds: 60 }),
    ]);

    const sandbox = await newClient(fetch).sandboxes.create({ autoPauseSeconds: 60 });

    expect(sentJSON(calls[0]!)).toEqual({ auto_pause_seconds: 60 });
    expect(sandbox.info.autoPauseSeconds).toBe(60);
  });
});

describe('snapshots', () => {
  const snapshotBody = {
    id: 'snap_1',
    name: 'after-setup',
    sandbox_id: 'sbx_1',
    image: 'busybox:latest',
    size_bytes: 1024,
    created_at: '2026-11-20T10:00:00Z',
  };

  it('create posts the name under the sandbox and returns the snapshot', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, snapshotBody)]);

    const snapshot = await newClient(fetch).snapshots.create({ sandboxId: 'sbx_1', name: 'after-setup' });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/snapshots`);
    expect(sentJSON(calls[0]!)).toEqual({ name: 'after-setup' });
    expect(snapshot).toEqual({
      id: 'snap_1',
      name: 'after-setup',
      sandboxId: 'sbx_1',
      image: 'busybox:latest',
      sizeBytes: 1024,
      createdAt: '2026-11-20T10:00:00Z',
    });
  });

  it('list returns the snapshots of the tenant', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, { snapshots: [snapshotBody] })]);

    const snapshots = await newClient(fetch).snapshots.list();

    expect(calls[0]!.url).toBe(`${BASE}/v1/snapshots`);
    expect(snapshots.map((s) => s.id)).toEqual(['snap_1']);
  });

  it('get fetches one snapshot by id', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, snapshotBody)]);

    const snapshot = await newClient(fetch).snapshots.get('snap_1');

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/snapshots/snap_1`);
    expect(snapshot.name).toBe('after-setup');
  });

  it('delete removes the snapshot by id', async () => {
    const { fetch, calls } = scriptedFetch([noContent()]);

    await newClient(fetch).snapshots.delete('snap_1');

    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/snapshots/snap_1`);
  });

  it('a name in use raises ConflictError', async () => {
    const { fetch } = scriptedFetch([errorResponse(409, 'already_exists')]);

    const err = await newClient(fetch)
      .snapshots.create({ sandboxId: 'sbx_1', name: 'after-setup' })
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ConflictError);
  });
});

describe('volumes', () => {
  const volumeBody = { id: 'vol_1', name: 'data', size_gb: 5, sandbox_id: 'sbx_1', created_at: '2026-07-16T00:00:00Z' };

  it('create posts sandbox_id, name and size_gb and returns the volume', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, volumeBody)]);

    const volume = await newClient(fetch).volumes.create({ sandboxId: 'sbx_1', name: 'data', sizeGb: 5 });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/volumes`);
    expect(sentJSON(calls[0]!)).toEqual({ sandbox_id: 'sbx_1', name: 'data', size_gb: 5 });
    expect(volume).toEqual({
      id: 'vol_1',
      name: 'data',
      sizeGb: 5,
      sandboxId: 'sbx_1',
      createdAt: '2026-07-16T00:00:00Z',
    });
  });

  it('list sends the sandbox id as a query parameter', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, { volumes: [volumeBody] })]);

    const volumes = await newClient(fetch).volumes.list('sbx_1');

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/volumes?sandbox_id=sbx_1`);
    expect(volumes.map((v) => v.id)).toEqual(['vol_1']);
  });

  it('delete removes the volume by id', async () => {
    const { fetch, calls } = scriptedFetch([noContent()]);

    await newClient(fetch).volumes.delete('vol_1');

    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/volumes/vol_1`);
  });

  it('a reached plan quota raises QuotaExceededError', async () => {
    const { fetch } = scriptedFetch([errorResponse(402, 'quota_exceeded')]);

    const err = await newClient(fetch)
      .volumes.create({ sandboxId: 'sbx_1', name: 'data', sizeGb: 5 })
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(QuotaExceededError);
    expect((err as QuotaExceededError).code).toBe('quota_exceeded');
  });
});

describe('API keys', () => {
  const keyBody = {
    id: 'key_1',
    name: 'ci',
    prefix: 'lsb_ab12',
    scopes: ['sandbox:execute'],
    created_at: '2026-07-16T00:00:00Z',
  };

  it('list returns the keys with revoked_at mapped to revokedAt', async () => {
    const { fetch, calls } = scriptedFetch([
      jsonResponse(200, { api_keys: [keyBody, { ...keyBody, id: 'key_2', revoked_at: '2026-07-17T00:00:00Z' }] }),
    ]);

    const keys = await newClient(fetch).apiKeys.list();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/api-keys`);
    expect(keys[0]).toEqual({
      id: 'key_1',
      name: 'ci',
      prefix: 'lsb_ab12',
      scopes: ['sandbox:execute'],
      createdAt: '2026-07-16T00:00:00Z',
    });
    expect(keys[1]!.revokedAt).toBe('2026-07-17T00:00:00Z');
  });

  it('create posts name and scopes and returns the secret', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, { ...keyBody, key: 'lsb_new_secret' })]);

    const created = await newClient(fetch).apiKeys.create({ name: 'ci', scopes: ['sandbox:execute'] });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/api-keys`);
    expect(sentJSON(calls[0]!)).toEqual({ name: 'ci', scopes: ['sandbox:execute'] });
    expect(created.key).toBe('lsb_new_secret');
    expect(created.id).toBe('key_1');
  });

  it('revoke deletes the key by id', async () => {
    const { fetch, calls } = scriptedFetch([noContent()]);

    await newClient(fetch).apiKeys.revoke('key_1');

    expect(calls[0]!.init.method).toBe('DELETE');
    expect(calls[0]!.url).toBe(`${BASE}/v1/api-keys/key_1`);
  });

  it('a key without the manage scope raises ForbiddenError', async () => {
    const { fetch } = scriptedFetch([errorResponse(403, 'forbidden')]);

    await expect(newClient(fetch).apiKeys.list()).rejects.toBeInstanceOf(ForbiddenError);
  });
});

describe('capabilities', () => {
  it('get returns the driver and its capabilities', async () => {
    const report = { driver: 'container', capabilities: [{ name: 'fork', supported: false }] };
    const { fetch, calls } = scriptedFetch([jsonResponse(200, report)]);

    const got = await newClient(fetch).capabilities.get();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/capabilities`);
    expect(got).toEqual(report);
  });

  it('an unsupported capability raises NotImplementedError without a retry', async () => {
    const { sandbox, calls } = await withSandbox([() => errorResponse(501, 'not_implemented')]);

    const err = await sandbox.metrics().catch((e: unknown) => e);

    expect(err).toBeInstanceOf(NotImplementedError);
    expect(err).toBeInstanceOf(ServerError);
    expect(calls).toHaveLength(1);
  });
});

describe('templates registry', () => {
  const registered = {
    id: 'tpl_1',
    name: 'py',
    image: 'python@sha256:abc',
    version: '1',
    digest: 'sha256:abc',
    public: false,
    scan_status: 'passed',
  };

  it('list maps a registered entry to camelCase and keeps a built-in one', async () => {
    const { fetch } = scriptedFetch([
      jsonResponse(200, {
        templates: [{ name: 'busybox', image: 'busybox:latest', description: 'x', builtin: true }, registered],
      }),
    ]);

    const list = await newClient(fetch).templates.list();

    expect(list[0]).toEqual({ name: 'busybox', image: 'busybox:latest', description: 'x', builtin: true });
    expect(list[1]!.id).toBe('tpl_1');
    expect(list[1]!.scanStatus).toBe('passed');
  });

  it('register posts the template and returns it with its scan status', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(201, registered)]);

    const template = await newClient(fetch).templates.register({
      name: 'py',
      image: 'python@sha256:abc',
      version: '1',
      public: false,
    });

    expect(calls[0]!.init.method).toBe('POST');
    expect(calls[0]!.url).toBe(`${BASE}/v1/templates`);
    expect(sentJSON(calls[0]!)).toEqual({ name: 'py', image: 'python@sha256:abc', version: '1', public: false });
    expect(template).toEqual({
      id: 'tpl_1',
      name: 'py',
      image: 'python@sha256:abc',
      version: '1',
      digest: 'sha256:abc',
      public: false,
      scanStatus: 'passed',
    });
  });

  it('get reads a registered template by id', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, registered)]);

    const template = await newClient(fetch).templates.get('tpl_1');

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/templates/tpl_1`);
    expect(template.scanStatus).toBe('passed');
  });

  it('a failed vulnerability scan surfaces status 422 and the code scan_failed', async () => {
    const { fetch } = scriptedFetch([errorResponse(422, 'scan_failed')]);

    const err = await newClient(fetch)
      .templates.register({ name: 'py', image: 'python@sha256:abc', public: true })
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(LoopSandboxError);
    expect((err as LoopSandboxError).status).toBe(422);
    expect((err as LoopSandboxError).code).toBe('scan_failed');
  });
});

describe('platform probes', () => {
  it('health reads the liveness route without the API key', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, { status: 'ok' })]);

    const status = await newClient(fetch).platform.health();

    expect(calls[0]!.init.method).toBe('GET');
    expect(calls[0]!.url).toBe(`${BASE}/v1/healthz`);
    expect(sentHeaders(calls[0]!)['Authorization']).toBeUndefined();
    expect(status).toEqual({ status: 'ok' });
  });

  it('ready reads the readiness route', async () => {
    const { fetch, calls } = scriptedFetch([jsonResponse(200, { status: 'ready' })]);

    const status = await newClient(fetch).platform.ready();

    expect(calls[0]!.url).toBe(`${BASE}/v1/readyz`);
    expect(status).toEqual({ status: 'ready' });
  });

  it('ready raises ServerError with the code not_ready when a dependency is down', async () => {
    const { fetch } = scriptedFetch([() => errorResponse(503, 'not_ready')]);

    const err = await newClient(fetch, { maxRetries: 0 })
      .platform.ready()
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ServerError);
    expect((err as ServerError).code).toBe('not_ready');
  });

  it('metrics returns the Prometheus text', async () => {
    const { fetch, calls } = scriptedFetch([new Response('# HELP up\nup 1\n', { status: 200 })]);

    const text = await newClient(fetch).platform.metrics();

    expect(calls[0]!.url).toBe(`${BASE}/metrics`);
    expect(text).toBe('# HELP up\nup 1\n');
  });
});

describe('rate limiting', () => {
  it('raises RateLimitError with the seconds of the Retry-After header', async () => {
    const { fetch } = scriptedFetch([() => errorResponse(429, 'rate_limited', { 'Retry-After': '7' })]);

    const err = await newClient(fetch, { maxRetries: 0 })
      .sandboxes.get('sbx_1')
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(RateLimitError);
    expect((err as RateLimitError).retryAfterSeconds).toBe(7);
    expect((err as RateLimitError).code).toBe('rate_limited');
    expect((err as RateLimitError).status).toBe(429);
  });

  it('leaves retryAfterSeconds undefined when the header is missing', async () => {
    const { fetch } = scriptedFetch([() => errorResponse(429, 'rate_limited')]);

    const err = await newClient(fetch, { maxRetries: 0 })
      .sandboxes.get('sbx_1')
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(RateLimitError);
    expect((err as RateLimitError).retryAfterSeconds).toBeUndefined();
  });

  it('raises RateLimitError at once on a request that is never retried', async () => {
    const { sandbox, calls } = await withSandbox([() => errorResponse(429, 'rate_limited', { 'Retry-After': '3' })]);

    const err = await sandbox.commands.run(['true']).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(RateLimitError);
    expect((err as RateLimitError).retryAfterSeconds).toBe(3);
    expect(calls).toHaveLength(1);
  });

  it('waits the Retry-After time before retrying an idempotent request', async () => {
    vi.useFakeTimers();
    try {
      const { fetch, calls } = scriptedFetch([
        errorResponse(429, 'rate_limited', { 'Retry-After': '2' }),
        jsonResponse(200, sandboxBody),
      ]);
      const pending = newClient(fetch).sandboxes.get('sbx_1');

      await vi.advanceTimersByTimeAsync(1999);
      const callsBeforeTheWaitEnds = calls.length;
      await vi.advanceTimersByTimeAsync(1);
      const sandbox = await pending;

      expect(callsBeforeTheWaitEnds).toBe(1);
      expect(calls).toHaveLength(2);
      expect(sandbox.id).toBe('sbx_1');
    } finally {
      vi.useRealTimers();
    }
  });
});
