import { Browser } from './browser.js';
import { Commands } from './executions.js';
import { Files } from './files.js';
import { sandboxRoute, withQuery, type HttpClient } from './http.js';
import { Processes } from './processes.js';
import { Pty } from './pty.js';
import { Sessions, type Session, type CellResult } from './sessions.js';
import type {
  CreateSandboxOptions,
  CursorOptions,
  ListSandboxesOptions,
  LogEntry,
  SandboxInfo,
  SandboxLogs,
  SandboxMetrics,
} from './types.js';
import { Watches } from './watches.js';

interface WireSandbox {
  id: string;
  tenant_id: string;
  image?: string;
  gpu?: string;
  state: string;
  created_at: string;
  expires_at?: string;
  auto_pause_seconds?: number;
  secrets?: string[];
  metadata?: Record<string, string>;
  env?: Record<string, string>;
}

function mapInfo(w: WireSandbox): SandboxInfo {
  const info: SandboxInfo = {
    id: w.id,
    tenantId: w.tenant_id,
    image: w.image ?? '',
    state: w.state,
    createdAt: w.created_at,
    expiresAt: w.expires_at ?? '',
  };
  if (w.gpu) info.gpu = w.gpu;
  if (w.auto_pause_seconds) info.autoPauseSeconds = w.auto_pause_seconds;
  if (w.secrets) info.secrets = w.secrets;
  if (w.metadata) info.metadata = w.metadata;
  if (w.env) info.env = w.env;
  return info;
}

/** One page of a sandbox listing. */
export interface SandboxPage {
  sandboxes: Sandbox[];
  /** Present when more sandboxes follow. Send it as "cursor", with the same
   *  filters, to get the next page. */
  nextCursor?: string;
}

/** Sandbox is a handle to one sandbox: its files, commands, processes, code
 *  sessions, terminal and browser, plus its lifecycle. `info` is the state the
 *  API reported when the handle was obtained; lifecycle methods return a new
 *  handle with the fresh state instead of changing this one. */
export class Sandbox {
  readonly id: string;
  readonly info: SandboxInfo;
  readonly files: Files;
  readonly commands: Commands;
  readonly sessions: Sessions;
  /** Background processes. */
  readonly processes: Processes;
  /** Directory watches. */
  readonly watches: Watches;
  /** The browser of the sandbox. */
  readonly browser: Browser;
  /** Interactive terminals. */
  readonly pty: Pty;
  private readonly collection: Sandboxes;
  private defaultSession?: Session;

  constructor(
    private readonly http: HttpClient,
    info: SandboxInfo,
  ) {
    this.id = info.id;
    this.info = info;
    this.files = new Files(http, info.id);
    this.commands = new Commands(http, info.id);
    this.sessions = new Sessions(http, info.id);
    this.processes = new Processes(http, info.id);
    this.watches = new Watches(http, info.id);
    this.browser = new Browser(http, info.id);
    this.pty = new Pty(http, info.id);
    this.collection = new Sandboxes(http);
  }

  /** runCode executes a code cell in a lazily-created DEFAULT session, so state is SHARED
   *  across successive runCode() calls (the E2B / OpenAI Code-Interpreter ergonomic). Use
   *  sandbox.sessions.start() for explicit, independent sessions. */
  async runCode(code: string, opts: { signal?: AbortSignal } = {}): Promise<CellResult> {
    if (!this.defaultSession) {
      this.defaultSession = await this.sessions.start({ signal: opts.signal });
    }
    return this.defaultSession.execute(code, opts);
  }

