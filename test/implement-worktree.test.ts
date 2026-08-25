import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

vi.mock('../src/workers/index.js', () => ({ getWorker: vi.fn() }));
vi.mock('../src/git/worktree.js', () => ({
  createWorktree: vi.fn(),
  isGitRepository: vi.fn(),
  finalizeRunWorktree: vi.fn().mockResolvedValue({ action: 'preserved', committed: false }),
}));

import { getWorker } from '../src/workers/index.js';
import { createWorktree, isGitRepository } from '../src/git/worktree.js';
import { buildDeps } from '../src/deps.js';
import { RunLogger } from '../src/logger.js';
import { planFromTicket } from '../src/planner.js';

const mockGetWorker = vi.mocked(getWorker);
const mockCreate = vi.mocked(createWorktree);
const mockIsRepo = vi.mocked(isGitRepository);

const plan = planFromTicket({
  id: 'ENG-42',
  title: 'Add GET /health endpoint returning status JSON',
  description: 'Endpoint returns service status as JSON including uptime.',
  labels: [],
  acceptanceCriteria: ['returns 200'],
});

function stageCfg(repoPath: string) {
  return {
    repoPath,
    maxLoops: 1,
    timeoutMs: 5000,
    worker: 'claude-code' as const,
    autoPr: false,
  };
}

describe('implementStage worktree isolation (FR-IMPL-01)', () => {
  beforeEach(() => {
    mockGetWorker.mockReset();
    mockCreate.mockReset();
    mockIsRepo.mockReset();
  });

  const logDir = () => mkdtempSync(join(tmpdir(), 'da-impl-'));
  const logger = (dir: string) => new RunLogger(dir);

  it('aborts with a clear error when worktree creation fails in a git repo', async () => {
    const dir = logDir();
    try {
      const repoPath = join(dir, 'repo');
      mockCreate.mockRejectedValue(new Error('branch "devagent/ENG-42" already exists'));
      mockIsRepo.mockResolvedValue(true);
      const spawn = vi.fn();
      mockGetWorker.mockReturnValue({ name: 'claude-code', spawn } as never);

      const cfg = stageCfg(repoPath);
      const log = logger(dir);
      const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
      await expect(d.implementStage!(cfg, plan, log)).rejects.toThrow(
        /worktree creation failed/,
      );
      // Worker never ran: no silent repo-root fallback
      expect(spawn).not.toHaveBeenCalled();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('still runs in the repo root when the path is not a git repository', async () => {
    const dir = logDir();
    try {
      mockCreate.mockRejectedValue(new Error('not a git repository'));
      mockIsRepo.mockResolvedValue(false);
      const spawn = vi.fn().mockResolvedValue({
        exitCode: 0,
        events: [],
        resultText: null,
        sessionId: null,
        durationMs: 1,
        timedOut: false,
      });
      mockGetWorker.mockReturnValue({ name: 'claude-code', spawn } as never);

      const log = logger(dir);
      const d = buildDeps({ linearApiKey: 'x' }, stageCfg(dir), log);
      const result = await d.implementStage!(stageCfg(dir), plan, log);
      expect(result.ok).toBe(true);
      expect(result.worktreePath).toBeUndefined();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
