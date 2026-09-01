import { afterEach, describe, expect, it, vi } from 'vitest';
import { existsSync, mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import {
  autoReviewAndMergeOne,
  defaultCiFixer,
  evaluateAutoReview,
  evaluateChecks,
  scanAddedLinesForHazards,
  sweepStalePrs,
  type CiFixRequest,
  type PrStatus,
  type RunGh,
} from '../src/integrations/autopr.js';
import { LEDGER_DIR } from '../src/orchestrator/ledger.js';

function prStatus(overrides: Partial<PrStatus> = {}): PrStatus {
  return {
    number: 9,
    title: 'Loop 48',
    headRefName: 'devagent/x',
    baseRefName: 'main',
    state: 'OPEN',
    mergeable: 'MERGEABLE',
    reviewDecision: '',
    headRefOid: 'abc123',
    updatedAt: new Date().toISOString(),
    checks: [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }],
    ...overrides,
  };
}

/** Scripted gh runner mapping "subcommand" to queued responses; records every call. */
function scriptedGh(responses: Record<string, string | Error>): { run: RunGh; calls: string[][] } {
  const calls: string[][] = [];
  const run: RunGh = async (args) => {
    // gh invocations all start with "pr"; key by the subcommand
    const key = args[0] === 'pr' ? args[1] : args[0];
    calls.push(args);
    const r = responses[key];
    if (r instanceof Error) throw r;
    return { stdout: r ?? '', stderr: '' };
  };
  return { run, calls };
}

/** Like scriptedGh, but each "pr view" consumes the next queued JSON payload (re-poll sequences). */
function sequencedGh(views: string[], rest: Record<string, string | Error> = {}): { run: RunGh; calls: string[][] } {
  const calls: string[][] = [];
  const queue = [...views];
  const run: RunGh = async (args) => {
    calls.push(args);
    const key = args[0] === 'pr' ? args[1] : args[0];
    if (key === 'view') {
      const payload = queue.shift();
      if (payload === undefined) throw new Error(`unexpected extra pr view (queue exhausted)`);
      return { stdout: payload, stderr: '' };
    }
    const r = rest[key];
    if (r instanceof Error) throw r;
    return { stdout: r ?? '', stderr: '' };
  };
  return { run, calls };
}

/** Fresh temp repo path so fixer ledger rows land on a real writable dir. */
function tempRepo(dirs: string[]): string {
  const d = mkdtempSync(join(tmpdir(), 'da-autopr-'));
  dirs.push(d);
  return d;
}

/** Parse the run ledger rows written under a repo path, oldest first. */
function readLedgerRows(repo: string): Array<Record<string, unknown>> {
  const file = join(repo, LEDGER_DIR, 'events.jsonl');
  if (!existsSync(file)) return [];
  const out: Array<Record<string, unknown>> = [];
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    if (!line.trim()) continue;
    try {
      out.push(JSON.parse(line) as Record<string, unknown>);
    } catch {
      // skip corrupt lines; a ledger is data, not truth
    }
  }
  return out;
}

