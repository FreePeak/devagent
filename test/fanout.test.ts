import { describe, expect, it, vi, beforeEach } from 'vitest';
import type { WorkerResult } from '../src/types.js';
import { RunLogger } from '../src/logger.js';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

vi.mock('../src/workers/index.js', () => ({ getWorker: vi.fn() }));
vi.mock('../src/git/worktree.js', () => ({ createWorktree: vi.fn() }));

import { getWorker } from '../src/workers/index.js';
import { createWorktree } from '../src/git/worktree.js';
import { runFanout } from '../src/workers/fanout.js';
import { planFromTicket } from '../src/planner.js';

const mockGetWorker = vi.mocked(getWorker);
const mockCreate = vi.mocked(createWorktree);

const plan = planFromTicket({
  id: 'ENG-9',
  title: 'Add GET /health endpoint returning status JSON',
  description: 'Endpoint returns service status as JSON including uptime.',
  labels: [],
  acceptanceCriteria: ['returns 200'],
});

const result = (over: Partial<WorkerResult> = {}): WorkerResult => ({
  exitCode: 0,
  events: [],
  resultText: null,
  sessionId: null,
  durationMs: 1000,
  timedOut: false,
  ...over,
});

function workerMock(exitCode: number) {
  return { name: 'claude-code' as const, spawn: vi.fn().mockResolvedValue(result({ exitCode })) };
}

describe('runFanout', () => {
  beforeEach(() => {
    mockGetWorker.mockReset();
    mockCreate.mockReset();
  });

  const logDir = () => mkdtempSync(join(tmpdir(), 'da-fan-'));
  const logger = (dir: string) => new RunLogger(dir);

  it('prefers the leg whose tests pass', async () => {
    const dir = logDir();
    try {
      const claude = workerMock(0);
      const open = workerMock(0);
      mockGetWorker.mockImplementation((n) => (n === 'claude-code' ? claude : open) as never);
      mockCreate.mockImplementation(async (_r, id) => ({
        worktreePath: `/tmp/leg-${id}`,
        branch: `devagent/${id}`,
      }));
      const score = vi.fn().mockResolvedValue(false).mockResolvedValueOnce(true); // claude leg passes

      const winner = await runFanout(plan, ['claude-code', 'opencode'], logger(dir), {
        repoPath: '/tmp/repo',
        timeoutMs: 5000,
        scoreLeg: score,
      });
      expect(winner?.worker).toBe('claude-code');
      expect(score).toHaveBeenCalledTimes(2);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('returns null when every leg fails', async () => {
    const dir = logDir();
    try {
      mockGetWorker.mockReturnValue(workerMock(1) as never);
      mockCreate.mockRejectedValue(new Error('branch exists')); // forces cwd fallback, still exit 1
      const winner = await runFanout(plan, ['claude-code'], logger(dir), { repoPath: '/tmp/r', timeoutMs: 5000 });
      expect(winner).toBeNull();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('falls back to any usable leg when scoring is unavailable', async () => {
    const dir = logDir();
    try {
      mockGetWorker.mockImplementation((n) =>
        n === 'claude-code' ? workerMock(1) : workerMock(0),
      ) as never;
      mockCreate.mockRejectedValue(new Error('no worktrees here'));
      const winner = await runFanout(plan, ['claude-code', 'opencode'], logger(dir), {
        repoPath: '/tmp/r',
        timeoutMs: 5000,
      });
      expect(winner?.worker).toBe('opencode');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
