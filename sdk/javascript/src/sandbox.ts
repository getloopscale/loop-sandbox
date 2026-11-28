import {
  CommandExitError,
  FileNotFoundError,
  LoopSandboxError,
  NotFoundError,
  SandboxNotFoundError,
  UnauthorizedError,
  ValidationError,
} from './errors.js';
import { HttpClient, type FetchLike, type WebSocketConstructor } from './http.js';
import type { ProcessInfo as StartedProcess } from './processes.js';
import type { PtyConnection } from './pty.js';
import { Sandbox as SandboxHandle, Sandboxes } from './sandboxes.js';
import type {
  NetworkDenial,
  NetworkPolicy,
  SandboxInfo as WireShapedInfo,
  SandboxMetrics as WireShapedMetrics,
  SandboxState,
} from './types.js';

/** Where the console of LoopScale answers when no domain is given. */
const DEFAULT_DOMAIN = 'app.getloopscale.com';
/** The shell a command line runs through. Every image ships /bin/sh; not every one ships bash. */
const SHELL = ['/bin/sh', '-c'] as const;
/** How long one poll for the output of a background command waits on the server. */
const OUTPUT_WAIT_MS = 10_000;
/** The destination that stands for every address in a deny rule. */
export const ALL_TRAFFIC = '0.0.0.0/0';

/** How to reach the API. Every field has a fallback in the environment. */
export interface ConnectionOpts {
  /** API key with the `sandbox:execute` scope. Defaults to LOOPSCALE_API_KEY. */
  apiKey?: string;
  /** Host of the console, without scheme. Defaults to LOOPSCALE_DOMAIN, then to app.getloopscale.com. */
  domain?: string;
  /** Full base URL. Wins over `domain`. Defaults to LOOPSCALE_API_URL. */
  apiUrl?: string;
  /** Injectable fetch (defaults to the global one). */
  fetch?: FetchLike;
  /** Max retries of an idempotent request (default 2). */
  maxRetries?: number;
  /** Base backoff in ms (default 100). */
  retryBaseMs?: number;
  /** WebSocket constructor for the terminal. See ClientOptions.webSocket. */
  webSocket?: WebSocketConstructor;
  signal?: AbortSignal;
}

export interface SandboxOpts extends ConnectionOpts {
  /** Lifetime of the sandbox in milliseconds. The server rounds it up to whole seconds. */
  timeoutMs?: number;
  metadata?: Record<string, string>;
  /** Environment of every command in the sandbox. */
  envs?: Record<string, string>;
  /** False creates the sandbox with every outbound destination denied. Default true. */
  allowInternetAccess?: boolean;
  /** Outbound rules. Wins over allowInternetAccess. */
  network?: NetworkPolicy;
  /** Names of the secrets to expose to the sandbox. */
  secrets?: string[];
}

export interface SandboxInfo {
  sandboxId: string;
  /** The image or template the sandbox was created from. */
  templateId: string;
  state: SandboxState | string;
  metadata: Record<string, string>;
  startedAt: Date;
  endAt: Date;
  network?: NetworkPolicy;
}

export interface SandboxMetrics {
  timestamp: Date;
  /** Memory in use, in bytes. */
  memUsed: number;
  /** Memory limit, in bytes. */
  memTotal: number;
  /** CPU time used since the sandbox started, in nanoseconds. */
  cpuUsageNs: number;
  /** Bytes in use in the workspace. Absent for a paused sandbox. */
  diskUsed?: number;
  /** Size of the workspace in bytes. Absent with diskUsed. */
  diskTotal?: number;
}

export interface SandboxListOpts extends ConnectionOpts {
  query?: { state?: SandboxState[]; metadata?: Record<string, string> };
  /** Sandboxes per page, 1 to 200. */
  limit?: number;
  nextToken?: string;
}

export interface CommandResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

export interface CommandStartOpts {
  /** True answers a handle at once instead of waiting for the command to end. */
  background?: boolean;
  cwd?: string;
  envs?: Record<string, string>;
  onStdout?: (data: string) => void | Promise<void>;
  onStderr?: (data: string) => void | Promise<void>;
  /** How long the command may run, in milliseconds. */
  timeoutMs?: number;
  signal?: AbortSignal;
}

