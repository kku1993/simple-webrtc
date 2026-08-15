// Localized, user-facing error messages for the signaling client.
//
// `getLocalizedErrorMessage(code, locale)` returns a translated string for any
// {@link ErrorCode} or {@link CloseCode}, choosing the most specific BCP 47
// locale available and falling back through the language subtag to `en`. When
// the code is retryable (per the DESIGN.md error/close-code tables), the
// message is suffixed with a per-locale "please retry" prompt in the matched
// locale's language.
//
// The client has no runtime dependencies and `src/` must not reference Node
// globals, so this module is plain data + a pure function.

import { CloseCode, ErrorCode } from './types.js';

/**
 * BCP 47 locale tag supported by {@link getLocalizedErrorMessage}. The lookup
 * tries the full tag first (e.g. `zh-Hans`), then the primary language subtag
 * (e.g. `zh`), then `en`.
 */
export type SupportedLocale =
  | 'en'
  | 'en-US'
  | 'en-GB'
  | 'zh-Hans'
  | 'zh-Hant'
  | 'fr'
  | 'de'
  | 'it'
  | 'ar'
  | 'es';

/** Union of every code {@link getLocalizedErrorMessage} knows how to translate. */
export type SignalCode = ErrorCode | CloseCode;

/**
 * A per-locale message table plus the retry prompt appended for retryable
 * codes. `messages` is typed as `Record<SignalCode, string>` so the TypeScript
 * compiler enforces an exhaustive translation for every {@link ErrorCode} and
 * {@link CloseCode} member — adding a new enum value without a translation is a
 * compile error.
 */
interface LocaleBundle {
  /** Maps an error/close code to a user-facing message (without retry prompt). */
  messages: Record<SignalCode, string>;
  /** Appended to the message when the code is retryable, e.g. " Please try again." */
  retrySuffix: string;
}

/**
 * Codes the client treats as retryable, per `docs/DESIGN.md` error and
 * close-code tables. `getLocalizedErrorMessage` uses this when the caller does
 * not pass an explicit `retryable` flag, so the retry prompt is correct even
 * when only a code is known.
 */
const RETRYABLE_CODES: ReadonlySet<SignalCode> = new Set<SignalCode>([
  ErrorCode.HANDSHAKE_TIMEOUT,
  ErrorCode.ROOM_NOT_FOUND,
  ErrorCode.TURNSTILE_REQUIRED,
  ErrorCode.TURNSTILE_INVALID,
  ErrorCode.RATE_LIMITED,
  ErrorCode.SERVER_AT_CAPACITY,
  ErrorCode.SIGNAL_BUFFER_OVERFLOW,
  CloseCode.RATE_LIMITED,
  CloseCode.ROOM_EXPIRED,
  CloseCode.SERVER_SHUTTING_DOWN,
]);

// ---------------------------------------------------------------------------
// English (base + regional variants)
// ---------------------------------------------------------------------------

