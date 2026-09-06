import { describe, expect, it, afterAll } from 'vitest';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, readFileSync, existsSync, readdirSync, chmodSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  archiveBoard,
  countBoard,
  decideBoardRecovery,
  decidePostRequeue,
  formatTimestamp,
  formatVerdict,
  runBoardRecovery,
} from '../../src/orchestrator/board-recovery.js';

// PRD:888 Q19 — the orchestrator driver's board-recovery decisions
// (requeue_parked, completed-board archive, stuck/undispatchable archive
// thresholds, re-bridge fall-through) folded out of
// scripts/orchestrate-loop.sh into src/orchestrator/board-recovery.ts,
// exposed as `devagent board-recovery`. The verdict word is the contract
// (wait / requeue / archive, same thin-caller pattern as selfbuild-gate in
// loops 121/122), so the CLI tests spawn the real binary against fixture
// boards, mirroring test/orchestrator/selfbuild-gate.test.ts.

const repoRoot = join(import.meta.dirname, '..', '..');
const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function t(id: string, status: string, attempts = 2): Record<string, unknown> {
  return { id, title: id, prompt: 'p', dependsOn: [], status, attempts };
}

/** Repo fixture whose .devagent-project.json holds exactly `tasks`. */
function boardRepo(tasks: unknown[]): string {
  const repo = mkdtempSync(join(tmpdir(), 'da-board-'));
  dirs.push(repo);
  const board = {
    goal: 'ship the factory',
    createdAt: '2026-09-01T00:00:00Z',
    updatedAt: '2026-09-06T00:00:00Z',
    roles: { planner: 'omp', executor: 'omp' },
    tasks,
  };
  writeFileSync(join(repo, '.devagent-project.json'), JSON.stringify(board, null, 2) + '\n');
  return repo;
}

function readBoardFile(repo: string): { tasks: Array<{ id: string; status: string; attempts: number }> } {
  return JSON.parse(readFileSync(join(repo, '.devagent-project.json'), 'utf8'));
}

function archiveFiles(repo: string, prefix: string): string[] {
  const dir = join(repo, '.devagent', 'archive');
  if (!existsSync(dir)) return [];
  return readdirSync(dir).filter((f) => f.startsWith(prefix));
}

/** Fixed local clock -> deterministic archive stamps. */
const NOW = new Date(2026, 8, 7, 1, 2, 3); // 2026-09-07 01:02:03 local
const BASE = { parkedPolls: 6, requeueAfter: 6, pollSecs: 600, now: NOW };

function recovery(repo: string, ...args: string[]): { status: number | null; out: string } {
  const r = spawnSync('npx', ['tsx', join(repoRoot, 'src/cli.ts'), 'board-recovery', ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: { PATH: process.env.PATH, HOME: process.env.HOME },
    timeout: 60_000,
  });
  return { status: r.status, out: `${r.stdout}${r.stderr}` };
}

describe('countBoard (PRD:888 port of the board_* helpers)', () => {
  it('classifies done/open/stuck/pending the way the shell counted', () => {
    const counts = countBoard([
      t('a', 'done'),
      t('b', 'failed'),
      t('c', 'blocked'),
      t('d', 'pending'),
      t('e', 'ready'),
      t('f', 'dispatched'),
      t('g', 'untrusted'),
      t('h', 'ask'),
    ]);
    expect(counts).toEqual({ total: 8, done: 1, open: 5, stuck: 2, pending: 1 });
  });

  it('tolerates malformed rows: missing/odd statuses count as open, never crash', () => {
    const counts = countBoard([{}, { status: 42 }, t('x', 'done')] as never[]);
    expect(counts).toEqual({ total: 3, done: 1, open: 2, stuck: 0, pending: 0 });
  });
});

