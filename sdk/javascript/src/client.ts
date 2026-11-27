import { ApiKeys } from './apikeys.js';
import { Capabilities } from './capabilities.js';
import { HttpClient, type ClientOptions } from './http.js';
import { Platform } from './platform.js';
import { Sandboxes } from './sandboxes.js';
import { SignedFiles } from './signedfiles.js';
import { Secrets } from './secrets.js';
import { Snapshots } from './snapshots.js';
import { Volumes } from './volumes.js';
import { Templates } from './templates.js';

/** LoopSandbox is the SDK entrypoint.
 *
 * @example
 * const client = new LoopSandbox({ baseUrl, apiKey });
 * const sandbox = await client.sandboxes.create({ image: 'busybox:latest' });
 * await sandbox.files.write('/workspace/hi.txt', 'hello');
 * const out = await sandbox.commands.run(['cat', '/workspace/hi.txt']);
 * await sandbox.destroy();
 */
export class LoopSandbox {
  /** Sandbox collection API (create, list, get, destroy, fork, pause, resume). */
  readonly sandboxes: Sandboxes;
  /** Template catalog API (list, register, get). */
  readonly templates: Templates;
  /** Durable volumes. */
  readonly volumes: Volumes;
  /** Secrets of the tenant. */
  readonly secrets: Secrets;
  /** Named snapshots of sandboxes. */
  readonly snapshots: Snapshots;
  /** API keys of the tenant. */
  readonly apiKeys: ApiKeys;
  /** What the active deployment supports. */
  readonly capabilities: Capabilities;
  /** Uploads and downloads through signed file URLs, without the API key. */
  readonly signedFiles: SignedFiles;
  /** Health, readiness and Prometheus metrics. */
  readonly platform: Platform;

  constructor(opts: ClientOptions) {
    const http = new HttpClient(opts);
    this.sandboxes = new Sandboxes(http);
    this.templates = new Templates(http);
    this.volumes = new Volumes(http);
    this.snapshots = new Snapshots(http);
    this.secrets = new Secrets(http);
    this.apiKeys = new ApiKeys(http);
    this.capabilities = new Capabilities(http);
    this.signedFiles = new SignedFiles(http);
    this.platform = new Platform(http);
  }
}
