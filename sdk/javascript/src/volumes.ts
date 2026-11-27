import { withQuery, type HttpClient } from './http.js';

/** A durable volume. It is mounted at /workspace/<name> and outlives the sandbox. */
export interface Volume {
  id: string;
  name: string;
  sizeGb: number;
  /** Absent while the volume is detached. */
  sandboxId?: string;
  createdAt: string;
}

/** Options for creating a volume. */
export interface CreateVolumeOptions {
  /** The sandbox to attach the volume to. */
  sandboxId: string;
  /** Creating with the name of an existing detached volume reattaches it. */
  name: string;
  sizeGb: number;
  signal?: AbortSignal;
}

interface WireVolume {
  id: string;
  name: string;
  size_gb: number;
  sandbox_id?: string;
  created_at: string;
}

function mapVolume(w: WireVolume): Volume {
  const volume: Volume = { id: w.id, name: w.name, sizeGb: w.size_gb, createdAt: w.created_at };
  if (w.sandbox_id) volume.sandboxId = w.sandbox_id;
  return volume;
}

/** Volumes manages durable volumes. Needs the capability "volumes". */
export class Volumes {
  constructor(private readonly http: HttpClient) {}

  /** create makes a volume and attaches it to a sandbox. A sandbox that cannot
   *  take another volume raises ConflictError. */
  async create(opts: CreateVolumeOptions): Promise<Volume> {
    const w = await this.http.sendJSON<WireVolume>({
      method: 'POST',
      path: '/v1/volumes',
      body: JSON.stringify({ sandbox_id: opts.sandboxId, name: opts.name, size_gb: opts.sizeGb }),
      contentType: 'application/json',
      retriable: false,
      signal: opts.signal,
    });
    return mapVolume(w);
  }

  /** list returns the volumes attached to a sandbox. */
  async list(sandboxId: string, signal?: AbortSignal): Promise<Volume[]> {
    const res = await this.http.sendJSON<{ volumes: WireVolume[] }>({
      method: 'GET',
      path: withQuery('/v1/volumes', { sandbox_id: sandboxId }),
      retriable: true,
      signal,
    });
    return res.volumes.map(mapVolume);
  }

  /** delete removes a volume and its data. */
  async delete(volumeId: string, signal?: AbortSignal): Promise<void> {
    await this.http.send({
      method: 'DELETE',
      path: `/v1/volumes/${encodeURIComponent(volumeId)}`,
      retriable: true,
      signal,
    });
  }
}
