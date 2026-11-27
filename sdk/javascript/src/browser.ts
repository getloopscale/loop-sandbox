import { sandboxRoute, type HttpClient } from './http.js';

/** One input sent to the page of the sandbox browser. */
export type BrowserInput =
  | { action: 'click'; x: number; y: number }
  | { action: 'type'; text: string }
  | { action: 'scroll'; x?: number; y?: number; dx?: number; dy?: number };

/** Browser drives the browser of a single sandbox. Needs the capability
 *  "browser". */
export class Browser {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  /** navigate loads a URL. The URL passes the egress policy first: private,
   *  loopback and cloud metadata addresses raise ValidationError. */
  async navigate(url: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'POST',
      path: sandboxRoute(this.sandboxId, '/browser/navigate'),
      body: JSON.stringify({ url }),
      contentType: 'application/json',
      retriable: false,
      signal,
    });
  }

  /** screenshot captures the current page as PNG bytes. */
  async screenshot(signal?: AbortSignal): Promise<Uint8Array> {
    const res = await this.http.send({
      method: 'GET',
      path: sandboxRoute(this.sandboxId, '/browser/screenshot'),
      retriable: true,
      signal,
    });
    return new Uint8Array(await res.arrayBuffer());
  }

  /** dom returns the HTML of the current page. */
  async dom(signal?: AbortSignal): Promise<string> {
    const res = await this.http.sendJSON<{ dom: string }>({
      method: 'GET',
      path: sandboxRoute(this.sandboxId, '/browser/dom'),
      retriable: true,
      signal,
    });
    return res.dom;
  }

  /** input sends a click, text or scroll to the page. Never retried, because a
   *  repeated input would act twice. */
  async input(input: BrowserInput, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'POST',
      path: sandboxRoute(this.sandboxId, '/browser/input'),
      body: JSON.stringify(input),
      contentType: 'application/json',
      retriable: false,
      signal,
    });
  }
}
