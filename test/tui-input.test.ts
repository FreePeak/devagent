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
});
