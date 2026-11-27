import type { HttpClient } from './http.js';

/** What an API key may do. */
export type ApiKeyScope = 'sandbox:execute' | 'api_keys:manage';

/** An API key of the tenant. The secret is never part of it. */
export interface ApiKey {
  id: string;
  name: string;
  /** Display label derived from the hash of the key. Not part of the secret. */
  prefix: string;
  scopes: ApiKeyScope[];
  createdAt: string;
  /** Present on a revoked key. */
  revokedAt?: string;
}

/** A key that was just created. This is the only place its secret appears. */
export interface CreatedApiKey extends ApiKey {
  /** The secret. The server keeps only its hash, so store it now. */
  key: string;
}

/** Options for creating an API key. */
export interface CreateApiKeyOptions {
  /** 1 to 64 characters. */
  name: string;
  /** Defaults to ["sandbox:execute"]. Only scopes the calling key holds. */
  scopes?: ApiKeyScope[];
  signal?: AbortSignal;
}

interface WireApiKey {
  id: string;
  name: string;
  prefix: string;
  scopes: ApiKeyScope[];
  created_at: string;
  revoked_at?: string;
  key?: string;
}

function mapApiKey(w: WireApiKey): ApiKey {
  const key: ApiKey = { id: w.id, name: w.name, prefix: w.prefix, scopes: w.scopes, createdAt: w.created_at };
  if (w.revoked_at) key.revokedAt = w.revoked_at;
  return key;
}

/** ApiKeys manages the API keys of the tenant. Every method needs a key with
 *  the scope "api_keys:manage"; without it the server answers ForbiddenError. */
export class ApiKeys {
  constructor(private readonly http: HttpClient) {}

  /** list returns the keys, oldest first, revoked ones included. */
  async list(signal?: AbortSignal): Promise<ApiKey[]> {
    const res = await this.http.sendJSON<{ api_keys: WireApiKey[] }>({
      method: 'GET',
      path: '/v1/api-keys',
      retriable: true,
      signal,
    });
    return res.api_keys.map(mapApiKey);
  }

  /** create makes a key and returns it with its secret. Never retried: a
   *  retry would create a second key. */
  async create(opts: CreateApiKeyOptions): Promise<CreatedApiKey> {
    const w = await this.http.sendJSON<WireApiKey>({
      method: 'POST',
      path: '/v1/api-keys',
      body: JSON.stringify({ name: opts.name, scopes: opts.scopes }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    if (!w.key) throw new Error('the server created an API key but returned no secret');
    return { ...mapApiKey(w), key: w.key };
  }

  /** revoke makes the key refused from the next request on. Revoking a
   *  revoked key is not an error. A key may revoke itself. */
  async revoke(keyId: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/api-keys/${encodeURIComponent(keyId)}`,
      retriable: true,
      signal,
    });
  }
}
