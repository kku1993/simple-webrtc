import type { Logger } from '../logger.js';
import { DataChannelHandle } from './channel-handle.js';
import type { DataChannelSpec, ResolvedChannelSpec } from './types.js';

/** Prefix reserved for engine-owned channels. Application labels may not use it. */
export const RESERVED_PREFIX = '__sps_';

/**
 * Carries renegotiation frames after the signaling socket is released.
 *
 * Giving control traffic its own channel is a correctness fix, not tidiness:
 * when protocol frames share the application's channel, every inbound chunk has
 * to be sniffed for a control discriminator, and an application that happens to
 * send a JSON object with that field collides with the protocol.
 */
export const CONTROL_CHANNEL_LABEL = `${RESERVED_PREFIX}ctrl`;

/**
 * The default application channel, backing `peer.send()` and the `data` event.
 * Ordered and reliable.
 */
export const DEFAULT_CHANNEL_LABEL = `${RESERVED_PREFIX}data`;

/** Callbacks the manager uses to surface channel activity on the peer. */
export interface ChannelManagerHooks {
  onOpen(label: string, handle: DataChannelHandle): void;
  onClose(label: string): void;
  onMessage(label: string, data: unknown): void;
  onError(label: string, message: string, cause?: unknown): void;
  /** The remote opened a channel we had not declared. */
  onRemoteChannel(label: string, handle: DataChannelHandle): void;
}

interface ChannelEntry {
  label: string;
  spec: ResolvedChannelSpec;
  handle: DataChannelHandle;
  /** True once this entry has been given an `RTCDataChannel` in this generation. */
  bound: boolean;
  /** Detaches this manager's hooks from the handle. */
  unsubscribe: () => void;
}

/**
 * Owns every data channel on one `RTCPeerConnection`.
 *
 * Allocation rule: **the initiator creates every declared channel; the
 * responder binds by label.** This is deterministic without any stream-id
 * arithmetic, has no collision window, and makes configuration mismatch
 * structurally impossible — a channel's ordering and reliability come from the
 * initiator's `createDataChannel` call, so the responder cannot disagree about
 * them. A label declared only on the responder simply never opens, which
 * {@link ChannelManager.unboundDeclaredLabels} surfaces as a warning.
 *
 * Either side may open *dynamic* channels at runtime. SCTP splits the stream-id
 * space by DTLS role, so simultaneous opens from both peers cannot collide.
 *
 * Handles may be supplied from outside via the `handles` option. `PeerConnection`
 * uses this to keep handle identity stable across peer rebuilds: the peer is
 * thrown away on an epoch change, but the application's `channel('chat')`
 * reference is not, so the replacement manager binds new `RTCDataChannel`s into
 * the handles that already exist.
 */
export class ChannelManager {
  private readonly entries = new Map<string, ChannelEntry>();
  private readonly pc: RTCPeerConnection;
  private readonly initiator: boolean;
  private readonly log: Logger;
  private readonly hooks: ChannelManagerHooks;
  /** Externally owned handle registry, when one was supplied. */
  private readonly shared: Map<string, DataChannelHandle> | null;
  private disposed = false;

  constructor(opts: {
    pc: RTCPeerConnection;
    initiator: boolean;
    declared: Record<string, DataChannelSpec>;
    log: Logger;
    hooks: ChannelManagerHooks;
    /**
     * Handle registry to reuse and populate. When given, this manager never
     * disposes a handle — the owner outlives it.
     */
    handles?: Map<string, DataChannelHandle>;
  }) {
    this.pc = opts.pc;
    this.initiator = opts.initiator;
    this.log = opts.log;
    this.hooks = opts.hooks;
    this.shared = opts.handles ?? null;

    for (const [label, spec] of Object.entries(opts.declared)) {
      this.register(label, spec);
    }

    // We own `ondatachannel` outright, so any inbound channel is routed by our
    // own registry rather than being claimed by a single-channel implementation.
    this.pc.ondatachannel = (event: RTCDataChannelEvent): void => {
      this.adopt(event.channel);
    };

    if (this.initiator) {
      for (const entry of this.entries.values()) this.createLocal(entry);
    }
  }

  /** Every handle, declared and dynamic. */
  get all(): ReadonlyMap<string, DataChannelHandle> {
    const out = new Map<string, DataChannelHandle>();
    for (const [label, entry] of this.entries) out.set(label, entry.handle);
    return out;
  }

  /** The handle for `label`, or `undefined` if it was never declared or opened. */
  get(label: string): DataChannelHandle | undefined {
    return this.entries.get(label)?.handle;
  }

  /** The control channel handle. Always present. */
  get control(): DataChannelHandle {
    return this.entries.get(CONTROL_CHANNEL_LABEL)!.handle;
  }

  /** The default application channel handle. Always present. */
  get defaultChannel(): DataChannelHandle {
    return this.entries.get(DEFAULT_CHANNEL_LABEL)!.handle;
  }

  /**
   * Declared labels that have not received a channel. On the responder a
   * non-empty result means the initiator did not declare the same set.
   */
  get unboundDeclaredLabels(): string[] {
    return [...this.entries.values()].filter((e) => !e.bound).map((e) => e.label);
  }

