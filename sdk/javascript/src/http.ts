import { LoopSandboxError, TimeoutError, errorFromResponse } from './errors.js';

/** Minimal fetch signature (global fetch on Node ≥18; injectable for tests). */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/** Configuration for the low-level HTTP client. */
export interface ClientOptions {
  /** Base URL of the LoopSandbox API, e.g. "https://sandbox.getloopscale.com". */
  baseUrl: string;
  /** API key (`lsb_...`). Sent as `Authorization: Bearer`. Never logged. */
  apiKey: string;
  /** Injectable fetch (defaults to global fetch). */
  fetch?: FetchLike;
  /** Max retries for transient failures on idempotent requests (default 2). */
  maxRetries?: number;
  /** Base backoff in ms; grows exponentially (default 100). */
  retryBaseMs?: number;
  /** WebSocket constructor for the terminal (sandbox.pty.open). Defaults to the
   *  global WebSocket. It must accept request headers as its second argument,
   *  which Node 22+ and the "ws" package do and browsers do not. */
  webSocket?: WebSocketConstructor;
}

/** The part of a WebSocket the SDK uses. The global WebSocket and the one of
 *  the "ws" package both satisfy it. */
export interface WebSocketLike {
  binaryType: string;
  send(data: string | Uint8Array): void;
  close(code?: number, reason?: string): void;
  addEventListener(type: string, listener: (event: { data?: unknown }) => void): void;
}

/** A WebSocket constructor that takes request headers in its second argument. */
export type WebSocketConstructor = new (
  url: string,
  options: { headers: Record<string, string> },
) => WebSocketLike;

interface SendRequest {
  method: string;
  /** Path under the base URL, or an absolute http(s) URL (signed file URLs). */
  path: string;
  /** False sends no Authorization header. Used by the routes that need no API
   *  key, so the key never travels to a signed URL on another host. Default true. */
  authenticated?: boolean;
  body?: BodyInit;
  contentType?: string;
  idempotencyKey?: string;
  signal?: AbortSignal;
  /** Retry transient failures. TRUE only for idempotent requests (GET/DELETE/PUT)
   *  or POSTs carrying an Idempotency-Key. A bare POST is never retried. */
  retriable: boolean;
}

// A response is transient (worth retrying an idempotent request) on 429/5xx, or on
// a 409 whose code is idempotency_in_progress: an in-flight reservation from a
// prior attempt that will resolve to a replay once it completes. A 501 is never
// transient: the deployment lacks the capability and a retry cannot change that.
async function transientReason(res: Response): Promise<boolean> {
  if (res.status === 501) return false;
  if (res.status === 429 || res.status >= 500) return true;
  if (res.status === 409) {
    try {
      const body = (await res.clone().json()) as { error?: { code?: string } };
      return body?.error?.code === 'idempotency_in_progress';
    } catch {
      return false;
    }
  }
  return false;
}

/** HttpClient owns auth, retry-with-backoff, and error mapping. It is the single
 *  place that touches the API key — nothing it throws contains that key. */
export class HttpClient {
  private readonly baseUrl: string;
  private readonly apiKey: string;
  private readonly fetchImpl: FetchLike;
  private readonly maxRetries: number;
  private readonly retryBaseMs: number;
  private readonly webSocketImpl: WebSocketConstructor | undefined;

  constructor(opts: ClientOptions) {
    if (!opts.baseUrl) throw new Error('baseUrl is required');
    if (!opts.apiKey) throw new Error('apiKey is required');
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
    this.apiKey = opts.apiKey;
    const f = opts.fetch ?? globalThis.fetch;
    if (typeof f !== 'function') {
      throw new Error('no fetch implementation available (Node ≥18 or pass opts.fetch)');
    }
    this.fetchImpl = f;
    this.maxRetries = opts.maxRetries ?? 2;
    this.retryBaseMs = opts.retryBaseMs ?? 100;
    this.webSocketImpl =
      opts.webSocket ?? (globalThis as { WebSocket?: unknown }).WebSocket as WebSocketConstructor | undefined;
  }

  /** openWebSocket starts a WebSocket to path with the API key in the
   *  Authorization header of the upgrade request. */
  openWebSocket(path: string): WebSocketLike {
    if (typeof this.webSocketImpl !== 'function') {
      throw new Error('no WebSocket implementation available (Node 22+ or pass opts.webSocket)');
    }
    const url = this.baseUrl.replace(/^http/, 'ws') + path;
    return new this.webSocketImpl(url, { headers: { Authorization: `Bearer ${this.apiKey}` } });
  }

