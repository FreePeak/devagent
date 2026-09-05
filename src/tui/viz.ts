/**
 * Visual primitives for the TUI, learned from the reference dashboards:
 * pilot renders sparkline metric cards; htop renders proportional meter bars.
 * Everything here is pure string math — no I/O — so layout bugs are unit
 * testable (test/tui-viz.test.ts).
 */

/** Visible width of a string: ANSI SGR color codes do not count. */
export function visibleLen(s: string): number {
  return s.replace(/\x1b\[[0-9;]*m/g, '').length;
}

/**
 * Sparkline: map `samples` onto half-block bars (pilot's metric-card cue).
 * Scale is relative to the series' own max, so an all-zero series renders as a
 * flat baseline rather than an empty string. `''` only for no samples yet.
 */
export function sparkline(samples: readonly number[], blocks = '▁▂▃▄▅▆▇█'): string {
  if (samples.length === 0) return '';
  const n = blocks.length;
  const hi = Math.max(...samples);
  const lo = Math.min(...samples, 0);
  const span = hi - lo;
  return samples
    .map((v) => {
      const frac = span > 0 ? (v - lo) / span : 0;
      return blocks[Math.min(n - 1, Math.floor(frac * n))]!;
    })
    .join('');
}

/**
 * Proportional meter bar (htop's gauge cue): `fill`+`empty` glyphs padded to
 * `width`. total <= 0 renders an empty bar — a negative gauge never lights up.
 */
export function meterBar(
  filled: number,
  total: number,
  width: number,
  fill = '█',
  empty = '░',
): string {
  const w = Math.max(0, Math.floor(width));
  const ratio = total > 0 ? Math.min(1, Math.max(0, filled / total)) : 0;
  const n = Math.round(ratio * w);
  return fill.repeat(n) + empty.repeat(Math.max(0, w - n));
}

export const LEVEL_COLORS: Record<string, string> = {
  error: '\x1b[31m',
  warn: '\x1b[33m',
  info: '\x1b[36m',
  debug: '\x1b[2m',
};

/** One structured line of a run log (DEVAGENT_HOME/runs/*.jsonl shape). */
export interface LogLine {
  ts?: string;
  level?: string;
  stage?: string;
  runId?: string;
  message: string;
  /** True when the raw line was not JSON and is shown verbatim. */
  raw?: boolean;
}

/**
 * Parse one SSE `data:` payload from the daemon's /events — either a
 * DEVAGENT_HOME/runs/*.jsonl line ({ts,level,stage,message}) or a repo
 * orchestration row ({kind:'event',event:'loop-phase',loop,phase,detail}).
 * Malformed lines degrade to a raw entry instead of throwing — a corrupt
 * line must never kill the tail view.
 */
export function parseLogLine(data: string): LogLine {
  const text = data.trim();
  if (!text.startsWith('{')) return { message: text, raw: true };
  try {
    const obj = JSON.parse(text) as Record<string, unknown>;
    const stage = typeof obj.stage === 'string' ? obj.stage : typeof obj.event === 'string' ? obj.event : undefined;
    let message = typeof obj.message === 'string' && obj.message ? obj.message : '';
    if (!message) {
      // Orchestration rows carry no message: synthesize one from their parts.
      const bits: string[] = [];
      if (typeof obj.phase === 'string') bits.push(`phase: ${obj.phase}`);
      if (typeof obj.detail === 'string' && obj.detail) bits.push(obj.detail);
      if (typeof obj.loop === 'number') bits.push(`(loop ${obj.loop})`);
      message = bits.join(' — ');
    }
    return {
      ts: typeof obj.ts === 'string' ? obj.ts : undefined,
      level: typeof obj.level === 'string' ? obj.level.toLowerCase() : undefined,
      stage,
      runId: typeof obj.runId === 'string' ? obj.runId : undefined,
      message: message || text,
    };
  } catch {
    return { message: text, raw: true };
  }
}

/** ISO ts → local HH:MM:SS; '        ' placeholder when absent/unparseable. */
function logClock(ts?: string): string {
  if (!ts) return '        ';
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return '        ';
  return d.toTimeString().slice(0, 8);
}

/** Render one log line to `width` visible columns: clock, level, stage, message. */
export function formatLogLine(l: LogLine, width: number, reset = '\x1b[0m'): string {
  const color = (l.level && LEVEL_COLORS[l.level]) || '';
  const lvl = l.raw ? 'raw' : (l.level ?? 'evt').slice(0, 5).padEnd(5, ' ');
  const clock = logClock(l.ts);
  const stage = l.stage ? `${l.stage.slice(0, 12)}  ` : '';
  const head = ` ${clock} ${color}${lvl}${reset} ${stage}`;
  const msgW = Math.max(10, width - visibleLen(head) - 1);
  const msg = l.message.length <= msgW ? l.message : `${l.message.slice(0, Math.max(0, msgW - 1))}…`;
  return `${head}${msg}`;
}