describe('decideBoardRecovery (PRD:888 port of the parked/completed block)', () => {
  const parked = { total: 3, done: 1, open: 0, stuck: 2, pending: 0 };

  it('a fully-done board takes the infinity-cycle archive', () => {
    expect(decideBoardRecovery({ total: 4, done: 4, open: 0, stuck: 0, pending: 0 }, BASE)).toEqual({
      kind: 'archive-complete',
    });
  });

  it('below the threshold the board waits, counter and sleep quoted verbatim', () => {
    const v = decideBoardRecovery(parked, { ...BASE, parkedPolls: 2 });
    expect(v).toEqual({ kind: 'wait', reason: '2 task(s) failed/blocked (2/6); sleeping 600s' });
  });

  it('requeue-after 0 parks forever: wait with the disabled note, never requeue', () => {
    const v = decideBoardRecovery(parked, { ...BASE, requeueAfter: 0 });
    expect(v).toEqual({ kind: 'wait', reason: '2 task(s) failed/blocked; sleeping 600s (requeue disabled)' });
  });

  it('at the threshold the gate requeues first', () => {
    expect(decideBoardRecovery(parked, { ...BASE, parkedPolls: 6 })).toEqual({ kind: 'requeue-now' });
  });

  it('defensive: open work or an empty board verdicts wait, never an action', () => {
    expect(decideBoardRecovery({ total: 2, done: 0, open: 1, stuck: 1, pending: 0 }, BASE)).toEqual({
      kind: 'wait',
      reason: 'board has 1 open task(s)',
    });
    expect(decideBoardRecovery({ total: 0, done: 0, open: 0, stuck: 0, pending: 0 }, BASE)).toEqual({
      kind: 'wait',
      reason: 'board has no tasks',
    });
  });
});

describe('decidePostRequeue (PRD:888 stuck-archive thresholds)', () => {
  it('still-stuck after the reset archives with the failed/blocked tally', () => {
    const v = decidePostRequeue({ total: 3, done: 0, open: 1, stuck: 2, pending: 1 });
    expect(v).toEqual({ kind: 'archive', detail: 'board stuck (2 failed/blocked)' });
  });

  it('every task pending means the scheduler cannot dispatch: archive for the bridge', () => {
    expect(decidePostRequeue({ total: 2, done: 0, open: 0, stuck: 0, pending: 2 })).toEqual({
      kind: 'archive',
      detail: 'board all-pending but undispatchable',
    });
  });

  it('a reset board with done work re-dispatches: requeue verdict, no archive', () => {
    expect(decidePostRequeue({ total: 3, done: 1, open: 0, stuck: 0, pending: 2 })).toEqual({ kind: 'requeue' });
  });
});

describe('formatTimestamp / formatVerdict', () => {
  it('stamps like `date +%Y%m%d-%H%M%S` in local time, zero-padded', () => {
    expect(formatTimestamp(new Date(2026, 0, 5, 9, 8, 7))).toBe('20260105-090807');
  });

  it('renders the verdict word before the first colon (the shell parses on it)', () => {
    expect(formatVerdict({ action: 'archive', reason: 'board stuck' })).toBe('archive: board stuck');
  });
});