  /** send performs one logical request with retry, returning the ok Response.
   *  On a non-2xx it throws the mapped typed error; on network failure a
   *  LoopSandboxError with status 0. */
  async send(req: SendRequest): Promise<Response> {
    const url = /^https?:\/\//.test(req.path) ? req.path : this.baseUrl + req.path;
    const headers: Record<string, string> = {};
    if (req.authenticated !== false) headers['Authorization'] = `Bearer ${this.apiKey}`;
    if (req.contentType) headers['Content-Type'] = req.contentType;
    if (req.idempotencyKey) headers['Idempotency-Key'] = req.idempotencyKey;

    const maxAttempts = req.retriable ? this.maxRetries + 1 : 1;
    let lastErr: unknown;
    for (let attempt = 0; attempt < maxAttempts; attempt++) {
      const isLast = attempt === maxAttempts - 1;
      try {
        const res = await this.fetchImpl(url, {
          method: req.method,
          headers,
          body: req.body,
          signal: req.signal,
        });
        if (res.ok) return res;
        if (!isLast && (await transientReason(res))) {
          // A rate-limited request waits at least what the server asked for.
          const retryAfterMs = (retryAfterSeconds(res) ?? 0) * 1000;
          await delay(Math.max(this.backoff(attempt), retryAfterMs), req.signal);
          continue;
        }
        throw await toError(res);
      } catch (e) {
        if (e instanceof LoopSandboxError) throw e; // already a mapped HTTP error
        if (isAbort(e)) throw new TimeoutError('request aborted', 0, 'timeout');
        lastErr = e;
        if (!isLast) {
          await delay(this.backoff(attempt), req.signal);
          continue;
        }
      }
    }
    throw new LoopSandboxError(`network error: ${errMessage(lastErr)}`, 0, 'network');
  }

  /** send + parse a JSON body. */
  async sendJSON<T>(req: SendRequest): Promise<T> {
    const res = await this.send(req);
    return (await res.json()) as T;
  }

  private backoff(attempt: number): number {
    return this.retryBaseMs * 2 ** attempt;
  }
}

/** Builds the typed error from a non-ok Response without leaking credentials. */
async function toError(res: Response): Promise<LoopSandboxError> {
  const requestId = res.headers.get('X-Request-ID') ?? undefined;
  let code = 'error';
  let message = `request failed with status ${res.status}`;
  try {
    const body = (await res.json()) as { error?: { code?: string; message?: string } };
    if (body?.error?.code) code = body.error.code;
    if (body?.error?.message) message = body.error.message;
  } catch {
    // non-JSON error body — keep the generic status message.
  }
  return errorFromResponse(res.status, code, message, requestId, retryAfterSeconds(res));
}

// retryAfterSeconds reads Retry-After as whole seconds, the only form the API
// sends. A missing header or an HTTP date gives undefined.
function retryAfterSeconds(res: Response): number | undefined {
  const raw = res.headers.get('Retry-After');
  if (raw === null || !/^\d+$/.test(raw.trim())) return undefined;
  return Number(raw.trim());
}

/** withQuery appends the defined parameters to path as a query string. An array
 *  repeats its parameter. */
export function withQuery(
  path: string,
  params: Record<string, string | number | readonly string[] | undefined>,
): string {
  const query = new URLSearchParams();
  for (const [name, value] of Object.entries(params)) {
    if (value === undefined) continue;
    if (typeof value === 'string' || typeof value === 'number') query.append(name, String(value));
    else for (const item of value) query.append(name, item);
  }
  const encoded = query.toString();
  return encoded ? `${path}?${encoded}` : path;
}

/** sandboxRoute builds "/v1/sandboxes/{id}" plus an optional suffix. The id is
 *  encoded because it may be caller-supplied. */
export function sandboxRoute(sandboxId: string, suffix = ''): string {
  return `/v1/sandboxes/${encodeURIComponent(sandboxId)}${suffix}`;
}

/** encodeFilePath prepares a sandbox file path for a {path...} route. The server
 *  re-prefixes the captured path with "/", so leading slashes are dropped; each
 *  segment is percent-encoded and the slashes between them are kept. */
export function encodeFilePath(path: string): string {
  return path.replace(/^\/+/, '').split('/').map(encodeURIComponent).join('/');
}

function isAbort(e: unknown): boolean {
  return typeof e === 'object' && e !== null && (e as { name?: string }).name === 'AbortError';
}

function errMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/** Delay that also rejects promptly if the caller aborts. */
function delay(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new TimeoutError('request aborted', 0, 'timeout'));
      return;
    }
    const t = setTimeout(resolve, ms);
    signal?.addEventListener(
      'abort',
      () => {
        clearTimeout(t);
        reject(new TimeoutError('request aborted', 0, 'timeout'));
      },
      { once: true },
    );
  });
}
