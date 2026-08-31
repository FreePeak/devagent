import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  autoReviewAndMergeOne,
  defaultCiFixer,
  evaluateAutoReview,
  evaluateChecks,
  scanAddedLinesForHazards,
  type CiFixRequest,
  type PrStatus,
  type RunGh,
} from '../src/integrations/autopr.js';

function prStatus(overrides: Partial<PrStatus> = {}): PrStatus {
  return {
    number: 9,
    title: 'Loop 48',
    headRefName: 'devagent/x',
    baseRefName: 'main',
    state: 'OPEN',
    mergeable: 'MERGEABLE',
    reviewDecision: '',
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
