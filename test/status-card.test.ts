import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildStatusView, renderStatusCard, statusJson } from '../src/commands/status.js';
import { saveBoard, createBoard } from '../src/orchestrator/store.js';
import { enqueueTask } from '../src/queue.js';
import type { OrchestratorTask, ProjectBoard } from '../src/orchestrator/types.js';
import type { SessionPaneInfo } from '../src/integrations/herdr.js';

/**
 * FR-SIMPLE-03/04: `devagent status` renders the current phase + one next
 * action in the §20.8 card/chip language. Panes are injected (no herdr in
 * unit tests); the CLI smoke runs the real binary against a temp repo.
 */

/** Strip ANSI escapes so chip/box contiguity can be asserted on visible text. */
function plain(s: string): string {
  return s.replace(/\x1b\[[0-9;]*m/g, '');
}

const NO_PANES: SessionPaneInfo[] = [];

function pane(taskId: string, state: SessionPaneInfo['state']): SessionPaneInfo {
  return {
    taskId,
    role: 'worker',
    worker: 'omp',
    paneId: 'w1:p9',
    workspaceId: 'w1',
    label: `${taskId}-a1`,
    cwd: join(tmpdir(), '.devagent-worktrees', `${taskId}-a1`),
    agentStatus: state === 'running' ? 'working' : 'idle',
    state,
    startedAt: new Date().toISOString(),
  };
}

function task(over: Partial<OrchestratorTask> & Pick<OrchestratorTask, 'id' | 'status'>): OrchestratorTask {
  return {
    title: `Task ${over.id}`,
    prompt: 'do the thing',
    dependsOn: [],
    attempts: 1,
    ...over,
  } as OrchestratorTask;
}

function boardWith(tasks: OrchestratorTask[], goal = 'Add CSV export'): ProjectBoard {
  return createBoard(goal, tasks, { planner: 'omp', executor: 'omp', auditor: 'omp' });
}

let repoPath: string;
let logs: string[];

beforeEach(() => {
  repoPath = mkdtempSync(join(tmpdir(), 'devagent-status-repo-'));
  logs = [];
  vi.spyOn(console, 'log').mockImplementation((...a: unknown[]) => {
    logs.push(a.map(String).join(' '));
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  rmSync(repoPath, { recursive: true, force: true });
});

describe('buildStatusView (phase + one next action)', () => {
  it('not-started repo points at devagent init', async () => {
    const v = await buildStatusView(repoPath, NO_PANES);
    expect(v.phase).toBe('not started');
    expect(v.nextAction).toContain('devagent init');
    expect(v.boardExists).toBe(false);
  });

  it('configured repo without a board points at the one-sentence goal command', async () => {
    writeFileSync(join(repoPath, 'devagent.json'), JSON.stringify({ worker: 'omp' }));
    const v = await buildStatusView(repoPath, NO_PANES);
    expect(v.phase).toBe('not started');
    expect(v.nextAction).toContain('devagent orchestrate --goal');
  });

  it('running task with a live pane yields the literal attach hint (FR-VIS-02)', async () => {
    saveBoard(repoPath, boardWith([task({ id: 'T1', status: 'dispatched' })]));
    const v = await buildStatusView(repoPath, [pane('T1', 'running')]);
    expect(v.phase).toBe('implementing');
    expect(v.chipState).toBe('running');
    expect(v.attachHint).toBe('devagent attach T1');
    expect(v.currentTask?.id).toBe('T1');
  });

  it('running task without a pane falls back to the board hint', async () => {
    saveBoard(repoPath, boardWith([task({ id: 'T1', status: 'dispatched' })]));
    const v = await buildStatusView(repoPath, NO_PANES);
    expect(v.attachHint).toBeNull();
    expect(v.nextAction).toContain('devagent project');
  });

  it('ask task surfaces the answer next action; failed task points at inspection', async () => {
    saveBoard(repoPath, boardWith([task({ id: 'T1', status: 'ask' }), task({ id: 'T2', status: 'failed', failureDetail: 'tests red' })]));
    const ask = await buildStatusView(repoPath, NO_PANES);
    expect(ask.phase).toBe('paused for you');
    expect(ask.nextAction).toContain(`--answer T1=`);
    expect(ask.currentTask?.id).toBe('T1');

    saveBoard(repoPath, boardWith([task({ id: 'T2', status: 'failed', failureDetail: 'tests red' })]));
    const failed = await buildStatusView(repoPath, NO_PANES);
    expect(failed.phase).toBe('failed');
    expect(failed.chipState).toBe('failed');
    expect(failed.nextAction).toContain('devagent project');
  });

  it('all-done board reports completion and the next goal step', async () => {
    saveBoard(repoPath, boardWith([task({ id: 'T1', status: 'done' })]));
    const v = await buildStatusView(repoPath, NO_PANES);
    expect(v.phase).toBe('all done');
    expect(v.chipState).toBe('ok');
    expect(v.nextAction).toContain('state a new goal');
  });

  it('queued tasks without a board surface the queue phase', async () => {
    enqueueTask(repoPath, { id: 'SCOUT-1', title: 'research', goal: 'research the thing' });
    const v = await buildStatusView(repoPath, NO_PANES);
    expect(v.phase).toBe('queued');
    expect(v.queue.pending).toBe(1);
    expect(v.nextAction).toContain('workers claim queued tasks automatically');
  });

  it('does not throw when the board file is corrupt', async () => {
    writeFileSync(join(repoPath, '.devagent-project.json'), '{not json');
    const v = await buildStatusView(repoPath, NO_PANES);
    expect(v.boardExists).toBe(false);
  });
});

describe('renderStatusCard + statusJson (§20.8 language, --json opt-out)', () => {
  it('card renders chip + next line with attach hint in the §20.8 box language', async () => {
    saveBoard(repoPath, boardWith([task({ id: 'T1', status: 'dispatched' })]));
    const v = await buildStatusView(repoPath, [pane('T1', 'running')]);
    const card = plain(renderStatusCard(v).join('\n'));
    expect(card).toContain('╭─ Project status');
    expect(card).toContain('●'); // status chip dot
    expect(card).toContain('implementing');
    expect(card).toContain('next:');
    expect(card).toContain('devagent attach T1');
    expect(card).toContain('╰'); // rounded box footer
  });

  it('json emits the same view as machine data; no ANSI codes', async () => {
    saveBoard(repoPath, boardWith([task({ id: 'T1', status: 'dispatched' })]));
    const v = await buildStatusView(repoPath, [pane('T1', 'running')]);
    const parsed = JSON.parse(statusJson(v)) as Record<string, unknown>;
    expect(parsed).toMatchObject({
      phase: 'implementing',
      chipState: 'running',
      attachHint: 'devagent attach T1',
      boardExists: true,
      goal: 'Add CSV export',
    });
    expect(statusJson(v)).not.toContain('\x1b[');
    expect((parsed.taskCounts as Record<string, number>).dispatched).toBe(1);
  });
});

// ---------- CLI smoke: real binary in a temp repo (per task verification spec) ----------

describe('devagent status CLI (smoke)', () => {
  let cliDir: string;
  let cliHome: string;
  beforeAll(() => {
    cliDir = mkdtempSync(join(tmpdir(), 'devagent-status-cli-'));
    cliHome = mkdtempSync(join(tmpdir(), 'devagent-status-cli-home-'));
    writeFileSync(join(cliDir, 'devagent.json'), JSON.stringify({ worker: 'omp' }));
  });
  afterAll(() => {
    rmSync(cliDir, { recursive: true, force: true });
    rmSync(cliHome, { recursive: true, force: true });
  });

  function runCli(args: string[]): { out: string; code: number } {
    const { execFileSync } = require('node:child_process') as typeof import('node:child_process');
    try {
      const out = execFileSync('npx', ['tsx', 'src/cli.ts', 'status', ...args], {
        cwd: join(import.meta.dirname, '..'),
        // Hermetic: own DEVAGENT_HOME so the run table reads no real runs.
        env: { PATH: process.env.PATH, HOME: process.env.HOME, DEVAGENT_HOME: cliHome, DEVAGENT_HERDR_BIN: '/nonexistent-herdr' },
        stdio: 'pipe',
        timeout: 60_000,
      }).toString();
      return { out, code: 0 };
    } catch (err) {
      const e = err as { status?: number; stdout?: Buffer };
      return { out: e.stdout?.toString() ?? '', code: e.status ?? 1 };
    }
  }

  it('default output leads with the phase card, run table after', () => {
    const { out, code } = runCli(['--repo', cliDir]);
    expect(code).toBe(0);
    const text = plain(out);
    expect(text).toContain('╭─ Project status');
    expect(text.indexOf('Project status')).toBeLessThan(text.indexOf('No runs yet.'));
    expect(text).toContain('devagent orchestrate --goal');
  });

  it('--json emits parseable phase JSON', () => {
    const { out, code } = runCli(['--repo', cliDir, '--json']);
    expect(code).toBe(0);
    const parsed = JSON.parse(out) as Record<string, unknown>;
    expect(parsed.phase).toBe('not started');
    expect(parsed.boardExists).toBe(false);
    expect(parsed.nextAction).toContain('devagent orchestrate --goal');
  });
});
