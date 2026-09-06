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
 * it to the next chunk — and the caller must arm a short flush timer, or a
 * lone Esc press would sit in pending forever and "Esc does nothing" until
 * the next key arrives (2026-09-05 keybinding incident #1).
 *
 * Unknown CSI sequences are consumed whole and dropped: decoding them as
 * ESC + leftover chars leaked keystrokes into the app (Delete `\x1b[3~`
 * became Esc + '3' + '~' — Esc closed the overlay and '3' switched the view;
 * incident #2).
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
  | { kind: 'end' }
  | { kind: 'delete' };

/** Runaway-sequence bound: past this many chars without a final byte, give up. */
const SEQ_MAX = 32;

/** Final-byte CSI map: \x1b[<final> (letter finals, no parameters). */
const CSI_FINALS: Record<string, Key> = {
  A: { kind: 'up' },
  B: { kind: 'down' },
  C: { kind: 'right' },
  D: { kind: 'left' },
  H: { kind: 'home' },
  F: { kind: 'end' },
};
/** Numeric tilde finals: \x1b[<n>~. Unknown n (F-keys, 2=Insert) → dropped. */
const CSI_TILDE: Record<number, Key> = {
  1: { kind: 'home' },
  3: { kind: 'delete' },
  4: { kind: 'end' },
  5: { kind: 'pgup' },
  6: { kind: 'pgdn' },
  7: { kind: 'home' },
  8: { kind: 'end' },
};
const SS3_FINALS: Record<string, Key> = {
  A: { kind: 'up' },
  B: { kind: 'down' },
  C: { kind: 'right' },
  D: { kind: 'left' },
  H: { kind: 'home' },
  F: { kind: 'end' },
};

/** One CSI sequence: parameters + final byte → key, or null (drop silently). */
function csiKey(params: string, final: string): Key | null {
  if (final === '~') {
    const n = Number.parseInt(params, 10);
    return Number.isFinite(n) ? (CSI_TILDE[n] ?? null) : null;
  }
  // Parameterized letter finals ("\x1b[1;5A" = Ctrl+Up) resolve by base final.
  return CSI_FINALS[final] ?? null;
}

/**
 * Decode a decoded-as-utf8 stdin chunk. Never throws. `pending` is a trailing
 * partial escape sequence ('' when the chunk ended on a key boundary) that
 * must be prepended to the next chunk; with `{ flush: true }` (the caller's
 * ESC-disambiguation timer fired) the partial is force-decoded to a single
 * Esc press instead of being held.
 */
export function decodeKeys(chunk: string, opts: { flush?: boolean } = {}): { keys: Key[]; pending: string } {
  const flush = opts.flush === true;
  const keys: Key[] = [];
  let s = chunk;
  while (s.length > 0) {
    const head = s[0]!;
    if (head === '\x1b') {
      // A bare trailing ESC is ambiguous (Esc key vs a sequence the terminal
      // split across chunks): hold it as pending until the caller flushes.
      if (s.length === 1) break;
      const c1 = s[1]!;
      if (c1 === '[') {
        // CSI: parameter bytes (0x30–0x3f) then intermediates (0x20–0x2f),
        // terminated by one final byte (0x40–0x7e). Scan for the final.
        let i = 2;
        while (i < s.length) {
          const c = s[i]!.charCodeAt(0);
          if (c < 0x20 || c > 0x3f) break;
          i++;
        }
        if (i >= s.length) {
          // Still mid-sequence: pending, unless it ran away (no final ever).
          if (s.length > SEQ_MAX) {
            keys.push({ kind: 'esc' });
            s = s.slice(1);
            continue;
          }
          break;
        }
        const final = s[i]!;
        if (final.charCodeAt(0) >= 0x40 && final.charCodeAt(0) <= 0x7e) {
          const key = csiKey(s.slice(2, i), final);
          if (key) keys.push(key); // unknown sequence: drop whole, no junk keys
          s = s.slice(i + 1);
          continue;
        }
        // Malformed (control byte inside the sequence): ESC, redecode the rest.
        keys.push({ kind: 'esc' });
        s = s.slice(1);
        continue;
      }
      if (c1 === 'O') {
        if (s.length < 3) break; // SS3 is always 3 chars; wait for the final
        const key = SS3_FINALS[s[2]!] ?? null;
        if (key) keys.push(key);
        s = s.slice(3);
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
  if (s.length > 0) {
    // Only an ESC-initiated partial prefix can remain here.
    if (flush) keys.push({ kind: 'esc' });
    else return { keys, pending: s };
  }
  return { keys, pending: '' };
}
