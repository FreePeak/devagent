/** Dev-only visual smoke: print each TUI view (ANSI kept) for eyeballing. */
import { renderDashboard } from '../src/tui/tui.js';
import type { Snapshot } from '../src/tui/tui.js';
import { parseLogLine } from '../src/tui/viz.js';

const snap: Snapshot = {
  status: {
    now: new Date().toISOString(),
    uptime_s: 19_325,
    runs: { active: 2, failed_recent: 1 },
    queue: { pending: 3, claimed: 2, done: 41 },
    circuit: 'closed',
    herdr: { enabled: true, session: 'devagent' },
    spawn: { visibility: 'visible' },
    capabilities: ['approve', 'dispatch', 'attach', 'kill-via-answer'],
  },
  agents: {
    panes: [
      {
        taskId: 'TASK-mtn9f85c', role: 'worker', worker: 'omp', paneId: 'w3:p1', workspaceId: 'w3',
        label: 'TASK-mtn9f85c-a2', cwd: '/Users/x/work/devagent/.devagent-worktrees/TASK-mtn9f85c-a2',
        agentStatus: 'working', state: 'running', startedAt: new Date(Date.now() - 42 * 60_000).toISOString(),
      },
      {
        taskId: 'TASK-mtnaxd5n', role: 'worker', worker: 'claude-code', paneId: 'w4:p1', workspaceId: 'w4',
        label: 'TASK-mtnaxd5n-a1', cwd: '/Users/x/work/devagent/.devagent-worktrees/TASK-mtnaxd5n-a1',
        agentStatus: 'waiting', state: 'idle', startedAt: new Date(Date.now() - 3 * 3_600_000).toISOString(),
      },
    ],
    queued: [{ id: 'TASK-mtnj8qmz', title: 'curate PRD backlog for the retry-memory epic', status: 'pending', createdAt: new Date().toISOString() }],
  },
  history: [
    { ts: new Date().toISOString(), kind: 'audit', taskId: 'TASK-mtn9f85c', verdict: 'pass' },
    { ts: new Date().toISOString(), kind: 'event', event: 'loop-phase', loop: 82, phase: 'task', detail: 'Ship the Q27 cross-board retry-memory' },
    { ts: new Date().toISOString(), kind: 'event', event: 'loop-result', loop: 81, status: 'skipped', goal: 'Goal: harden the starvation gate against provider-degraded rows' },
  ],
  sessions: null,
  reachable: true,
  fetchedAt: Date.now(),
};

const metrics = { samples: [0, 1, 1, 2, 3, 2, 2, 3, 4, 3, 2, 2, 3, 3, 2, 1, 2, 2, 3, 2], sampleMs: 2_000 };
const logLines = [
  parseLogLine(JSON.stringify({ ts: new Date(Date.now() - 90_000).toISOString(), runId: 'ffd7dbbf-25a7', stage: 'clarify', level: 'info', message: 'spec parsed (4 tickets)' })),
  parseLogLine(JSON.stringify({ ts: new Date(Date.now() - 60_000).toISOString(), runId: 'ffd7dbbf-25a7', stage: 'plan', level: 'warn', message: 'Linear comment failed: HTTP 401' })),
  parseLogLine(JSON.stringify({ ts: new Date().toISOString(), kind: 'event', event: 'loop-phase', loop: 82, phase: 'research', detail: 'timeout 900s' })),
];

const sep = (t: string) => `\n${'='.repeat(30)} ${t} ${'='.repeat(30)}\n`;
process.stdout.write(
  sep('WORKERS (selection=1)') + renderDashboard(snap, { selection: 1, metrics, spinnerFrame: 2 }) +
  sep('SESSIONS') + renderDashboard(snap, { view: 'sessions', metrics }) +
  sep('LOG') + renderDashboard(snap, { view: 'log', metrics, log: { lines: logLines, scroll: 0, follow: true, state: 'live', source: 'ffd7dbbf-25a7' } }) +
  sep('DETAIL OVERLAY') + renderDashboard(snap, { overlay: { kind: 'detail', item: snap.agents!.panes![0]! } }) +
  sep('UPGRADE OVERLAY') + renderDashboard(snap, { overlay: { kind: 'upgrade' } }) +
  sep('HELP') + renderDashboard(snap, { showHelp: true }) +
  sep('SMALL TERMINAL rows=18') + renderDashboard(snap, { metrics, rows: 18 }) +
  '\n',
);
