import { encodeFilePath, sandboxRoute, type HttpClient } from './http.js';
import type { ArtifactUrl, FileEntry, FileInfo, SignedFileUrl } from './types.js';

/** Files is the file API scoped to a single sandbox. All operations are confined
 *  to /workspace by the server; paths outside it are rejected server-side. */
export class Files {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  // The server route captures {path...} and re-prefixes it with '/', so we strip
  // leading slashes to build "/v1/.../files/workspace/x", not "//workspace/x". The
  // sandbox id is encoded (defense-in-depth: it may be caller-supplied).
  private route(path: string, resource = 'files'): string {
    return sandboxRoute(this.sandboxId, `/${resource}/${encodeFilePath(path)}`);
  }

  /** write uploads content to path (idempotent — safe to retry). */
  async write(path: string, content: string | Uint8Array, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'PUT',
      path: this.route(path),
      // string and Uint8Array are both valid BodyInit at runtime; the cast bridges
      // a TS lib generic (Uint8Array<ArrayBufferLike>) mismatch only.
      body: content as BodyInit,
      contentType: 'application/octet-stream',
      retriable: true,
      signal,
    });
  }

  /** read downloads path as UTF-8 text. Use readBytes for binary content. */
  async read(path: string, signal?: AbortSignal): Promise<string> {
    const res = await this.http.send({
      method: 'GET',
      path: this.route(path),
      retriable: true,
      signal,
    });
    return res.text();
  }

  /** readBytes downloads path as raw bytes (binary-safe — no UTF-8 decode). */
  async readBytes(path: string, signal?: AbortSignal): Promise<Uint8Array> {
    const res = await this.http.send({
      method: 'GET',
      path: this.route(path),
      retriable: true,
      signal,
    });
    return new Uint8Array(await res.arrayBuffer());
  }

  /** delete removes path (idempotent). */
  async delete(path: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: this.route(path),
      retriable: true,
      signal,
    });
  }

  /** list returns the entries of a directory (default /workspace). */
  async list(dir = '/workspace', signal?: AbortSignal): Promise<FileEntry[]> {
    const res = await this.http.sendJSON<{ files: WireFileEntry[] }>({
      method: 'GET',
      path: `/v1/sandboxes/${encodeURIComponent(this.sandboxId)}/files?path=${encodeURIComponent(dir)}`,
      retriable: true,
      signal,
    });
    return res.files.map((f) => ({ name: f.name, size: f.size, isDir: f.is_dir }));
  }

  /** info describes a file or directory. A symbolic link is described as the
   *  link itself. Needs the capability "file_ops". */
  async info(path: string, signal?: AbortSignal): Promise<FileInfo> {
    const w = await this.http.sendJSON<WireFileInfo>({
      method: 'GET',
      path: this.route(path, 'file-info'),
      retriable: true,
      signal,
    });
    return { name: w.name, path: w.path, size: w.size, isDir: w.is_dir, mode: w.mode, modifiedAt: w.modified_at };
  }

  /** makeDir creates a directory and any missing parent. A directory that
   *  already exists is not an error. Needs the capability "file_ops". */
  async makeDir(path: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'PUT',
      path: this.route(path, 'directories'),
      retriable: true,
      signal,
    });
  }

  /** move renames a file or directory and creates the parents of the
   *  destination. An existing destination raises ConflictError (code
   *  "already_exists") unless overwrite is true. Needs the capability "file_ops". */
  async move(from: string, to: string, opts: { overwrite?: boolean; signal?: AbortSignal } = {}): Promise<void> {
    await this.http.send({
      method: 'POST',
      path: sandboxRoute(this.sandboxId, '/file-moves'),
      body: JSON.stringify({ from, to, overwrite: opts.overwrite }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
  }

  /** createUrl returns a signed URL that uploads or downloads one file without
   *  an API key until it expires (default 300 seconds, at most 3600). Use it
   *  with client.signedFiles or any HTTP client. */
  async createUrl(
    path: string,
    operation: 'upload' | 'download',
    opts: { expiresInSeconds?: number; signal?: AbortSignal } = {},
  ): Promise<SignedFileUrl> {
    const w = await this.http.sendJSON<{ url: string; method: 'GET' | 'PUT'; expires_at: string }>({
      method: 'POST',
      path: sandboxRoute(this.sandboxId, '/file-urls'),
      body: JSON.stringify({ path, operation, expires_in_seconds: opts.expiresInSeconds }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return { url: w.url, method: w.method, expiresAt: w.expires_at };
  }

  /** artifactUrl copies the file to object storage and returns a time-limited
   *  download URL. Raises NotImplementedError when the deployment has no
   *  object storage. */
  async artifactUrl(path: string, signal?: AbortSignal): Promise<ArtifactUrl> {
    const w = await this.http.sendJSON<{ url: string; expires_in_seconds: number }>({
      method: 'GET',
      path: this.route(path, 'artifact-url'),
      retriable: true,
      signal,
    });
    return { url: w.url, expiresInSeconds: w.expires_in_seconds };
  }
}

interface WireFileInfo {
  name: string;
  path: string;
  size: number;
  is_dir: boolean;
  mode: string;
  modified_at: string;
}

interface WireFileEntry {
  name: string;
  size: number;
  is_dir: boolean;
}
