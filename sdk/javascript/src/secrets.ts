import type { HttpClient } from './http.js';

/** A secret of the tenant. The value is write-only: the API never returns it. */
export interface Secret {
  /** Also the name of the environment variable a sandbox receives. */
  name: string;
  createdAt: string;
  updatedAt: string;
}

interface WireSecret {
  name: string;
  created_at: string;
  updated_at: string;
}

function mapSecret(w: WireSecret): Secret {
  return { name: w.name, createdAt: w.created_at, updatedAt: w.updated_at };
}

/** Secrets manages the secrets of the tenant. Needs the scope "secrets:manage".
 *  A sandbox receives a secret when it is created with the name in `secrets`:
 *  `client.sandboxes.create({ secrets: ['API_TOKEN'] })`. */
export class Secrets {
  constructor(private readonly http: HttpClient) {}

  /** put creates the secret or replaces its value. A new value reaches the next
   *  command of the sandboxes that already name the secret. */
  async put(name: string, value: string, signal?: AbortSignal): Promise<Secret> {
    const w = await this.http.sendJSON<WireSecret>({
      method: 'PUT',
      path: `/v1/secrets/${encodeURIComponent(name)}`,
      body: JSON.stringify({ value }),
      contentType: 'application/json',
      retriable: true,
      signal,
    });
    return mapSecret(w);
  }

  /** list returns the secrets ordered by name, without their values. */
  async list(signal?: AbortSignal): Promise<Secret[]> {
    const res = await this.http.sendJSON<{ secrets: WireSecret[] }>({
      method: 'GET',
      path: '/v1/secrets',
      retriable: true,
      signal,
    });
    return res.secrets.map(mapSecret);
  }

  /** delete removes a secret. Commands of a sandbox that names it then fail
   *  with ConflictError until a secret with the name exists again. */
  async delete(name: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/secrets/${encodeURIComponent(name)}`,
      retriable: true,
      signal,
    });
  }
}
