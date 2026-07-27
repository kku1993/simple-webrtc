/**
 * Structured logger interface used throughout the client.
 *
 * Every method is optional so a partial object (or `{}`) is a valid logger and
 * call sites use optional invocation (`log.debug?.(...)`). Lives in its own
 * module so `src/rtc/` can depend on it without importing `peer-connection.ts`,
 * which would be circular.
 */
export interface Logger {
  debug?(msg: string, ctx?: Record<string, unknown>): void;
  info?(msg: string, ctx?: Record<string, unknown>): void;
  warn?(msg: string, ctx?: Record<string, unknown>): void;
  error?(msg: string, ctx?: Record<string, unknown>): void;
}

/** A logger that discards everything. */
export const NOOP_LOGGER: Logger = {};
