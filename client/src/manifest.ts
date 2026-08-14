// Client configuration manifest: shard directory.
//
// In production the signaling backend is a set of single-process shards, each
// one identified by the leading character of every room id it mints (see
// docs/ROOM_ID_SPEC.md). Clients discover those shards through a JSON manifest
// fetched from a known URL:
//
//   - the **host** always loads the manifest before creating a room and picks a
//     shard by weighted random choice, so operators can steer new rooms by
//     editing a config file rather than redeploying clients;
//   - the **guest** loads the same manifest and picks the shard that owns the
//     room id it was given, so it lands on the process that holds the room.
//
// The manifest is the client's shard directory only. ICE/TURN configuration is
// not carried here: the server mints short-lived TURN credentials and returns
// them in the handshake responses (create-room / join-room / rejoin-room), so
// credentials rotate per connection without a manifest republish. See
// docs/DESIGN.md §"CreateRoom".
//
// A manifest can come from a live URL (the production default) or from a
// static object compiled into the app (local development and tests).

import type { Logger } from './logger.js';
import { normalizeRoomId } from './roomid.js';

/**
 * Shard name that matches every room id. Used for single-endpoint deployments
 * (and by `PeerConnection`'s plain `url` option, which is sugar for a
 * one-wildcard-shard manifest).
 */
export const WILDCARD_SHARD = '*';

/** Manifest schema version this client understands. */
export const MANIFEST_VERSION = 1;

/** Default time a fetched manifest is reused before being refetched. */
export const DEFAULT_MANIFEST_TTL_MS = 60_000;

/** Default timeout for a manifest fetch. */
export const DEFAULT_MANIFEST_TIMEOUT_MS = 5_000;

/** One signaling shard. */
export interface ShardEntry {
  /**
   * The shard's room-id prefix — normally the single Crockford base32
   * character the shard is started with (`SHARD_NAME`), or {@link WILDCARD_SHARD}
   * for a shard that serves every room id.
   *
   * Matching is by longest normalized prefix, so a future multi-character
   * scheme works without a client update.
   */
  name: string;
  /** WebSocket URL of the shard's signaling endpoint (`ws://` or `wss://`). */
  url: string;
  /**
   * Relative weight for host-side room creation. Defaults to 1. A weight of 0
   * drains the shard: no new rooms are sent to it, but guests holding an
   * existing room id still reach it.
   */
  weight?: number;
}

/** The client-side config file. */
export interface SignalManifest {
  /** Schema version. Currently {@link MANIFEST_VERSION}. */
  version: number;
  /** The shard directory. Must contain at least one entry. */
  shards: ShardEntry[];
  /**
   * Arbitrary application-level settings. The client never interprets these;
   * they are parsed, carried through, and exposed via
   * `PeerConnection.manifest` for the application to read.
   */
  settings?: Record<string, unknown>;
}

/**
 * A manifest as authored, before defaults are filled in. `version` may be
 * omitted; {@link parseManifest} stamps the current {@link MANIFEST_VERSION}.
 */
export type SignalManifestInput = Omit<SignalManifest, 'version'> & { version?: number };

/** Raised when a manifest cannot be loaded, parsed, or resolved to a shard. */
export class ManifestError extends Error {
  override readonly name = 'ManifestError';

  constructor(message: string, options: { cause?: unknown } = {}) {
    super(message, { cause: options.cause });
  }
}

/** The slice of a `fetch` `Response` the loader relies on. */
export interface ManifestResponseLike {
  readonly ok: boolean;
  readonly status: number;
  json(): Promise<unknown>;
}

/** Injectable `fetch`, so the loader is testable without a network. */
export type ManifestFetch = (
  url: string,
  init?: { signal?: AbortSignal },
) => Promise<ManifestResponseLike>;

/** Load the manifest from a live URL. The production default. */
export interface RemoteManifestOptions {
  /** URL of the JSON manifest, e.g. `https://cdn.example/signal-manifest.json`. */
  url: string;
  /** How long a fetched manifest is reused. Defaults to 60s. */
  ttlMs?: number;
  /** Per-request timeout. Defaults to 5s. */
  timeoutMs?: number;
  /** Override `fetch` (mainly for tests). */
  fetch?: ManifestFetch;
  /**
   * Manifest to fall back to when the fetch fails and nothing has been cached
   * yet. Without one, a failed first load rejects the `createRoom`/`joinRoom`
   * call that triggered it.
   */
  fallback?: SignalManifestInput;
}

/** Use a manifest already in memory. For local development and tests. */
export interface StaticManifestOptions {
  static: SignalManifestInput;
}

/** Where {@link ManifestProvider} gets its manifest from. */
export type ManifestOptions = RemoteManifestOptions | StaticManifestOptions;

function isStaticOptions(o: ManifestOptions): o is StaticManifestOptions {
  return 'static' in o;
}

