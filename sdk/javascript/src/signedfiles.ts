import type { HttpClient } from './http.js';

/** SignedFiles moves one file through a signed URL made by
 *  sandbox.files.createUrl. The URL is the credential: the API key is never
 *  sent with these requests. An expired or altered URL, or one issued for the
 *  other operation, raises ForbiddenError. */
export class SignedFiles {
  constructor(private readonly http: HttpClient) {}

  // Accepts the full signed URL or only its token, the last path segment.
  private route(urlOrToken: string): string {
    if (/^https?:\/\//.test(urlOrToken)) return urlOrToken;
    return `/v1/signed-files/${encodeURIComponent(urlOrToken)}`;
  }

  /** download reads the file of a download URL as raw bytes. */
  async download(urlOrToken: string, signal?: AbortSignal): Promise<Uint8Array> {
    const res = await this.http.send({
      method: 'GET',
      path: this.route(urlOrToken),
      authenticated: false,
      retriable: true,
      signal,
    });
    return new Uint8Array(await res.arrayBuffer());
  }

  /** upload stores content as the file of an upload URL, replacing one that
   *  exists. */
  async upload(urlOrToken: string, content: string | Uint8Array, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'PUT',
      path: this.route(urlOrToken),
      authenticated: false,
      body: content as BodyInit,
      contentType: 'application/octet-stream',
      retriable: true,
      signal,
    });
  }
}