describe('runBoardRecovery on fixture boards (each decision branch)', () => {
  it('completed board: archived as board-<stamp>.json, verdict wait (next cycle re-bridges)', () => {
    const repo = boardRepo([t('a', 'done'), t('b', 'done')]);
    const v = runBoardRecovery(repo, BASE);
    expect(v.action).toBe('wait');
    expect(v.reason).toBe('board complete (2 done); archived to .devagent/archive/board-20260907-010203.json');
    expect(existsSync(join(repo, '.devagent-project.json'))).toBe(false);
    expect(archiveFiles(repo, 'board-2026')).toEqual(['board-20260907-010203.json']);
  });

  it('completed board prunes merged worktrees through the repo script, safe-gated on executability', () => {
    const repo = boardRepo([t('a', 'done')]);
    mkdirSync(join(repo, 'scripts'), { recursive: true });
    const script = join(repo, 'scripts', 'git-cleanup-merged.sh');
    writeFileSync(script, '#!/bin/sh\ntouch "$2/.cleanup-ran"\nexit 0\n');
    chmodSync(script, 0o755);
    runBoardRecovery(repo, BASE);
    expect(existsSync(join(repo, '.cleanup-ran'))).toBe(true);

    const repo2 = boardRepo([t('a', 'done')]);
    mkdirSync(join(repo2, 'scripts'), { recursive: true });
    writeFileSync(join(repo2, 'scripts', 'git-cleanup-merged.sh'), 'not executable');
    runBoardRecovery(repo2, BASE);
    expect(existsSync(join(repo2, '.cleanup-ran'))).toBe(false);
  });

  it('parked below threshold: wait, board untouched byte-for-byte', () => {
    const repo = boardRepo([t('a', 'done'), t('b', 'failed'), t('c', 'blocked')]);
    const before = readFileSync(join(repo, '.devagent-project.json'), 'utf8');
    const v = runBoardRecovery(repo, { ...BASE, parkedPolls: 3 });
    expect(v).toEqual({ action: 'wait', reason: '2 task(s) failed/blocked (3/6); sleeping 600s' });
    expect(readFileSync(join(repo, '.devagent-project.json'), 'utf8')).toBe(before);
    expect(archiveFiles(repo, 'board-')).toEqual([]);
  });

  it('requeue disabled: wait with the disabled note, board untouched', () => {
    const repo = boardRepo([t('a', 'failed')]);
    const v = runBoardRecovery(repo, { ...BASE, requeueAfter: 0 });
    expect(v).toEqual({ action: 'wait', reason: '1 task(s) failed/blocked; sleeping 600s (requeue disabled)' });
    expect(readBoardFile(repo).tasks[0]?.status).toBe('failed');
  });

  it('threshold with done work: requeues in place (pending, attempts reset), verdict requeue', () => {
    const repo = boardRepo([t('a', 'done'), t('b', 'failed', 5), t('c', 'blocked', 3)]);
    const v = runBoardRecovery(repo, BASE);
    expect(v).toEqual({ action: 'requeue', reason: 'reset 2 parked task(s) to pending; sleeping 600s' });
    const tasks = readBoardFile(repo).tasks;
    expect(tasks.map((x) => x.status)).toEqual(['done', 'pending', 'pending']);
    expect(tasks.map((x) => x.attempts)).toEqual([2, 0, 0]);
    expect(archiveFiles(repo, 'board-')).toEqual([]);
  });

  it('threshold on an all-failed board: requeue then undispatchable archive, verdict archive', () => {
    const repo = boardRepo([t('a', 'failed', 4), t('b', 'blocked', 9)]);
    const v = runBoardRecovery(repo, BASE);
    expect(v.action).toBe('archive');
    expect(v.reason).toBe(
      'reset 2 parked task(s) to pending; board all-pending but undispatchable; ' +
        'archived to .devagent/archive/board-stuck-20260907-010203.json',
    );
    expect(existsSync(join(repo, '.devagent-project.json'))).toBe(false);
    expect(archiveFiles(repo, 'board-stuck-')).toEqual(['board-stuck-20260907-010203.json']);
    // the archive keeps the requeued shape — the shell wrote the reset before moving the file
    const archived = JSON.parse(
      readFileSync(join(repo, '.devagent', 'archive', 'board-stuck-20260907-010203.json'), 'utf8'),
    );
    expect(archived.tasks.map((x: { status: string; attempts: number }) => [x.status, x.attempts])).toEqual([
      ['pending', 0],
      ['pending', 0],
    ]);
  });

  it('missing or corrupt board reads as wait (the shell guard keeps the gate out; conservative fallback)', () => {
    const empty = mkdtempSync(join(tmpdir(), 'da-board-empty-'));
    dirs.push(empty);
    expect(runBoardRecovery(empty, BASE)).toEqual({ action: 'wait', reason: 'board unreadable' });
    const corrupt = boardRepo([t('a', 'done')]);
    writeFileSync(join(corrupt, '.devagent-project.json'), '{not json');
    expect(runBoardRecovery(corrupt, BASE)).toEqual({ action: 'wait', reason: 'board unreadable' });
  });

  it('archiveBoard stamps the filename and preserves the board content', () => {
    const repo = boardRepo([t('a', 'done')]);
    const rel = archiveBoard(repo, join(repo, '.devagent-project.json'), 'board', '20260907-010203');
    expect(rel).toBe(join('.devagent', 'archive', 'board-20260907-010203.json'));
    expect(readFileSync(join(repo, rel), 'utf8')).toContain('"goal": "ship the factory"');
  });
});

