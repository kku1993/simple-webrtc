import { test } from 'node:test';
import assert from 'node:assert/strict';
import { getLocalizedErrorMessage, type SignalCode } from '../src/i18n.js';
import { CloseCode, ErrorCode } from '../src/types.js';

test('exact locale match is used', () => {
  const msg = getLocalizedErrorMessage(ErrorCode.ROOM_NOT_FOUND, 'zh-Hans');
  assert.equal(msg, '未找到该房间。 请重试。');
});

test('retryable error gets a retry prompt in the matched locale', () => {
  const en = getLocalizedErrorMessage(ErrorCode.RATE_LIMITED, 'en');
  assert.ok(en.endsWith(' Please try again.'));
  const fr = getLocalizedErrorMessage(ErrorCode.RATE_LIMITED, 'fr');
  assert.ok(fr.endsWith(' Veuillez réessayer.'));
  const ar = getLocalizedErrorMessage(ErrorCode.RATE_LIMITED, 'ar');
  assert.ok(ar.endsWith(' يرجى المحاولة مرة أخرى.'));
});

test('non-retryable error has no retry prompt', () => {
  const msg = getLocalizedErrorMessage(ErrorCode.ROOM_CLOSED, 'en');
  assert.equal(msg, 'The room has been closed.');
  assert.ok(!msg.includes('try again'));
});

test('falls back from specific tag to language subtag', () => {
  // `zh-Hant` is a known bundle; `zh-Hant-TW` is not, falls back to `zh-Hant`.
  const msg = getLocalizedErrorMessage(ErrorCode.ROOM_FULL, 'zh-Hant-TW');
  assert.equal(msg, '房間已滿。');
});

test('falls back from language subtag to a script-default bundle for zh', () => {
  const msg = getLocalizedErrorMessage(ErrorCode.ROOM_FULL, 'zh');
  // `zh` alone resolves to the Simplified default.
  assert.equal(msg, '房间已满。');
});

test('falls back to en when locale is unknown', () => {
  const msg = getLocalizedErrorMessage(ErrorCode.ROOM_FULL, 'ja');
  assert.equal(msg, 'The room is full.');
});

test('falls back to en when locale is empty', () => {
  const msg = getLocalizedErrorMessage(ErrorCode.ROOM_FULL, '');
  assert.equal(msg, 'The room is full.');
});

test('locale lookup is case-insensitive', () => {
  const lower = getLocalizedErrorMessage(ErrorCode.ROOM_NOT_FOUND, 'zh-hans');
  const mixed = getLocalizedErrorMessage(ErrorCode.ROOM_NOT_FOUND, 'ZH-Hans');
  assert.equal(lower, mixed);
  assert.equal(lower, '未找到该房间。 请重试。');
});

test('en-US and en-GB resolve without falling through to en', () => {
  const us = getLocalizedErrorMessage(ErrorCode.ROOM_NOT_FOUND, 'en-US');
  const gb = getLocalizedErrorMessage(ErrorCode.ROOM_NOT_FOUND, 'en-GB');
  // Both share vocabulary with en but are distinct bundles; retryable, so both
  // get the English retry prompt.
  assert.equal(us, 'The room was not found. Please try again.');
  assert.equal(gb, 'The room was not found. Please try again.');
});

test('close codes are translated', () => {
  const msg = getLocalizedErrorMessage(CloseCode.SERVER_SHUTTING_DOWN, 'de');
  assert.equal(msg, 'Der Server wird heruntergefahren. Bitte versuchen Sie es erneut.');
});

test('4200 released-after-peer-connected is not retryable', () => {
  const msg = getLocalizedErrorMessage(CloseCode.RELEASED_AFTER_PEER_CONNECTED, 'en');
  assert.ok(!msg.includes('try again'));
});

test('explicit retryable override wins over the built-in table', () => {
  // ROOM_CLOSED is non-retryable by default; forcing retryable adds the prompt.
  const forced = getLocalizedErrorMessage(ErrorCode.ROOM_CLOSED, 'en', { retryable: true });
  assert.ok(forced.endsWith(' Please try again.'));
  // RATE_LIMITED is retryable by default; forcing non-retryable drops the prompt.
  const suppressed = getLocalizedErrorMessage(ErrorCode.RATE_LIMITED, 'en', {
    retryable: false,
  });
  assert.equal(suppressed, 'You are being rate limited.');
});

test('unknown code falls back to a generic per-locale message', () => {
  // The server may add new codes in the future; callers pass them via
  // `as SignalCode`. The generic fallback handles them at runtime.
  const unknown = 9999 as SignalCode;
  const en = getLocalizedErrorMessage(unknown, 'en');
  assert.match(en, /An unexpected error occurred \(code 9999\)\./);
  const es = getLocalizedErrorMessage(unknown, 'es');
  assert.match(es, /Ocurrió un error inesperado \(código 9999\)\./);
});

test('unknown code is not retryable by default', () => {
  const msg = getLocalizedErrorMessage(9999 as SignalCode, 'en');
  assert.ok(!msg.includes('try again'));
});

test('every supported locale has a message for every known code', () => {
  const locales = [
    'en',
    'en-US',
    'en-GB',
    'zh-Hans',
    'zh-Hant',
    'fr',
    'de',
    'it',
    'ar',
    'es',
  ] as const;
  // Numeric enums are reverse-mapped, so Object.values includes the string
  // names too; keep only the numeric codes, typed as SignalCode for the call.
  const codes: SignalCode[] = [
    ...Object.values(ErrorCode),
    ...Object.values(CloseCode),
  ].filter((c): c is number => typeof c === 'number');
  for (const locale of locales) {
    for (const code of codes) {
      const msg = getLocalizedErrorMessage(code, locale, { retryable: false });
      assert.ok(
        msg.length > 0,
        `missing ${locale} message for code ${code}`,
      );
      // No generic fallback should leak for known codes.
      assert.ok(
        !/unexpected error|意外错误|意外錯誤|erreur inattendue|unerwarteter Fehler|errore imprevisto|خطأ غير متوقع|error inesperado/i.test(
          msg,
        ),
        `generic fallback leaked for ${locale} code ${code}: ${msg}`,
      );
    }
  }
});