describe('evaluateChecks', () => {
  it('passes when all completed checks succeeded', () => {
    const v = evaluateChecks(prStatus());
    expect(v.pending).toBe(false);
    expect(v.passed).toBe(true);
    expect(v.failedChecks).toEqual([]);
  });

  it('treats SKIPPED as passing but FAILURE as blocking', () => {
    const v = evaluateChecks(
      prStatus({
        checks: [
          { name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' },
          { name: 'gitStream.cm', status: 'COMPLETED', conclusion: 'SKIPPED' },
          { name: 'lint', status: 'COMPLETED', conclusion: 'FAILURE' },
        ],
      }),
    );
    expect(v.passed).toBe(false);
    expect(v.failedChecks).toEqual(['lint=FAILURE']);
  });

  it('marks pending when any check is still running', () => {
    const v = evaluateChecks(
      prStatus({ checks: [{ name: 'test', status: 'IN_PROGRESS', conclusion: null }] }),
    );
    expect(v.pending).toBe(true);
  });
});

describe('scanAddedLinesForHazards', () => {
  it('scans only added lines of new files and reports DA101', () => {
    const diff = [
      'diff --git a/src/new.ts b/src/new.ts',
      '--- /dev/null',
      '+++ b/src/new.ts',
      '@@ -0,0 +1,3 @@',
      '+const x = 1;',
      '+fetch(url).then((r) => r.json());',
      '+context line that is not added',
    ].join('\n');
    const findings = scanAddedLinesForHazards(diff);
    expect(findings.some((f) => f.ruleId === 'DA101' && f.file === 'src/new.ts')).toBe(true);
  });

  it('reports nothing for a clean diff', () => {
    const findings = scanAddedLinesForHazards(
      ['diff --git a/src/ok.ts b/src/ok.ts', '+++ b/src/ok.ts', '+const y = await Promise.resolve(1);'].join('\n'),
    );
    expect(findings.filter((f) => f.severity === 'high')).toEqual([]);
  });
});

describe('evaluateAutoReview', () => {
  it('approves on green CI, mergeable, no hazards', () => {
    const r = evaluateAutoReview(prStatus(), { hazards: [], mergeMethod: 'squash' });
    expect(r.event).toBe('APPROVE');
    expect(r.body).toContain('approved');
  });

  it('blocks on red CI', () => {
    const r = evaluateAutoReview(
      prStatus({
        checks: [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }],
      }),
      { hazards: [], mergeMethod: 'squash' },
    );
    expect(r.event).toBe('REQUEST_CHANGES');
    expect(r.reason).toContain('CI failed');
  });

  it('blocks on conflicts but keeps hazards advisory', () => {
    const conflict = evaluateAutoReview(prStatus({ mergeable: 'CONFLICTING' }), {
      hazards: [],
      mergeMethod: 'squash',
    });
    expect(conflict.reason).toContain('conflicts');
    expect(conflict.event).toBe('REQUEST_CHANGES');

    // Line-level hazard findings are surfaced in the body, never blocking:
    // the added-lines view cannot see multi-line catch handlers.
    const hazard = evaluateAutoReview(prStatus(), {
      hazards: [{ ruleId: 'DA101', severity: 'high', message: 'm', file: 'a.ts', line: 1 }],
      mergeMethod: 'squash',
    });
    expect(hazard.event).toBe('APPROVE');
    expect(hazard.body).toContain('DA101');
  });
});

