import type { ClientMessage, ServerMessage } from './types.js';
import { isPlainObject } from './util.js';

/**
 * The minimal slice of a `WebSocket` the transport relies on. Defining it
 * locally keeps the transport testable with a stub.
 */
export interface WebSocketLike {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  addEventListener(event: 'open', handler: () => void): void;
  addEventListener(event: 'message', handler: (ev: { data: unknown }) => void): void;
  addEventListener(event: 'close', handler: (ev: { code: number; reason: string }) => void): void;
  addEventListener(event: 'error', handler: (ev: unknown) => void): void;
  removeEventListener(event: 'open', handler: () => void): void;
  removeEventListener(event: 'message', handler: (ev: { data: unknown }) => void): void;
  removeEventListener(event: 'close', handler: (ev: { code: number; reason: string }) => void): void;
  removeEventListener(event: 'error', handler: (ev: unknown) => void): void;
}

export type WebSocketFactory = (url: string) => WebSocketLike;

/** Resolve the global `WebSocket` constructor (browsers and Node >= 22). */
function resolveWebSocketCtor(): new (url: string) => WebSocketLike {
  const Ctor = (globalThis as { WebSocket?: new (url: string) => WebSocketLike }).WebSocket;
  if (!Ctor) {
    throw new Error(
      'No global `WebSocket` constructor found. Run in a browser or Node >= 22.',
    );
  }
  return Ctor;
}

/**
 * A thin WebSocket wrapper that frames JSON and exposes a small event surface.
 *
 * It does **not** implement the signaling protocol — that lives in
 * {@link PeerConnection}. The transport only knows how to open a socket, send
 * client messages, and emit parsed server messages.
 */
export class Transport {
  private socket: WebSocketLike | null = null;
  private readonly factory: WebSocketFactory;
  private closed = false;

  constructor(factory: WebSocketFactory = (url) => new (resolveWebSocketCtor())(url)) {
    this.factory = factory;
  }

  /** Open the socket and resolve once the upgrade completes. */
  connect(url: string): Promise<void> {
    this.closed = false;
    return new Promise<void>((resolve, reject) => {
      let socket: WebSocketLike;
      try {
        socket = this.factory(url);
      } catch (e) {
        reject(e instanceof Error ? e : new Error(String(e)));
        return;
      }
      this.socket = socket;

      // Lifetime listeners (stay attached until the socket closes).
      const onMessage = (ev: { data: unknown }): void => {
        this.emitMessage(parseFrame(ev.data));
      };
      const onClose = (ev: { code: number; reason: string }): void => {
        socket.removeEventListener('message', onMessage);
        socket.removeEventListener('close', onClose);
        socket.removeEventListener('error', onError);
        this.socket = null;
        this.emitClose(ev.code, ev.reason);
      };
      const onError = (ev: unknown): void => {
        const err = ev instanceof Error ? ev : new Error('WebSocket error');
        this.emitError(err);
      };
      socket.addEventListener('message', onMessage);
      socket.addEventListener('close', onClose);
      socket.addEventListener('error', onError);

      // Connect-phase listeners (removed once the upgrade settles).
      let settled = false;
      const onOpen = (): void => {
        if (settled) return;
        settled = true;
        socket.removeEventListener('open', onOpen);
        socket.removeEventListener('error', onConnectError);
        resolve();
      };
      const onConnectError = (ev: unknown): void => {
        if (settled) return;
        settled = true;
        socket.removeEventListener('open', onOpen);
        socket.removeEventListener('error', onConnectError);
        const err = ev instanceof Error ? ev : new Error('WebSocket error during connect');
        reject(err);
      };
      socket.addEventListener('open', onOpen);
      socket.addEventListener('error', onConnectError);
    });
  }

  /** Send a client→server message. Throws if the socket is not open. */
  send(msg: ClientMessage): void {
    const s = this.socket;
    if (!s || s.readyState !== 1 /* OPEN */) {
      throw new Error('Transport is not open');
    }
    s.send(JSON.stringify(msg));
  }

  /** Close the socket. Idempotent. */
  close(code?: number, reason?: string): void {
    this.closed = true;
    this.socket?.close(code, reason);
  }

  /** True once the user explicitly closed the transport (no reconnect expected). */
  get isUserClosed(): boolean {
    return this.closed;
  }

  // --- event surface -------------------------------------------------------
  private readonly messageHandlers = new Set<(m: ServerMessage | null) => void>();
  private readonly closeHandlers = new Set<(e: { code: number; reason: string }) => void>();
  private readonly errorHandlers = new Set<(e: Error) => void>();

  onMessage(handler: (m: ServerMessage | null) => void): () => void {
    this.messageHandlers.add(handler);
    return () => this.messageHandlers.delete(handler);
  }

  onClose(handler: (e: { code: number; reason: string }) => void): () => void {
    this.closeHandlers.add(handler);
    return () => this.closeHandlers.delete(handler);
  }

  onError(handler: (e: Error) => void): () => void {
    this.errorHandlers.add(handler);
    return () => this.errorHandlers.delete(handler);
  }

  private emitMessage(m: ServerMessage | null): void {
    for (const h of [...this.messageHandlers]) h(m);
  }
  private emitClose(code: number, reason: string): void {
    for (const h of [...this.closeHandlers]) h({ code, reason });
  }
  protected emitError(e: Error): void {
    for (const h of [...this.errorHandlers]) h(e);
  }
}

/** Parse a WebSocket text frame into a server message, or null on bad JSON. */
export function parseFrame(data: unknown): ServerMessage | null {
  if (typeof data !== 'string') return null;
  let obj: unknown;
  try {
    obj = JSON.parse(data);
  } catch {
    return null;
  }
  if (!isPlainObject(obj) || typeof obj['type'] !== 'string') return null;
  return obj as unknown as ServerMessage;
}
