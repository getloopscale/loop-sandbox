import { afterEach, describe, expect, it, vi } from 'vitest';
import { AuthenticationError, CommandExitError, FileNotFoundError, InvalidArgumentError, SandboxError } from './errors.js';
import type { FetchLike } from './http.js';
import { Sandbox } from './sandbox.js';

const API_KEY = 'theo_live_secret_do_not_leak';
const BASE = 'https://app.test';

interface Call {
  url: string;
  init: RequestInit;
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

const wire = {
  id: 'sbx_1',
  tenant_id: 'tenant-a',
  image: 'python:3.12-slim',
  state: 'running',
  created_at: '2026-11-28T10:00:00Z',
  expires_at: '2026-11-28T10:05:00Z',
  metadata: { app: 'demo' },
};

function scripted(responses: (Response | (() => Response))[]): { fetch: FetchLike; calls: Call[] } {
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

const conn = (fetch: FetchLike) => ({ apiKey: API_KEY, apiUrl: BASE, fetch, retryBaseMs: 0 });

function bodyOf(call: Call): Record<string, unknown> {
  return JSON.parse(String(call.init.body)) as Record<string, unknown>;
}

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('Sandbox.create', () => {
  it('reads the key and the domain from the environment', async () => {
    vi.stubEnv('LOOPSCALE_API_KEY', API_KEY);
    vi.stubEnv('LOOPSCALE_DOMAIN', 'app.staging.example');
    const { fetch, calls } = scripted([json(201, wire)]);

    const sandbox = await Sandbox.create({ fetch });

    expect(sandbox.sandboxId).toBe('sbx_1');
    expect(calls[0]!.url).toBe('https://app.staging.example/v1/sandboxes');
    expect((calls[0]!.init.headers as Record<string, string>).Authorization).toBe(`Bearer ${API_KEY}`);
  });

  it('refuses to start without a key and says where to get one', async () => {
    vi.stubEnv('LOOPSCALE_API_KEY', '');
    vi.stubEnv('LOOPSANDBOX_API_KEY', '');
    const { fetch, calls } = scripted([json(201, wire)]);

    await expect(Sandbox.create({ fetch })).rejects.toBeInstanceOf(AuthenticationError);
    await expect(Sandbox.create({ fetch })).rejects.toThrow(/LOOPSCALE_API_KEY/);
    expect(calls).toHaveLength(0);
  });

  it('sends timeoutMs as seconds, envs as env, and the outbound rules', async () => {
    const { fetch, calls } = scripted([json(201, wire)]);

    await Sandbox.create({
      ...conn(fetch),
      timeoutMs: 90_500,
      metadata: { app: 'demo' },
      envs: { A: '1' },
      network: { allowOut: ['pypi.org'], denyOut: ['0.0.0.0/0'] },
    });

    expect(bodyOf(calls[0]!)).toMatchObject({
      ttl_seconds: 91,
      metadata: { app: 'demo' },
      env: { A: '1' },
      network: { allow_out: ['pypi.org'], deny_out: ['0.0.0.0/0'] },
    });
  });

  it('takes the first argument as an image when it names one, and as a template otherwise', async () => {
    const { fetch, calls } = scripted([() => json(201, wire)]);

    await Sandbox.create('python:3.12-slim', conn(fetch));
    await Sandbox.create('data-science', conn(fetch));

    expect(bodyOf(calls[0]!)).toMatchObject({ image: 'python:3.12-slim' });
    expect(bodyOf(calls[0]!).template).toBeUndefined();
    expect(bodyOf(calls[1]!)).toMatchObject({ template: 'data-science' });
    expect(bodyOf(calls[1]!).image).toBeUndefined();
  });

  it('allowInternetAccess false denies every destination', async () => {
    const { fetch, calls } = scripted([json(201, wire)]);

    await Sandbox.create({ ...conn(fetch), allowInternetAccess: false });

    expect(bodyOf(calls[0]!)).toMatchObject({ network: { allow_out: [], deny_out: ['0.0.0.0/0'] } });
  });
});

describe('Sandbox lifecycle', () => {
  it('connect reads the sandbox and resumes it when it is paused', async () => {
    const { fetch, calls } = scripted([json(200, { ...wire, state: 'paused' }), json(200, wire)]);

    const sandbox = await Sandbox.connect('sbx_1', conn(fetch));

    expect(sandbox.sandboxId).toBe('sbx_1');
    expect(calls.map((c) => `${c.init.method} ${c.url.slice(BASE.length)}`)).toEqual([
      'GET /v1/sandboxes/sbx_1',
      'POST /v1/sandboxes/sbx_1/resume',
    ]);
  });

  it('kill answers true, and false when the sandbox is already gone', async () => {
    const gone = json(404, { error: { code: 'not_found', message: 'sandbox not found' } });
    const { fetch } = scripted([new Response(null, { status: 204 }), gone]);

    expect(await Sandbox.kill('sbx_1', conn(fetch))).toBe(true);
    expect(await Sandbox.kill('sbx_1', conn(fetch))).toBe(false);
  });

  it('setTimeout takes milliseconds', async () => {
    const { fetch, calls } = scripted([json(201, wire), json(200, wire)]);
    const sandbox = await Sandbox.create(conn(fetch));

    await sandbox.setTimeout(120_000);

    expect(calls[1]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/timeout`);
    expect(bodyOf(calls[1]!)).toEqual({ ttl_seconds: 120 });
  });

  it('getInfo answers with the field names of e2b', async () => {
    const { fetch } = scripted([json(200, { ...wire, network: { allow_out: ['pypi.org'], deny_out: [] } })]);

    const info = await Sandbox.getInfo('sbx_1', conn(fetch));

    expect(info).toMatchObject({
      sandboxId: 'sbx_1',
      templateId: 'python:3.12-slim',
      state: 'running',
      metadata: { app: 'demo' },
      network: { allowOut: ['pypi.org'], denyOut: [] },
    });
    expect(info.startedAt).toEqual(new Date('2026-11-28T10:00:00Z'));
    expect(info.endAt).toEqual(new Date('2026-11-28T10:05:00Z'));
  });

  it('isRunning is false for a paused sandbox and for one that no longer exists', async () => {
    const gone = json(404, { error: { code: 'not_found', message: 'sandbox not found' } });
    const { fetch } = scripted([json(201, wire), json(200, wire), json(200, { ...wire, state: 'paused' }), gone]);
    const sandbox = await Sandbox.create(conn(fetch));

    expect(await sandbox.isRunning()).toBe(true);
    expect(await sandbox.isRunning()).toBe(false);
    expect(await sandbox.isRunning()).toBe(false);
  });

  it('getMetrics answers one sample with memory in bytes', async () => {
    const sample = { cpu_usage_ns: 5, memory_used_bytes: 100, memory_limit_bytes: 400, at: '2026-11-28T10:01:00Z' };
    const { fetch } = scripted([json(200, sample)]);

    const metrics = await Sandbox.getMetrics('sbx_1', conn(fetch));

    expect(metrics).toEqual([
      { timestamp: new Date('2026-11-28T10:01:00Z'), memUsed: 100, memTotal: 400, cpuUsageNs: 5 },
    ]);
  });

  it('getMetrics carries the use of the workspace when the API reports it', async () => {
    const sample = {
      cpu_usage_ns: 5,
      memory_used_bytes: 100,
      memory_limit_bytes: 400,
      disk_used_bytes: 4096,
      disk_limit_bytes: 65536,
      at: '2026-11-28T10:01:00Z',
    };
    const { fetch } = scripted([json(200, sample)]);

    const [metrics] = await Sandbox.getMetrics('sbx_1', conn(fetch));

    expect(metrics).toMatchObject({ diskUsed: 4096, diskTotal: 65536 });
  });

  it('list pages through the sandboxes', async () => {
    const { fetch, calls } = scripted([
      json(200, { sandboxes: [wire], next_cursor: 'c1' }),
      json(200, { sandboxes: [{ ...wire, id: 'sbx_2' }] }),
    ]);

    const paginator = Sandbox.list({ ...conn(fetch), limit: 1, query: { state: ['running'], metadata: { app: 'demo' } } });
    const seen: string[] = [];
    while (paginator.hasNext) {
      for (const info of await paginator.nextItems()) seen.push(info.sandboxId);
    }

    expect(seen).toEqual(['sbx_1', 'sbx_2']);
    expect(calls[0]!.url).toContain('state=running');
    expect(calls[0]!.url).toContain('limit=1');
    expect(calls[1]!.url).toContain('cursor=c1');
  });
});

describe('sandbox.commands', () => {
  async function created(responses: Response[]): Promise<{ sandbox: Sandbox; calls: Call[] }> {
    const { fetch, calls } = scripted([json(201, wire), ...responses]);
    return { sandbox: await Sandbox.create(conn(fetch)), calls };
  }

  it('runs a command line through the shell', async () => {
    const { sandbox, calls } = await created([json(200, { exit_code: 0, stdout: 'hi\n', stderr: '' })]);

    const result = await sandbox.commands.run('echo hi', { cwd: '/workspace', envs: { A: '1' }, timeoutMs: 5000 });

    expect(result).toEqual({ exitCode: 0, stdout: 'hi\n', stderr: '' });
    expect(bodyOf(calls[1]!)).toEqual({
      cmd: ['/bin/sh', '-c', 'echo hi'],
      env: { A: '1' },
      workdir: '/workspace',
      timeout_ms: 5000,
    });
  });

  it('throws CommandExitError with the output when the command fails', async () => {
    const { sandbox } = await created([json(200, { exit_code: 3, stdout: 'out', stderr: 'boom' })]);

    const err = await sandbox.commands.run('exit 3').catch((e: unknown) => e);

    expect(err).toBeInstanceOf(CommandExitError);
    expect(err).toBeInstanceOf(SandboxError);
    expect(err).toMatchObject({ exitCode: 3, stdout: 'out', stderr: 'boom' });
  });

  it('delivers output to onStdout and onStderr as it arrives', async () => {
    const stream =
      'event: stdout\ndata: {"data":"a"}\n\n' +
      'event: stderr\ndata: {"data":"b"}\n\n' +
      'event: stdout\ndata: {"data":"c"}\n\n' +
      'event: done\ndata: {"exit_code":0}\n\n';
    const { sandbox, calls } = await created([new Response(stream, { status: 200 })]);
    const out: string[] = [];
    const err: string[] = [];

    const result = await sandbox.commands.run('make', {
      onStdout: (d) => void out.push(d),
      onStderr: (d) => void err.push(d),
    });

    expect(calls[1]!.url).toContain('/executions?stream=1');
    expect(out).toEqual(['a', 'c']);
    expect(err).toEqual(['b']);
    expect(result).toEqual({ exitCode: 0, stdout: 'ac', stderr: 'b' });
  });

  it('starts a background command and waits for it through its handle', async () => {
    const proc = { pid: 7, cmd: ['/bin/sh', '-c', 'sleep 1'], started_at: '2026-11-28T10:00:01Z', running: true, exit_code: null };
    const { sandbox, calls } = await created([
      json(201, proc),
      json(200, { stdout: 'do', stderr: '', next: 2, truncated: false, process: proc }),
      json(200, { stdout: 'ne', stderr: '', next: 4, truncated: false, process: { ...proc, running: false, exit_code: 0 } }),
    ]);

    const handle = await sandbox.commands.run('sleep 1', { background: true });
    const result = await handle.wait();

    expect(handle.pid).toBe(7);
    expect(calls[1]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/processes`);
    expect(result).toEqual({ exitCode: 0, stdout: 'done', stderr: '' });
  });
});

describe('sandbox.files', () => {
  async function created(responses: (Response | (() => Response))[]): Promise<{ sandbox: Sandbox; calls: Call[] }> {
    const { fetch, calls } = scripted([json(201, wire), ...responses]);
    return { sandbox: await Sandbox.create(conn(fetch)), calls };
  }
  const missing = (): Response => json(404, { error: { code: 'not_found', message: 'no such file' } });

  it('writes a file and answers where it went', async () => {
    const { sandbox, calls } = await created([new Response(null, { status: 204 })]);

    const written = await sandbox.files.write('/workspace/a.txt', 'hello');

    expect(calls[1]!.init.method).toBe('PUT');
    expect(written).toEqual({ name: 'a.txt', path: '/workspace/a.txt', type: 'file' });
  });

  it('resolves a relative path against the workspace', async () => {
    const { sandbox, calls } = await created([new Response(null, { status: 204 })]);

    const written = await sandbox.files.write('notes/a.txt', 'hello');

    expect(calls[1]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/files/workspace/notes/a.txt`);
    expect(written.path).toBe('/workspace/notes/a.txt');
  });

  it('refuses a path outside the workspace before calling the API, and says where files live', async () => {
    const { sandbox, calls } = await created([new Response(null, { status: 204 })]);

    const err = await sandbox.files.write('/etc/passwd', 'hello').catch((e: unknown) => e);

    expect(err).toBeInstanceOf(InvalidArgumentError);
    expect((err as Error).message).toMatch(/under \/workspace.*\/etc\/passwd/);
    expect(calls).toHaveLength(1);
  });

  it('takes /home/user, the home of an e2b sandbox, as the workspace', async () => {
    const { sandbox, calls } = await created([new Response(null, { status: 204 }), new Response(null, { status: 204 })]);

    const written = await sandbox.files.write('/home/user/notes/a.txt', 'hello');
    await sandbox.files.makeDir('/home/user');

    expect(calls[1]!.url).toBe(`${BASE}/v1/sandboxes/sbx_1/files/workspace/notes/a.txt`);
    expect(written.path).toBe('/workspace/notes/a.txt');
    expect(calls[2]!.url).toContain('/workspace');
  });

  it.each(['/home/username/a.txt', '/home/user/../../etc/passwd', '/home/user/a/../../../etc/passwd'])(
    'refuses %s, which only looks like the home of an e2b sandbox',
    async (path) => {
      const { sandbox, calls } = await created([new Response(null, { status: 204 })]);

      const err = await sandbox.files.write(path, 'hello').catch((e: unknown) => e);

      expect(err).toBeInstanceOf(InvalidArgumentError);
      expect(calls).toHaveLength(1);
    },
  );

  it('reads text by default and bytes when asked', async () => {
    const { sandbox } = await created([() => new Response('hello', { status: 200 })]);

    expect(await sandbox.files.read('/workspace/a.txt')).toBe('hello');
    expect(await sandbox.files.read('/workspace/a.txt', { format: 'bytes' })).toEqual(new TextEncoder().encode('hello'));
  });

  it('exists is false for a missing path instead of an error', async () => {
    const info = { name: 'a.txt', path: '/workspace/a.txt', size: 5, is_dir: false, mode: '0644', modified_at: '2026-11-28T10:00:00Z' };
    const { sandbox } = await created([json(200, info), missing()]);

    expect(await sandbox.files.exists('/workspace/a.txt')).toBe(true);
    expect(await sandbox.files.exists('/workspace/b.txt')).toBe(false);
  });

  it('lists a directory as entries with a type and a full path', async () => {
    const { sandbox } = await created([
      json(200, { files: [{ name: 'a.txt', size: 5, is_dir: false }, { name: 'src', size: 0, is_dir: true }] }),
    ]);

    expect(await sandbox.files.list('/workspace')).toEqual([
      { name: 'a.txt', path: '/workspace/a.txt', type: 'file', size: 5 },
      { name: 'src', path: '/workspace/src', type: 'dir', size: 0 },
    ]);
  });

  it('remove of a missing file raises FileNotFoundError', async () => {
    const { sandbox } = await created([missing()]);

    await expect(sandbox.files.remove('/workspace/b.txt')).rejects.toBeInstanceOf(FileNotFoundError);
  });

  it('rename moves the file and answers the new entry', async () => {
    const info = { name: 'b.txt', path: '/workspace/b.txt', size: 5, is_dir: false, mode: '0644', modified_at: '2026-11-28T10:00:00Z' };
    const { sandbox, calls } = await created([new Response(null, { status: 204 }), json(200, info)]);

    const entry = await sandbox.files.rename('/workspace/a.txt', '/workspace/b.txt');

    expect(bodyOf(calls[1]!)).toMatchObject({ from: '/workspace/a.txt', to: '/workspace/b.txt' });
    expect(entry).toMatchObject({ name: 'b.txt', path: '/workspace/b.txt', type: 'file', size: 5 });
  });
});

describe('where the API key is sent', () => {
  it('refuses an address without TLS before sending the key', async () => {
    const { fetch, calls } = scripted([json(201, wire)]);

    const err = await Sandbox.create({ apiKey: API_KEY, apiUrl: 'http://app.example.com', fetch }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(InvalidArgumentError);
    expect((err as Error).message).toMatch(/https/);
    expect(calls).toHaveLength(0);
  });

  it.each(['http://localhost:8080', 'http://127.0.0.1:8080', 'http://[::1]:8080'])('accepts %s, an address of this machine', async (apiUrl) => {
    const { fetch, calls } = scripted([json(201, wire)]);

    await Sandbox.create({ apiKey: API_KEY, apiUrl, fetch });

    expect(calls).toHaveLength(1);
  });

  it('refuses a domain that carries a scheme or a path', async () => {
    const { fetch, calls } = scripted([json(201, wire)]);

    const err = await Sandbox.create({ apiKey: API_KEY, domain: 'http://app.example.com/x', fetch }).catch((e: unknown) => e);

    expect(err).toBeInstanceOf(InvalidArgumentError);
    expect(calls).toHaveLength(0);
  });
});