describe('autoReviewAndMergeOne', () => {
  const basePr = {
    number: 9,
    title: 'T',
    headRefName: 'devagent/x',
    baseRefName: 'main',
    state: 'OPEN',
    mergeable: 'MERGEABLE',
    reviewDecision: '',
    author: { login: 'someone-else' },
    statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }],
  };
  const viewJson = JSON.stringify(basePr);

  it('approves then merges on green evidence', async () => {
    const { run, calls } = scriptedGh({ view: viewJson, diff: '', review: '', merge: '' });
    const o = await autoReviewAndMergeOne('/repo', 9, {}, run);
    expect(o.action).toBe('merged');
    const reviewCall = calls.find((c) => c[1] === 'review')!;
    expect(reviewCall).toContain('--approve');
    const mergeCall = calls.find((c) => c[1] === 'merge')!;
    expect(mergeCall.join(' ')).toContain('--squash');
    expect(mergeCall.join(' ')).toContain('--delete-branch');
  });

  it('merges with an advisory hazard note in the approval body', async () => {
    const hazardous = JSON.parse(viewJson) as Record<string, unknown>;
    hazardous.statusCheckRollup = [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }];
    const { run, calls } = scriptedGh({
      view: JSON.stringify(hazardous),
      diff: ['+++ b/src/a.ts', '+fetch(url).then((r) => r.json());'].join('\n'),
      review: '',
      merge: '',
    });
    const o = await autoReviewAndMergeOne('/repo', 9, {}, run);
    expect(o.action).toBe('merged');
    const reviewCall = calls.find((c) => c[1] === 'review')!;
    expect(reviewCall.some((a) => a.includes('DA101'))).toBe(true);
  });

  it('comments instead of approving a self-authored PR, then merges', async () => {
    const mine = JSON.parse(viewJson) as Record<string, unknown>;
    (mine as { author: unknown }).author = { login: 'linh.doan' };
    const { run, calls } = scriptedGh({
      view: JSON.stringify(mine),
      api: 'linh.doan',
      diff: '',
      review: new Error('self-approval is forbidden'),
      comment: '',
      merge: '',
    });
    const o = await autoReviewAndMergeOne('/repo', 9, {}, run);
    expect(o.action).toBe('merged');
    expect(calls.some((c) => c[1] === 'comment')).toBe(true);
    expect(calls.some((c) => c[1] === 'review')).toBe(false);
  });

  it('posts request-changes and never merges when CI is red and no fixer dispatch is possible', async () => {
    const red = JSON.parse(viewJson) as Record<string, unknown>;
    red.statusCheckRollup = [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }];
    const { run, calls } = scriptedGh({
      view: JSON.stringify(red),
      diff: '',
      review: '',
      merge: new Error('should not be called'),
    });
    const o = await autoReviewAndMergeOne('/repo', 9, {}, run);
    // default fixer is unconfigured (no DEVAGENT_REMOTE_TARGET): structured
    // ci-fix-failed outcome instead of a bare request-changes dead end
    expect(o.action).toBe('ci-fix-failed');
    expect(o.detail).toContain('DEVAGENT_REMOTE_TARGET');
    expect(calls.some((c) => c[1] === 'merge')).toBe(false);
    expect(calls.some((c) => c[1] === 'review')).toBe(false);
  });

  it('skips PRs whose base does not match the filter', async () => {
    const otherBase = JSON.parse(viewJson) as Record<string, unknown>;
    otherBase.baseRefName = 'pre-dogfood-r1';
    const { run, calls } = scriptedGh({ view: JSON.stringify(otherBase), review: '', merge: '' });
    const o = await autoReviewAndMergeOne('/repo', 1, { baseBranch: 'main' }, run);
    expect(o.action).toBe('skipped');
    expect(o.detail).toContain('base is pre-dogfood-r1');
    expect(calls.some((c) => c[1] === 'review')).toBe(false);
  });

  it('dry-run evaluates without posting reviews or merging', async () => {
    const { run, calls } = scriptedGh({ view: viewJson, diff: '', review: '', merge: '' });
    const o = await autoReviewAndMergeOne('/repo', 9, { dryRun: true }, run);
    expect(o.detail.startsWith('[dry-run]')).toBe(true);
    expect(calls.some((c) => c[1] === 'review')).toBe(false);
    expect(calls.some((c) => c[1] === 'merge')).toBe(false);
  });

  it('falls back to --auto merge when direct merge is refused by protection', async () => {
    const { run, calls } = scriptedGh({ view: viewJson, diff: '', review: '' });
    let merges = 0;
    const wrapped: RunGh = async (args, cwd) => {
      if (args[0] === 'pr' && args[1] === 'merge') {
        merges += 1;
        if (!args.includes('--auto')) throw new Error('gh: pull request 9 is not mergeable: required reviews');
      }
      return run(args, cwd);
    };
    const o = await autoReviewAndMergeOne('/repo', 9, {}, wrapped);
    expect(o.action).toBe('merged');
    expect(merges).toBe(2);
  });

  describe('ci-fix', () => {
    const redView = JSON.stringify({
      ...basePr,
      statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }],
    });

    it('failed-then-green: dispatches the fixer once, re-polls, and merges', async () => {
      const { run, calls } = sequencedGh([redView, JSON.stringify(basePr)], { diff: '', review: '', merge: '' });
      const fixCalls: CiFixRequest[] = [];
      const o = await autoReviewAndMergeOne(
        '/repo',
        9,
        {
          fixer: async (req) => {
            fixCalls.push(req);
            return { ok: true, note: 'fix pushed' };
          },
        },
        run,
      );
      expect(fixCalls).toHaveLength(1);
      expect(fixCalls[0]!.taskId).toBe('TASK-fix-9');
      expect(fixCalls[0]!.failedChecks).toEqual(['test=FAILURE']);
      expect(fixCalls[0]!.prompt).toContain('PR #9');
      expect(fixCalls[0]!.prompt).toContain('test=FAILURE');
      expect(o.action).toBe('merged');
      expect(calls.filter((c) => c[1] === 'view')).toHaveLength(2); // initial + re-poll after fix
    });

    it('still-red: records a structured ci-fix-failed outcome and never merges', async () => {
      const { run, calls } = sequencedGh([redView, redView], { diff: '', merge: '' });
      const o = await autoReviewAndMergeOne('/repo', 9, { fixer: async () => ({ ok: true, note: 'fix pushed' }) }, run);
      expect(o.action).toBe('ci-fix-failed');
      expect(o.failedChecks).toEqual(['test=FAILURE']);
      expect(o.attempts).toBe(1);
      expect(o.summary).toContain('failed: test');
      expect(calls.some((c) => c[1] === 'merge')).toBe(false);
      // no request-changes review was posted before the fix attempt either
      expect(calls.some((c) => c[1] === 'review')).toBe(false);
    });

    it('no-fixer outcome propagates as ci-fix-failed without re-polling', async () => {
      const { run, calls } = sequencedGh([redView], { diff: '', merge: '' });
      const o = await autoReviewAndMergeOne(
        '/repo',
        9,
        { fixer: async () => ({ ok: false, note: 'remote preflight failed' }) },
        run,
      );
      expect(o.action).toBe('ci-fix-failed');
      expect(o.failedChecks).toEqual(['test=FAILURE']);
      expect(o.attempts).toBe(1);
      expect(o.summary).toContain('failed: test');
      expect(o.detail).toContain('remote preflight failed');
      // only the initial status read; the PR was never re-polled for a fix
      expect(calls.filter((c) => c[1] === 'view')).toHaveLength(1);
    });

    it('dispatch throw is caught and reported as ci-fix-failed', async () => {
      const { run } = sequencedGh([redView], { diff: '', merge: '' });
      const o = await autoReviewAndMergeOne(
        '/repo',
        9,
        { fixer: async () => { throw new Error('ssh exploded'); } },
        run,
      );
      expect(o.action).toBe('ci-fix-failed');
      expect(o.detail).toContain('ssh exploded');
    });

    it('defaults to the built-in dispatcher, which reports not-ok without DEVAGENT_REMOTE_TARGET', async () => {
      delete process.env.DEVAGENT_REMOTE_TARGET;
      const { run } = sequencedGh([redView], { diff: '', merge: '' });
      const o = await autoReviewAndMergeOne('/repo', 9, {}, run);
      expect(o.action).toBe('ci-fix-failed');
      expect(o.detail).toContain('DEVAGENT_REMOTE_TARGET');
    });

    describe('ledger rows', () => {
      const fixDirs: string[] = [];
      afterEach(() => {
        for (const d of fixDirs) rmSync(d, { recursive: true, force: true });
        fixDirs.length = 0;
      });

      it('writes a dispatch row and a failed-then-green outcome row', async () => {
        const repo = tempRepo(fixDirs);
        const { run } = sequencedGh([redView, JSON.stringify(basePr)], { diff: '', review: '', merge: '' });
        const o = await autoReviewAndMergeOne(repo, 9, { fixer: async () => ({ ok: true, note: 'fix pushed' }) }, run);
        expect(o.action).toBe('merged');
        const rows = readLedgerRows(repo);
        expect(rows).toHaveLength(2);
        expect(rows[0]).toMatchObject({
          kind: 'event', event: 'ci-fix-dispatched', taskId: 'TASK-fix-9', pr: 9,
          failedChecks: ['test=FAILURE'],
        });
        expect(rows[1]).toMatchObject({
          kind: 'event', event: 'ci-fix-outcome', taskId: 'TASK-fix-9', pr: 9,
          outcome: 'failed-then-green',
        });
      });

      it('writes a dispatch row and a still-red outcome row', async () => {
        const repo = tempRepo(fixDirs);
        const { run } = sequencedGh([redView, redView], { diff: '', merge: '' });
        const o = await autoReviewAndMergeOne(repo, 9, { fixer: async () => ({ ok: true, note: 'fix pushed' }) }, run);
        expect(o.action).toBe('ci-fix-failed');
        const rows = readLedgerRows(repo);
        expect(rows).toHaveLength(2);
        expect(rows[0]).toMatchObject({ event: 'ci-fix-dispatched', pr: 9 });
        expect(rows[1]).toMatchObject({ event: 'ci-fix-outcome', outcome: 'still-red', pr: 9 });
        // round-trip: one dispatch pairs with exactly one outcome
        const dispatched = rows.filter((r) => r.event === 'ci-fix-dispatched');
        const outcomes = rows.filter((r) => r.event === 'ci-fix-outcome');
        expect(dispatched).toHaveLength(1);
        expect(outcomes).toHaveLength(1);
      });

      it('writes only a ci-fix-failed outcome row when dispatch was never dispatched', async () => {
        const repo = tempRepo(fixDirs);
        const { run } = sequencedGh([redView], { diff: '', merge: '' });
        const o = await autoReviewAndMergeOne(repo, 9, { fixer: async () => ({ ok: false, note: 'remote preflight failed' }) }, run);
        expect(o.action).toBe('ci-fix-failed');
        const rows = readLedgerRows(repo);
        expect(rows).toHaveLength(1);
        expect(rows[0]).toMatchObject({ event: 'ci-fix-outcome', outcome: 'ci-fix-failed', pr: 9 });
      });

      it('writes only a ci-fix-failed outcome row when dispatch throws', async () => {
        const repo = tempRepo(fixDirs);
        const { run } = sequencedGh([redView], { diff: '', merge: '' });
        const o = await autoReviewAndMergeOne(repo, 9, { fixer: async () => { throw new Error('ssh exploded'); } }, run);
        expect(o.action).toBe('ci-fix-failed');
        const rows = readLedgerRows(repo);
        expect(rows).toHaveLength(1);
        expect(rows[0]).toMatchObject({ event: 'ci-fix-outcome', outcome: 'ci-fix-failed', pr: 9 });
      });

      it('still-red sequences emit countable round-trip rows across multiple fix attempts', async () => {
        const repo = tempRepo(fixDirs);
        // Two consecutive still-red fix attempts: both dispatch, both outcome
        const { run } = sequencedGh([redView, redView, redView, redView], { diff: '', merge: '' });
        // First attempt: still-red
        let o = await autoReviewAndMergeOne(repo, 9, { fixer: async () => ({ ok: true, note: 'fix pushed' }) }, run);
        expect(o.action).toBe('ci-fix-failed');
        let rows = readLedgerRows(repo);
        expect(rows).toHaveLength(2);
        expect(rows[0]).toMatchObject({ event: 'ci-fix-dispatched' });
        expect(rows[1]).toMatchObject({ event: 'ci-fix-outcome', outcome: 'still-red' });
        // Second attempt: still-red again
        o = await autoReviewAndMergeOne(repo, 9, { fixer: async () => ({ ok: true, note: 'fix pushed' }) }, run);
        expect(o.action).toBe('ci-fix-failed');
        rows = readLedgerRows(repo);
        expect(rows).toHaveLength(4);
        // Each dispatch has a matching outcome (round-trip)
        const dispatched = rows.filter((r) => r.event === 'ci-fix-dispatched');
        const outcomes = rows.filter((r) => r.event === 'ci-fix-outcome');
        expect(dispatched).toHaveLength(2);
        expect(outcomes).toHaveLength(2);
        expect(outcomes.every((r) => r.outcome === 'still-red')).toBe(true);
      });
    });
  });
});