export interface CommandHandle {
  readonly pid: number;
  /** Waits for the command to end. Throws CommandExitError on a non-zero exit code. */
  wait(): Promise<CommandResult>;
  kill(): Promise<boolean>;
  /** Stops following the command, which keeps running. */
  disconnect(): void;
}

export interface ProcessInfo {
  pid: number;
  cmd: string;
  args: string[];
}

export type FileType = 'file' | 'dir';

export interface WriteInfo {
  name: string;
  path: string;
  type: FileType;
}

export interface EntryInfo extends WriteInfo {
  size: number;
  mode?: string;
  modifiedTime?: Date;
}

export interface WriteEntry {
  path: string;
  data: string | Uint8Array;
}

export interface PtyCreateOpts {
  cols: number;
  rows: number;
  onData: (data: Uint8Array) => void;
}

function env(name: string): string | undefined {
  const value = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env?.[name];
  return value === undefined || value === '' ? undefined : value;
}

function httpFrom(opts: ConnectionOpts): HttpClient {
  const apiKey = opts.apiKey ?? env('LOOPSCALE_API_KEY') ?? env('LOOPSANDBOX_API_KEY');
  if (!apiKey) {
    throw new UnauthorizedError(
      'no API key: pass apiKey or set LOOPSCALE_API_KEY. Create a key with the sandbox:execute scope under API Tokens in the console.',
      0,
      'missing_api_key',
    );
  }
  const baseUrl = opts.apiUrl ?? env('LOOPSCALE_API_URL') ?? `https://${opts.domain ?? env('LOOPSCALE_DOMAIN') ?? DEFAULT_DOMAIN}`;
  return new HttpClient({
    baseUrl,
    apiKey,
    fetch: opts.fetch,
    maxRetries: opts.maxRetries,
    retryBaseMs: opts.retryBaseMs,
    webSocket: opts.webSocket,
  });
}

/** Rethrows a plain 404 as the more specific error the caller's code catches. */
async function naming<T>(specific: new (...args: ConstructorParameters<typeof NotFoundError>) => NotFoundError, work: () => Promise<T>): Promise<T> {
  try {
    return await work();
  } catch (e) {
    if (e instanceof NotFoundError && e.constructor === NotFoundError) {
      throw new specific(e.message, e.status, e.code, e.requestId);
    }
    throw e;
  }
}

/** Runs work and answers `missing` when the API says the thing does not exist. */
async function orWhenMissing<T>(missing: T, work: () => Promise<T>): Promise<T> {
  try {
    return await work();
  } catch (e) {
    if (e instanceof NotFoundError) return missing;
    throw e;
  }
}

function toInfo(info: WireShapedInfo): SandboxInfo {
  const out: SandboxInfo = {
    sandboxId: info.id,
    templateId: info.image,
    state: info.state,
    metadata: info.metadata ?? {},
    startedAt: new Date(info.createdAt),
    endAt: new Date(info.expiresAt),
  };
  if (info.network) out.network = info.network;
  return out;
}

function toMetrics(m: WireShapedMetrics): SandboxMetrics {
  const out: SandboxMetrics = {
    timestamp: new Date(m.at),
    memUsed: m.memoryUsedBytes,
    memTotal: m.memoryLimitBytes,
    cpuUsageNs: m.cpuUsageNs,
  };
  if (m.diskLimitBytes !== undefined) {
    out.diskUsed = m.diskUsedBytes ?? 0;
    out.diskTotal = m.diskLimitBytes;
  }
  return out;
}

function seconds(ms: number): number {
  return Math.ceil(ms / 1000);
}

/** Where the files of a sandbox live. It is also where a command starts. */
const WORKSPACE = '/workspace';

/** Resolves a path the way a command in the sandbox would: a relative one
 *  starts at the workspace. A path outside the workspace is refused here, with
 *  the reason, because the API only reads and writes under it. */
