import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

/**
 * Executor wiring for gate G5: a browserCheck failure must take the same
 * repair pathway as a G1 failure, with the G5 detail text in the repair
 * prompt; a pass must not block completion and must surface evidence.
 */

const prompts: string[] = [];
const gitCommands: string[][] = [];
// Controls what the mocked runBrowserGate resolves to on each call.
let browserGateResults: Array<unknown> = [];

vi.mock('../src/workers/index.js', () => ({
  getWorker: () => ({
    spawn: async (opts: { prompt: string }) => {
      prompts.push(opts.prompt);
      return { exitCode: 0, stdout: '', stderr: '', timedOut: false, resultText: 'done' };
    },
  }),
}));

vi.mock('../src/workers/spawn-utils.js', () => ({
  spawnCli: async (_cmd: string, args: string[]) => {
    gitCommands.push(args);
    return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
  },
}));

vi.mock('../src/git/worktree.js', () => ({
  createWorktree: async () => ({ worktreePath: '/tmp/fake-wt', branchName: 'devagent/T-1' }),
  sanitizeTicketId: (id: string) => id.replace(/[^A-Za-z0-9\-_]/g, ''),
}));

vi.mock('../src/validation/test-gate.js', () => ({
  runTestGate: async () => ({ gate: 'G1-tests', passed: true, findings: [], detail: 'ok' }),
}));

vi.mock('../src/validation/browser-gate.js', () => ({
  runBrowserGate: async () => {
    const next = browserGateResults.shift();
    if (next === undefined) throw new Error('unexpected extra runBrowserGate call');
    return next;
  },
  readG5Evidence: () => undefined,
  g5EvidenceDir: (root: string, id: string) => join(root, '.devagent', 'runs', id, 'g5'),
}));

import { executeTask } from '../src/orchestrator/executor.js';
import { RunLogger } from '../src/logger.js';
import type { OrchestratorTask, ProjectBoard } from '../src/orchestrator/types.js';

function harness() {
  const repoPath = mkdtempSync(join(tmpdir(), 'da-exec-g5-'));
  const log = new RunLogger();
  const task = {
    id: 'T1',
    title: 't',
    prompt: 'p',
    status: 'in_progress',
    attempts: 0,
    recoveries: 0,
  } as unknown as OrchestratorTask;
  const board = { goal: 'g', tasks: [task] } as unknown as ProjectBoard;
  const cleanup = () => rmSync(repoPath, { recursive: true, force: true });
  return { repoPath, log, task, board, cleanup };
}

describe('executeTask G5 wiring', () => {
  beforeEach(() => {
    prompts.length = 0;
    gitCommands.length = 0;
    browserGateResults = [];
  });

  it('sends G5 failure detail into the repair prompt and succeeds on retry', async () => {
    const h = harness();
    try {
      const failResult = { gate: 'G5-browser', passed: false, findings: [], detail: 'FAIL text:checkout missing (not found in DOM)' };
      const passResult = { gate: 'G5-browser', passed: true, findings: [], detail: 'PASS text:checkout' };
      browserGateResults = [failResult, passResult];
      const r = await executeTask({
        task: h.task,
        board: h.board,
        repoPath: h.repoPath,
        timeoutMs: 1000,
        log: h.log,
        executor: 'opencode',
      });
      expect(r.ok).toBe(true);
      expect(prompts).toHaveLength(2);
      // Repair attempt carries the G5 detail text (same pathway as G1 failure)
      expect(prompts[1]).toContain('FAIL text:checkout missing');
      // Work committed after G5 passed on retry
      expect(gitCommands.some((a) => a[0] === 'commit')).toBe(true);
      expect(r.detail).toContain('PASS text:checkout');
    } finally {
      h.cleanup();
    }
  });

  it('fails the leg when G5 never passes and never commits', async () => {
    const h = harness();
    try {
      const failResult = { gate: 'G5-browser', passed: false, findings: [], detail: 'FAIL selector:#app' };
      browserGateResults = [failResult, failResult];
      const r = await executeTask({
        task: h.task,
        board: h.board,
        repoPath: h.repoPath,
        timeoutMs: 1000,
        log: h.log,
        executor: 'opencode',
      });
      expect(r.ok).toBe(false);
      expect(r.detail).toContain('test gate failed after executor attempts');
      expect(gitCommands.some((a) => a[0] === 'commit')).toBe(false);
    } finally {
      h.cleanup();
    }
  });
});