const enMessages: Record<SignalCode, string> = {
  [ErrorCode.MALFORMED_MESSAGE]: 'The request was malformed.',
  [ErrorCode.UNKNOWN_MESSAGE_TYPE]: 'The server received an unknown message type.',
  [ErrorCode.PAYLOAD_TOO_LARGE]: 'The request payload was too large.',
  [ErrorCode.UNEXPECTED_STATE]: 'The request was sent at an unexpected time.',
  [ErrorCode.HANDSHAKE_TIMEOUT]: 'The handshake timed out.',
  [ErrorCode.ROOM_NOT_FOUND]: 'The room was not found.',
  [ErrorCode.ROOM_FULL]: 'The room is full.',
  [ErrorCode.ROOM_CLOSED]: 'The room has been closed.',
  [ErrorCode.ROOM_EXPIRED]: 'The room has expired.',
  [ErrorCode.INVALID_GUEST_PASSWORD]: 'The guest password is incorrect.',
  [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]:
    'Too many incorrect password attempts. The room has been closed.',
  [ErrorCode.INVALID_REJOIN_TOKEN]: 'The rejoin token is invalid.',
  [ErrorCode.TURNSTILE_REQUIRED]: 'A verification check is required.',
  [ErrorCode.TURNSTILE_INVALID]: 'The verification check failed.',
  [ErrorCode.RATE_LIMITED]: 'You are being rate limited.',
  [ErrorCode.SERVER_AT_CAPACITY]: 'The server is at capacity.',
  [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: 'The signal buffer overflowed.',
  [ErrorCode.ORIGIN_NOT_ALLOWED]: 'This origin is not allowed.',
  [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: 'The protocol version is unsupported.',
  [CloseCode.PROTOCOL_ERROR]: 'A protocol error occurred.',
  [CloseCode.POLICY_VIOLATION]: 'A policy violation occurred.',
  [CloseCode.RATE_LIMITED]: 'You are being rate limited.',
  [CloseCode.ROOM_CLOSED]: 'The room has been closed.',
  [CloseCode.ROOM_EXPIRED]: 'The room has expired.',
  [CloseCode.RELEASED_AFTER_PEER_CONNECTED]:
    'The connection was released after the peer connected.',
  [CloseCode.SERVER_SHUTTING_DOWN]: 'The server is shutting down.',
  [CloseCode.REPLACED]: 'This connection was replaced by a newer one.',
};

const en: LocaleBundle = { messages: enMessages, retrySuffix: ' Please try again.' };

// Regional English variants. Messages are identical to `en` (the vocabulary in
// these messages does not vary by region); they are declared explicitly so the
// locale lookup resolves `en-US` / `en-GB` without falling through, and so a
// future regional tweak only needs to override the differing entry.
const enUS: LocaleBundle = { messages: { ...enMessages }, retrySuffix: ' Please try again.' };
const enGB: LocaleBundle = { messages: { ...enMessages }, retrySuffix: ' Please try again.' };

// ---------------------------------------------------------------------------
// Chinese (Simplified / Han)
// ---------------------------------------------------------------------------

const zhHans: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: '请求格式错误。',
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: '服务器收到了未知的消息类型。',
    [ErrorCode.PAYLOAD_TOO_LARGE]: '请求负载过大。',
    [ErrorCode.UNEXPECTED_STATE]: '请求发送的时机不对。',
    [ErrorCode.HANDSHAKE_TIMEOUT]: '握手超时。',
    [ErrorCode.ROOM_NOT_FOUND]: '未找到该房间。',
    [ErrorCode.ROOM_FULL]: '房间已满。',
    [ErrorCode.ROOM_CLOSED]: '房间已关闭。',
    [ErrorCode.ROOM_EXPIRED]: '房间已过期。',
    [ErrorCode.INVALID_GUEST_PASSWORD]: '访客密码不正确。',
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]: '密码错误次数过多，房间已关闭。',
    [ErrorCode.INVALID_REJOIN_TOKEN]: '重新加入令牌无效。',
    [ErrorCode.TURNSTILE_REQUIRED]: '需要完成验证。',
    [ErrorCode.TURNSTILE_INVALID]: '验证未通过。',
    [ErrorCode.RATE_LIMITED]: '您的请求过于频繁。',
    [ErrorCode.SERVER_AT_CAPACITY]: '服务器当前已满。',
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: '信令缓冲区已溢出。',
    [ErrorCode.ORIGIN_NOT_ALLOWED]: '该来源不被允许。',
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: '协议版本不受支持。',
    [CloseCode.PROTOCOL_ERROR]: '发生协议错误。',
    [CloseCode.POLICY_VIOLATION]: '发生策略违规。',
    [CloseCode.RATE_LIMITED]: '您的请求过于频繁。',
    [CloseCode.ROOM_CLOSED]: '房间已关闭。',
    [CloseCode.ROOM_EXPIRED]: '房间已过期。',
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]: '对等连接建立后连接已释放。',
    [CloseCode.SERVER_SHUTTING_DOWN]: '服务器正在关闭。',
    [CloseCode.REPLACED]: '此连接已被更新的连接取代。',
  },
  retrySuffix: ' 请重试。',
};

// ---------------------------------------------------------------------------
// Chinese (Traditional / Han)
// ---------------------------------------------------------------------------