  /**
   * Open a channel that was not declared at construction. Safe from either
   * side. Returns the existing handle if the label is already known.
   */
  open(label: string, spec: DataChannelSpec = {}): DataChannelHandle {
    assertUsableLabel(label);
    const existing = this.entries.get(label);
    if (existing) {
      if (!existing.bound) this.createLocal(existing);
      return existing.handle;
    }
    const entry = this.register(label, spec);
    this.createLocal(entry);
    return entry.handle;
  }

  /** Detach every channel, keeping handle identity for the next generation. */
  unbindAll(): void {
    this.pc.ondatachannel = null;
    for (const entry of this.entries.values()) {
      entry.bound = false;
      entry.handle.unbind();
    }
  }

  /**
   * Retire this manager. Hooks are always detached, so a superseded generation
   * cannot keep emitting on a shared handle. Handles themselves are disposed
   * only when this manager created them — a shared registry belongs to the
   * caller and outlives any single peer.
   */
  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.pc.ondatachannel = null;
    for (const entry of this.entries.values()) {
      entry.unsubscribe();
      if (this.shared) entry.handle.unbind();
      else entry.handle.dispose();
    }
    this.entries.clear();
  }

  // --- private -------------------------------------------------------------

  private register(label: string, spec: DataChannelSpec): ChannelEntry {
    const resolved = resolveSpec(spec);
    // Reuse a handle from the shared registry so identity survives a rebuild.
    const existing = this.shared?.get(label);
    const handle = existing ?? new DataChannelHandle(label, resolved);
    if (!existing) this.shared?.set(label, handle);
    const offs = [
      handle.on('open', () => this.hooks.onOpen(label, handle)),
      handle.on('close', () => this.hooks.onClose(label)),
      handle.on('message', (data) => this.hooks.onMessage(label, data)),
      handle.on('error', ({ message, cause }) => this.hooks.onError(label, message, cause)),
    ];
    const entry: ChannelEntry = {
      label,
      spec: resolved,
      handle,
      bound: false,
      unsubscribe: () => {
        for (const off of offs) off();
      },
    };
    this.entries.set(label, entry);
    return entry;
  }

  private createLocal(entry: ChannelEntry): void {
    if (this.disposed) return;
    try {
      const channel = this.pc.createDataChannel(entry.label, toInit(entry.spec));
      entry.bound = true;
      entry.handle.bind(channel);
      this.log.debug?.('created data channel', { label: entry.label });
    } catch (e) {
      this.hooks.onError(entry.label, 'createDataChannel failed', e);
    }
  }

  private adopt(channel: RTCDataChannel): void {
    if (this.disposed) return;
    const label = channel.label;
    const entry = this.entries.get(label);
    if (entry) {
      entry.bound = true;
      entry.handle.bind(channel);
      this.log.debug?.('bound inbound data channel', { label });
      return;
    }
    // The remote opened something we did not declare. Adopt it with a spec
    // reconstructed from the channel itself — the creating side is the
    // authority on ordering and reliability.
    const dynamic = this.register(label, specFromChannel(channel));
    dynamic.bound = true;
    dynamic.handle.bind(channel);
    this.log.info?.('adopted remote data channel', { label });
    this.hooks.onRemoteChannel(label, dynamic.handle);
  }
}

/** Fill in every default on a user-supplied spec. */
export function resolveSpec(spec: DataChannelSpec): ResolvedChannelSpec {
  if (spec.maxRetransmits !== undefined && spec.maxPacketLifeTime !== undefined) {
    throw new TypeError(
      'DataChannelSpec: maxRetransmits and maxPacketLifeTime are mutually exclusive',
    );
  }
  const ordered = spec.ordered ?? true;
  const reliable = spec.maxRetransmits === undefined && spec.maxPacketLifeTime === undefined;
  return {
    ...spec,
    ordered,
    // Buffering is right for a reliable channel and wrong for an unreliable
    // one, where a flush after reconnect delivers stale state.
    whenClosed: spec.whenClosed ?? (ordered && reliable ? 'buffer' : 'throw'),
    bufferLimit: spec.bufferLimit ?? 64,
  };
}

/** Reject labels that collide with engine-reserved names. */
export function assertUsableLabel(label: string): void {
  if (label.startsWith(RESERVED_PREFIX)) {
    throw new TypeError(
      `Data channel label "${label}" uses the reserved "${RESERVED_PREFIX}" prefix`,
    );
  }
}

function toInit(spec: ResolvedChannelSpec): RTCDataChannelInit {
  const init: RTCDataChannelInit = { ordered: spec.ordered };
  if (spec.maxRetransmits !== undefined) init.maxRetransmits = spec.maxRetransmits;
  if (spec.maxPacketLifeTime !== undefined) init.maxPacketLifeTime = spec.maxPacketLifeTime;
  if (spec.protocol !== undefined) init.protocol = spec.protocol;
  return init;
}

function specFromChannel(channel: RTCDataChannel): DataChannelSpec {
  const spec: DataChannelSpec = { ordered: channel.ordered };
  if (channel.maxRetransmits !== null) spec.maxRetransmits = channel.maxRetransmits;
  if (channel.maxPacketLifeTime !== null) spec.maxPacketLifeTime = channel.maxPacketLifeTime;
  if (channel.protocol) spec.protocol = channel.protocol;
  return spec;
}
