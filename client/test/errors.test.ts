import { test } from 'node:test';
import assert from 'node:assert/strict';
import { SignalingError } from '../src/errors.js';
import { CloseCode, ErrorCode } from '../src/types.js';

test('fromErrorResponse maps all fields', () => {
  const err = SignalingError.fromErrorResponse({
    type: 'error-response',
    errorCode: ErrorCode.ROOM_NOT_FOUND,
    message: 'no such room',
    retryable: true,
    retryAfterMs: 250,
  });
  assert.equal(err.code, ErrorCode.ROOM_NOT_FOUND);
  assert.equal(err.message, 'no such room');
  assert.equal(err.retryable, true);
  assert.equal(err.retryAfterMs, 250);
  assert.equal(err.isErrorResponse, true);
});

test('fromCloseCode marks retryable close codes', () => {
  assert.equal(SignalingError.fromCloseCode(CloseCode.RATE_LIMITED, '').retryable, true);
  assert.equal(SignalingError.fromCloseCode(CloseCode.ROOM_EXPIRED, '').retryable, true);
  assert.equal(SignalingError.fromCloseCode(CloseCode.SERVER_SHUTTING_DOWN, '').retryable, true);
});

test('fromCloseCode marks terminal close codes non-retryable', () => {
  assert.equal(SignalingError.fromCloseCode(CloseCode.ROOM_CLOSED, '').retryable, false);
  assert.equal(SignalingError.fromCloseCode(CloseCode.REPLACED, '').retryable, false);
  assert.equal(SignalingError.fromCloseCode(CloseCode.RELEASED_AFTER_PEER_CONNECTED, '').retryable, false);
  assert.equal(SignalingError.fromCloseCode(CloseCode.PROTOCOL_ERROR, '').retryable, false);
});

test('isErrorResponse is false for close-code-derived errors', () => {
  const err = SignalingError.fromCloseCode(CloseCode.ROOM_CLOSED, 'bye');
  assert.equal(err.isErrorResponse, false);
});

test('default retryable is false', () => {
  const err = new SignalingError(0, 'x');
  assert.equal(err.retryable, false);
  assert.equal(err.retryAfterMs, undefined);
});