const zhHant: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: '請求格式錯誤。',
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: '伺服器收到了未知的訊息類型。',
    [ErrorCode.PAYLOAD_TOO_LARGE]: '請求負載過大。',
    [ErrorCode.UNEXPECTED_STATE]: '請求傳送的時機不對。',
    [ErrorCode.HANDSHAKE_TIMEOUT]: '握手逾時。',
    [ErrorCode.ROOM_NOT_FOUND]: '找不到該房間。',
    [ErrorCode.ROOM_FULL]: '房間已滿。',
    [ErrorCode.ROOM_CLOSED]: '房間已關閉。',
    [ErrorCode.ROOM_EXPIRED]: '房間已過期。',
    [ErrorCode.INVALID_GUEST_PASSWORD]: '訪客密碼不正確。',
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]: '密碼錯誤次數過多，房間已關閉。',
    [ErrorCode.INVALID_REJOIN_TOKEN]: '重新加入權杖無效。',
    [ErrorCode.TURNSTILE_REQUIRED]: '需要完成驗證。',
    [ErrorCode.TURNSTILE_INVALID]: '驗證未通過。',
    [ErrorCode.RATE_LIMITED]: '您的請求過於頻繁。',
    [ErrorCode.SERVER_AT_CAPACITY]: '伺服器目前已滿。',
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: '信令緩衝區已溢出。',
    [ErrorCode.ORIGIN_NOT_ALLOWED]: '該來源不被允許。',
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: '協定版本不受支援。',
    [CloseCode.PROTOCOL_ERROR]: '發生協定錯誤。',
    [CloseCode.POLICY_VIOLATION]: '發生策略違規。',
    [CloseCode.RATE_LIMITED]: '您的請求過於頻繁。',
    [CloseCode.ROOM_CLOSED]: '房間已關閉。',
    [CloseCode.ROOM_EXPIRED]: '房間已過期。',
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]: '對等連線建立後連線已釋放。',
    [CloseCode.SERVER_SHUTTING_DOWN]: '伺服器正在關閉。',
    [CloseCode.REPLACED]: '此連線已被更新的連線取代。',
  },
  retrySuffix: ' 請重試。',
};

// ---------------------------------------------------------------------------
// French
// ---------------------------------------------------------------------------

const fr: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: "La requête est mal formée.",
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: "Le serveur a reçu un type de message inconnu.",
    [ErrorCode.PAYLOAD_TOO_LARGE]: "La charge utile de la requête est trop grande.",
    [ErrorCode.UNEXPECTED_STATE]: "La requête a été envoyée au mauvais moment.",
    [ErrorCode.HANDSHAKE_TIMEOUT]: "La poignée de main a expiré.",
    [ErrorCode.ROOM_NOT_FOUND]: "Le salon est introuvable.",
    [ErrorCode.ROOM_FULL]: "Le salon est plein.",
    [ErrorCode.ROOM_CLOSED]: "Le salon a été fermé.",
    [ErrorCode.ROOM_EXPIRED]: "Le salon a expiré.",
    [ErrorCode.INVALID_GUEST_PASSWORD]: "Le mot de passe invité est incorrect.",
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]:
      "Trop de tentatives de mot de passe incorrectes. Le salon a été fermé.",
    [ErrorCode.INVALID_REJOIN_TOKEN]: "Le jeton de reconnexion est invalide.",
    [ErrorCode.TURNSTILE_REQUIRED]: "Une vérification est requise.",
    [ErrorCode.TURNSTILE_INVALID]: "La vérification a échoué.",
    [ErrorCode.RATE_LIMITED]: "Vous êtes limité par le débit.",
    [ErrorCode.SERVER_AT_CAPACITY]: "Le serveur est saturé.",
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: "Le tampon de signalisation a débordé.",
    [ErrorCode.ORIGIN_NOT_ALLOWED]: "Cette origine n'est pas autorisée.",
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: "La version du protocole n'est pas prise en charge.",
    [CloseCode.PROTOCOL_ERROR]: "Une erreur de protocole s'est produite.",
    [CloseCode.POLICY_VIOLATION]: "Une violation de politique s'est produite.",
    [CloseCode.RATE_LIMITED]: "Vous êtes limité par le débit.",
    [CloseCode.ROOM_CLOSED]: "Le salon a été fermé.",
    [CloseCode.ROOM_EXPIRED]: "Le salon a expiré.",
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]:
      "La connexion a été libérée après l'établissement de la connexion pair.",
    [CloseCode.SERVER_SHUTTING_DOWN]: "Le serveur est en cours d'arrêt.",
    [CloseCode.REPLACED]: "Cette connexion a été remplacée par une plus récente.",
  },
  retrySuffix: ' Veuillez réessayer.',
};

