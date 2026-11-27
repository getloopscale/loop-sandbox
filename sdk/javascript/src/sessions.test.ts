import { describe, expect, it, vi } from 'vitest';
import { LoopSandbox } from './client.js';
import type { FetchLike } from './http.js';

const BASE = 'https://sandbox.test';

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const sandboxBody = {
  id: 'sbx_1',
  tenant_id: 'tenant-a',
  image: 'busybox',
  state: 'running',
  created_at: '2026-07-17T00:00:00Z',
  expires_at: '2026-07-17T00:05:00Z',
};

describe('code-interpreter sessions', () => {
  it('runCode reuses one default session (state shared) across calls', async () => {
    const calls: string[] = [];
    const fetchMock = vi.fn<FetchLike>(async (url, init) => {
      const u = String(url);
      calls.push(`${init?.method} ${u.replace(BASE, '')}`);
      if (u.endsWith('/v1/sandboxes')) return jsonResponse(201, sandboxBody);
      if (u.endsWith('/sessions')) return jsonResponse(201, { session_id: 'ses_1' });
      if (u.endsWith('/execute')) {
        const body = JSON.parse(String(init?.body));
        // echo back deterministic output based on the code (mock kernel)
        return jsonResponse(200, { exit_code: 0, stdout: body.code.includes('print') ? '42\n' : '', stderr: '' });
      }
      throw new Error(`unexpected ${u}`);
    });
    const client = new LoopSandbox({ apiKey: 'lsb_k', baseUrl: BASE, fetch: fetchMock });
    const sbx = await client.sandboxes.create();

    const r1 = await sbx.runCode('x = 41');
    const r2 = await sbx.runCode('print(x + 1)');

    expect(r1.exitCode).toBe(0);
    expect(r2.stdout.trim()).toBe('42');
    // Exactly ONE session was opened (default session reused), then two executes.
    const starts = calls.filter((c) => c === 'POST /v1/sandboxes/sbx_1/sessions');
    const execs = calls.filter((c) => c === 'POST /v1/sandboxes/sbx_1/sessions/ses_1/execute');
    expect(starts).toHaveLength(1);
    expect(execs).toHaveLength(2);
  });

  it('a cell that raises is a normal result (exitCode 1), not a thrown error', async () => {
    const fetchMock = vi.fn<FetchLike>(async (url) => {
      const u = String(url);
      if (u.endsWith('/v1/sandboxes')) return jsonResponse(201, sandboxBody);
      if (u.endsWith('/sessions')) return jsonResponse(201, { session_id: 'ses_1' });
      if (u.endsWith('/execute')) return jsonResponse(200, { exit_code: 1, stdout: '', stderr: 'ValueError: boom' });
      throw new Error(`unexpected ${u}`);
    });
    const client = new LoopSandbox({ apiKey: 'lsb_k', baseUrl: BASE, fetch: fetchMock });
    const sbx = await client.sandboxes.create();
    const r = await sbx.runCode("raise ValueError('boom')");
    expect(r.exitCode).toBe(1);
    expect(r.stderr).toContain('ValueError: boom');
  });
});
