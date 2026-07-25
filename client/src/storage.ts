import type { Role } from './types.js';

/**
 * The subset of room state a client must persist in `sessionStorage` to
 * survive a page reload or a server restart. See DESIGN.md "Data persistence".
 *
 * Both epochs are stored under their canonical names so a client never has to
 * translate between "my epoch" and "peer epoch" — it compares the stored value
 * against whatever the server reports for the same name.
 */
export interface RoomSession {
  roomId: string;
  role: Role;
  rejoinToken: string;
  hostEpoch: string | null;
  guestEpoch: string | null;
  /** ISO timestamp the rejoin token expires at, for surfacing to the user. */
  rejoinTokenExpiresAt?: string;
}

/**
 * Abstraction over `sessionStorage` so the client can run in Node tests and in
 * browsers. Implementations only need {@link Storage.get}, {@link Storage.set},
 * and {@link Storage.delete}.
 */
export interface SessionStore {
  get(key: string): string | null;
  set(key: string, value: string): void;
  delete(key: string): void;
}

/** Browser `sessionStorage`-backed implementation. */
export class BrowserSessionStore implements SessionStore {
  private readonly storage: Storage;

  constructor(storage: Storage = sessionStorage) {
    this.storage = storage;
  }

  get(key: string): string | null {
    return this.storage.getItem(key);
  }

  set(key: string, value: string): void {
    this.storage.setItem(key, value);
  }

  delete(key: string): void {
    this.storage.removeItem(key);
  }
}

/** In-memory implementation for Node tests and SSR. */
export class MemorySessionStore implements SessionStore {
  private readonly map = new Map<string, string>();

  get(key: string): string | null {
    return this.map.get(key) ?? null;
  }

  set(key: string, value: string): void {
    this.map.set(key, value);
  }

  delete(key: string): void {
    this.map.delete(key);
  }
}

/**
 * Typed wrapper that (de)serializes {@link RoomSession} records under a stable
 * key. One record per room id, so multiple concurrent rooms are supported.
 */
export class RoomSessionStore {
  private readonly keyPrefix: string;

  constructor(
    private readonly store: SessionStore = new MemorySessionStore(),
    keyPrefix = 'peer-client.room.',
  ) {
    this.keyPrefix = keyPrefix;
  }

  private key(roomId: string): string {
    return `${this.keyPrefix}${roomId}`;
  }

  load(roomId: string): RoomSession | null {
    const raw = this.store.get(this.key(roomId));
    if (!raw) return null;
    try {
      const parsed = JSON.parse(raw) as unknown;
      if (!isRoomSession(parsed)) return null;
      return parsed;
    } catch {
      return null;
    }
  }

  save(session: RoomSession): void {
    this.store.set(this.key(session.roomId), JSON.stringify(session));
  }

  delete(roomId: string): void {
    this.store.delete(this.key(roomId));
  }
}

function isRoomSession(v: unknown): v is RoomSession {
  if (typeof v !== 'object' || v === null) return false;
  const o = v as Record<string, unknown>;
  return (
    typeof o['roomId'] === 'string' &&
    (o['role'] === 'host' || o['role'] === 'guest') &&
    typeof o['rejoinToken'] === 'string' &&
    (o['hostEpoch'] === null || typeof o['hostEpoch'] === 'string') &&
    (o['guestEpoch'] === null || typeof o['guestEpoch'] === 'string')
  );
}