// ---------------------------------------------------------------------------
// German
// ---------------------------------------------------------------------------

const de: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: 'Die Anfrage ist fehlerhaft.',
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: 'Der Server hat einen unbekannten Nachrichtentyp erhalten.',
    [ErrorCode.PAYLOAD_TOO_LARGE]: 'Die Anfragedaten sind zu groß.',
    [ErrorCode.UNEXPECTED_STATE]: 'Die Anfrage wurde zum falschen Zeitpunkt gesendet.',
    [ErrorCode.HANDSHAKE_TIMEOUT]: 'Der Handshake hat die Zeit überschritten.',
    [ErrorCode.ROOM_NOT_FOUND]: 'Der Raum wurde nicht gefunden.',
    [ErrorCode.ROOM_FULL]: 'Der Raum ist voll.',
    [ErrorCode.ROOM_CLOSED]: 'Der Raum wurde geschlossen.',
    [ErrorCode.ROOM_EXPIRED]: 'Der Raum ist abgelaufen.',
    [ErrorCode.INVALID_GUEST_PASSWORD]: 'Das Gastpasswort ist falsch.',
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]:
      'Zu viele falsche Passwortversuche. Der Raum wurde geschlossen.',
    [ErrorCode.INVALID_REJOIN_TOKEN]: 'Das Rejoin-Token ist ungültig.',
    [ErrorCode.TURNSTILE_REQUIRED]: 'Eine Verifizierung ist erforderlich.',
    [ErrorCode.TURNSTILE_INVALID]: 'Die Verifizierung ist fehlgeschlagen.',
    [ErrorCode.RATE_LIMITED]: 'Sie werden rate-limited.',
    [ErrorCode.SERVER_AT_CAPACITY]: 'Der Server ist ausgelastet.',
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: 'Der Signalpuffer ist übergelaufen.',
    [ErrorCode.ORIGIN_NOT_ALLOWED]: 'Diese Herkunft ist nicht erlaubt.',
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: 'Die Protokollversion wird nicht unterstützt.',
    [CloseCode.PROTOCOL_ERROR]: 'Ein Protokollfehler ist aufgetreten.',
    [CloseCode.POLICY_VIOLATION]: 'Eine Richtlinienverletzung ist aufgetreten.',
    [CloseCode.RATE_LIMITED]: 'Sie werden rate-limited.',
    [CloseCode.ROOM_CLOSED]: 'Der Raum wurde geschlossen.',
    [CloseCode.ROOM_EXPIRED]: 'Der Raum ist abgelaufen.',
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]:
      'Die Verbindung wurde nach dem Aufbau der Peer-Verbindung freigegeben.',
    [CloseCode.SERVER_SHUTTING_DOWN]: 'Der Server wird heruntergefahren.',
    [CloseCode.REPLACED]: 'Diese Verbindung wurde durch eine neuere ersetzt.',
  },
  retrySuffix: ' Bitte versuchen Sie es erneut.',
};

// ---------------------------------------------------------------------------
// Italian
// ---------------------------------------------------------------------------

