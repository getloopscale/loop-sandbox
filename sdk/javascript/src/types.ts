// Public wire types. Field names mirror the REST contract (api/openapi.yaml) but
// are exposed to callers as idiomatic camelCase where the SDK adds a mapping.

/** Sandbox metadata as returned by the API. */
export interface SandboxInfo {
  id: string;
  tenantId: string;
  image: string;
  state: SandboxState | string;
  createdAt: string;
  expiresAt: string;
  /** GPU class, present on a GPU-backed sandbox. */
  gpu?: string;
  /** Names of the secrets the sandbox receives. */
  secrets?: string[];
  /** Idle seconds after which the sandbox pauses itself, when it does. */
  autoPauseSeconds?: number;
  /** Labels given at creation. */
  metadata?: Record<string, string>;
  /** Default environment of the commands run in the sandbox. */
  env?: Record<string, string>;
}

/** The lifecycle states of a sandbox. */
export type SandboxState = 'pending' | 'running' | 'paused' | 'destroyed';

/** Options for creating a sandbox. */
export interface CreateSandboxOptions {
  /** Container image (must be on the server allow-list). */
  image?: string;
  /** Time-to-live in seconds (server clamps to its min/max bounds). */
  ttlSeconds?: number;
  /** Pause the sandbox after this many seconds without a request that uses it,
   *  and resume it on the next one. Between 10 and 86400. Needs the capability
   *  "pause". */
  autoPauseSeconds?: number;
  /** Names of the tenant's secrets. Commands and background processes of the
   *  sandbox receive them as environment variables. */
  secrets?: string[];
  /** Id of a registered template to provision from. Cannot be combined with gpu. */
  template?: string;
  /** Id of a snapshot to start from. Cannot be combined with image, template or gpu. */
  snapshot?: string;
  /** GPU class for a GPU-backed sandbox. Cannot be combined with template. */
  gpu?: string;
  /** Labels returned as given and usable as list filters. Fixed after creation. */
  metadata?: Record<string, string>;
  /** Default environment of every command run through sandbox.commands and
   *  sandbox.processes. Code sessions and terminals do not receive it. */
  env?: Record<string, string>;
  /** Idempotency key; a retry with the same key + payload replays the first result. */
  idempotencyKey?: string;
  /** Abort the request (timeout / cancellation). */
  signal?: AbortSignal;
}

/** Options for running a command. */
export interface RunOptions {
  /** Environment variables for the command. */
  env?: Record<string, string>;
  /** Working directory (confined to /workspace by the server). */
  workdir?: string;
  /** Per-command timeout in milliseconds (server enforces; over-limit → 504). */
  timeoutMs?: number;
  /** Abort the request. */
  signal?: AbortSignal;
}

/** Result of a command execution. A non-zero exitCode is a normal result (US-02). */
export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
}

/** A directory entry from a file listing. */
export interface FileEntry {
  name: string;
  size: number;
  isDir: boolean;
}

/** Filters and paging for listing sandboxes. */
export interface ListSandboxesOptions {
  /** Keep only sandboxes in one of these states. */
  state?: SandboxState | readonly SandboxState[];
  /** Keep only sandboxes that carry every one of these labels. */
  metadata?: Record<string, string>;
  /** Page size, 1 to 200 (server default 50). */
  limit?: number;
  /** The nextCursor of the previous page. */
  cursor?: string;
  signal?: AbortSignal;
}

/** The CPU and memory use of a sandbox at one instant. */
export interface SandboxMetrics {
  /** CPU time used since the sandbox started, in nanoseconds. */
  cpuUsageNs: number;
  memoryUsedBytes: number;
  memoryLimitBytes: number;
  /** When the sample was taken (RFC 3339). */
  at: string;
}

/** One chunk of output written by a background process. */
export interface LogEntry {
  pid: number;
  stream: 'stdout' | 'stderr';
  data: string;
  at: string;
}

/** A page of the combined output of the background processes of a sandbox. */
export interface SandboxLogs {
  entries: LogEntry[];
  /** Cursor to send as "after" in the following request. */
  next: number;
  /** True when the cursor pointed to output the sandbox no longer keeps. */
  truncated: boolean;
}

/** Cursor options shared by the long-poll reads (logs, process output, watch events). */
export interface CursorOptions {
  /** Cursor from the "next" of an earlier answer. Default 0, the start. */
  after?: number;
  /** With nothing after the cursor, wait up to this long, 0 to 30000 ms. */
  waitMs?: number;
  signal?: AbortSignal;
}

/** Size, type, permissions and modification time of a file or directory. */
export interface FileInfo {
  name: string;
  path: string;
  /** Bytes. 0 for a directory. */
  size: number;
  isDir: boolean;
  /** Permission bits in octal, such as "0644". */
  mode: string;
  modifiedAt: string;
}

/** A signed URL that uploads or downloads one file without an API key. */
export interface SignedFileUrl {
  url: string;
  /** The HTTP method to use with the URL. */
  method: 'GET' | 'PUT';
  expiresAt: string;
}

/** A presigned object storage URL for a file copied out of the sandbox. */
export interface ArtifactUrl {
  url: string;
  expiresInSeconds: number;
}
