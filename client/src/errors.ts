import { CloseCode, ErrorCode, type ErrorResponseMessage } from './types.js';

/**
 * Error thrown by the signaling client for protocol-level failures.
 *
 * The `code` field is either an {@link ErrorCode} (when the server sent an
 * `error-response`) or a {@link CloseCode} (when the socket closed without an
 * error-response). `retryable` and `retryAfterMs` mirror the server's guidance
 * and should be preferred over hard-coded behavior — see the DESIGN.md error
 * table.
 */
export class SignalingError extends Error {
  /** {@link ErrorCode} or {@link CloseCode}, or NaN for an unknown close. */
  readonly code: number;
  readonly retryable: boolean;
  readonly retryAfterMs?: number;

  constructor(
    code: number,
    message: string,
    opts: { retryable?: boolean; retryAfterMs?: number; cause?: unknown } = {},
  ) {
    super(message, { cause: opts.cause });
    this.name = 'SignalingError';
    this.code = code;
    this.retryable = opts.retryable ?? false;
    if (opts.retryAfterMs !== undefined) {
      this.retryAfterMs = opts.retryAfterMs;
    }
  }

  static fromErrorResponse(msg: ErrorResponseMessage): SignalingError {
    return new SignalingError(msg.errorCode, msg.message, {
      retryable: msg.retryable,
      retryAfterMs: msg.retryAfterMs,
    });
  }

  static fromCloseCode(code: number, reason: string): SignalingError {
    // Map well-known close codes to retryability per DESIGN.md close-code table.
    let retryable = false;
    switch (code) {
      case CloseCode.RATE_LIMITED: // 4008 — backoff, then reconnect
      case CloseCode.ROOM_EXPIRED: // 4014 — rejoin with token if still valid
      case CloseCode.SERVER_SHUTTING_DOWN: // 4300 — reconnect after reconnectAfterMs
        retryable = true;
        break;
      default:
        retryable = false;
    }
    return new SignalingError(code, `WebSocket closed: ${code} (${reason})`, { retryable });
  }

  /** True when this error originated from an `error-response`. */
  get isErrorResponse(): boolean {
    return Object.values(ErrorCode).some((c) => Number(c) === this.code);
  }
}

/**
 * Thrown by `DataChannelHandle.send()` when the channel is not open and the
 * channel's `whenClosed` policy is `'throw'`.
 *
 * Distinct from {@link SignalingError}: this is a local programming/timing
 * condition on one channel, not a room or protocol failure, and it never
 * affects the connection.
 */
export class DataChannelNotOpenError extends Error {
  readonly label: string;
  readonly readyState: RTCDataChannelState;

  constructor(label: string, readyState: RTCDataChannelState) {
    super(`Data channel "${label}" is not open (readyState: ${readyState})`);
    this.name = 'DataChannelNotOpenError';
    this.label = label;
    this.readyState = readyState;
  }
}
