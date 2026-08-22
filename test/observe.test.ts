import { describe, expect, it } from 'vitest';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { collectRunSummaries, renderDashboard, writeDashboard } from '../src/observe.js';

function tempHome(runs: Record<string, string>): string {
  const home = mkdtempSync(join(tmpdir(), 'da-obs-'));
  mkdirSync(join(home, 'runs'), { recursive: true });
  for (const [name, content] of Object.entries(runs)) {
    writeFileSync(join(home, 'runs', name), content);
  }
  return home;
}

describe('collectRunSummaries', () => {
  it('summarizes first/last events per run file', () => {
    const dir = tempHome({
      'run1.jsonl': [
        JSON.stringify({ runId: 'aaaa1111', ts: 'T1', stage: 'fetch', level: 'info', message: 'start' }),
        JSON.stringify({ runId: 'aaaa1111', ts: 'T2', stage: 'publish', level: 'info', message: 'PR opened' }),
      ].join('\n'),
      'run2.jsonl': [JSON.stringify({ runId: 'bbbb2222', ts: 'T3', stage: 'validate', level: 'error', message: 'gate failed' })].join('\n'),
    });
    try {
      const s = collectRunSummaries(join(dir, 'runs'));
      expect(s).toHaveLength(2);
      const ok = s.find((x) => x.runId === 'aaaa1111')!;
      expect(ok.eventCount).toBe(2);
      expect(ok.ok).toBe(true);
      expect(ok.lastStage).toBe('publish');
      const err = s.find((x) => x.runId === 'bbbb2222')!;
      expect(err.ok).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('returns empty for missing or empty dirs', () => {
    expect(collectRunSummaries('/nonexistent-da')).toEqual([]);
    const dir = mkdtempSync(join(tmpdir(), 'da-obs-empty-'));
    try {
      expect(collectRunSummaries(dir)).toEqual([]); // no runs subdir
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('skips malformed lines without crashing', () => {
    const dir = tempHome({
      'r.jsonl': ['{not json', JSON.stringify({ runId: 'cccc3333', ts: 'T', stage: 's', level: 'info', message: 'm' })].join('\n'),
    });
    try {
      expect(collectRunSummaries(join(dir, 'runs'))).toHaveLength(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('renderDashboard / writeDashboard', () => {
  it('escapes HTML in messages and marks error rows', () => {
    const html = renderDashboard([
      { runId: 'ddd44444', file: 'd.jsonl', startedAt: 'T', lastAt: 'T', lastStage: 'implement', lastLevel: 'error', lastMessage: '<script>alert(1)</script>', eventCount: 3, ok: false },
    ]);
    expect(html).toContain('&lt;script&gt;');
    expect(html).not.toContain('<script>alert');
    expect(html).toContain('class="err"');
  });

  it('writes dashboard.html into the home dir', () => {
    const dir = tempHome({
      'r.jsonl': JSON.stringify({ runId: 'eeee5555', ts: 'T', stage: 'fetch', level: 'info', message: 'hello' }),
    });
    try {
      const { path, runs } = writeDashboard(dir);
      expect(runs).toBe(1);
      expect(path.endsWith('.devagent/dashboard.html') || path.includes('dashboard.html')).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
