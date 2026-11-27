import type { HttpClient } from './http.js';

/** The body of a health or readiness probe. */
export interface ProbeStatus {
  status: string;
}

/** Platform reaches the operational endpoints of the API. None of them needs an
 *  API key, and none is sent. */
export class Platform {
  constructor(private readonly http: HttpClient) {}

  /** health resolves when the API process is alive. */
  async health(signal?: AbortSignal): Promise<ProbeStatus> {
    return this.http.sendJSON<ProbeStatus>({
      method: 'GET',
      path: '/v1/healthz',
      authenticated: false,
      retriable: true,
      signal,
    });
  }

  /** ready resolves when the API can reach its dependencies, and raises
   *  ServerError (503, code "not_ready") when it cannot. */
  async ready(signal?: AbortSignal): Promise<ProbeStatus> {
    return this.http.sendJSON<ProbeStatus>({
      method: 'GET',
      path: '/v1/readyz',
      authenticated: false,
      retriable: true,
      signal,
    });
  }

  /** metrics returns the Prometheus text exposition. A deployment that does not
   *  expose metrics raises NotFoundError. */
  async metrics(signal?: AbortSignal): Promise<string> {
    const res = await this.http.send({
      method: 'GET',
      path: '/metrics',
      authenticated: false,
      retriable: true,
      signal,
    });
    return res.text();
  }
}
