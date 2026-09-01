import { afterEach, describe, expect, it } from 'vitest';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import {
  graceAgeHours,
  sweepTaskPrHygiene,
  type PrHygieneOutcome,
  type RunGh,
} from '../src/orchestrator/pr-hygiene.js';
import { LEDGER_DIR } from '../src/orchestrator/ledger.js';

const dirs: string[] = [];
afterEach(() => {
  let d: string | undefined;
  while ((d = dirs.pop())) rmSync(d, { recursive: true, force: true });
});

function tempRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-prhygiene-'));
  dirs.push(d);
  return d;
}

/** Raw gh JSON for one open TASK PR. */
function taskPr(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    number: 9,
    title: 'T1',
    headRefName: 'devagent/TASK-mtioq4ik-T1-a0',
    baseRefName: 'devagent/TASK-mtioq4ik-T0-a0',
    state: 'OPEN',
    mergeable: 'MERGEABLE',
    reviewDecision: '',
    headRefOid: 'abc123',
    updatedAt: new Date().toISOString(),
    author: { login: 'devagent[bot]' },
    statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }],
    ...overrides,
  };
}

const GREEN = [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }];
const RED = [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }];
const PENDING = [{ name: 'test', status: 'IN_PROGRESS', conclusion: null }];

/** Scripted gh runner keyed by subcommand; records every call. */
function scriptedGh(responses: Record<string, string | Error>): { run: RunGh; calls: string[][] } {
  const calls: string[][] = [];
  const run: RunGh = async (args) => {
    calls.push(args);
    const key = args[0] === 'pr' ? args[1] : args[0];
    const r = responses[key];
    if (r instanceof Error) throw r;
    return { stdout: r ?? '', stderr: '' };
  };
  return { run, calls };
}

/** gh runner whose branch lookups (api) 404 for the given base names, succeed for other api calls. */
function scriptedGhWithDeadBases(
  deadBases: string[],
  rest: Record<string, string | Error> = {},
): { run: RunGh; calls: string[][] } {
  const calls: string[][] = [];
  const run: RunGh = async (args) => {
    calls.push(args);
    if (args[0] === 'api') {
      if (deadBases.some((b) => args.join(' ').includes(b))) {
        throw new Error('gh: Not Found (404)');
      }
      return { stdout: '', stderr: '' };
    }
    const key = args[0] === 'pr' ? args[1] : args[0];
    const r = rest[key];
    if (r instanceof Error) throw r;
    return { stdout: r ?? '', stderr: '' };
  };
  return { run, calls };
}

/** Ledger rows under a repo path, oldest first. */
function readLedgerRows(repo: string): Array<Record<string, unknown>> {
  const file = join(repo, LEDGER_DIR, 'events.jsonl');
  if (!existsSync(file)) return [];
  return readFileSync(file, 'utf8')
    .split('\n')
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l) as Record<string, unknown>);
}

function hygieneRows(repo: string): Array<Record<string, unknown>> {
  return readLedgerRows(repo).filter((r) => r.event === 'pr-hygiene');
}

describe('graceAgeHours', () => {
  it('computes non-negative hours since updatedAt', () => {
    const age = graceAgeHours(new Date(Date.now() - 48 * 3_600_000).toISOString(), Date.now());
    expect(age).not.toBeNull();
    expect(age!).toBeGreaterThanOrEqual(47.9);
    expect(age!).toBeLessThan(48.1);
  });

  it('returns null for missing or unparseable timestamps', () => {
    expect(graceAgeHours('', Date.now())).toBeNull();
    expect(graceAgeHours('not-a-date', Date.now())).toBeNull();
  });
});

