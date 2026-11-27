import type { HttpClient } from './http.js';

/** The saved memory and disk state of a sandbox, from which new sandboxes start. */
export interface Snapshot {
  id: string;
  /** Unique within the tenant. */
  name: string;
  /** The sandbox the snapshot was taken from. It may no longer exist. */
  sandboxId: string;
  image?: string;
  sizeBytes: number;
  createdAt: string;
}

/** Options for taking a snapshot. */
export interface CreateSnapshotOptions {
  /** The running sandbox to save. It keeps running. */
  sandboxId: string;
  /** Lower-case letters, digits, dot, underscore and hyphen; at most 63 characters. */
  name: string;
  signal?: AbortSignal;
}

interface WireSnapshot {
  id: string;
  name: string;
  sandbox_id: string;
  image?: string;
  size_bytes: number;
  created_at: string;
}

function mapSnapshot(w: WireSnapshot): Snapshot {
  const snapshot: Snapshot = {
    id: w.id,
    name: w.name,
    sandboxId: w.sandbox_id,
    sizeBytes: w.size_bytes,
    createdAt: w.created_at,
  };
  if (w.image) snapshot.image = w.image;
  return snapshot;
}

/** Snapshots manages named snapshots. Needs the capability "snapshots".
 *  Start a sandbox from one with `client.sandboxes.create({ snapshot: id })`. */
export class Snapshots {
  constructor(private readonly http: HttpClient) {}

  /** create saves the state of a running sandbox. A name in use, or a tenant at
   *  its limit of snapshots, raises ConflictError. */
  async create(opts: CreateSnapshotOptions): Promise<Snapshot> {
    const w = await this.http.sendJSON<WireSnapshot>({
      method: 'POST',
      path: `/v1/sandboxes/${encodeURIComponent(opts.sandboxId)}/snapshots`,
      body: JSON.stringify({ name: opts.name }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return mapSnapshot(w);
  }

  /** list returns the snapshots of the tenant, oldest first. */
  async list(signal?: AbortSignal): Promise<Snapshot[]> {
    const res = await this.http.sendJSON<{ snapshots: WireSnapshot[] }>({
      method: 'GET',
      path: '/v1/snapshots',
      retriable: true,
      signal,
    });
    return res.snapshots.map(mapSnapshot);
  }

  /** get returns one snapshot. */
  async get(snapshotId: string, signal?: AbortSignal): Promise<Snapshot> {
    const w = await this.http.sendJSON<WireSnapshot>({
      method: 'GET',
      path: `/v1/snapshots/${encodeURIComponent(snapshotId)}`,
      retriable: true,
      signal,
    });
    return mapSnapshot(w);
  }

  /** delete removes a snapshot. Sandboxes already started from it keep running. */
  async delete(snapshotId: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/snapshots/${encodeURIComponent(snapshotId)}`,
      retriable: true,
      signal,
    });
  }
}