const it: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: 'La richiesta è malformata.',
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: 'Il server ha ricevuto un tipo di messaggio sconosciuto.',
    [ErrorCode.PAYLOAD_TOO_LARGE]: 'Il payload della richiesta è troppo grande.',
    [ErrorCode.UNEXPECTED_STATE]: 'La richiesta è stata inviata in un momento non previsto.',
    [ErrorCode.HANDSHAKE_TIMEOUT]: 'L\'handshake è scaduto.',
    [ErrorCode.ROOM_NOT_FOUND]: 'La stanza non è stata trovata.',
    [ErrorCode.ROOM_FULL]: 'La stanza è piena.',
    [ErrorCode.ROOM_CLOSED]: 'La stanza è stata chiusa.',
    [ErrorCode.ROOM_EXPIRED]: 'La stanza è scaduta.',
    [ErrorCode.INVALID_GUEST_PASSWORD]: 'La password dell\'ospite non è corretta.',
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]:
      'Troppi tentativi di password errati. La stanza è stata chiusa.',
    [ErrorCode.INVALID_REJOIN_TOKEN]: 'Il token di rientro non è valido.',
    [ErrorCode.TURNSTILE_REQUIRED]: 'È richiesta una verifica.',
    [ErrorCode.TURNSTILE_INVALID]: 'La verifica non è riuscita.',
    [ErrorCode.RATE_LIMITED]: 'Sei limitato dalla frequenza delle richieste.',
    [ErrorCode.SERVER_AT_CAPACITY]: 'Il server è al massimo della capacità.',
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: 'Il buffer dei segnali è traboccato.',
    [ErrorCode.ORIGIN_NOT_ALLOWED]: 'Questa origine non è consentita.',
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: 'La versione del protocollo non è supportata.',
    [CloseCode.PROTOCOL_ERROR]: 'Si è verificato un errore di protocollo.',
    [CloseCode.POLICY_VIOLATION]: 'Si è verificata una violazione delle regole.',
    [CloseCode.RATE_LIMITED]: 'Sei limitato dalla frequenza delle richieste.',
    [CloseCode.ROOM_CLOSED]: 'La stanza è stata chiusa.',
    [CloseCode.ROOM_EXPIRED]: 'La stanza è scaduta.',
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]:
      'La connessione è stata rilasciata dopo la connessione del peer.',
    [CloseCode.SERVER_SHUTTING_DOWN]: 'Il server si sta spegnendo.',
    [CloseCode.REPLACED]: 'Questa connessione è stata sostituita da una più recente.',
  },
  retrySuffix: ' Riprova.',
};

// ---------------------------------------------------------------------------
// Arabic
// ---------------------------------------------------------------------------

const ar: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: 'الطلب غير صالح.',
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: 'استقبل الخادم نوع رسالة غير معروف.',
    [ErrorCode.PAYLOAD_TOO_LARGE]: 'حمولة الطلب كبيرة جدًا.',
    [ErrorCode.UNEXPECTED_STATE]: 'تم إرسال الطلب في وقت غير متوقع.',
    [ErrorCode.HANDSHAKE_TIMEOUT]: 'انتهت مهلة المصافحة.',
    [ErrorCode.ROOM_NOT_FOUND]: 'لم يتم العثور على الغرفة.',
    [ErrorCode.ROOM_FULL]: 'الغرفة ممتلئة.',
    [ErrorCode.ROOM_CLOSED]: 'تم إغلاق الغرفة.',
    [ErrorCode.ROOM_EXPIRED]: 'انتهت صلاحية الغرفة.',
    [ErrorCode.INVALID_GUEST_PASSWORD]: 'كلمة مرور الضيف غير صحيحة.',
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]:
      'محاولات كلمة مرور خاطئة كثيرة جدًا. تم إغلاق الغرفة.',
    [ErrorCode.INVALID_REJOIN_TOKEN]: 'رمز إعادة الانضمام غير صالح.',
    [ErrorCode.TURNSTILE_REQUIRED]: 'مطلوب إجراء تحقق.',
    [ErrorCode.TURNSTILE_INVALID]: 'فشل التحقق.',
    [ErrorCode.RATE_LIMITED]: 'أنت خاضع لحد معدل الطلبات.',
    [ErrorCode.SERVER_AT_CAPACITY]: 'الخادم في طاقته القصوى.',
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: 'تجاوز سعة المخزن المؤقت للإشارات.',
    [ErrorCode.ORIGIN_NOT_ALLOWED]: 'هذا المصدر غير مسموح به.',
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: 'إصدار البروتوكول غير مدعوم.',
    [CloseCode.PROTOCOL_ERROR]: 'حدث خطأ في البروتوكول.',
    [CloseCode.POLICY_VIOLATION]: 'حدث انتهاك للسياسة.',
    [CloseCode.RATE_LIMITED]: 'أنت خاضع لحد معدل الطلبات.',
    [CloseCode.ROOM_CLOSED]: 'تم إغلاق الغرفة.',
    [CloseCode.ROOM_EXPIRED]: 'انتهت صلاحية الغرفة.',
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]:
      'تم تحرير الاتصال بعد اتصال النظير.',
    [CloseCode.SERVER_SHUTTING_DOWN]: 'الخادم قيد الإيقاف.',
    [CloseCode.REPLACED]: 'تم استبدال هذا الاتصال باتصال أحدث.',
  },
  retrySuffix: ' يرجى المحاولة مرة أخرى.',
};

