import { describe, expect, it } from 'vitest';
import { mkdtempSync, rmSync, existsSync, readFileSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { collectProgressAsync, trackOnce, readTrackerHeartbeat, renderProgressMarkdown } from '../src/tracker.js';
import { enqueueTask, claimTask, setTaskStatus } from '../src/queue.js';
import { writeHeartbeat } from '../src/scout.js';

type Runner = (
  cmd: string,
  args: string[],
  opts: { cwd: string; timeoutMs: number },
) => Promise<{ exitCode: number; stdout: string; stderr: string; timedOut: boolean }>;

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-track-'));
  mkdirSync(join(d, '.selfbuild'), { recursive: true });
  writeFileSync(join(d, '.selfbuild', 'ledger.jsonl'), '{"loop":1,"ts":"2026-08-25T00:00:00Z","status":"ok","goal":"seed"}\n');
  return d;
}

const okRunner: Runner = async (cmd, args) => {
  if (cmd === 'git') {
    return { exitCode: 0, stdout: 'abc1234 feat: x\ndef5678 fix: y\n', stderr: '', timedOut: false };
  }
  if (cmd === 'gh') {
    return {
      exitCode: 0,
      stdout: JSON.stringify([{ number: 41, title: 'Add thing', url: 'https://example/pr/41', state: 'OPEN' }]),
      stderr: '',
      timedOut: false,
    };
  }
  return { exitCode: 1, stdout: '', stderr: '', timedOut: false };
};

describe('tracker', () => {
  it('collects queue + scout + ledger + git + gh into a snapshot', async () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'T-1', title: 'one', goal: 'Goal: one' });
      enqueueTask(repo, { id: 'T-2', title: 'two', goal: 'Goal: two' });
      claimTask(repo, 'T-1', 'w1');
      setTaskStatus(repo, 'T-2', 'failed', 'boom');
      writeHeartbeat(repo, { lastStatus: 'ok', lastDetail: 'enqueued T-1', worker: 'opencode', intervalMinutes: 30, lastTaskId: 'T-1' });

      const snap = await collectProgressAsync({ repoPath: repo }, okRunner);
      expect(snap.queue.total).toBe(2);
      expect(snap.queue.claimed).toBe(1);
      expect(snap.queue.failed).toBe(1);
      expect(snap.scout?.alive).toBe(true);
      expect(snap.ledgerTail.some((l) => l.includes('seed'))).toBe(true);
      expect(snap.recentCommits).toContain('abc1234 feat: x');
      expect(snap.openPrs[0]).toContain('#41');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('trackOnce writes progress.md + progress.json + heartbeat', async () => {
    const repo = tmpRepo();
    try {
      enqueueTask(repo, { id: 'W-1', title: 'work', goal: 'Goal: work' });
      const r = await trackOnce({ repoPath: repo }, okRunner);
      expect(r.ok).toBe(true);
      expect(existsSync(join(repo, '.selfbuild', 'progress.md'))).toBe(true);
      expect(existsSync(join(repo, '.selfbuild', 'progress.json'))).toBe(true);
      expect(readFileSync(join(repo, '.selfbuild', 'progress.md'), 'utf8')).toContain('# DevAgent Self-Build Progress');
      expect(readFileSync(join(repo, '.selfbuild', 'progress.md'), 'utf8')).toContain('[pending] W-1');
      const hb = readTrackerHeartbeat(repo);
      expect(hb?.lastStatus).toBe('ok');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('degrades cleanly when git/gh fail and scout never ran', async () => {
    const repo = tmpRepo();
    try {
      const failRunner: Runner = async () => ({ exitCode: 127, stdout: '', stderr: 'not found', timedOut: false });
      const r = await trackOnce({ repoPath: repo }, failRunner);
      expect(r.ok).toBe(true);
      expect(r.snapshot?.recentCommits).toEqual([]);
      expect(r.snapshot?.openPrs).toEqual([]);
      expect(r.snapshot?.scout).toBeNull();
      expect(readTrackerHeartbeat(repo)?.lastStatus).toBe('ok');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('marks scout dead when heartbeat is stale (>6h)', async () => {
    const repo = tmpRepo();
    try {
      writeHeartbeat(repo, {
        lastRunAt: new Date(Date.now() - 7 * 3_600_000).toISOString(),
        lastStatus: 'ok',
        lastDetail: 'old',
        worker: 'opencode',
        intervalMinutes: 30,
      });
      const snap = await collectProgressAsync({ repoPath: repo }, okRunner);
      expect(snap.scout?.alive).toBe(false);
      expect(renderProgressMarkdown(snap)).toContain('worker=opencode');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('includes orchestrator board when .devagent-project.json exists', async () => {
    const repo = tmpRepo();
    try {
      writeFileSync(
        join(repo, '.devagent-project.json'),
        JSON.stringify({
          goal: 'Example goal',
          createdAt: new Date().toISOString(),
          updatedAt: new Date().toISOString(),
          roles: { planner: 'claude-code', executor: 'opencode' },
          tasks: [
            { id: 'T1', title: 'First', prompt: 'do t1', dependsOn: [], status: 'done', attempts: 1 },
            { id: 'T2', title: 'Second', prompt: 'do t2', dependsOn: ['T1'], status: 'blocked', attempts: 0, failureDetail: 'upstream blocked' },
          ],
        }),
      );
      const snap = await collectProgressAsync({ repoPath: repo }, okRunner);
      expect(snap.board?.counts.done).toBe(1);
      expect(snap.board?.counts.blocked).toBe(1);
      const md = renderProgressMarkdown(snap);
      expect(md).toContain('Orchestrator board');
      expect(md).toContain('Example goal');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});
