import { describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  collectRunSummaries,
  deriveLabel,
  deriveRunStatus,
  loadBoards,
  mapBoardStatus,
  renderDashboard,
  writeDashboard,
} from '../src/observe.js';
import type { RunSummary } from '../src/observe.js';

function tempHome(runs: Record<string, string>): string {
  const home = mkdtempSync(join(tmpdir(), 'da-obs-'));
  mkdirSync(join(home, 'runs'), { recursive: true });
  for (const [name, content] of Object.entries(runs)) {
    writeFileSync(join(home, 'runs', name), content);
  }
  return home;
}

function summary(overrides: Partial<RunSummary> = {}): RunSummary {
  return {
    runId: 'aaaa1111',
    file: 'a.jsonl',
    startedAt: '2026-08-24T03:00:00Z',
    lastAt: '2026-08-24T03:05:00Z',
    lastStage: 'implement',
    lastLevel: 'info',
    lastMessage: 'ok',
    eventCount: 2,
    ok: true,
    timeline: [{ ts: '2026-08-24T03:01:00Z', stage: 'implement', level: 'info', message: 'working' }],
    ...overrides,
  };
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

  it('enriches summaries from structured payloads and extracts PR/ticket', () => {
    const events = Array.from({ length: 80 }, (_, i) =>
      JSON.stringify({ runId: 'ffff6666', ts: `2026-08-24T00:${String(i % 60).padStart(2, '0')}:00Z`, stage: 'implement', level: 'info', message: `step ${i}` }),
    );
    events.push(
      JSON.stringify({
        runId: 'ffff6666',
        ts: '2026-08-24T01:00:00Z',
        stage: 'publish',
        level: 'info',
        message: 'PR opened: https://github.com/acme/api/pull/42.',
        data: { title: 'Fix gate G2', repo: '/repos/api', ticket: 'LIN-9', durationMs: 95000, exitCode: 0 },
      }),
    );
    const s = collectRunSummaries(tempHome({ 'r.jsonl': events.join('\n') }) + '/runs');
    expect(s).toHaveLength(1);
    const run = s[0];
    expect(run.title).toBe('Fix gate G2');
    expect(run.repo).toBe('/repos/api');
    expect(run.ticket).toBe('LIN-9');
    expect(run.durationMs).toBe(95000);
    expect(run.exitCode).toBe(0);
    expect(run.prUrl).toBe('https://github.com/acme/api/pull/42'); // trailing punctuation stripped
    expect(run.timeline).toHaveLength(50); // capped inline view, keeps the newest
    expect(run.timeline!.at(-1)!.message).toContain('PR opened');
  });
});

describe('status derivation', () => {
  it('maps runs onto kanban columns', () => {
    expect(deriveRunStatus(summary({ ok: false, lastLevel: 'error' }))).toBe('failed');
    expect(deriveRunStatus(summary({ timedOut: true }))).toBe('failed');
    expect(deriveRunStatus(summary({ prUrl: 'https://x/pull/1' }))).toBe('done');
    expect(deriveRunStatus(summary({}))).toBe('inprogress'); // implement-stage activity
    expect(deriveRunStatus(summary({ timeline: [{ ts: 't', stage: 'plan', level: 'info', message: 'planning' }] }))).toBe('todo');
  });

  it('maps project-board statuses onto the four columns', () => {
    expect(mapBoardStatus('pending')).toBe('todo');
    expect(mapBoardStatus('ready')).toBe('todo');
    expect(mapBoardStatus('blocked')).toBe('todo');
    expect(mapBoardStatus('ask')).toBe('todo');
    expect(mapBoardStatus('dispatched')).toBe('inprogress');
    expect(mapBoardStatus('untrusted')).toBe('inprogress');
    expect(mapBoardStatus('done')).toBe('done');
    expect(mapBoardStatus('failed')).toBe('failed');
    expect(mapBoardStatus('mystery')).toBe('todo'); // unknown statuses fail safe
  });
});

describe('deriveLabel', () => {
  const base = summary();
  it('prefers structured title, then ticket, then informative message', () => {
    expect(deriveLabel(base)).toBe('working');
    expect(deriveLabel({ ...base, timeline: undefined, ticket: 'LIN-204' })).toBe('LIN-204');
    expect(
      deriveLabel({
        ...base,
        ticket: undefined,
        timeline: [
          { ts: 't', stage: 'fetch', level: 'info', message: 'Run aaaa1111-2222 starting' },
          { ts: 't', stage: 'fetch', level: 'info', message: 'Fetched ticket from linear: implement rate limiting' },
        ],
      }),
    ).toBe('Fetched ticket from linear: implement rate limiting');
    expect(
      deriveLabel({ ...base, timeline: [{ ts: 't', stage: 'fetch', level: 'info', message: 'x'.repeat(120) }] }).length,
    ).toBeLessThanOrEqual(90);
  });

  it('names orchestrator dispatch loops instead of showing a UUID', () => {
    expect(
      deriveLabel({
        ...base,
        timeline: [
          { ts: 't', stage: 'task', level: 'info', message: 'Dispatching T1: T1' },
          { ts: 't', stage: 'task', level: 'info', message: 'T1 done (audited)' },
        ],
      }),
    ).toBe('Orchestrator dispatch loop');
    expect(deriveLabel({ ...base, timeline: undefined })).toMatch(/^[0-9a-f]{8}$/);
  });
});

