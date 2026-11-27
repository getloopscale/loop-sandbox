import { sandboxRoute, withQuery, type HttpClient } from './http.js';
import type { CursorOptions } from './types.js';

/** A background process started through sandbox.processes. */
export interface ProcessInfo {
  pid: number;
  cmd: string[];
  startedAt: string;
  running: boolean;
  /** Null while the process runs. A process ended by a signal has 128 plus the
   *  signal number, so 143 after TERM. */
  exitCode: number | null;
}

/** Options for starting a background process. */
export interface StartProcessOptions {
  /** Environment variables. One of these wins over the sandbox env. */
  env?: Record<string, string>;
  /** Working directory under /workspace. */
  workdir?: string;
  signal?: AbortSignal;
}

/** A page of the output of one background process. */
export interface ProcessOutput {
  stdout: string;
  stderr: string;
  /** Cursor to send as "after" in the following request. */
  next: number;
  /** True when the cursor pointed to output the sandbox no longer keeps. */
  truncated: boolean;
  /** The state of the process when the output was read. */
  process: ProcessInfo;
}

/** The signals a background process accepts. */
export type ProcessSignal = 'HUP' | 'INT' | 'QUIT' | 'KILL' | 'TERM' | 'USR1' | 'USR2' | 'STOP' | 'CONT';

interface WireProcess {
  pid: number;
  cmd: string[];
  started_at: string;
  running: boolean;
  exit_code: number | null;
}

function mapProcess(w: WireProcess): ProcessInfo {
  return { pid: w.pid, cmd: w.cmd, startedAt: w.started_at, running: w.running, exitCode: w.exit_code };
}

/** Processes runs background processes in a single sandbox: commands that
 *  outlive the request that started them. Needs the capability "processes". */
export class Processes {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  private route(pid?: number, suffix = ''): string {
    const process = pid === undefined ? '' : `/${encodeURIComponent(String(pid))}`;
    return sandboxRoute(this.sandboxId, `/processes${process}${suffix}`);
  }

  /** start launches argv and returns at once, without waiting for it to end.
   *  A sandbox at its limit of running processes raises ConflictError (code
   *  "too_many_processes"). Never retried. */
  async start(cmd: string[], opts: StartProcessOptions = {}): Promise<ProcessInfo> {
    if (cmd.length === 0) throw new Error('cmd must have at least one element');
    const w = await this.http.sendJSON<WireProcess>({
      method: 'POST',
      path: this.route(),
      body: JSON.stringify({ cmd, env: opts.env, workdir: opts.workdir }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return mapProcess(w);
  }

  /** list returns the processes started through this API, oldest first,
   *  including the most recent ones that ended. */
  async list(signal?: AbortSignal): Promise<ProcessInfo[]> {
    const res = await this.http.sendJSON<{ processes: WireProcess[] }>({
      method: 'GET',
      path: this.route(),
      retriable: true,
      signal,
    });
    return res.processes.map(mapProcess);
  }

  /** get describes one process. */
  async get(pid: number, signal?: AbortSignal): Promise<ProcessInfo> {
    const w = await this.http.sendJSON<WireProcess>({
      method: 'GET',
      path: this.route(pid),
      retriable: true,
      signal,
    });
    return mapProcess(w);
  }

  /** output reads what the process wrote after the cursor opts.after. Send the
   *  "next" of one answer as the "after" of the following call. The output
   *  stays readable after the process ends. */
  async output(pid: number, opts: CursorOptions = {}): Promise<ProcessOutput> {
    const w = await this.http.sendJSON<{
      stdout: string;
      stderr: string;
      next: number;
      truncated: boolean;
      process: WireProcess;
    }>({
      method: 'GET',
      path: withQuery(this.route(pid, '/output'), { after: opts.after, wait_ms: opts.waitMs }),
      retriable: true,
      signal: opts.signal,
    });
    return {
      stdout: w.stdout,
      stderr: w.stderr,
      next: w.next,
      truncated: w.truncated,
      process: mapProcess(w.process),
    };
  }

  /** writeStdin writes data, as is, to the standard input of the process, up
   *  to 1 MiB per call. A process that ended raises ConflictError (code
   *  "process_exited"); a closed input raises it with "stdin_closed".
   *  Never retried, because a repeated write would duplicate the input. */
  async writeStdin(pid: number, data: string | Uint8Array, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'POST',
      path: this.route(pid, '/stdin'),
      body: data as BodyInit,
      contentType: 'application/octet-stream',
      retriable: false,
      signal,
    });
  }

  /** closeStdin closes the standard input, so the process reads its end.
   *  Closing twice is not an error. */
  async closeStdin(pid: number, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: this.route(pid, '/stdin'),
      retriable: true,
      signal,
    });
  }

  /** signal sends a signal to the process and to the processes it started.
   *  A process that ended raises ConflictError (code "process_exited"). */
  async signal(pid: number, signal: ProcessSignal, abortSignal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'POST',
      path: this.route(pid, '/signal'),
      body: JSON.stringify({ signal }),
      contentType: 'application/json',
      retriable: false,
      signal: abortSignal,
    });
  }
}
