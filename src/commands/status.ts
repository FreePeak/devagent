/**
 * Human-readable status card (PRD §21 FR-SIMPLE-03/04): `devagent status`
 * renders the current phase + the one next action in the §20.8 card/chip
 * visual language, reusing src/tui/tui.ts helpers. The renderer composes
 * only existing state — the orchestrator board (.devagent-project.json via
 * loadBoard), the queue, and the herdr pane roster (FR-VIS-02) — no second
 * status system. Machine consumers read the same view as JSON via --json.
 */
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { listSessionPanes } from '../integrations/herdr.js';
import { loadBoard } from '../orchestrator/store.js';
import type { OrchestratorTask, ProjectBoard } from '../orchestrator/types.js';
import { taskCount } from '../queue.js';
import type { SessionPaneInfo } from '../integrations/herdr.js';
import { boxLines, chipFor, cyan, dim, truncate } from '../tui/tui.js';

/** Aggregate view backing both the human card and the --json payload. */
export interface StatusView {
  /** Chip phase label (plain language, ≤18 chars). */
  phase: string;
  /** Chip state driving the dot color (running/ok/idle/failed). */
  chipState: 'running' | 'ok' | 'idle' | 'failed';
  /** The one next action, in the §20.8 progressive-disclosure sense. */
  nextAction: string;
  /** Literal `devagent attach <taskId>` when a live pane exists. */
  attachHint: string | null;
  /** One-line context: current task title, counts, or goal. */
  detail: string;
  boardExists: boolean;
  goal: string | null;
  currentTask: { id: string; title: string; status: OrchestratorTask['status'] } | null;
  taskCounts: Record<string, number>;
  queue: { total: number; pending: number; claimed: number; done: number; failed: number };
}

/** First task in scheduler wave order for a given status. */
function firstTask(board: ProjectBoard | null, statuses: string[]): OrchestratorTask | null {
  if (!board) return null;
  for (const s of statuses) {
    const t = board.tasks.find((x) => x.status === s);
    if (t) return t;
  }
  return null;
}

/** One plain-language next action for a dispatched/running board. */
function boardNextAction(board: ProjectBoard, running: OrchestratorTask | null, pane: SessionPaneInfo | null): { nextAction: string; attachHint: string | null } {
  if (running && pane) {
    return { nextAction: 'watch the worker', attachHint: `devagent attach ${running.id}` };
  }
  if (running) {
    return { nextAction: 'watch the board: devagent project', attachHint: null };
  }
  const ask = firstTask(board, ['ask']);
  if (ask) return { nextAction: `answer the paused task: devagent orchestrate --goal "" --resume --answer ${ask.id}="..."`, attachHint: null };
  const failed = firstTask(board, ['failed']);
  if (failed) return { nextAction: `inspect the failure: devagent project (task ${failed.id})`, attachHint: null };
  if (board.tasks.some((t) => t.status === 'untrusted')) return { nextAction: 'nothing — the auditor is verifying finished work', attachHint: null };
  return { nextAction: 'nothing — executors pick up the remaining tasks automatically', attachHint: null };
}

/**
 * Compose the status view from existing state (FR-SIMPLE-03/04): current
 * phase + one next action. Never throws — missing state degrades to the
 * not-started view. `panes` is an injection seam for tests; omitted, the
 * live herdr roster is read (empty when herdr is unavailable).
 */
export async function buildStatusView(repoPath: string, panes?: SessionPaneInfo[]): Promise<StatusView> {
  const roster = panes ?? (await listSessionPanes());
  const board = loadBoard(repoPath);
  const queue = taskCount(repoPath);

  if (!board) {
    const pending = queue.pending ?? 0;
    if (pending > 0) {
      return {
        phase: 'queued', chipState: 'idle',
        nextAction: 'nothing — workers claim queued tasks automatically',
        attachHint: null,
        detail: `${pending} task(s) waiting in the queue`,
        boardExists: false, goal: null, currentTask: null,
        taskCounts: {}, queue,
      };
    }
    const hasConfig = existsSync(join(repoPath, 'devagent.json')) || existsSync(join(repoPath, '.devagent.json'));
    return {
      phase: 'not started', chipState: 'idle',
      nextAction: hasConfig
        ? 'state your goal in one sentence: devagent orchestrate --goal "..."'
        : 'run devagent init to set up this repository',
      attachHint: null,
      detail: hasConfig ? 'config found — no goal dispatched yet' : 'no setup yet',
      boardExists: false, goal: null, currentTask: null,
      taskCounts: {}, queue,
    };
  }

  const counts: Record<string, number> = {};
  for (const t of board.tasks) counts[t.status] = (counts[t.status] ?? 0) + 1;
  const running = firstTask(board, ['dispatched', 'untrusted']);
  const failed = firstTask(board, ['failed']);
  const ask = firstTask(board, ['ask']);
  const allDone = board.tasks.length > 0 && board.tasks.every((t) => t.status === 'done');
  const pane = running ? (roster.find((p) => p.taskId === running.id && p.state === 'running') ?? null) : null;

  const base = { boardExists: true, goal: board.goal, taskCounts: counts, queue };
  if (allDone) {
    return {
      ...base,
      phase: 'all done', chipState: 'ok',
      nextAction: 'state a new goal: devagent orchestrate --goal "..."',
      attachHint: null,
      detail: `goal: ${truncate(board.goal, 60)}`,
      currentTask: null,
    };
  }
  if (ask) {
    return {
      ...base,
      phase: 'paused for you', chipState: 'idle',
      ...boardNextAction(board, null, null),
      detail: `task ${ask.id} needs your answer: ${truncate(ask.title, 50)}`,
      currentTask: { id: ask.id, title: ask.title, status: ask.status },
    };
  }
  if (failed) {
    return {
      ...base,
      phase: 'failed', chipState: 'failed',
      ...boardNextAction(board, null, null),
      detail: `task ${failed.id}: ${truncate(failed.failureDetail ?? failed.title, 60)}`,
      currentTask: { id: failed.id, title: failed.title, status: failed.status },
    };
  }
  if (running) {
    return {
      ...base,
      phase: running.status === 'untrusted' ? 'awaiting audit' : 'implementing', chipState: 'running',
      ...boardNextAction(board, running, pane),
      detail: `${running.id}: ${truncate(running.title, 50)}`,
      currentTask: { id: running.id, title: running.title, status: running.status },
    };
  }
  return {
    ...base,
    phase: 'implementing', chipState: 'running',
    ...boardNextAction(board, null, null),
    detail: `${board.tasks.length} task(s) on the board`,
    currentTask: null,
  };
}

/** §20.8 card lines: chip + next action (+ attach hint) in a rounded box. */
export function renderStatusCard(view: StatusView): string[] {
  const width = Math.max(46, Math.min(process.stdout?.columns ?? 100, 100));
  const next = view.attachHint
    ? `next: ${view.nextAction} — ${cyan(view.attachHint)}`
    : `next: ${view.nextAction}`;
  return boxLines('Project status', [` ${chipFor(view.chipState, view.phase)}  ${dim(view.detail)}`, ` ${next}`], width);
}

/** The --json payload for scripts (FR-SIMPLE-03 machine format). */
export function statusJson(view: StatusView): string {
  return JSON.stringify(
    {
      phase: view.phase,
      chipState: view.chipState,
      goal: view.goal,
      boardExists: view.boardExists,
      currentTask: view.currentTask,
      taskCounts: view.taskCounts,
      queue: view.queue,
      nextAction: view.nextAction,
      attachHint: view.attachHint,
    },
    null,
    2,
  );
}