describe('sweepTaskPrHygiene', () => {
  it('closes a base-superseded TASK PR (base branch deleted) and writes a ledger row', async () => {
    const repo = tempRepo();
    const { run, calls } = scriptedGhWithDeadBases(['devagent/TASK-mtioq4ik-T0-a0'], {
      list: JSON.stringify([taskPr()]),
      comment: '',
      close: '',
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(repo, { dryRun: false }, run);
    expect(skipAutoMerge).toBe(false);
    const o = outcomes[0]!;
    expect(o.action).toBe('closed');
    expect(o.reason).toBe('base-superseded');
    expect(o.detail).toContain('merged or deleted');
    expect(calls.find((c) => c[1] === 'close')).toEqual(['pr', 'close', '9']);
    expect(calls.find((c) => c[1] === 'comment')!.join(' ')).toContain('merged or deleted');
    const rows = hygieneRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      kind: 'event',
      event: 'pr-hygiene',
      pr: 9,
      action: 'closed',
      reason: 'base-superseded',
    });
    expect(rows[0]!.graceAgeHours).toEqual(expect.any(Number));
    expect(typeof rows[0]!.ts).toBe('string');
    expect(typeof rows[0]!.taskId).toBe('string');
    expect(typeof rows[0]!.attempt).toBe('number');
  });

  it('flags base-superseded in dry-run: no comment, no close, still one ledger row', async () => {
    const repo = tempRepo();
    const { run, calls } = scriptedGhWithDeadBases(['devagent/TASK-mtioq4ik-T0-a0'], {
      list: JSON.stringify([taskPr()]),
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(repo, {}, run);
    expect(skipAutoMerge).toBe(false);
    expect(outcomes[0]!.action).toBe('flagged');
    expect(outcomes[0]!.detail).toContain('[dry-run]');
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
    const rows = hygieneRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ action: 'flagged', reason: 'base-superseded' });
  });

  it('flags a red-across-grace PR, sets skipAutoMerge with autoMerge on, and writes a ledger row', async () => {
    const repo = tempRepo();
    const stale = new Date(Date.now() - 30 * 3_600_000).toISOString();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([taskPr({ baseRefName: 'main', statusCheckRollup: RED, updatedAt: stale })]),
      api: '',
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, autoMerge: true, graceHours: 24 },
      run,
    );
    expect(skipAutoMerge).toBe(true);
    const o = outcomes[0]!;
    expect(o.action).toBe('flagged');
    expect(o.reason).toBe('red-across-grace');
    expect(o.detail).toContain('24h grace');
    expect(o.detail).toContain('autoMerge skipped');
    // flagged, never closed
    expect(calls.filter((c) => c[1] === 'close')).toHaveLength(0);
    const rows = hygieneRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ action: 'flagged', reason: 'red-across-grace', pr: 9 });
    expect(rows[0]!.graceAgeHours).toBeGreaterThanOrEqual(29);
  });

  it('reports red-across-grace without skipAutoMerge when autoMerge is off', async () => {
    const repo = tempRepo();
    const stale = new Date(Date.now() - 30 * 3_600_000).toISOString();
    const { run } = scriptedGh({
      list: JSON.stringify([taskPr({ baseRefName: 'main', statusCheckRollup: RED, updatedAt: stale })]),
      api: '',
    });
    const { skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, autoMerge: false, graceHours: 24 },
      run,
    );
    expect(skipAutoMerge).toBe(false);
  });

  it('leaves a red PR within the grace window untouched (no skip, no ledger row)', async () => {
    const repo = tempRepo();
    const fresh = new Date(Date.now() - 2 * 3_600_000).toISOString();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([taskPr({ baseRefName: 'main', statusCheckRollup: RED, updatedAt: fresh })]),
      api: '',
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, autoMerge: true, graceHours: 24 },
      run,
    );
    expect(skipAutoMerge).toBe(false);
    expect(outcomes[0]!.action).toBe('untouched');
    expect(outcomes[0]!.reason).toBe('red-within-grace');
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
    expect(readLedgerRows(repo)).toHaveLength(0);
  });

  it('treats an unparseable updatedAt as overdue for red-across-grace', async () => {
    const repo = tempRepo();
    const { run } = scriptedGh({
      list: JSON.stringify([taskPr({ baseRefName: 'main', statusCheckRollup: RED, updatedAt: 'not-a-date' })]),
      api: '',
    });
    const { outcomes } = await sweepTaskPrHygiene(repo, { dryRun: false, graceHours: 24 }, run);
    expect(outcomes[0]!.action).toBe('flagged');
    expect(outcomes[0]!.reason).toBe('red-across-grace');
    expect(outcomes[0]!.graceAgeHours).toBeNull();
  });

  it('leaves green and pending TASK PRs untouched with an intact base', async () => {
    const repo = tempRepo();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        taskPr({ number: 3, statusCheckRollup: GREEN }),
        taskPr({ number: 4, statusCheckRollup: PENDING }),
      ]),
      api: '',
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, autoMerge: true, graceHours: 0 },
      run,
    );
    expect(skipAutoMerge).toBe(false);
    expect(outcomes.map((o) => [o.pr, o.action, o.reason])).toEqual([
      [3, 'untouched', 'green'],
      [4, 'untouched', 'pending'],
    ]);
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
    expect(readLedgerRows(repo)).toHaveLength(0);
  });

  it('never touches non-TASK PRs regardless of state', async () => {
    const repo = tempRepo();
    const stale = new Date(Date.now() - 100 * 3_600_000).toISOString();
    const prs = [
      taskPr({ number: 5, headRefName: 'feature/manual', baseRefName: 'gone-branch' }),
      taskPr({
        number: 6,
        headRefName: 'hotfix/x',
        baseRefName: 'main',
        statusCheckRollup: RED,
        updatedAt: stale,
      }),
    ];
    const runner: RunGh = async (args) => {
      if (args[0] === 'pr' && args[1] === 'list') {
        return { stdout: JSON.stringify(prs), stderr: '' };
      }
      if (args[0] === 'api') {
        if (args.join(' ').includes('gone-branch')) throw new Error('gh: Not Found (404)');
        return { stdout: '', stderr: '' };
      }
      return { stdout: '', stderr: '' };
    };
    const calls: string[][] = [];
    const recording: RunGh = async (args) => {
      calls.push(args);
      return runner(args);
    };
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, autoMerge: true, graceHours: 24 },
      recording,
    );
    expect(skipAutoMerge).toBe(false);
    expect(outcomes.filter((o) => o.action === 'untouched').map((o) => o.reason)).toEqual([
      'not-a-task-pr',
      'not-a-task-pr',
    ]);
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
    expect(readLedgerRows(repo)).toHaveLength(0);
  });

  it('skips non-open PRs', async () => {
    const repo = tempRepo();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([taskPr({ state: 'CLOSED' })]),
      api: new Error('gh: Not Found (404)'),
    });
    const { outcomes } = await sweepTaskPrHygiene(repo, { dryRun: false }, run);
    expect(outcomes[0]!.action).toBe('skipped');
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
    expect(readLedgerRows(repo)).toHaveLength(0);
  });

  it('respects a zero grace window (any red is flagged immediately)', async () => {
    const repo = tempRepo();
    const { run } = scriptedGh({
      list: JSON.stringify([taskPr({ baseRefName: 'main', statusCheckRollup: RED })]),
      api: '',
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, autoMerge: true, graceHours: 0 },
      run,
    );
    expect(skipAutoMerge).toBe(true);
    expect(outcomes[0]!.action).toBe('flagged');
    expect(outcomes[0]!.reason).toBe('red-across-grace');
  });

  it('closes a red-within-grace PR whose base is gone (dead base cannot be fixed by waiting)', async () => {
    const repo = tempRepo();
    const { run, calls } = scriptedGhWithDeadBases(['devagent/TASK-mtioq4ik-T0-a0'], {
      list: JSON.stringify([taskPr({ statusCheckRollup: RED })]),
      comment: '',
      close: '',
    });
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { dryRun: false, graceHours: 24 },
      run,
    );
    expect(skipAutoMerge).toBe(false);
    expect(outcomes[0]!.action).toBe('closed');
    expect(outcomes[0]!.reason).toBe('base-superseded');
    expect(calls.find((c) => c[1] === 'close')).toEqual(['pr', 'close', '9']);
    const rows = hygieneRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ action: 'closed', reason: 'base-superseded' });
  });

  it('writes one ledger row per action across a mixed dry-run sweep', async () => {
    const repo = tempRepo();
    const stale = new Date(Date.now() - 48 * 3_600_000).toISOString();
    const prs = [
      taskPr({ number: 11 }), // base-superseded -> flagged (dry-run)
      taskPr({ number: 12, baseRefName: 'main', statusCheckRollup: RED, updatedAt: stale }), // red across grace -> flagged
      taskPr({ number: 13, baseRefName: 'main' }), // green -> untouched
    ];
    const runner: RunGh = async (args) => {
      if (args[0] === 'pr' && args[1] === 'list') {
        return { stdout: JSON.stringify(prs), stderr: '' };
      }
      if (args[0] === 'api') {
        if (args.join(' ').includes('devagent/TASK-mtioq4ik-T0-a0')) {
          throw new Error('gh: Not Found (404)');
        }
        return { stdout: '', stderr: '' };
      }
      return { stdout: '', stderr: '' };
    };
    const { outcomes, skipAutoMerge } = await sweepTaskPrHygiene(
      repo,
      { autoMerge: true, graceHours: 24 },
      runner,
    );
    expect(outcomes.map((o) => [o.pr, o.action, o.reason])).toEqual([
      [11, 'flagged', 'base-superseded'],
      [12, 'flagged', 'red-across-grace'],
      [13, 'untouched', 'green'],
    ]);
    // red-across-grace flags hold autoMerge even in a dry run
    expect(skipAutoMerge).toBe(true);
    const rows = hygieneRows(repo);
    expect(rows).toHaveLength(2);
    expect(rows.map((r) => [r.pr, r.action, r.reason])).toEqual([
      [11, 'flagged', 'base-superseded'],
      [12, 'flagged', 'red-across-grace'],
    ]);
  });
});