// ---------------------------------------------------------------------------
// Parsing / validation
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * Validate and normalize an untrusted manifest document.
 *
 * Shard names and urls are trimmed and shard names are lowercased, so a
 * manifest authored with display-cased shard names still matches normalized
 * room ids. Throws {@link ManifestError} with an operator-readable message on
 * anything malformed — unlike room ids, a manifest is authored by the operator,
 * so failing loudly at load time is better than failing at connect time.
 */
export function parseManifest(raw: unknown): SignalManifest {
  if (!isRecord(raw)) throw new ManifestError('manifest must be a JSON object');

  const version = raw['version'];
  if (version !== undefined && typeof version !== 'number') {
    throw new ManifestError('manifest.version must be a number');
  }
  if (typeof version === 'number' && version > MANIFEST_VERSION) {
    throw new ManifestError(
      `manifest.version ${version} is newer than this client supports (${MANIFEST_VERSION})`,
    );
  }

  const shardsRaw = raw['shards'];
  if (!Array.isArray(shardsRaw) || shardsRaw.length === 0) {
    throw new ManifestError('manifest.shards must be a non-empty array');
  }

  const shards: ShardEntry[] = shardsRaw.map((entry, i) => parseShard(entry, i));
  const seen = new Set<string>();
  for (const s of shards) {
    if (seen.has(s.name)) {
      throw new ManifestError(`manifest.shards has duplicate shard name '${s.name}'`);
    }
    seen.add(s.name);
  }

  const manifest: SignalManifest = {
    version: typeof version === 'number' ? version : MANIFEST_VERSION,
    shards,
  };

  const settings = raw['settings'];
  if (settings !== undefined) {
    if (!isRecord(settings)) throw new ManifestError('manifest.settings must be an object');
    manifest.settings = settings;
  }

  return manifest;
}

function parseShard(entry: unknown, i: number): ShardEntry {
  const at = `manifest.shards[${i}]`;
  if (!isRecord(entry)) throw new ManifestError(`${at} must be an object`);

  const rawName = entry['name'];
  if (typeof rawName !== 'string' || rawName.trim() === '') {
    throw new ManifestError(`${at}.name must be a non-empty string`);
  }
  const name = rawName.trim().toLowerCase();

  const rawUrl = entry['url'];
  if (typeof rawUrl !== 'string' || rawUrl.trim() === '') {
    throw new ManifestError(`${at}.url must be a non-empty string`);
  }
  const url = rawUrl.trim();
  if (!/^wss?:\/\//i.test(url)) {
    throw new ManifestError(`${at}.url must be a ws:// or wss:// URL (got '${url}')`);
  }

  const shard: ShardEntry = { name, url };

  const weight = entry['weight'];
  if (weight !== undefined) {
    if (typeof weight !== 'number' || !Number.isFinite(weight) || weight < 0) {
      throw new ManifestError(`${at}.weight must be a finite number >= 0`);
    }
    shard.weight = weight;
  }

  return shard;
}

/** Build a one-shard manifest for a deployment with a single signaling URL. */
export function singleShardManifest(url: string): SignalManifest {
  return parseManifest({
    version: MANIFEST_VERSION,
    shards: [{ name: WILDCARD_SHARD, url, weight: 1 }],
  });
}

// ---------------------------------------------------------------------------
// Shard selection
// ---------------------------------------------------------------------------

function weightOf(s: ShardEntry): number {
  return s.weight ?? 1;
}

/**
 * Pick a shard for a **new** room by weighted random choice.
 *
 * Shards with a weight of 0 are excluded, which is how an operator drains one
 * ahead of a restart: new rooms stop arriving while existing room ids keep
 * resolving through {@link shardForRoomId}.
 *
 * @param rng - returns a number in `[0, 1)`. Injectable so tests are deterministic.
 */
export function selectShard(manifest: SignalManifest, rng: () => number = Math.random): ShardEntry {
  const candidates = manifest.shards.filter((s) => weightOf(s) > 0);
  if (candidates.length === 0) {
    throw new ManifestError('manifest has no shard with a positive weight (all drained?)');
  }
  const total = candidates.reduce((sum, s) => sum + weightOf(s), 0);
  let r = rng() * total;
  for (const s of candidates) {
    r -= weightOf(s);
    if (r < 0) return s;
  }
  // Only reachable through floating-point drift at the very top of the range.
  return candidates[candidates.length - 1]!;
}

/**
 * Find the shard that owns `roomId`.
 *
 * The room id is normalized (Crockford base32 fuzzy decoding) before matching,
 * so a user-typed `TA0000` resolves the same as `ta0000`. The shard whose name
 * is the **longest** matching prefix wins; a {@link WILDCARD_SHARD} entry is
 * used only when no named shard matches.
 */
