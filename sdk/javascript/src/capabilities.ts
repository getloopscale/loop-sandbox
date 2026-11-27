import type { HttpClient } from './http.js';

/** One capability of the runtime driver and whether the deployment has it. */
export interface Capability {
  /** Such as "fork", "pause", "processes" or "browser". */
  name: string;
  supported: boolean;
}

/** What the active deployment supports. */
export interface CapabilityReport {
  /** Name of the runtime driver, such as "container". */
  driver: string;
  capabilities: Capability[];
}

/** Capabilities reports what the active runtime driver supports. An operation
 *  outside that list raises NotImplementedError. */
export class Capabilities {
  constructor(private readonly http: HttpClient) {}

  /** get returns the driver name and one entry per known capability. */
  async get(signal?: AbortSignal): Promise<CapabilityReport> {
    return this.http.sendJSON<CapabilityReport>({
      method: 'GET',
      path: '/v1/capabilities',
      retriable: true,
      signal,
    });
  }
}