// ---------------------------------------------------------------------------
// Spanish
// ---------------------------------------------------------------------------

const es: LocaleBundle = {
  messages: {
    [ErrorCode.MALFORMED_MESSAGE]: 'La solicitud está mal formada.',
    [ErrorCode.UNKNOWN_MESSAGE_TYPE]: 'El servidor recibió un tipo de mensaje desconocido.',
    [ErrorCode.PAYLOAD_TOO_LARGE]: 'La carga de la solicitud es demasiado grande.',
    [ErrorCode.UNEXPECTED_STATE]: 'La solicitud se envió en un momento inesperado.',
    [ErrorCode.HANDSHAKE_TIMEOUT]: 'El handshake ha expirado.',
    [ErrorCode.ROOM_NOT_FOUND]: 'No se encontró la sala.',
    [ErrorCode.ROOM_FULL]: 'La sala está llena.',
    [ErrorCode.ROOM_CLOSED]: 'La sala ha sido cerrada.',
    [ErrorCode.ROOM_EXPIRED]: 'La sala ha expirado.',
    [ErrorCode.INVALID_GUEST_PASSWORD]: 'La contraseña de invitado es incorrecta.',
    [ErrorCode.TOO_MANY_PASSWORD_ATTEMPTS]:
      'Demasiados intentos de contraseña incorrectos. La sala ha sido cerrada.',
    [ErrorCode.INVALID_REJOIN_TOKEN]: 'El token de reincorporación no es válido.',
    [ErrorCode.TURNSTILE_REQUIRED]: 'Se requiere una verificación.',
    [ErrorCode.TURNSTILE_INVALID]: 'La verificación ha fallado.',
    [ErrorCode.RATE_LIMITED]: 'Estás limitado por la frecuencia de solicitudes.',
    [ErrorCode.SERVER_AT_CAPACITY]: 'El servidor está al máximo de su capacidad.',
    [ErrorCode.SIGNAL_BUFFER_OVERFLOW]: 'El búfer de señalización se ha desbordado.',
    [ErrorCode.ORIGIN_NOT_ALLOWED]: 'Este origen no está permitido.',
    [ErrorCode.UNSUPPORTED_PROTOCOL_VERSION]: 'La versión del protocolo no es compatible.',
    [CloseCode.PROTOCOL_ERROR]: 'Se produjo un error de protocolo.',
    [CloseCode.POLICY_VIOLATION]: 'Se produjo una violación de política.',
    [CloseCode.RATE_LIMITED]: 'Estás limitado por la frecuencia de solicitudes.',
    [CloseCode.ROOM_CLOSED]: 'La sala ha sido cerrada.',
    [CloseCode.ROOM_EXPIRED]: 'La sala ha expirado.',
    [CloseCode.RELEASED_AFTER_PEER_CONNECTED]:
      'La conexión se liberó tras la conexión del par.',
    [CloseCode.SERVER_SHUTTING_DOWN]: 'El servidor se está apagando.',
    [CloseCode.REPLACED]: 'Esta conexión fue reemplazada por una más reciente.',
  },
  retrySuffix: ' Inténtalo de nuevo.',
};

// ---------------------------------------------------------------------------
// Lookup table + resolution
// ---------------------------------------------------------------------------

const BUNDLES: Record<string, LocaleBundle> = {
  en,
  'en-US': enUS,
  'en-GB': enGB,
  'zh-Hans': zhHans,
  'zh-Hant': zhHant,
  fr,
  de,
  it,
  ar,
  es,
};

// Case-normalized lookup so input tags can be matched case-insensitively
// (e.g. `zh-hant` → the `zh-Hant` bundle) without mutating the canonical keys.
const BUNDLES_BY_LOWER_TAG: Record<string, LocaleBundle> = Object.fromEntries(
  Object.entries(BUNDLES).map(([tag, bundle]) => [tag.toLowerCase(), bundle]),
);

