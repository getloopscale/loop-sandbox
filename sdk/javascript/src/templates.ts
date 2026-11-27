import type { HttpClient } from './http.js';

/** An entry of the template catalog (GET /v1/templates). A built-in template
 *  has name, image, description and builtin=true; a registered one has the
 *  fields of RegisteredTemplate and an empty description. */
export interface Template {
  name: string;
  image: string;
  description: string;
  id?: string;
  builtin?: boolean;
  version?: string;
  digest?: string;
  public?: boolean;
  scanStatus?: string;
}

/** A template registered from a digest-pinned image. */
export interface RegisteredTemplate {
  id: string;
  name: string;
  image: string;
  version: string;
  digest?: string;
  public: boolean;
  scanStatus: string;
}

/** Options for registering a template. */
export interface RegisterTemplateOptions {
  name: string;
  /** Digest-pinned image reference (name@sha256:...). */
  image: string;
  version?: string;
  /** A public template must pass the vulnerability scan. */
  public?: boolean;
  signal?: AbortSignal;
}

interface WireTemplate {
  id?: string;
  name: string;
  image: string;
  description?: string;
  builtin?: boolean;
  version?: string;
  digest?: string;
  public?: boolean;
  scan_status?: string;
}

interface WireRegisteredTemplate {
  id: string;
  name: string;
  image: string;
  version: string;
  digest?: string;
  public: boolean;
  scan_status: string;
}

function mapListEntry(w: WireTemplate): Template {
  const template: Template = { name: w.name, image: w.image, description: w.description ?? '' };
  if (w.id !== undefined) template.id = w.id;
  if (w.builtin !== undefined) template.builtin = w.builtin;
  if (w.version !== undefined) template.version = w.version;
  if (w.digest !== undefined) template.digest = w.digest;
  if (w.public !== undefined) template.public = w.public;
  if (w.scan_status !== undefined) template.scanStatus = w.scan_status;
  return template;
}

function mapRegistered(w: WireRegisteredTemplate): RegisteredTemplate {
  const template: RegisteredTemplate = {
    id: w.id,
    name: w.name,
    image: w.image,
    version: w.version,
    public: w.public,
    scanStatus: w.scan_status,
  };
  if (w.digest) template.digest = w.digest;
  return template;
}

/** Templates lists the catalog of base images and registers new ones. */
export class Templates {
  constructor(private readonly http: HttpClient) {}

  /** list returns the built-in templates, the caller's own and the public ones. */
  async list(signal?: AbortSignal): Promise<Template[]> {
    const res = await this.http.sendJSON<{ templates: WireTemplate[] }>({
      method: 'GET',
      path: '/v1/templates',
      retriable: true,
      signal,
    });
    return res.templates.map(mapListEntry);
  }

  /** register adds a template from a digest-pinned image. A public template
   *  that fails the vulnerability scan raises a LoopSandboxError with status
   *  422 and code "scan_failed". */
  async register(opts: RegisterTemplateOptions): Promise<RegisteredTemplate> {
    const w = await this.http.sendJSON<WireRegisteredTemplate>({
      method: 'POST',
      path: '/v1/templates',
      body: JSON.stringify({ name: opts.name, image: opts.image, version: opts.version, public: opts.public }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return mapRegistered(w);
  }

  /** get fetches a registered template by id. */
  async get(id: string, signal?: AbortSignal): Promise<RegisteredTemplate> {
    const w = await this.http.sendJSON<WireRegisteredTemplate>({
      method: 'GET',
      path: `/v1/templates/${encodeURIComponent(id)}`,
      retriable: true,
      signal,
    });
    return mapRegistered(w);
  }
}
