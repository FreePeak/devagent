import { describe, expect, it } from 'vitest';
import { decodeKeys } from '../src/tui/input.js';

/**
 * FR-TUI key decoding: terminals deliver arrows/PgUp/Home as multi-byte
 * escape sequences; the v1 handler read only byte[0] so every arrow leaked a
 * stray ESC. These tests pin the whole-sequence contract.
 */

describe('decodeKeys', () => {
  it('decodes arrows as whole sequences, not stray ESC', () => {
    expect(decodeKeys('\x1b[A').keys).toEqual([{ kind: 'up' }]);
    expect(decodeKeys('\x1b[B').keys).toEqual([{ kind: 'down' }]);
    expect(decodeKeys('\x1b[C').keys).toEqual([{ kind: 'right' }]);
    expect(decodeKeys('\x1b[D').keys).toEqual([{ kind: 'left' }]);
    expect(decodeKeys('\x1b[A').pending).toBe('');
  });

  it('holds a partial sequence as pending until the rest arrives', () => {
    const a = decodeKeys('\x1b');
    expect(a.keys).toEqual([]);
    expect(a.pending).toBe('\x1b');
    const b = decodeKeys(a.pending + '[B');
    expect(b.keys).toEqual([{ kind: 'down' }]);
    expect(b.pending).toBe('');
    const c = decodeKeys('\x1b[');
    expect(c.pending).toBe('\x1b[');
  });

  it('decodes paging keys (numeric tilde + home/end finals)', () => {
    expect(decodeKeys('\x1b[5~').keys).toEqual([{ kind: 'pgup' }]);
    expect(decodeKeys('\x1b[6~').keys).toEqual([{ kind: 'pgdn' }]);
    expect(decodeKeys('\x1b[H').keys).toEqual([{ kind: 'home' }]);
    expect(decodeKeys('\x1b[F').keys).toEqual([{ kind: 'end' }]);
    expect(decodeKeys('\x1bOH').keys).toEqual([{ kind: 'home' }]);
  });

  it('decodes several keys in one chunk', () => {
    expect(decodeKeys('kq').keys).toEqual([
      { kind: 'char', ch: 'k' },
      { kind: 'char', ch: 'q' },
    ]);
    expect(decodeKeys('\x1b[Ay').keys).toEqual([{ kind: 'up' }, { kind: 'char', ch: 'y' }]);
  });

  it('decodes control bytes', () => {
    expect(decodeKeys('\r').keys).toEqual([{ kind: 'enter' }]);
    expect(decodeKeys('\n').keys).toEqual([{ kind: 'enter' }]);
    expect(decodeKeys('\t').keys).toEqual([{ kind: 'tab' }]);
    expect(decodeKeys('\x03').keys).toEqual([{ kind: 'ctrl', ch: '\x03' }]);
  });

  it('unknown escape sequence degrades to ESC + the remaining keys', () => {
    expect(decodeKeys('\x1bZj').keys).toEqual([
      { kind: 'esc' },
      { kind: 'char', ch: 'Z' },
      { kind: 'char', ch: 'j' },
    ]);
  });

  it('preserves a multibyte utf8 char as one key', () => {
    expect(decodeKeys('ü').keys).toEqual([{ kind: 'char', ch: 'ü' }]);
  });

  // 2026-09-05 keybinding incident: a tilde sequence split across chunks was
  // decoded early (SEQ_MAX=3 vs the 4-char "\x1b[5~"), and the trailing '~'
  // leaked into the next chunk as a literal character.
  it('holds a split tilde sequence pending until the ~ arrives', () => {
    const a = decodeKeys('\x1b[5');
    expect(a.keys).toEqual([]);
    expect(a.pending).toBe('\x1b[5');
    const b = decodeKeys(`${a.pending}~`);
    expect(b.keys).toEqual([{ kind: 'pgup' }]);
    expect(b.pending).toBe('');
  });

  it('flush mode degrades a stuck partial to a single Esc press', () => {
    // The caller's ESC-disambiguation timer fired: the held prefix becomes the
    // Esc key instead of stalling until another keypress arrives.
    expect(decodeKeys('\x1b', { flush: true }).keys).toEqual([{ kind: 'esc' }]);
    expect(decodeKeys('\x1b[', { flush: true }).keys).toEqual([{ kind: 'esc' }]);
    expect(decodeKeys('\x1b[5', { flush: true }).keys).toEqual([{ kind: 'esc' }]);
    expect(decodeKeys('\x1bO', { flush: true }).keys).toEqual([{ kind: 'esc' }]);
    // Without flush the partial is still held, not decoded.
    expect(decodeKeys('\x1b').pending).toBe('\x1b');
  });

  it('consumes unknown tilde sequences whole (Delete no longer leaks esc+3+~)', () => {
    // "\x1b[3~" (Delete) used to decode as Esc + '3' + '~': Esc closed the
    // overlay and '3' switched the view. Now it is one key; unknown codes drop.
    expect(decodeKeys('\x1b[3~').keys).toEqual([{ kind: 'delete' }]);
    expect(decodeKeys('\x1b[2~').keys).toEqual([]); // Insert
    expect(decodeKeys('\x1b[15~').keys).toEqual([]); // F5 — no home+'5'+'~' garbage
  });

  it('consumes unknown CSI finals whole (shift-tab no longer leaks esc+Z)', () => {
    expect(decodeKeys('\x1b[Zj').keys).toEqual([{ kind: 'char', ch: 'j' }]);
  });

  it('maps parameterized arrows by base final', () => {
    expect(decodeKeys('\x1b[1;5A').keys).toEqual([{ kind: 'up' }]);
  });
});
