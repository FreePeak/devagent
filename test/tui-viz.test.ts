import { describe, expect, it } from 'vitest';
import { formatLogLine, meterBar, parseLogLine, sparkline, visibleLen } from '../src/tui/viz.js';
import { clampLine, renderFrame } from '../src/tui/frame.js';

/** Visual primitives (pilot sparkline, htop meter) + log-line formatting. */

describe('sparkline', () => {
  it('empty for no samples; flat baseline for an all-zero series', () => {
    expect(sparkline([])).toBe('');
    expect(sparkline([0, 0, 0])).toBe('▁▁▁');
  });
  it('scales to the series max', () => {
    expect(sparkline([0, 4])).toBe('▁█');
    expect(sparkline([1, 2, 3])).toBe('▃▆█');
  });
});

describe('meterBar', () => {
  it('fills proportionally', () => {
    expect(meterBar(5, 10, 10)).toBe('█████░░░░░');
  });
  it('never lights up on zero/negative totals or overfills', () => {
    expect(meterBar(0, 0, 5)).toBe('░░░░░');
    expect(meterBar(-1, 5, 5)).toBe('░░░░░');
    expect(meterBar(12, 6, 6)).toBe('██████');
  });
});

describe('parseLogLine / formatLogLine', () => {
  it('parses the runs/*.jsonl shape', () => {
    const l = parseLogLine(
      JSON.stringify({ ts: '2026-09-05T10:00:00.000Z', level: 'warn', stage: 'clarify', runId: 'r1', message: 'm' }),
    );
    expect(l).toMatchObject({ level: 'warn', stage: 'clarify', runId: 'r1', message: 'm' });
    expect(l.raw).toBeUndefined();
  });
  it('degrades malformed and non-JSON lines to raw instead of throwing', () => {
    expect(parseLogLine('oops')).toMatchObject({ message: 'oops', raw: true });
    expect(parseLogLine('{oops')).toMatchObject({ raw: true });
  });
  it('formats within width with level and stage columns', () => {
    const out = formatLogLine(
      parseLogLine(JSON.stringify({ ts: '2026-09-05T10:00:00.000Z', level: 'warn', stage: 'clarify', message: 'Insufficient specification' })),
      100,
    );
    expect(visibleLen(out)).toBeLessThanOrEqual(100);
    expect(out).toContain('warn');
    expect(out).toContain('clarify');
    expect(out).toContain('Insufficient specification');
    expect(out).toContain('\x1b[33m'); // warn = yellow
  });
  it('truncates long messages to the width budget', () => {
    const out = formatLogLine(parseLogLine(JSON.stringify({ level: 'info', message: 'x'.repeat(200) })), 60);
    expect(visibleLen(out)).toBeLessThanOrEqual(60);
    expect(out).toContain('…');
  });
});

describe('clampLine', () => {
  it('keeps short lines untouched', () => {
    expect(clampLine('abc', 80)).toBe('abc');
  });
  it('cuts to width while preserving ANSI codes, ending with …', () => {
    const line = `\x1b[31m${'a'.repeat(200)}\x1b[0m`;
    const out = clampLine(line, 80);
    expect(visibleLen(out)).toBeLessThanOrEqual(80);
    expect(out.startsWith('\x1b[31m')).toBe(true);
    expect(out).toContain('…');
  });
});

describe('renderFrame (flicker-free incremental redraw)', () => {
  it('full paint: home, per-line erase-to-EOL, no screen clear', () => {
    const seq = renderFrame(null, ['a', 'b'], 80);
    expect(seq.startsWith('\x1b[H')).toBe(true);
    expect(seq).toContain('a\x1b[K');
    expect(seq).toContain('b\x1b[K');
    expect(seq).not.toContain('\x1b[2J');
    expect(seq.endsWith('\x1b[J')).toBe(true);
  });
  it('identical frame emits only cursor moves', () => {
    expect(renderFrame(['a'], ['a'], 80)).toBe('\x1b[H\x1b[1B');
  });
  it('shrinking frame erases leftover rows', () => {
    expect(renderFrame(['a', 'b', 'c'], ['a'], 80)).toContain('\x1b[J');
  });
  it('a changed middle row rewrites only that row', () => {
    const seq = renderFrame(['a', 'b', 'c'], ['a', 'X', 'c'], 80);
    expect(seq).toContain('\x1b[1B'); // skipped identical rows
    expect(seq).toContain('X\x1b[K');
    expect(seq).not.toContain('a\x1b[K');
  });
  it('clamps over-wide lines so nothing wraps and desyncs the diff', () => {
    const seq = renderFrame(null, ['x'.repeat(300)], 80);
    const row = seq.slice('\x1b[H'.length);
    expect(row).toContain('…');
    const content = row.slice(0, row.indexOf('\x1b[K')).replace(/^\r/, '');
    expect(visibleLen(content)).toBeLessThanOrEqual(80); // 79 glyphs + ellipsis
  });
});
