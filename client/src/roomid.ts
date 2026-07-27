// Frontend room ID normalization per docs/ROOM_ID_SPEC.md §"Frontend handling".
//
// The frontend applies Crockford base32 fuzzy decoding as a per-character
// transform (case-insensitive, `O→0`, `I→1`, `L→1`) and lowercases the result,
// but does NOT validate or reject ids. Malformed ids are passed through so the
// backend remains the single source of truth for validation — see
// docs/ROOM_ID_SPEC.md §"Backend handling". This keeps the schema free to
// change on the server without forcing a frontend update.

/**
 * Normalize a user-entered room id via Crockford base32 fuzzy decoding rules.
 *
 * Applies, per character:
 *   - case-insensitive (output is always lowercase)
 *   - `O`/`o` → `0`
 *   - `I`/`i` → `1`
 *   - `L`/`l` → `1`
 *
 * Characters that are not part of the fuzzy map (e.g. `-`, `_`, digits, or
 * anything outside the base32 alphabet) are lowercased and passed through
 * unchanged. The function never throws and never rejects an id — callers
 * should send the result to the backend, which owns validation and rejection.
 *
 * See docs/ROOM_ID_SPEC.md §"Frontend handling".
 */
export function normalizeRoomId(id: string): string {
  let out = '';
  for (let i = 0; i < id.length; i++) {
    out += normalizeRoomIdChar(id[i]!);
  }
  return out;
}

/**
 * Per-character Crockford base32 fuzzy transform. Lowercases the input and
 * maps `O→0`, `I→1`, `L→1`. Any other character is lowercased and returned
 * as-is so unknown characters flow through to the backend unchanged.
 */
function normalizeRoomIdChar(ch: string): string {
  const lower = ch.toLowerCase();
  switch (lower) {
    case 'o':
      return '0';
    case 'i':
    case 'l':
      return '1';
    default:
      return lower;
  }
}
