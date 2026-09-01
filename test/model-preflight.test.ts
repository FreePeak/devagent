import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { validateWorkerModel } from '../src/config.js';
import { getWorker } from '../src/workers/index.js';
import { createWorktree } from '../src/git/worktree.js';
import { buildDeps } from '../src/deps.js';
import { executeTask } from '../src/orchestrator/executor.js';
import { RunLogger } from '../src/logger.js';
import { planFromTicket } from '../src/planner.js';
import type { OrchestratorTask, ProjectBoard } from '../src/orchestrator/types.js';

vi.mock('../src/workers/index.js', () => ({ getWorker: vi.fn() }));
vi.mock('../src/git/worktree.js', () => ({
  createWorktree: vi.fn(),
  isGitRepository: vi.fn(),
  finalizeRunWorktree: vi.fn().mockResolvedValue({ action: 'preserved', committed: false }),
  sanitizeTicketId: (id: string) => id.replace(/[^A-Za-z0-9\-_]/g, ''),
}));

const mockGetWorker = vi.mocked(getWorker);
const mockCreate = vi.mocked(createWorktree);

const plan = planFromTicket({
  id: 'ENG-99',
  title: 'Add GET /health endpoint returning status JSON',
  description: 'Endpoint returns service status as JSON including uptime.',
  labels: [],
  acceptanceCriteria: ['returns 200'],
});

describe('validateWorkerModel (dispatch model-id gate, PRD Q32)', () => {
  it('accepts unset/empty model for every adapter (adapter default applies)', () => {
    for (const w of ['omp', 'pi', 'claude-code', 'opencode'] as const) {
      expect(validateWorkerModel(w, undefined)).toBeNull();
      expect(validateWorkerModel(w, '')).toBeNull();
      expect(validateWorkerModel(w, '   ')).toBeNull();
    }
  });

  it('accepts provider-qualified ids for omp and pi', () => {
    expect(validateWorkerModel('omp', 'omniroute/bai/glm-5.3-flash')).toBeNull();
    expect(validateWorkerModel('pi', 'openai/gpt-4o')).toBeNull();
    // multi-segment provider paths are real omp ids — still qualified
    expect(validateWorkerModel('omp', ' a/b ')).toBeNull();
  });

  it('rejects driver tier aliases (no "/") for omp and pi with an actionable reason', () => {
    for (const w of ['omp', 'pi'] as const) {
      const problem = validateWorkerModel(w, 'coding');
      expect(problem).not.toBeNull();
      expect(problem).toContain('provider-qualified');
      expect(problem).toContain('coding');
      expect(problem).toContain(`"${w}"`);
    }
  });

  it('passes any model through for claude-code and opencode (adapter owns id semantics)', () => {
    expect(validateWorkerModel('claude-code', 'coding')).toBeNull();
    expect(validateWorkerModel('claude-code', 'claude-sonnet-4-5')).toBeNull();
    expect(validateWorkerModel('opencode', 'anything-goes')).toBeNull();
  });
});

