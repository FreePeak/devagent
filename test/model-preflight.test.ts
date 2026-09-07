import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import { validateWorkerModel } from '../src/config.js';
import { getWorker } from '../src/workers/index.js';
import { createWorktree } from '../src/git/worktree.js';
import { buildDeps } from '../src/deps.js';
import { executeTask, instructionPayloadBytes, DEFAULT_MAX_PROMPT_BYTES } from '../src/orchestrator/executor.js';
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

describe('executeTask prompt-size preflight (Q18 / PRD:915)', () => {
  let savedMaxPrompt: string | undefined;
  beforeEach(() => {
    mockGetWorker.mockReset();
    mockCreate.mockReset();
    savedMaxPrompt = process.env.DEVAGENT_MAX_PROMPT_BYTES;
    delete process.env.DEVAGENT_MAX_PROMPT_BYTES;
  });
  afterEach(() => {
    if (savedMaxPrompt === undefined) delete process.env.DEVAGENT_MAX_PROMPT_BYTES;
    else process.env.DEVAGENT_MAX_PROMPT_BYTES = savedMaxPrompt;
  });

  // ASCII prompt of an exact byte length (instructionPayloadBytes counts only
  // prompt + boundaryConstraints + evidenceGaps, so a bare prompt measures
  // its own length).
  const taskWithPromptBytes = (n: number): OrchestratorTask => ({
    id: 'T1',
    title: 'T1',
    prompt: 'x'.repeat(n),
    dependsOn: [],
    status: 'ready',
    attempts: 0,
  });

  function board(t: OrchestratorTask): ProjectBoard {
    return {
      goal: 'test goal',
      createdAt: new Date().toISOString(),
      updatedAt: new Date().toISOString(),
      roles: { planner: 'pi', executor: 'pi', auditor: 'pi' },
      tasks: [t],
    };
  }

  async function runWithMaxBytes(dir: string, maxBytes: number, promptBytes: number) {
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ worker: 'pi', resilience: { maxPromptBytes: maxBytes } }));
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
      task: taskWithPromptBytes(promptBytes),
      board: board(taskWithPromptBytes(promptBytes)),
      repoPath: dir,
      timeoutMs: 1_000,
      log,
      executor: 'pi',
    });
    return { r, spawn };
  }

  it('refuses a prompt over the threshold: no worktree, no worker, failureClass "prompt-oversized"', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-oversized-'));
    try {
      const { r, spawn } = await runWithMaxBytes(dir, 100, 101);
      expect(r.ok).toBe(false);
      expect(r.failureClass).toBe('prompt-oversized');
      expect(r.detail).toMatch(/prompt oversized: 101 bytes > resilience\.maxPromptBytes=100/);
      expect(r.detail).toMatch(/plan-split/);
      expect(mockCreate).not.toHaveBeenCalled();
      expect(spawn).not.toHaveBeenCalled();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('allows a prompt exactly at the threshold (refuse only strictly over)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-at-'));
    try {
      const { r, spawn } = await runWithMaxBytes(dir, 100, 100);
      expect(r.failureClass).not.toBe('prompt-oversized');
      expect(mockCreate).toHaveBeenCalledTimes(1);
      expect(spawn).toHaveBeenCalledTimes(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('allows a prompt under the threshold', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-under-'));
    try {
      const { r, spawn } = await runWithMaxBytes(dir, 100, 99);
      expect(r.failureClass).not.toBe('prompt-oversized');
      expect(spawn).toHaveBeenCalledTimes(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('maxPromptBytes: 0 disables the guard (a huge prompt still dispatches)', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-off-'));
    try {
      const { r, spawn } = await runWithMaxBytes(dir, 0, 10_000);
      expect(r.failureClass).not.toBe('prompt-oversized');
      expect(spawn).toHaveBeenCalledTimes(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('honors DEVAGENT_MAX_PROMPT_BYTES over the config value', async () => {
    const saved = process.env.DEVAGENT_MAX_PROMPT_BYTES;
    process.env.DEVAGENT_MAX_PROMPT_BYTES = '50';
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-env-'));
    try {
      // config says 10_000 (would allow), env tightens to 50 (refuses 100 bytes)
      const { r } = await runWithMaxBytes(dir, 10_000, 100);
      expect(r.failureClass).toBe('prompt-oversized');
      expect(r.detail).toMatch(/maxPromptBytes=50/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
      if (saved === undefined) delete process.env.DEVAGENT_MAX_PROMPT_BYTES;
      else process.env.DEVAGENT_MAX_PROMPT_BYTES = saved;
    }
  });

  it('falls back to DEFAULT_MAX_PROMPT_BYTES when unset', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-exec-default-'));
    try {
      writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ worker: 'pi' }));
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
        task: taskWithPromptBytes(DEFAULT_MAX_PROMPT_BYTES + 1),
        board: board(taskWithPromptBytes(DEFAULT_MAX_PROMPT_BYTES + 1)),
        repoPath: dir,
        timeoutMs: 1_000,
        log,
        executor: 'pi',
      });
      expect(r.failureClass).toBe('prompt-oversized');
      expect(r.detail).toMatch(new RegExp(`maxPromptBytes=${DEFAULT_MAX_PROMPT_BYTES}`));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('instructionPayloadBytes counts prompt + boundaryConstraints + evidenceGaps, not criteria', () => {
    const t: OrchestratorTask = {
      id: 'T1',
      title: 'T1',
      prompt: 'aaaa', // 4
      boundaryConstraints: ['bbbb'], // + 1 sep + 4
      evidenceGaps: ['cccc'], // + 1 sep + 4
      acceptanceCriteria: ['z'.repeat(1000)], // excluded
      expectedOutput: 'y'.repeat(1000), // excluded
      dependsOn: [],
      status: 'ready',
      attempts: 0,
    };
    // 'aaaa\nbbbb\ncccc' = 4 + 1 + 4 + 1 + 4 = 14 bytes
    expect(instructionPayloadBytes(t)).toBe(14);
  });
});
