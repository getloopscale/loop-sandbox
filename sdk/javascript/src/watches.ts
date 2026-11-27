import { sandboxRoute, withQuery, type HttpClient } from './http.js';
import type { CursorOptions } from './types.js';

/** A directory watch that is recording changes. */
export interface Watch {
  watchId: string;
  /** The watched directory, as the server normalized it. */
  path: string;
  recursive: boolean;
}

/** One change to a watched directory. A rename is a "rename" of the old path
 *  followed by a "create" of the new one. "overflow" means events were lost
 *  and the directory should be listed again. */
export interface FileEvent {
  type: 'create' | 'write' | 'remove' | 'rename' | 'chmod' | 'overflow';
  /** Absent in an "overflow" event. */
  path?: string;
  isDir: boolean;
}

/** A page of the events of a directory watch. */
export interface FileEvents {
  events: FileEvent[];
  /** Cursor to send as "after" in the following request. */
  next: number;
  /** True when the cursor pointed to events the sandbox no longer keeps. */
  truncated: boolean;
  /** True when the watch ended, as when its directory was removed. */
  closed: boolean;
}

interface WireFileEvent {
  type: FileEvent['type'];
  path?: string;
  is_dir: boolean;
}

/** Watches records the changes to directories of a single sandbox. Needs the
 *  capability "file_watch". */
export class Watches {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  private route(watchId?: string, suffix = ''): string {
    const watch = watchId === undefined ? '' : `/${encodeURIComponent(watchId)}`;
    return sandboxRoute(this.sandboxId, `/watches${watch}${suffix}`);
  }

  /** start begins recording the changes to a directory, and to the directories
   *  under it when recursive is true. A sandbox at its limit of watches raises
   *  ConflictError (code "too_many_watches"). */
  async start(path: string, opts: { recursive?: boolean; signal?: AbortSignal } = {}): Promise<Watch> {
    const w = await this.http.sendJSON<{ watch_id: string; path: string; recursive: boolean }>({
      method: 'POST',
      path: this.route(),
      body: JSON.stringify({ path, recursive: opts.recursive }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return { watchId: w.watch_id, path: w.path, recursive: w.recursive };
  }

  /** events reads the events after the cursor opts.after. Send the "next" of
   *  one answer as the "after" of the following call. */
  async events(watchId: string, opts: CursorOptions = {}): Promise<FileEvents> {
    const w = await this.http.sendJSON<{
      events: WireFileEvent[];
      next: number;
      truncated: boolean;
      closed: boolean;
    }>({
      method: 'GET',
      path: withQuery(this.route(watchId, '/events'), { after: opts.after, wait_ms: opts.waitMs }),
      retriable: true,
      signal: opts.signal,
    });
    return {
      events: w.events.map((e) =>
        e.path === undefined ? { type: e.type, isDir: e.is_dir } : { type: e.type, path: e.path, isDir: e.is_dir },
      ),
      next: w.next,
      truncated: w.truncated,
      closed: w.closed,
    };
  }

  /** stop ends a watch. */
  async stop(watchId: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: this.route(watchId),
      retriable: true,
      signal,
    });
  }
}
