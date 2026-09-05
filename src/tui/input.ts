/**
 * Raw-stdin key decoding for the TUI (FR-TUI). Terminals deliver keys as byte
 * sequences: plain characters, control bytes (Ctrl+C = 0x03, Enter = 0x0d) and
 * CSI/SS3 escape sequences (arrows, PgUp/PgDn, Home/End). The v1 handler read
 * only the first byte, so every arrow key leaked a stray ESC and closed the
 * help overlay — this module consumes whole sequences instead.
 *
 * A chunk may hold several keys at once (fast j/k typing), and a sequence may
 * be split across chunks (ESC and `[A` arriving separately). decodeKeys
 * returns `pending` for a trailing partial sequence so the caller can prepend
 * it to the next chunk.
 */

export type Key =
  | { kind: 'char'; ch: string }
  | { kind: 'ctrl'; ch: string }
  | { kind: 'enter' }
  | { kind: 'esc' }
  | { kind: 'tab' }
  | { kind: 'up' }
  | { kind: 'down' }
  | { kind: 'left' }
  | { kind: 'right' }
  | { kind: 'pgup' }
  | { kind: 'pgdn' }
  | { kind: 'home' }
  | { kind: 'end' };

/** Longest escape prefix we recognize; anything else starting with ESC is ESC + noise. */
const SEQ_MAX = 3;

/** Final-byte CSI map: \x1b[<final> — single-char finals plus the ~ family. */
const CSI_FINALS: Record<string, Key> = {
  A: { kind: 'up' },
  B: { kind: 'down' },
  C: { kind: 'right' },
  D: { kind: 'left' },
  H: { kind: 'home' },
  F: { kind: 'end' },
};
const CSI_TILDE: Record<string, Key> = {
  '1': { kind: 'home' },
  '4': { kind: 'end' },
  '5': { kind: 'pgup' },
  '6': { kind: 'pgdn' },
  '7': { kind: 'home' },
  '8': { kind: 'end' },
};
const SS3_FINALS: Record<string, Key> = {
  A: { kind: 'up' },
  B: { kind: 'down' },
  C: { kind: 'right' },
  D: { kind: 'left' },
  H: { kind: 'home' },
  F: { kind: 'end' },
};

/**
 * Decode a decoded-as-utf8 stdin chunk. Never throws; unknown bytes are
 * dropped. `pending` is a trailing partial escape sequence ('' when the chunk
 * ended on a key boundary) that must be prepended to the next chunk.
 */
export function decodeKeys(chunk: string): { keys: Key[]; pending: string } {
  const keys: Key[] = [];
  let s = chunk;
  while (s.length > 0) {
    const head = s[0]!;
    if (head === '\x1b') {
      // Only a bare ESC (or ESC followed by junk) means the Esc key.
      if (s.length === 1) return { keys, pending: s };
      if (s[1] === '[' || s[1] === 'O') {
        if (s.length < SEQ_MAX) return { keys, pending: s };
        const third = s[2]!;
        const isCsi = s[1] === '[';
        // CSI numeric finals end with '~' ("\x1b[5~" = 4 chars); letter finals
        // are 3 chars ("\x1b[A"). SS3 finals are always 3 chars ("\x1bOA").
        const key = isCsi
          ? third >= '0' && third <= '9'
            ? CSI_TILDE[third]
            : CSI_FINALS[third]
          : SS3_FINALS[third];
        if (key) {
          const consumed = isCsi && third >= '0' && third <= '9' && s[3] === '~' ? 4 : SEQ_MAX;
          keys.push(key);
          s = s.slice(consumed);
        } else {
          keys.push({ kind: 'esc' });
          s = s.slice(2);
        }
        continue;
      }
      // ESC + non-sequence char (e.g. alt-j): treat as ESC, redecode the char.
      keys.push({ kind: 'esc' });
      s = s.slice(1);
      continue;
    }
    if (head === '\r' || head === '\n') {
      keys.push({ kind: 'enter' });
      s = s.slice(1);
      continue;
    }
    if (head === '\t') {
      keys.push({ kind: 'tab' });
      s = s.slice(1);
      continue;
    }
    if (head < ' ' || head === '\x7f') {
      keys.push({ kind: 'ctrl', ch: head });
      s = s.slice(1);
      continue;
    }
    // Plain (possibly multi-byte utf8) character.
    keys.push({ kind: 'char', ch: head });
    s = s.slice(1);
  }
  return { keys, pending: '' };
}
