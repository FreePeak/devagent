/**
 * Incremental frame renderer (the htop lesson): never clear the screen per
 * refresh. The v1 loop wrote `\x1b[H\x1b[2J` + the whole frame every poll —
 * a visible strobe on every 2s refresh and unusable for any faster animation.
 *
 * renderFrame diffs two line arrays and emits the smallest escape sequence
 * that turns the terminal's current content into `next`: cursor home, then
 * per line either a skip (`\x1b[1B` cursor-down — identical lines) or a
 * rewrite (content + `\x1b[K` erase-to-end-of-line), and `\x1b[J` to erase
 * leftover rows when the frame shrank. Lines are clamped to `width` visible
 * columns first so no line ever wraps (a wrapped line would desync the diff).
 */

import { visibleLen } from './viz.js';

/** Copy any leading ANSI SGR sequence at position i without burning width budget. */
function sgrAt(line: string, i: number): string | null {
  if (line[i] !== '\x1b' || line[i + 1] !== '[') return null;
  let j = i + 2;
  while (j < line.length && /[0-9;]/.test(line[j]!)) j++;
  return line[j] === 'm' ? line.slice(i, j + 1) : null;
}

/** Cut a line to `width` visible columns, ANSI-aware, appending … on overflow. */
export function clampLine(line: string, width: number, reset = '\x1b[0m'): string {
  if (visibleLen(line) <= width) return line;
  let out = '';
  let vis = 0;
  const limit = Math.max(1, width - 1);
  let i = 0;
  while (i < line.length) {
    const sgr = sgrAt(line, i);
    if (sgr !== null) {
      out += sgr;
      i += sgr.length;
      continue;
    }
    const ch = line[i]!;
    const w = ch.codePointAt(0)! > 0xffff ? 2 : 1; // ballpark: astral ≈ 2 cells
    if (vis + w > limit) break;
    out += ch;
    vis += w;
    i += ch.length;
  }
  return `${out}…${reset}`;
}

/**
 * Escape sequence transforming the screen from `prev` to `next`. `prev` null
 * (or a width change the caller detected) means full repaint without `\x1b[2J`
 * — rows not covered by `next` are still erased by the trailing `\x1b[J`.
 */
export function renderFrame(prev: string[] | null, next: string[], width: number): string {
  const lines = next.map((l) => clampLine(l, width));
  const out: string[] = ['\x1b[H'];
  let row = 0;
  for (; row < lines.length; row++) {
    if (prev && row < prev.length && clampLine(prev[row]!, width) === lines[row]) {
      out.push('\x1b[1B'); // skip an identical row without touching it
      continue;
    }
    // Rewriting the row: a leading \r guards against a wrapped earlier frame.
    out.push(`\r${lines[row]}\x1b[K`);
    if (row < lines.length - 1) out.push('\n');
  }
  if (!prev || prev.length > lines.length) out.push('\x1b[J'); // erase leftover rows
  return out.join('');
}
