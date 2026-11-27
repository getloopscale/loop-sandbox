import type { HttpClient } from './http.js';
import { readOutputStream, type OutputStreamEvent } from './stream.js';

/** CellResult is the outcome of one code cell. A non-zero exitCode (the cell raised) is a
 *  normal result, NOT an error (US-02) — the traceback is in stderr. */
export interface CellResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

interface WireCell {
  exit_code: number;
  stdout: string;
  stderr: string;
}

/** Session is a stateful code-interpreter session: state (variables, imports, dataframes)
 *  is SHARED across execute() calls — the OpenAI Code-Interpreter / E2B runCode pattern. */
export class Session {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
    readonly id: string,
  ) {}

  /** execute runs one code cell; prior cells' state is visible. */
  async execute(code: string, opts: { signal?: AbortSignal } = {}): Promise<CellResult> {
    const res = await this.http.sendJSON<WireCell>({
      method: 'POST',
      path: `/v1/sandboxes/${encodeURIComponent(this.sandboxId)}/sessions/${encodeURIComponent(this.id)}/execute`,
      body: JSON.stringify({ code }),
      contentType: 'application/json',
      retriable: false, // running a cell mutates session state — not idempotent
      signal: opts.signal,
    });
    return { exitCode: res.exit_code, stdout: res.stdout, stderr: res.stderr };
  }

  /** executeStream runs one code cell and yields its output while it runs:
   *  "stdout" and "stderr" events, then one "done" event with the exit code and
   *  the traceback of a cell that raised. A failure is thrown from the
   *  iteration. Needs the capability "session_stream". Never retried. */
  async *executeStream(
    code: string,
    opts: { signal?: AbortSignal } = {},
  ): AsyncGenerator<OutputStreamEvent, void, void> {
    const res = await this.http.send({
      method: 'POST',
      path: `/v1/sandboxes/${encodeURIComponent(this.sandboxId)}/sessions/${encodeURIComponent(this.id)}/execute?stream=1`,
      body: JSON.stringify({ code }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    yield* readOutputStream(res);
  }

  /** close terminates the session's interpreter (idempotent). */
  async close(opts: { signal?: AbortSignal } = {}): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/sandboxes/${encodeURIComponent(this.sandboxId)}/sessions/${encodeURIComponent(this.id)}`,
      retriable: true, // close is idempotent
      signal: opts.signal,
    });
  }
}

/** Sessions opens stateful code-interpreter sessions in a sandbox. */
export class Sessions {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  /** start opens a new session with an empty interpreter namespace. */
  async start(opts: { signal?: AbortSignal } = {}): Promise<Session> {
    const res = await this.http.sendJSON<{ session_id: string }>({
      method: 'POST',
      path: `/v1/sandboxes/${encodeURIComponent(this.sandboxId)}/sessions`,
      body: '{}',
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return new Session(this.http, this.sandboxId, res.session_id);
  }

  /** attach returns a handle to a session that is already open, by its id,
   *  without a request. */
  attach(sessionId: string): Session {
    return new Session(this.http, this.sandboxId, sessionId);
  }
}