  /** destroy tears the sandbox down (idempotent — a second destroy is a no-op 404). */
  async destroy(signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/sandboxes/${encodeURIComponent(this.id)}`,
      retriable: true,
      signal,
    });
  }

  /** setTimeout makes the sandbox expire ttlSeconds from now. See Sandboxes.setTimeout. */
  setTimeout(ttlSeconds: number, signal?: AbortSignal): Promise<Sandbox> {
    return this.collection.setTimeout(this.id, ttlSeconds, signal);
  }

  /** fork creates a new sandbox from the memory and disk state of this one. */
  fork(signal?: AbortSignal): Promise<Sandbox> {
    return this.collection.fork(this.id, signal);
  }

  /** forkMany creates count independent forks of this sandbox, all or nothing. */
  forkMany(count: number, signal?: AbortSignal): Promise<Sandbox[]> {
    return this.collection.forkMany(this.id, count, signal);
  }

  /** pause snapshots the sandbox to durable storage and releases its compute. */
  pause(signal?: AbortSignal): Promise<void> {
    return this.collection.pause(this.id, signal);
  }

  /** resume brings the paused sandbox back. */
  resume(signal?: AbortSignal): Promise<Sandbox> {
    return this.collection.resume(this.id, signal);
  }

  /** metrics reads the CPU and memory use of the sandbox. */
  metrics(signal?: AbortSignal): Promise<SandboxMetrics> {
    return this.collection.metrics(this.id, signal);
  }

  /** logs reads the combined output of the background processes. */
  logs(opts: CursorOptions = {}): Promise<SandboxLogs> {
    return this.collection.logs(this.id, opts);
  }
}

/** Sandboxes is the sandbox collection API on the client. */
export class Sandboxes {
  constructor(private readonly http: HttpClient) {}

  /** create provisions a sandbox. Pass idempotencyKey to make the create safely
   *  retriable (same key + payload replays the first result). */
  async create(opts: CreateSandboxOptions = {}): Promise<Sandbox> {
    const body = JSON.stringify({
      image: opts.image,
      template: opts.template,
      snapshot: opts.snapshot,
      gpu: opts.gpu,
      ttl_seconds: opts.ttlSeconds,
      auto_pause_seconds: opts.autoPauseSeconds,
      secrets: opts.secrets,
      metadata: opts.metadata,
      env: opts.env,
    });
    const w = await this.http.sendJSON<WireSandbox>({
      method: 'POST',
      path: '/v1/sandboxes',
      body,
      contentType: 'application/json',
      idempotencyKey: opts.idempotencyKey,
      retriable: Boolean(opts.idempotencyKey),
      signal: opts.signal,
    });
    return new Sandbox(this.http, mapInfo(w));
  }

  /** list returns one page of the caller's sandboxes, newest first. */
  async list(opts: ListSandboxesOptions = {}): Promise<SandboxPage> {
    const states = typeof opts.state === 'string' ? [opts.state] : opts.state;
    const res = await this.http.sendJSON<{ sandboxes: WireSandbox[]; next_cursor?: string }>({
      method: 'GET',
      path: withQuery('/v1/sandboxes', {
        state: states && states.length > 0 ? states.join(',') : undefined,
        metadata: opts.metadata ? Object.entries(opts.metadata).map(([key, value]) => `${key}=${value}`) : undefined,
        limit: opts.limit,
        cursor: opts.cursor,
      }),
      retriable: true,
      signal: opts.signal,
    });
    const page: SandboxPage = { sandboxes: res.sandboxes.map((w) => new Sandbox(this.http, mapInfo(w))) };
    if (res.next_cursor) page.nextCursor = res.next_cursor;
    return page;
  }

  /** get fetches an existing sandbox by id. */
  async get(id: string, signal?: AbortSignal): Promise<Sandbox> {
    const w = await this.http.sendJSON<WireSandbox>({
      method: 'GET',
      path: `/v1/sandboxes/${encodeURIComponent(id)}`,
      retriable: true,
      signal,
    });
    return new Sandbox(this.http, mapInfo(w));
  }

  /** destroy tears down a sandbox by id (idempotent). */
  async destroy(id: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/sandboxes/${encodeURIComponent(id)}`,
      retriable: true,
      signal,
    });
  }

  /** setTimeout makes the sandbox expire ttlSeconds after this call, whatever
   *  its previous expiry was, so it can shorten or extend the lifetime. The
   *  server caps the value as it does at creation. */
  async setTimeout(id: string, ttlSeconds: number, signal?: AbortSignal): Promise<Sandbox> {
    const w = await this.http.sendJSON<WireSandbox>({
      method: 'POST',
      path: sandboxRoute(id, '/timeout'),
      body: JSON.stringify({ ttl_seconds: ttlSeconds }),
      contentType: 'application/json',
      retriable: false,
      signal,
    });
    return new Sandbox(this.http, mapInfo(w));
  }

  /** fork creates a new sandbox that starts from the memory and disk state of
   *  a running one. Needs the capability "fork". Never retried. */
  async fork(id: string, signal?: AbortSignal): Promise<Sandbox> {
    const w = await this.http.sendJSON<WireSandbox>({
      method: 'POST',
      path: sandboxRoute(id, '/fork'),
      retriable: false,
      signal,
    });
    return new Sandbox(this.http, mapInfo(w));
  }

  /** forkMany creates count independent forks (1 to 16) in one request. The
   *  batch is all or nothing: when one fork fails, the ones already made are
   *  destroyed and the call rejects. Never retried. */
  async forkMany(id: string, count: number, signal?: AbortSignal): Promise<Sandbox[]> {
    const res = await this.http.sendJSON<{ sandboxes: WireSandbox[] }>({
      method: 'POST',
      path: sandboxRoute(id, '/fork'),
      body: JSON.stringify({ count }),
      contentType: 'application/json',
      retriable: false,
      signal,
    });
    return res.sandboxes.map((w) => new Sandbox(this.http, mapInfo(w)));
  }

  /** pause snapshots a sandbox to durable storage and releases its compute.
   *  Its state becomes "paused". Needs the capability "pause". */
  async pause(id: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'POST',
      path: sandboxRoute(id, '/pause'),
      retriable: false,
      signal,
    });
  }

  /** resume brings a paused sandbox back. Needs the capability "pause". */
  async resume(id: string, signal?: AbortSignal): Promise<Sandbox> {
    const w = await this.http.sendJSON<WireSandbox>({
      method: 'POST',
      path: sandboxRoute(id, '/resume'),
      retriable: false,
      signal,
    });
    return new Sandbox(this.http, mapInfo(w));
  }

  /** metrics reads the CPU and memory use of a sandbox. cpuUsageNs counts from
   *  the start of the sandbox: for a rate, take two samples and divide the
   *  difference by the time between their "at". Needs the capability "metrics". */
  async metrics(id: string, signal?: AbortSignal): Promise<SandboxMetrics> {
    const w = await this.http.sendJSON<{
      cpu_usage_ns: number;
      memory_used_bytes: number;
      memory_limit_bytes: number;
      at: string;
    }>({
      method: 'GET',
      path: sandboxRoute(id, '/metrics'),
      retriable: true,
      signal,
    });
    return {
      cpuUsageNs: w.cpu_usage_ns,
      memoryUsedBytes: w.memory_used_bytes,
      memoryLimitBytes: w.memory_limit_bytes,
      at: w.at,
    };
  }

  /** logs reads, in the order it was written, what the background processes
   *  of a sandbox wrote after the cursor opts.after. Send the "next" of one
   *  answer as the "after" of the following call. Commands run through
   *  sandbox.commands are not in the log. Needs the capability "logs". */
  async logs(id: string, opts: CursorOptions = {}): Promise<SandboxLogs> {
    const w = await this.http.sendJSON<{ entries: LogEntry[]; next: number; truncated: boolean }>({
      method: 'GET',
      path: withQuery(sandboxRoute(id, '/logs'), { after: opts.after, wait_ms: opts.waitMs }),
      retriable: true,
      signal: opts.signal,
    });
    return { entries: w.entries, next: w.next, truncated: w.truncated };
  }
}