export function shardForRoomId(manifest: SignalManifest, roomId: string): ShardEntry {
  const normalized = normalizeRoomId(roomId);
  if (normalized === '') throw new ManifestError('cannot resolve a shard for an empty room id');

  let best: ShardEntry | null = null;
  for (const s of manifest.shards) {
    if (s.name === WILDCARD_SHARD) continue;
    if (!normalized.startsWith(s.name)) continue;
    if (!best || s.name.length > best.name.length) best = s;
  }
  if (best) return best;

  const wildcard = manifest.shards.find((s) => s.name === WILDCARD_SHARD);
  if (wildcard) return wildcard;

  throw new ManifestError(
    `no shard in the manifest serves room id '${normalized}' ` +
      `(known shards: ${manifest.shards.map((s) => s.name).join(', ')})`,
  );
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

function resolveFetch(): ManifestFetch {
  const f = (globalThis as { fetch?: ManifestFetch }).fetch;
  if (!f) {
    throw new ManifestError(
      'No global `fetch` found. Run in a browser or Node >= 18, or pass `manifest.fetch`.',
    );
  }
  // Bind so implementations that require a `this` of `globalThis` still work.
  return (url, init) => f.call(globalThis, url, init);
}

/**
 * Loads and caches the manifest.
 *
 * A remote manifest is fetched at most once per TTL, concurrent loads share one
 * request, and a load that fails falls back to the last good manifest (even if
 * stale) before considering the configured `fallback`. Losing the config server
 * should not take down clients that already know where the shards are.
 */
export class ManifestProvider {
  private readonly remote: RemoteManifestOptions | null;
  private readonly staticManifest: SignalManifest | null;
  private readonly doFetch: ManifestFetch | null;
  private readonly ttlMs: number;
  private readonly timeoutMs: number;
  private readonly fallback: SignalManifest | null;
  private readonly log: Logger;

  private cachedManifest: SignalManifest | null = null;
  private cachedAt = 0;
  private inFlight: Promise<SignalManifest> | null = null;

  constructor(opts: ManifestOptions, log: Logger = {}) {
    this.log = log;
    if (isStaticOptions(opts)) {
      // Parse eagerly: a bad static manifest is a programming error and should
      // surface at construction, not at `createRoom`.
      this.staticManifest = parseManifest(opts.static);
      this.remote = null;
      this.doFetch = null;
      this.ttlMs = Infinity;
      this.timeoutMs = 0;
      this.fallback = null;
      return;
    }
    if (typeof opts.url !== 'string' || opts.url.trim() === '') {
      throw new ManifestError('manifest.url must be a non-empty string');
    }
    this.staticManifest = null;
    this.remote = opts;
    this.doFetch = opts.fetch ?? null;
    this.ttlMs = opts.ttlMs ?? DEFAULT_MANIFEST_TTL_MS;
    this.timeoutMs = opts.timeoutMs ?? DEFAULT_MANIFEST_TIMEOUT_MS;
    this.fallback = opts.fallback !== undefined ? parseManifest(opts.fallback) : null;
  }

  /** The manifest URL, or null when the manifest is static. */
  get manifestUrl(): string | null {
    return this.remote?.url ?? null;
  }

  /** The last successfully loaded manifest, without triggering a load. */
  get cached(): SignalManifest | null {
    return this.staticManifest ?? this.cachedManifest;
  }

  /**
   * Return the manifest, fetching it if there is no fresh cached copy.
   *
   * @param opts.forceRefresh - ignore the TTL and refetch (no-op when static).
   */
  async get(opts: { forceRefresh?: boolean } = {}): Promise<SignalManifest> {
    if (this.staticManifest) return this.staticManifest;
    const fresh =
      this.cachedManifest !== null && Date.now() - this.cachedAt < this.ttlMs;
    if (fresh && !opts.forceRefresh) return this.cachedManifest!;
    // Concurrent callers share a single request.
    this.inFlight ??= this.load().finally(() => {
      this.inFlight = null;
    });
    return this.inFlight;
  }

  private async load(): Promise<SignalManifest> {
    const url = this.remote!.url;
    try {
      const manifest = await this.fetchManifest(url);
      this.cachedManifest = manifest;
      this.cachedAt = Date.now();
      this.log.debug?.('manifest loaded', { url, shards: manifest.shards.length });
      return manifest;
    } catch (e) {
      const message = e instanceof Error ? e.message : String(e);
      if (this.cachedManifest) {
        this.log.warn?.('manifest refresh failed; using last known manifest', { url, message });
        return this.cachedManifest;
      }
      if (this.fallback) {
        this.log.warn?.('manifest fetch failed; using fallback manifest', { url, message });
        return this.fallback;
      }
      this.log.error?.('manifest fetch failed', { url, message });
      throw e instanceof ManifestError
        ? e
        : new ManifestError(`failed to load manifest from ${url}: ${message}`, { cause: e });
    }
  }

  private async fetchManifest(url: string): Promise<SignalManifest> {
    const fetchImpl = this.doFetch ?? resolveFetch();
    const controller = this.timeoutMs > 0 ? new AbortController() : null;
    const timer =
      controller && this.timeoutMs > 0
        ? setTimeout(() => controller.abort(), this.timeoutMs)
        : null;
    try {
      const res = await fetchImpl(url, controller ? { signal: controller.signal } : undefined);
      if (!res.ok) {
        throw new ManifestError(`manifest fetch for ${url} returned HTTP ${res.status}`);
      }
      return parseManifest(await res.json());
    } finally {
      if (timer !== null) clearTimeout(timer);
    }
  }
}