describe('defaultCiFixer', () => {
  const req: CiFixRequest = {
    repoPath: '/repo',
    pr: 42,
    taskId: 'TASK-fix-42',
    failedChecks: ['test=FAILURE'],
    prompt: 'Fix the failing CI checks on PR #42.',
  };

  afterEach(() => {
    delete process.env.DEVAGENT_REMOTE_TARGET;
    vi.restoreAllMocks();
  });

  it('short-circuits without DEVAGENT_REMOTE_TARGET and never imports remote transport', async () => {
    delete process.env.DEVAGENT_REMOTE_TARGET;
    const spy = vi.spyOn(await import('../src/remote.js'), 'runRemoteTask');
    const res = await defaultCiFixer(req);
    expect(res.ok).toBe(false);
    expect(res.note).toContain('DEVAGENT_REMOTE_TARGET');
    expect(spy).not.toHaveBeenCalled();
  });

  it('delegates to runRemoteTask with the target, prompt, and TASK-fix-<pr> id', async () => {
    process.env.DEVAGENT_REMOTE_TARGET = 'deploy@host:/srv/app';
    const runRemoteTask = vi.fn().mockResolvedValue({ ok: true, prUrl: 'https://github.com/o/r/pull/43', note: 'remote PR opened' });
    vi.doMock('../src/remote.js', () => ({ runRemoteTask }));
    try {
      const { defaultCiFixer: freshFixer } = await import('../src/integrations/autopr.js');
      const res = await freshFixer(req);
      expect(res.ok).toBe(true);
      expect(res.note).toBe('remote PR opened');
      expect(runRemoteTask).toHaveBeenCalledTimes(1);
      const [opts, deps] = runRemoteTask.mock.calls[0]!;
      expect(opts.target).toBe('deploy@host:/srv/app');
      expect(opts.prompt).toBe('Fix the failing CI checks on PR #42.');
      expect(opts.taskId).toBe('TASK-fix-42');
      expect(typeof opts.log.warn).toBe('function');
      expect(typeof deps.run).toBe('function');
    } finally {
      vi.doUnmock('../src/remote.js');
    }
  });
});

