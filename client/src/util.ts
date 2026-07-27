// Small helpers shared across the client. Kept dependency-free so they can be
// exercised in unit tests without a browser or WebSocket.

/** Maximum length of an epoch string, per DESIGN.md payload limits. */
export const MAX_EPOCH_CHARS = 64;

/**
 * Generate a fresh 128-bit epoch as base64url (22 chars, no padding).
 *
 * Uses `crypto.getRandomValues` in the browser and falls back to Node's
 * `crypto` when available. The result is URL-safe per the protocol's
 * identifier encoding rule.
 */
export function generateEpoch(byteLength = 16): string {
  const bytes = randomBytes(byteLength);
  return base64url(bytes);
}

/**
 * Cryptographically strong random bytes.
 *
 * `globalThis.crypto` is present in every browser and in Node >= 18, which
 * covers every supported target, so there is no fallback path.
 */
export function randomBytes(length: number): Uint8Array {
  const g = globalThis as { crypto?: Crypto };
  if (!g.crypto?.getRandomValues) {
    throw new Error('globalThis.crypto.getRandomValues is unavailable in this environment');
  }
  return g.crypto.getRandomValues(new Uint8Array(length));
}

/** RFC 4648 base64url encoding without padding. */
export function base64url(bytes: Uint8Array): string {
  let bin = '';
  for (let i = 0; i < bytes.length; i++) {
    bin += String.fromCharCode(bytes[i]!);
  }
  const b64 = base64Encode(bin);
  return b64.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/** `btoa` is a global in every browser and in Node >= 16. */
function base64Encode(str: string): string {
  const g = globalThis as { btoa?: (s: string) => string };
  if (!g.btoa) {
    throw new Error('globalThis.btoa is unavailable in this environment');
  }
  return g.btoa(str);
}

/**
 * Full-jitter exponential backoff as described in the DESIGN.md reconnect
 * algorithm: `random(0, min(500ms * 2^attempt, 30s))`.
 *
 * @param attempt zero-based attempt counter
 * @param baseMs  base delay (default 500)
 * @param capMs   maximum delay (default 30 000)
 */
export function fullJitterBackoff(
  attempt: number,
  baseMs = 500,
  capMs = 30_000,
): number {
  const upper = Math.min(baseMs * 2 ** attempt, capMs);
  return Math.floor(Math.random() * upper);
}

/** A monotonic, per-(slot, epoch) sequence counter starting at 1. */
export class SequenceCounter {
  private value = 0;

  next(): number {
    return ++this.value;
  }

  reset(): void {
    this.value = 0;
  }

  get current(): number {
    return this.value;
  }
}

/** Type guard for "is this a plain JSON object". */
export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/** Generate a short opaque id for `requestId` correlation. */
export function generateRequestId(byteLength = 9): string {
  return base64url(randomBytes(byteLength));
}