describe('implementStage model preflight', () => {
  beforeEach(() => {
    mockGetWorker.mockReset();
    mockCreate.mockReset();
  });

  it('fails fast with failureClass "config" before any worker spend (pi + alias model)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-preflight-'));
    try {
      const spawn = vi.fn();
      mockGetWorker.mockReturnValue({ name: 'pi', spawn } as never);
      const log = new RunLogger(dir);
      const d = buildDeps({ linearApiKey: 'x' }, { repoPath: dir, autoPr: false }, log);
      const result = await d.implementStage!(
        { repoPath: dir, maxLoops: 3, timeoutMs: 5_000, worker: 'pi', autoPr: false, model: 'coding' },
        plan,
        log,
      );
      expect(result.ok).toBe(false);
      expect(result.failureClass).toBe('config');
      expect(result.attempts).toBe(0);
      // No worktree created, no worker launched: the gate stopped it in seconds.
      expect(mockCreate).not.toHaveBeenCalled();
      expect(spawn).not.toHaveBeenCalled();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('lets a provider-qualified model through to dispatch (pi)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-preflight-'));
    try {
      mockCreate.mockResolvedValue({ worktreePath: join(dir, 'wt'), branch: 'devagent/ENG-99' });
      const spawn = vi.fn().mockResolvedValue({
        exitCode: 0,
        events: [],
        resultText: 'ok',
        sessionId: null,
        durationMs: 1,
        timedOut: false,
      });
      mockGetWorker.mockReturnValue({ name: 'pi', spawn } as never);
      const log = new RunLogger(dir);
      const d = buildDeps({ linearApiKey: 'x' }, { repoPath: dir, autoPr: false }, log);
      const result = await d.implementStage!(
        {
          repoPath: dir,
          maxLoops: 1,
          timeoutMs: 5_000,
          worker: 'pi',
          autoPr: false,
          model: 'omniroute/bai/glm-5.3-flash',
        },
        plan,
        log,
      );
      expect(result.ok).toBe(true);
      expect(spawn).toHaveBeenCalledTimes(1);
      // Forwarded argv must carry the qualified id (adapter contract).
      const spawnOpts = spawn.mock.calls[0]![0] as { model?: string };
      expect(spawnOpts.model).toBe('omniroute/bai/glm-5.3-flash');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('passes alias models through untouched for pass-through adapters (claude-code)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-preflight-'));
    try {
      mockCreate.mockResolvedValue({ worktreePath: join(dir, 'wt'), branch: 'devagent/ENG-99' });
      const spawn = vi.fn().mockResolvedValue({
        exitCode: 0,
        events: [],
        resultText: 'ok',
        sessionId: null,
        durationMs: 1,
        timedOut: false,
      });
      mockGetWorker.mockReturnValue({ name: 'claude-code', spawn } as never);
      const log = new RunLogger(dir);
      const d = buildDeps({ linearApiKey: 'x' }, { repoPath: dir, autoPr: false }, log);
      const result = await d.implementStage!(
        { repoPath: dir, maxLoops: 1, timeoutMs: 5_000, worker: 'claude-code', autoPr: false, model: 'coding' },
        plan,
        log,
      );
      expect(result.ok).toBe(true);
      expect(spawn).toHaveBeenCalledTimes(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('executeTask model preflight (orchestrator dispatch path)', () => {
  beforeEach(() => {
    mockGetWorker.mockReset();
    mockCreate.mockReset();
  });

  function task(): OrchestratorTask {
    return { id: 'T1', title: 'T1', prompt: 'do it', dependsOn: [], status: 'ready', attempts: 0 };
  }

  function board(): ProjectBoard {
    return {
      goal: 'test goal',
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      roles: { planner: 'pi', executor: 'pi', auditor: 'pi' },
      tasks: [task()],
    };
  }

  it('fails at the gate in seconds: no worktree, no worker, failureClass "config"', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-preflight-'));
    try {
      // config.model carries the driver tier alias that loop 58 burned 3 attempts on
      writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ worker: 'pi', model: 'coding' }));
      const spawn = vi.fn();
      mockGetWorker.mockReturnValue({ name: 'pi', spawn } as never);
      const log = new RunLogger(dir);
      const r = await executeTask({
        task: task(),
        board: board(),
        repoPath: dir,
        timeoutMs: 1_000,
        log,
        executor: 'pi',
      });
      expect(r.ok).toBe(false);
      expect(r.failureClass).toBe('config');
      expect(r.detail).toMatch(/dispatch preflight.*provider-qualified/);
      expect(mockCreate).not.toHaveBeenCalled();
      expect(spawn).not.toHaveBeenCalled();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('does not block a provider-qualified model (proceeds past preflight)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-preflight-'));
    try {
      writeFileSync(
        join(dir, 'devagent.json'),
        JSON.stringify({ worker: 'pi', model: 'omniroute/bai/glm-5.3-flash' }),
      );
      const spawn = vi.fn().mockResolvedValue({
        exitCode: 0,
        events: [],
        resultText: 'done',
        sessionId: null,
        durationMs: 1,
        timedOut: false,
      });
      mockGetWorker.mockReturnValue({ name: 'pi', spawn } as never);
      mockCreate.mockResolvedValue({ worktreePath: join(dir, 'wt'), branch: 'devagent/T1-a1' });
      const log = new RunLogger(dir);
      const r = await executeTask({
        task: task(),
        board: board(),
        repoPath: dir,
        timeoutMs: 1_000,
        log,
        executor: 'pi',
      });
      // Past preflight: the worker ran (final verdict depends on the real gate
      // machinery, which is out of scope here — the assertion is that
      // preflight did not reject a qualified id).
      expect(r.failureClass).not.toBe('config');
      expect(spawn).toHaveBeenCalledTimes(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
