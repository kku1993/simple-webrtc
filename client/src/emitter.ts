/**
 * Minimal typed event emitter. Deliberately tiny so the client has no runtime
 * dependency on `events` (which keeps browser bundles small and avoids the
 * Node-vs-browser `EventEmitter` typing split).
 *
 * `on` and `once` return an unsubscribe function so callers (notably UI
 * frameworks such as React) can release a listener without retaining a
 * reference to the handler and calling `off` explicitly:
 *
 * ```ts
 * const off = peer.on("stream", (s) => (video.srcObject = s));
 * // later:
 * off();
 * ```
 */
export class Emitter<Events> {
  private readonly listeners = new Map<keyof Events, Set<(arg: unknown) => void>>();

  on<K extends keyof Events>(event: K, fn: (arg: Events[K]) => void): () => void {
    const set = this.listeners.get(event) ?? new Set<(arg: unknown) => void>();
    set.add(fn as (arg: unknown) => void);
    this.listeners.set(event, set);
    return () => this.off(event, fn);
  }

  off<K extends keyof Events>(event: K, fn: (arg: Events[K]) => void): this {
    this.listeners.get(event)?.delete(fn as (arg: unknown) => void);
    return this;
  }

  once<K extends keyof Events>(event: K, fn: (arg: Events[K]) => void): () => void {
    const wrapper = (arg: Events[K]): void => {
      this.off(event, wrapper);
      fn(arg);
    };
    return this.on(event, wrapper);
  }

  protected emit<K extends keyof Events>(event: K, arg: Events[K]): void {
    // Copy the set so listeners can detach while iterating.
    const set = this.listeners.get(event);
    if (!set) return;
    for (const fn of [...set]) fn(arg);
  }

  removeAllListeners(): void {
    this.listeners.clear();
  }
}
