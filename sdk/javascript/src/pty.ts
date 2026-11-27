import { LoopSandboxError } from './errors.js';
import { sandboxRoute, type HttpClient, type WebSocketLike } from './http.js';

/** Options for opening a terminal. */
export interface OpenPtyOptions {
  /** Initial window size. Both are needed for the resize to be sent. */
  rows?: number;
  cols?: number;
}

/** PtyConnection is an open terminal attached to a shell in the sandbox. */
export class PtyConnection {
  /** Settles when the terminal closes, as when the shell exits. */
  readonly closed: Promise<void>;
  private readonly listeners: ((chunk: Uint8Array) => void)[] = [];
  // Output that arrived before the first onData listener, kept so that the
  // first bytes of the shell are not lost.
  private pending: Uint8Array[] = [];

  /** Built by Pty.open once the socket is open. */
  constructor(private readonly socket: WebSocketLike) {
    socket.addEventListener('message', (event) => this.deliver(toBytes(event.data)));
    this.closed = new Promise((resolve) => socket.addEventListener('close', () => resolve()));
  }

  /** onData registers a listener for the raw bytes the terminal writes. */
  onData(listener: (chunk: Uint8Array) => void): void {
    this.listeners.push(listener);
    const buffered = this.pending;
    this.pending = [];
    for (const chunk of buffered) listener(chunk);
  }

  /** write sends raw bytes to the terminal, as typed input. */
  write(data: string | Uint8Array): void {
    // Always a binary frame: the server reads text frames as control messages.
    this.socket.send(typeof data === 'string' ? new TextEncoder().encode(data) : data);
  }

  /** resize changes the window size of the terminal. */
  resize(rows: number, cols: number): void {
    this.socket.send(JSON.stringify({ type: 'resize', rows, cols }));
  }

  /** close ends the terminal. */
  close(): void {
    this.socket.close(1000);
  }

  private deliver(chunk: Uint8Array): void {
    if (this.listeners.length === 0) {
      this.pending.push(chunk);
      return;
    }
    for (const listener of this.listeners) listener(chunk);
  }
}

function toBytes(data: unknown): Uint8Array {
  if (typeof data === 'string') return new TextEncoder().encode(data);
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (ArrayBuffer.isView(data)) return new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
  throw new LoopSandboxError('the terminal sent a frame of an unknown type', 0, 'pty_protocol');
}

/** Pty opens interactive terminals in a single sandbox, over a WebSocket.
 *  Needs the capability "pty" and a WebSocket that accepts request headers
 *  (Node 22+, or the "ws" package passed as the client option webSocket). */
export class Pty {
  constructor(
    private readonly http: HttpClient,
    private readonly sandboxId: string,
  ) {}

  /** open attaches a terminal to a new shell and resolves once it is ready.
   *  A refused connection (bad key, unknown sandbox, unsupported capability)
   *  rejects with a LoopSandboxError of code "pty_connect_failed"; a
   *  WebSocket does not expose the HTTP status of a refused upgrade. */
  open(opts: OpenPtyOptions = {}): Promise<PtyConnection> {
    return new Promise((resolve, reject) => {
      let socket: WebSocketLike;
      try {
        socket = this.http.openWebSocket(sandboxRoute(this.sandboxId, '/pty'));
      } catch (e) {
        reject(e);
        return;
      }
      socket.binaryType = 'arraybuffer';
      let opened = false;
      const fail = (): void => {
        if (!opened) reject(new LoopSandboxError('could not open the terminal', 0, 'pty_connect_failed'));
      };
      socket.addEventListener('error', fail);
      socket.addEventListener('close', fail);
      socket.addEventListener('open', () => {
        opened = true;
        const connection = new PtyConnection(socket);
        if (opts.rows !== undefined && opts.cols !== undefined) connection.resize(opts.rows, opts.cols);
        resolve(connection);
      });
    });
  }
}