function inWorkspace(path: string): string {
  if (!path.startsWith('/')) return joinPath(WORKSPACE, path.replace(/^\.\//, ''));
  if (path === WORKSPACE || path.startsWith(`${WORKSPACE}/`)) return path;
  throw new ValidationError(
    `the files of a sandbox live under ${WORKSPACE}: use a relative path or one that starts with ${WORKSPACE} (got ${path})`,
    0,
    'path_outside_workspace',
  );
}

function joinPath(dir: string, name: string): string {
  return dir.endsWith('/') ? dir + name : `${dir}/${name}`;
}

function baseName(path: string): string {
  const trimmed = path.replace(/\/+$/, '');
  return trimmed.slice(trimmed.lastIndexOf('/') + 1);
}

/** Pages through the sandboxes of the organization, newest first. */
export class SandboxPaginator {
  private started = false;
  private token: string | undefined;

  constructor(
    private readonly sandboxes: Sandboxes,
    private readonly opts: SandboxListOpts,
  ) {
    this.token = opts.nextToken;
  }

  /** True until the last page was read. */
  get hasNext(): boolean {
    return !this.started || this.token !== undefined;
  }

  /** The cursor of the next page, to resume the listing in another paginator. */
  get nextToken(): string | undefined {
    return this.token;
  }

  async nextItems(): Promise<SandboxInfo[]> {
    if (!this.hasNext) throw new LoopSandboxError('no more sandboxes to list', 0, 'list_exhausted');
    const page = await this.sandboxes.list({
      state: this.opts.query?.state,
      metadata: this.opts.query?.metadata,
      limit: this.opts.limit,
      cursor: this.token,
      signal: this.opts.signal,
    });
    this.started = true;
    this.token = page.nextCursor;
    return page.sandboxes.map((s) => toInfo(s.info));
  }
}

/** Commands of one sandbox, with the method names of the e2b SDK. */
export class SandboxCommands {
  constructor(private readonly handle: SandboxHandle) {}

  run(cmd: string, opts: CommandStartOpts & { background: true }): Promise<CommandHandle>;
  run(cmd: string, opts?: CommandStartOpts & { background?: false }): Promise<CommandResult>;
  async run(cmd: string, opts: CommandStartOpts = {}): Promise<CommandResult | CommandHandle> {
    const argv = [...SHELL, cmd];
    const cwd = opts.cwd === undefined ? undefined : inWorkspace(opts.cwd);
    if (opts.background) {
      const started = await this.handle.processes.start(argv, { env: opts.envs, workdir: cwd, signal: opts.signal });
      return this.follow(started, opts);
    }
    const runOpts = { env: opts.envs, workdir: cwd, timeoutMs: opts.timeoutMs, signal: opts.signal };
    if (!opts.onStdout && !opts.onStderr) {
      return finished(await this.handle.commands.run(argv, runOpts));
    }
    let stdout = '';
    let stderr = '';
    for await (const event of this.handle.commands.stream(argv, runOpts)) {
      if (event.type === 'done') return finished({ exitCode: event.exitCode, stdout, stderr });
      if (event.type === 'stdout') {
        stdout += event.data;
        await opts.onStdout?.(event.data);
      } else {
        stderr += event.data;
        await opts.onStderr?.(event.data);
      }
    }
    throw new LoopSandboxError('the command stream ended without an exit code', 0, 'stream_incomplete');
  }

  /** Follows a command started in the background by its pid. */
  async connect(pid: number, opts: Pick<CommandStartOpts, 'onStdout' | 'onStderr' | 'signal'> = {}): Promise<CommandHandle> {
    return this.follow(await this.handle.processes.get(pid, opts.signal), opts);
  }

  /** The background commands of the sandbox, running or ended. */
  async list(): Promise<ProcessInfo[]> {
    const processes = await this.handle.processes.list();
    return processes.map((p) => ({ pid: p.pid, cmd: p.cmd[0] ?? '', args: p.cmd.slice(1) }));
  }

  /** Kills a background command. False when no command has that pid. */
  kill(pid: number): Promise<boolean> {
    return orWhenMissing(false, async () => {
      await this.handle.processes.signal(pid, 'KILL');
      return true;
    });
  }

  sendStdin(pid: number, data: string | Uint8Array): Promise<void> {
    return this.handle.processes.writeStdin(pid, data);
  }

  closeStdin(pid: number): Promise<void> {
    return this.handle.processes.closeStdin(pid);
  }

  private follow(started: StartedProcess, opts: Pick<CommandStartOpts, 'onStdout' | 'onStderr' | 'signal'>): CommandHandle {
    const processes = this.handle.processes;
    let following = true;
    return {
      pid: started.pid,
      wait: async () => {
        let stdout = '';
        let stderr = '';
        let after: number | undefined;
        while (following) {
          const out = await processes.output(started.pid, { after, waitMs: OUTPUT_WAIT_MS, signal: opts.signal });
          if (out.stdout) {
            stdout += out.stdout;
            await opts.onStdout?.(out.stdout);
          }
          if (out.stderr) {
            stderr += out.stderr;
            await opts.onStderr?.(out.stderr);
          }
          after = out.next;
          if (!out.process.running) return finished({ exitCode: out.process.exitCode ?? -1, stdout, stderr });
        }
        throw new LoopSandboxError('disconnected from the command before it ended', 0, 'command_disconnected');
      },
      kill: () => this.kill(started.pid),
      disconnect: () => {
        following = false;
      },
    };
  }
}

function finished(result: CommandResult): CommandResult {
  if (result.exitCode !== 0) throw new CommandExitError(result.exitCode, result.stdout, result.stderr);
  return result;
}

/** Files of one sandbox, with the method names of the e2b SDK. */
export class SandboxFiles {
  constructor(private readonly handle: SandboxHandle) {}

  read(path: string, opts?: { format?: 'text'; signal?: AbortSignal }): Promise<string>;
  read(path: string, opts: { format: 'bytes'; signal?: AbortSignal }): Promise<Uint8Array>;
  read(path: string, opts: { format: 'blob'; signal?: AbortSignal }): Promise<Blob>;
  async read(
    path: string,
    opts: { format?: 'text' | 'bytes' | 'blob'; signal?: AbortSignal } = {},
  ): Promise<string | Uint8Array | Blob> {
    path = inWorkspace(path);
    return naming(FileNotFoundError, async () => {
      if (opts.format === undefined || opts.format === 'text') return this.handle.files.read(path, opts.signal);
      const bytes = await this.handle.files.readBytes(path, opts.signal);
      return opts.format === 'blob' ? new Blob([bytes as BlobPart]) : bytes;
    });
  }

  write(path: string, data: string | Uint8Array): Promise<WriteInfo>;
  write(files: WriteEntry[]): Promise<WriteInfo[]>;
  async write(target: string | WriteEntry[], data?: string | Uint8Array): Promise<WriteInfo | WriteInfo[]> {
    if (typeof target !== 'string') return this.writeFiles(target);
    if (data === undefined) throw new LoopSandboxError('files.write needs the data to write', 0, 'invalid_argument');
    const path = inWorkspace(target);
    await this.handle.files.write(path, data);
    return { name: baseName(path), path, type: 'file' };
  }

  async writeFiles(files: WriteEntry[]): Promise<WriteInfo[]> {
    const written: WriteInfo[] = [];
    for (const file of files) written.push(await this.write(file.path, file.data));
    return written;
  }

  /** The entries directly under a directory. */
  async list(path: string): Promise<EntryInfo[]> {
    path = inWorkspace(path);
    const entries = await naming(FileNotFoundError, () => this.handle.files.list(path));
    return entries.map((e) => ({ name: e.name, path: joinPath(path, e.name), type: e.isDir ? 'dir' : 'file', size: e.size }));
  }

  exists(path: string): Promise<boolean> {
    return orWhenMissing(false, async () => {
      await this.handle.files.info(inWorkspace(path));
      return true;
    });
  }

  async getInfo(path: string): Promise<EntryInfo> {
    const info = await naming(FileNotFoundError, () => this.handle.files.info(inWorkspace(path)));
    return {
      name: info.name,
      path: info.path,
      type: info.isDir ? 'dir' : 'file',
      size: info.size,
      mode: info.mode,
      modifiedTime: new Date(info.modifiedAt),
    };
  }

  /** Creates a directory and its missing parents. */
  async makeDir(path: string): Promise<boolean> {
    await this.handle.files.makeDir(inWorkspace(path));
    return true;
  }

  remove(path: string): Promise<void> {
    return naming(FileNotFoundError, () => this.handle.files.delete(inWorkspace(path)));
  }

  async rename(oldPath: string, newPath: string): Promise<EntryInfo> {
    const to = inWorkspace(newPath);
    await naming(FileNotFoundError, () => this.handle.files.move(inWorkspace(oldPath), to));
    return this.getInfo(to);
  }
}

/** Terminals of one sandbox. A pid here names a terminal opened by this object. */
export class SandboxPty {
  private readonly open = new Map<number, PtyConnection>();
  private lastPid = 0;

  constructor(private readonly handle: SandboxHandle) {}

  async create(opts: PtyCreateOpts): Promise<CommandHandle> {
    const connection = await this.handle.pty.open({ rows: opts.rows, cols: opts.cols });
    const pid = ++this.lastPid;
    this.open.set(pid, connection);
    connection.onData(opts.onData);
    void connection.closed.then(() => this.open.delete(pid));
    return {
      pid,
      wait: async () => {
        await connection.closed;
        return { exitCode: 0, stdout: '', stderr: '' };
      },
      kill: () => this.kill(pid),
      disconnect: () => connection.close(),
    };
  }

  async sendInput(pid: number, data: Uint8Array | string): Promise<void> {
    this.terminal(pid).write(data);
  }

  async resize(pid: number, size: { cols: number; rows: number }): Promise<void> {
    this.terminal(pid).resize(size.rows, size.cols);
  }

  async kill(pid: number): Promise<boolean> {
    const connection = this.open.get(pid);
    if (!connection) return false;
    connection.close();
    this.open.delete(pid);
    return true;
  }

  private terminal(pid: number): PtyConnection {
    const connection = this.open.get(pid);
    if (!connection) throw new LoopSandboxError(`no open terminal with pid ${pid}`, 0, 'pty_not_found');
    return connection;
  }
}

/** A sandbox, with the shape of the Sandbox class of the e2b SDK.
 *
 * @example
 * const sandbox = await Sandbox.create();            // reads LOOPSCALE_API_KEY
 * await sandbox.files.write('/workspace/hi.txt', 'hello');
 * const out = await sandbox.commands.run('cat /workspace/hi.txt');
 * await sandbox.kill();
 */
export class Sandbox {
  readonly sandboxId: string;
  readonly commands: SandboxCommands;
  readonly files: SandboxFiles;
  readonly pty: SandboxPty;

  private constructor(private readonly handle: SandboxHandle) {
    this.sandboxId = handle.id;
    this.commands = new SandboxCommands(handle);
    this.files = new SandboxFiles(handle);
    this.pty = new SandboxPty(handle);
  }

  /** Creates a sandbox. The first argument is an image (`python:3.12-slim`) or
   *  the name of a template; without it the sandbox uses the default image. */
  static create(opts?: SandboxOpts): Promise<Sandbox>;
  static create(template: string, opts?: SandboxOpts): Promise<Sandbox>;
  static async create(first?: string | SandboxOpts, second?: SandboxOpts): Promise<Sandbox> {
    const template = typeof first === 'string' ? first : undefined;
    const opts = (typeof first === 'string' ? second : first) ?? {};
    const isImage = template !== undefined && /[:/]/.test(template);
    const network = opts.network ?? (opts.allowInternetAccess === false ? { allowOut: [], denyOut: [ALL_TRAFFIC] } : undefined);
    const handle = await new Sandboxes(httpFrom(opts)).create({
      image: isImage ? template : undefined,
      template: isImage ? undefined : template,
      ttlSeconds: opts.timeoutMs === undefined ? undefined : seconds(opts.timeoutMs),
      metadata: opts.metadata,
      env: opts.envs,
      network,
      secrets: opts.secrets,
      signal: opts.signal,
    });
    return new Sandbox(handle);
  }

  /** Connects to a sandbox that exists, resuming it when it is paused. */
  static async connect(sandboxId: string, opts: ConnectionOpts = {}): Promise<Sandbox> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    return naming(SandboxNotFoundError, async () => {
      const handle = await sandboxes.get(sandboxId, opts.signal);
      return new Sandbox(handle.info.state === 'paused' ? await sandboxes.resume(sandboxId, opts.signal) : handle);
    });
  }

  static list(opts: SandboxListOpts = {}): SandboxPaginator {
    return new SandboxPaginator(new Sandboxes(httpFrom(opts)), opts);
  }

  /** Kills a sandbox. False when it no longer exists. */
  static kill(sandboxId: string, opts: ConnectionOpts = {}): Promise<boolean> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    return orWhenMissing(false, async () => {
      await sandboxes.destroy(sandboxId, opts.signal);
      return true;
    });
  }

  static async getInfo(sandboxId: string, opts: ConnectionOpts = {}): Promise<SandboxInfo> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    return toInfo((await naming(SandboxNotFoundError, () => sandboxes.get(sandboxId, opts.signal))).info);
  }

  /** The current use of CPU and memory, as a list of one sample. */
  static async getMetrics(sandboxId: string, opts: ConnectionOpts = {}): Promise<SandboxMetrics[]> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    const m = await naming(SandboxNotFoundError, () => sandboxes.metrics(sandboxId, opts.signal));
    return [toMetrics(m)];
  }

  /** Pauses a sandbox. Sandbox.connect resumes it. */
  static async pause(sandboxId: string, opts: ConnectionOpts = {}): Promise<boolean> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    await naming(SandboxNotFoundError, () => sandboxes.pause(sandboxId, opts.signal));
    return true;
  }

  /** Makes the sandbox expire timeoutMs from now. */
  static async setTimeout(sandboxId: string, timeoutMs: number, opts: ConnectionOpts = {}): Promise<void> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    await naming(SandboxNotFoundError, () => sandboxes.setTimeout(sandboxId, seconds(timeoutMs), opts.signal));
  }

  /** Replaces the outbound rules of a running sandbox. */
  static updateNetwork(sandboxId: string, network: NetworkPolicy, opts: ConnectionOpts = {}): Promise<NetworkPolicy> {
    const sandboxes = new Sandboxes(httpFrom(opts));
    return naming(SandboxNotFoundError, () => sandboxes.updateNetwork(sandboxId, network, opts.signal));
  }

  async kill(): Promise<boolean> {
    return orWhenMissing(false, async () => {
      await this.handle.destroy();
      return true;
    });
  }

  /** True while the sandbox exists and is running. */
  isRunning(): Promise<boolean> {
    return orWhenMissing(false, async () => (await this.current()).state === 'running');
  }

  async getInfo(): Promise<SandboxInfo> {
    return toInfo(await naming(SandboxNotFoundError, () => this.current()));
  }

  async getMetrics(): Promise<SandboxMetrics[]> {
    const m = await naming(SandboxNotFoundError, () => this.handle.metrics());
    return [toMetrics(m)];
  }

  async pause(): Promise<boolean> {
    await naming(SandboxNotFoundError, () => this.handle.pause());
    return true;
  }

  async setTimeout(timeoutMs: number): Promise<void> {
    await naming(SandboxNotFoundError, () => this.handle.setTimeout(seconds(timeoutMs)));
  }

  updateNetwork(network: NetworkPolicy): Promise<NetworkPolicy> {
    return naming(SandboxNotFoundError, () => this.handle.updateNetwork(network));
  }

  /** The outbound connections the sandbox was refused, newest first. Not in the e2b SDK. */
  networkDenials(limit?: number): Promise<NetworkDenial[]> {
    return naming(SandboxNotFoundError, () => this.handle.networkDenials(limit));
  }

  private async current(): Promise<WireShapedInfo> {
    return (await this.handle.refreshed()).info;
  }
}