/**
 * Resolve a BCP 47 tag to the best available {@link LocaleBundle}.
 *
 * Tries the full tag (e.g. `zh-Hant-TW`), then progressively strips trailing
 * subtags (`zh-Hant`, then `zh`), then falls back to `en`. Comparison is
 * case-insensitive, so `zh-hans`, `ZH-Hans`, and `en-us` all resolve. For a
 * bare `zh` with no script subtag, the Simplified bundle is used as the
 * Chinese default. Returns the `en` bundle as the final fallback.
 */
function resolveBundle(locale: string): LocaleBundle {
  const tag = locale.trim();
  if (!tag) return en;
  const lower = tag.toLowerCase();
  // Try the full tag, then strip trailing subtags one at a time.
  const parts = lower.split('-');
  for (let i = parts.length; i > 0; i--) {
    const candidate = parts.slice(0, i).join('-');
    const bundle = BUNDLES_BY_LOWER_TAG[candidate];
    if (bundle) return bundle;
  }
  // A bare `zh` with no script subtag resolves to the Simplified default.
  if (parts[0] === 'zh') return zhHans;
  return en;
}

export interface LocalizedMessageOptions {
  /**
   * Override the retryability decision. When omitted, retryability is derived
   * from the built-in {@link RETRYABLE_CODES} table, which mirrors the
   * `docs/DESIGN.md` error and close-code tables. Pass the `retryable` field
   * of a {@link SignalingError} when you have one so server guidance wins.
   */
  retryable?: boolean;
}

/**
 * Returns a localized, user-facing message for the given error or close code.
 *
 * @param code    An {@link ErrorCode} or {@link CloseCode}. The type is
 *                `SignalCode` (= `ErrorCode | CloseCode`) so the compiler
 *                guides callers toward known codes; the message tables are
 *                `Record<SignalCode, string>`, so adding a new enum member
 *                without a translation is a compile error. Unknown numeric
 *                codes the server may add in the future can be passed via
 *                `code as SignalCode` and fall back to a generic message.
 * @param locale  A BCP 47 language tag, e.g. `zh-Hans`, `en-GB`, `fr`.
 *                Resolution: full tag → primary language subtag → `en`.
 * @param opts    Optional overrides (e.g. `retryable` from a `SignalingError`).
 *
 * When the code is retryable, the matched locale's retry prompt is appended to
 * the message. Unknown codes fall back to a generic per-locale "an unexpected
 * error occurred" message (with the retry prompt only when retryable).
 */
export function getLocalizedErrorMessage(
  code: SignalCode,
  locale: string,
  opts: LocalizedMessageOptions = {},
): string {
  const bundle = resolveBundle(locale);
  const retryable = opts.retryable ?? RETRYABLE_CODES.has(code);
  const base = bundle.messages[code];
  const text = base ?? genericMessage(bundle, code);
  return retryable ? text + bundle.retrySuffix : text;
}

/** Per-locale generic fallback for codes not present in the message table. */
function genericMessage(bundle: LocaleBundle, code: SignalCode): string {
  // The generic strings are keyed off the retry suffix's locale by reusing the
  // same bundle. We keep one generic string per language inline to avoid a
  // second table; `en` is the safe default for any bundle lacking an entry.
  const generics = new Map<string, string>([
    ['en', `An unexpected error occurred (code ${code}).`],
    ['en-US', `An unexpected error occurred (code ${code}).`],
    ['en-GB', `An unexpected error occurred (code ${code}).`],
    ['zh-Hans', `发生意外错误（代码 ${code}）。`],
    ['zh-Hant', `發生意外錯誤（代碼 ${code}）。`],
    ['fr', `Une erreur inattendue s'est produite (code ${code}).`],
    ['de', `Ein unerwarteter Fehler ist aufgetreten (Code ${code}).`],
    ['it', `Si è verificato un errore imprevisto (codice ${code}).`],
    ['ar', `حدث خطأ غير متوقع (الرمز ${code}).`],
    ['es', `Ocurrió un error inesperado (código ${code}).`],
  ]);
  // Find the bundle's key in BUNDLES to pick the matching generic string.
  const key = Object.keys(BUNDLES).find((k) => BUNDLES[k] === bundle);
  return (key && generics.get(key)) ?? generics.get('en')!;
}