describe('sweepStalePrs', () => {
  /** Raw gh JSON for a listOpenPrs response. */
  function prJson(overrides: Record<string, unknown>): string {
    return JSON.stringify({
      number: 9,
      title: 'T',
      headRefName: 'devagent/x',
      baseRefName: 'main',
      state: 'OPEN',
      mergeable: 'MERGEABLE',
      reviewDecision: '',
      headRefOid: 'abc123',
      updatedAt: new Date().toISOString(),
      author: { login: 'devagent[bot]' },
      statusCheckRollup: [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }],
      ...overrides,
    });
  }

  const GREEN = [{ name: 'test', status: 'COMPLETED', conclusion: 'SUCCESS' }];
  const RED = [{ name: 'test', status: 'COMPLETED', conclusion: 'FAILURE' }];

  it('comments the superseded PR and leaves the green candidate untouched', async () => {
    // #9 is green and mergeable (candidate); #7 shares the base, is green but
    // conflicting — so its head is not a mergeable candidate.
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 7, title: 'old', headRefName: 'devagent/old', baseRefName: 'main',
          state: 'OPEN', mergeable: 'CONFLICTING', reviewDecision: '', headRefOid: 'old111',
          updatedAt: new Date().toISOString(), author: { login: 'devagent[bot]' },
          statusCheckRollup: GREEN,
        },
        {
          number: 9, title: 'new', headRefName: 'devagent/new', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'new222',
          updatedAt: new Date().toISOString(), author: { login: 'devagent[bot]' },
          statusCheckRollup: GREEN,
        },
      ]),
      comment: '',
    });
    const outcomes = await sweepStalePrs('/repo', { dryRun: false, graceDays: 7 }, run);
    expect(outcomes).toHaveLength(2);
    const o7 = outcomes.find((o) => o.pr === 7)!;
    expect(o7.action).toBe('superseded');
    expect(o7.detail).toContain('#9');
    expect(o7.detail).toContain('old111');
    const comment = calls.find((c) => c[1] === 'comment')!;
    expect(comment).toEqual(['pr', 'comment', '7', '--body', expect.stringContaining('#9')]);
    // The green candidate is untouched: no close, no second comment
    expect(calls.filter((c) => c[1] === 'close')).toHaveLength(0);
    expect(calls.filter((c) => c[1] === 'comment')).toHaveLength(1);
  });

  it('auto-closes a PR red across the grace window', async () => {
    const stale = new Date(Date.now() - 10 * 86_400_000).toISOString();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 5, title: 'stale red', headRefName: 'devagent/stale', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'f00d',
          updatedAt: stale, author: { login: 'devagent[bot]' },
          statusCheckRollup: RED,
        },
      ]),
      comment: '',
      close: '',
    });
    const outcomes = await sweepStalePrs('/repo', { dryRun: false, graceDays: 7 }, run);
    expect(outcomes[0]!.action).toBe('closed');
    expect(outcomes[0]!.detail).toContain('grace');
    const comment = calls.find((c) => c[1] === 'comment')!;
    expect(comment.join(' ')).toContain('grace');
    expect(calls.find((c) => c[1] === 'close')).toEqual(['pr', 'close', '5']);
  });

  it('auto-closes a red PR whose updatedAt is unparseable (unknown age counts as stale)', async () => {
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 6, title: 'red no timestamp', headRefName: 'devagent/nots', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'bad1',
          updatedAt: 'not-a-date', author: { login: 'devagent[bot]' },
          statusCheckRollup: RED,
        },
      ]),
    });
    const outcomes = await sweepStalePrs('/repo', { dryRun: false, graceDays: 7 }, run);
    expect(outcomes[0]!.action).toBe('closed');
    expect(calls.find((c) => c[1] === 'close')).toEqual(['pr', 'close', '6']);
  });

  it('leaves a PR red within the grace window untouched', async () => {
    const fresh = new Date(Date.now() - 1 * 86_400_000).toISOString();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 5, title: 'fresh red', headRefName: 'devagent/fresh', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'beef',
          updatedAt: fresh, author: { login: 'devagent[bot]' },
          statusCheckRollup: RED,
        },
      ]),
    });
    const outcomes = await sweepStalePrs('/repo', { dryRun: false, graceDays: 7 }, run);
    expect(outcomes[0]!.action).toBe('untouched');
    expect(outcomes[0]!.detail).toContain('within grace');
    expect(calls.filter((c) => c[1] === 'close')).toHaveLength(0);
  });

  it('dry-run reports verdicts without commenting or closing anything', async () => {
    const stale = new Date(Date.now() - 10 * 86_400_000).toISOString();
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 5, title: 'stale red', headRefName: 'devagent/stale', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'f00d',
          updatedAt: stale, author: { login: 'devagent[bot]' },
          statusCheckRollup: RED,
        },
        {
          number: 7, title: 'superseded', headRefName: 'devagent/sup', baseRefName: 'main',
          state: 'OPEN', mergeable: 'CONFLICTING', reviewDecision: '', headRefOid: 'dead',
          updatedAt: new Date().toISOString(), author: { login: 'devagent[bot]' },
          statusCheckRollup: GREEN,
        },
        {
          number: 9, title: 'candidate', headRefName: 'devagent/new', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'new222',
          updatedAt: new Date().toISOString(), author: { login: 'devagent[bot]' },
          statusCheckRollup: GREEN,
        },
      ]),
    });
    const outcomes = await sweepStalePrs('/repo', {}, run);
    expect(outcomes.map((o) => [o.pr, o.action])).toEqual([
      [5, 'closed'], [7, 'superseded'], [9, 'untouched'],
    ]);
    expect(outcomes[0]!.detail).toContain('[dry-run]');
    expect(outcomes[1]!.detail).toContain('[dry-run]');
    // nothing was written to GitHub
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
  });

  it('leaves PRs with pending or no checks untouched (no evidence either way)', async () => {
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 3, title: 'pending', headRefName: 'devagent/p', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'p3',
          updatedAt: new Date(Date.now() - 30 * 86_400_000).toISOString(),
          author: { login: 'devagent[bot]' },
          statusCheckRollup: [{ name: 'test', status: 'IN_PROGRESS', conclusion: null }],
        },
        {
          number: 4, title: 'no checks', headRefName: 'devagent/n', baseRefName: 'main',
          state: 'OPEN', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'n4',
          updatedAt: new Date(Date.now() - 30 * 86_400_000).toISOString(),
          author: { login: 'devagent[bot]' },
          statusCheckRollup: [],
        },
      ]),
    });
    const outcomes = await sweepStalePrs('/repo', { dryRun: false, graceDays: 0 }, run);
    expect(outcomes.map((o) => o.action)).toEqual(['untouched', 'untouched']);
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
  });

  it('skips PRs that are not open (state snapshot raced a close)', async () => {
    const { run, calls } = scriptedGh({
      list: JSON.stringify([
        {
          number: 2, title: 'closed mid-sweep', headRefName: 'devagent/x', baseRefName: 'main',
          state: 'CLOSED', mergeable: 'MERGEABLE', reviewDecision: '', headRefOid: 'c2',
          updatedAt: new Date(Date.now() - 30 * 86_400_000).toISOString(),
          author: { login: 'devagent[bot]' },
          statusCheckRollup: RED,
        },
      ]),
    });
    const outcomes = await sweepStalePrs('/repo', { dryRun: false, graceDays: 0 }, run);
    expect(outcomes[0]!.action).toBe('skipped');
    expect(outcomes[0]!.detail).toContain('CLOSED');
    expect(calls.filter((c) => c[1] === 'close' || c[1] === 'comment')).toHaveLength(0);
  });
});
