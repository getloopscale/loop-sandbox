import { sandboxRoute, type HttpClient } from './http.js';
import { readOutputStream, type OutputStreamEvent } from './stream.js';
import type { ExecResult, RunOptions } from './types.js';

/** Commands runs shell-free command executions in a single sandbox. */
export class Commands {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  /** run executes argv (no shell interpolation) and returns stdout/stderr/exit.
   *  A non-zero exitCode is a normal result, NOT an error (US-02). The request is
   *  NOT retried — running a command twice is not idempotent. */
  async run(cmd: string[], opts: RunOptions = {}): Promise<ExecResult> {
    if (cmd.length === 0) throw new Error('cmd must have at least one element');
    const body = JSON.stringify({
      cmd,
      env: opts.env,
      workdir: opts.workdir,
      timeout_ms: opts.timeoutMs,
    });
    const res = await this.http.sendJSON<WireExecResult>({
      method: 'POST',
      path: `/v1/sandboxes/${encodeURIComponent(this.sandboxId)}/executions`,
      body,
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return { exitCode: res.exit_code, stdout: res.stdout, stderr: res.stderr };
  }

  /** stream runs argv and yields its output while it runs: "stdout" and
   *  "stderr" events, then one "done" event with the exit code. A failure after
   *  the output started is thrown from the iteration. Needs the capability
   *  "exec_stream". Never retried.
   *
   * @example
   * for await (const event of sandbox.commands.stream(['make', 'build'])) {
   *   if (event.type === 'done') console.log('exit', event.exitCode);
   *   else process.stdout.write(event.data);
   * }
   */
  async *stream(cmd: string[], opts: RunOptions = {}): AsyncGenerator<OutputStreamEvent, void, void> {
    if (cmd.length === 0) throw new Error('cmd must have at least one element');
    const res = await this.http.send({
      method: 'POST',
      path: sandboxRoute(this.sandboxId, '/executions?stream=1'),
      body: JSON.stringify({ cmd, env: opts.env, workdir: opts.workdir, timeout_ms: opts.timeoutMs }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    yield* readOutputStream(res);
  }
}

interface WireExecResult {
  exit_code: number;
  stdout: string;
  stderr: string;
}