describe('board-recovery CLI (verdict contract: wait / requeue / archive)', () => {
  it('prints the archive verdict on a fixture board at the threshold, exit 0', () => {
    const repo = boardRepo([t('a', 'failed'), t('b', 'blocked')]);
    const r = recovery(repo, '--repo', repo, '--parked-polls', '6', '--requeue-after', '6', '--poll-secs', '600');
    expect(r.status).toBe(0);
    expect(r.out).toContain('archive: reset 2 parked task(s) to pending; board all-pending but undispatchable');
    expect(r.out).toContain('archived to .devagent/archive/board-stuck-');
  });

  it('prints the wait verdict below the threshold, exit 0', () => {
    const repo = boardRepo([t('a', 'done'), t('b', 'failed')]);
    const r = recovery(repo, '--repo', repo, '--parked-polls', '1');
    expect(r.status).toBe(0);
    expect(r.out).toContain('wait: 1 task(s) failed/blocked (1/6); sleeping 600s');
  });

  it('performs the requeue write through the CLI, verdict requeue', () => {
    const repo = boardRepo([t('a', 'done'), t('b', 'failed', 7)]);
    const r = recovery(repo, '--repo', repo, '--parked-polls', '6');
    expect(r.status).toBe(0);
    expect(r.out).toContain('requeue: reset 1 parked task(s) to pending');
    expect(readBoardFile(repo).tasks[1]?.status).toBe('pending');
  });

  it('a missing board is a wait verdict, not a crash (exit 0)', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-board-none-'));
    dirs.push(repo);
    const r = recovery(repo, '--repo', repo, '--parked-polls', '6');
    expect(r.status).toBe(0);
    expect(r.out).toContain('wait: board unreadable');
  });

  it('unresolved (exit 2): negative or non-numeric counters — the shell must fall back to wait', () => {
    const repo = boardRepo([t('a', 'failed')]);
    expect(recovery(repo, '--repo', repo, '--parked-polls', '-1').status).toBe(2);
    expect(recovery(repo, '--repo', repo, '--requeue-after', 'abc').status).toBe(2);
    // and the gate acted on nothing
    expect(readBoardFile(repo).tasks[0]?.status).toBe('failed');
  });
});

describe('orchestrate-loop.sh wiring (PRD:888 Q19 thin caller)', () => {
  const script = readFileSync(join(repoRoot, 'scripts', 'orchestrate-loop.sh'), 'utf8');

  it('dispatches the gate instead of embedding the recovery decisions', () => {
    expect(script).toContain(
      `RECOVERY="$("${'${DEVAGENT[@]}'}" board-recovery --repo "$REPO" --parked-polls "$parked_polls" --requeue-after "$REQUEUE_AFTER" --poll-secs "$POLL_SECS" 2>&1)" || RC=$?`,
    );
    expect(script).not.toContain('requeue_parked');
    expect(script).not.toContain('cleanup_merged_worktrees');
    expect(script).not.toContain('ARCHIVED');
  });

  it('honors a verdict word only with rc 0 — a crashed gate must not archive or hot-loop', () => {
    expect(script).toContain(`[ "$RC" -eq 0 ] && VERDICT="${'${RECOVERY%%:*}'}"`);
    expect(script).toContain('case "$VERDICT" in');
    expect(script).toContain('[recovery] board-recovery gate unresolved');
  });

  it('maps the verdicts: archive falls through to the bridge, wait/requeue sleep', () => {
    const archiveArm = script.slice(script.indexOf('case "$VERDICT" in'), script.indexOf('requeue)'));
    expect(archiveArm).toContain('parked_polls=0');
    expect(archiveArm).not.toContain('sleep');
    const requeueArm = script.slice(script.indexOf('requeue)'), script.indexOf('wait)'));
    expect(requeueArm).toContain('parked_polls=0');
    expect(requeueArm).toContain('sleep "$POLL_SECS"');
    expect(requeueArm).toContain('continue');
  });
});