describe('loadBoards', () => {
  it('reads .devagent-project.json boards from candidate dirs', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-board-'));
    writeFileSync(
      join(dir, '.devagent-project.json'),
      JSON.stringify({
        goal: 'ship payments',
        tasks: [
          { id: 'T1', title: 'Add ledger table', status: 'done', attempts: 2 },
          { id: 'T2', prompt: 'Wire webhook retries', status: 'ask', evidenceGaps: ['no retry test'] },
          { id: 'T3', title: 'Bad row skipped below' },
        ],
      }),
    );
    writeFileSync(join(dir, '.devagent-project.json.broken'), '{corrupt');
    try {
      const boards = loadBoards([dir, '/nonexistent-da']);
      expect(boards).toHaveLength(1);
      const b = boards[0];
      expect(b.goal).toBe('ship payments');
      expect(b.tasks[0].attempts).toBe(2);
      expect(b.tasks[1].title).toBe('Wire webhook retries'); // falls back to prompt when no title
      expect(b.tasks[1].evidenceGaps).toEqual(['no retry test']);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('skips corrupted boards instead of throwing', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-board-bad-'));
    writeFileSync(join(dir, '.devagent-project.json'), '{not json');
    try {
      expect(loadBoards([dir])).toEqual([]);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

function escHtml(s: string): string {
  return s.replace(/&/g, '&amp;').replace(/</g, '&lt;');
}

describe('renderDashboard / writeDashboard', () => {
  it('escapes HTML in messages and marks failed runs', () => {
    const html = renderDashboard([
      summary({ runId: 'ddd44444', lastLevel: 'error', lastMessage: '<script>alert(1)</script>', ok: false }),
    ]);
    expect(html).toContain('&lt;script&gt;');
    expect(html).not.toContain('<script>alert');
    expect(html).toContain('details class="run failed"'); // triage styling via status class
  });

  it('renders board columns, date groups, feature groups and tabs', () => {
    const html = renderDashboard([
      summary({}),
      summary({ runId: 'bbbb2222', startedAt: '2026-08-23T09:00:00Z', title: 'Feature X', prUrl: 'https://github.com/a/b/pull/7' }),
      summary({ runId: 'cccc3333', startedAt: '2026-08-24T05:00:00Z', title: 'Feature X', ok: false, lastLevel: 'error' }),
    ]);
    // Tabs exist
    for (const t of ['tb-board', 'tb-runs', 'tb-features']) expect(html).toContain(`id="${t}"`);
    // Kanban columns with counts
    for (const c of ['todo', 'in progress', 'done', 'failed']) expect(html).toContain(c);
    // Runs grouped under per-day headers
    expect(html).toMatch(/2026-08-24[\s\S]*?2026-08-23/s);
    // Feature tab groups both runs of Feature X into one block (the untitled
    // run forms its own repo+day group)
    expect(html.match(/<summary><span class="rid">Feature X<\/span>/g)).toHaveLength(1);
    expect(html).toContain('https://github.com/a/b/pull/7');
    // Lazy detail embeds are present as safe JSON script blocks
    expect(html).toContain('type="application/json"');
    expect(html).not.toContain('</script>alert');
  });

  it('renders an expandable timeline with escaped content per run', () => {
    const html = renderDashboard([
      summary({ timeline: [{ ts: '2026-08-24T03:01:00Z', stage: 'implement', level: 'error', message: '<img src=x onerror=alert(1)>' }] }),
    ]);
    expect(html).toContain('<details class="run inprogress"');
    expect(html).toContain('class="timeline"');
    expect(html).toContain('&lt;img src=x onerror=alert(1)&gt;');
    expect(html).not.toContain('<img src=x');
  });

  it('renders empty state without crashing', () => {
    const html = renderDashboard([], 'DevAgent Runs');
    expect(html).toContain('(0 runs)');
    expect(html).toContain('No .devagent-project.json boards found');
  });

  it('writes dashboard.html into the home dir and reports boards', () => {
    const dir = tempHome({
      'r.jsonl': JSON.stringify({ runId: 'eeee5555', ts: 'T', stage: 'fetch', level: 'info', message: 'hello' }),
    });
    try {
      const { path, runs, boards } = writeDashboard(dir, [dir]);
      expect(runs).toBe(1);
      expect(boards).toBe(0); // no board file in this temp home
      expect(path.endsWith('.devagent/dashboard.html') || path.includes('dashboard.html')).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('embeds full run payload as escaped JSON for the lazy detail tab', () => {
    const html = renderDashboard([summary({ title: 'Has </script> inside' })]);
    expect(html).toContain('\\u003c/script\\u003e'); // breakout-proof embedding
    expect(html).not.toMatch(/<\/script>[^<]*inside/);
    void escHtml;
  });
});
