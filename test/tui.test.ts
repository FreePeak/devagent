import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { chmodSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startDaemon } from '../src/server/daemon.js';
import { runTui, renderDashboard, renderLines, aggregateStatus } from '../src/tui/tui.js';
import type { Snapshot } from '../src/tui/tui.js';
import { parseLogLine } from '../src/tui/viz.js';
import { appendAuditRecord } from '../src/orchestrator/ledger.js';

/**
 * FR-TUI smoke: runTui's non-TTY one-shot path against a live daemon on a
 * random port. Bounded (<5s): no polling loop — a single snapshot fetch.
 */

let home: string;
let repo: string;
let herdrStub = '';
let stop: (() => Promise<void>) | null = null;
let port = 0;
let token = '';

beforeAll(async () => {
  home = mkdtempSync(join(tmpdir(), 'devagent-tui-home-'));
  repo = mkdtempSync(join(tmpdir(), 'devagent-tui-repo-'));
  process.env.DEVAGENT_HOME = home;
  delete process.env.DEVAGENT_DAEMON_TOKEN;
  // Deterministic roster: point DEVAGENT_HERDR_BIN at an empty-session stub
  // so the operator's live devagent herdr session (real worker panes from a
  // running factory) never leaks into the rendered snapshot.
  herdrStub = mkdtempSync(join(tmpdir(), 'devagent-tui-herdr-'));
  const stub = join(herdrStub, 'herdr-stub.sh');
  writeFileSync(stub, '#!/bin/sh\necho \'{"id":"x","result":{"agents":[],"type":"agent_list"}}\'\n');
  chmodSync(stub, 0o755);
  process.env.DEVAGENT_HERDR_BIN = stub;
  const d = await startDaemon({ port: 0, repoPath: repo });
  stop = d.stop;
  port = d.port;
  token = d.token;
});

afterAll(async () => {
  await stop?.();
  delete process.env.DEVAGENT_HOME;
  delete process.env.DEVAGENT_HERDR_BIN;
  try {
    rmSync(home, { recursive: true, force: true });
    rmSync(repo, { recursive: true, force: true });
    if (herdrStub) rmSync(herdrStub, { recursive: true, force: true });
  } catch {
    /* tmp cleanup best-effort */
  }
});

/** Capture stdout of one non-TTY runTui invocation. */
async function captureStdout(opts: Parameters<typeof runTui>[0]): Promise<string> {
  const chunks: string[] = [];
  const origWrite = process.stdout.write.bind(process.stdout);
  process.stdout.write = ((chunk: unknown) => {
    chunks.push(String(chunk));
    return true;
  }) as typeof process.stdout.write;
  try {
    await runTui(opts);
  } finally {
    process.stdout.write = origWrite;
  }
  return chunks.join('');
}

describe('tui one-shot (non-TTY smoke path)', () => {
  it('prints a snapshot with queue depth, herdr session, attach hints; exit 0', async () => {
    const out = await captureStdout({ url: `http://127.0.0.1:${port}`, token });
    expect(out).toContain('DevAgent');
    expect(out).toContain('0p/0c/0d'); // queue depth from /status (compact bar format)
    expect(out).toContain('herdr:devagent'); // session name from /status
    expect(out).toContain('no workers, queue empty');
    expect(out).toContain('[1] workers [2] sessions [3] log'); // view switcher
    expect(out).toContain('k kill'); // kill stays on k (FR-TUI-05)
    expect(out).toContain('q] quit');
  }, 8_000);

  it('degrades to DAEMON UNREACHABLE instead of throwing on a dead port', async () => {
    const out = await captureStdout({ url: 'http://127.0.0.1:1', token });
    expect(out).toContain('DAEMON UNREACHABLE');
  }, 8_000);
});

describe('renderDashboard', () => {
  const snap: Snapshot = {
    status: {
      now: new Date().toISOString(),
      uptime_s: 5,
      runs: { active: 1, failed_recent: 0 },
      queue: { pending: 2, claimed: 1, done: 3 },
      circuit: 'closed',
      herdr: { enabled: true, session: 'devagent' },
      spawn: { visibility: 'visible' },
      capabilities: ['approve', 'dispatch', 'attach', 'kill-via-answer'],
    },
    agents: {
      panes: [
        {
          taskId: 'TASK-abc',
          role: 'worker',
          worker: 'omp',
          paneId: 'w1:p1',
          workspaceId: 'w1',
          label: 'TASK-abc-a1',
          cwd: '/tmp/.devagent-worktrees/TASK-abc-a1',
          agentStatus: 'working',
          state: 'running',
          startedAt: new Date(Date.now() - 5 * 60_000).toISOString(),
        },
      ],
      queued: [{ id: 'TASK-xyz', title: 'do the thing', status: 'pending', createdAt: new Date().toISOString() }],
    },
    history: [
      { ts: new Date().toISOString(), kind: 'audit', taskId: 'TASK-abc', attempt: 1, verdict: 'pass', integrity: 'ok', unmetCriteria: [], summary: '' },
      { ts: new Date().toISOString(), kind: 'event', event: 'watchdog-health', taskId: 'TASK-mtnmnp1g-f4j9', attempt: 1, worker: 'omp', site: 'herdr-pane', watchdogFired: true },
      { ts: new Date().toISOString(), kind: 'event', event: 'loop-result', loop: 76, status: 'skipped', goal: `Goal: ${'a'.repeat(100)}` },
    ],
    sessions: null,
    reachable: true,
    fetchedAt: Date.now(),
  };

  it('draws header, pane cards with attach hints, queued cards, history', () => {
    const raw = renderDashboard(snap);
    // Chip text is split by reset codes (dot + label colored separately);
    // assert against the visible text with ANSI stripped.
    const out = raw.replace(/\x1b\[[0-9;]*m/g, '');
    expect(out).toContain('RUNNING');
    expect(out).toContain('2p/1c/3d');
    expect(out).toContain('herdr:devagent');
    expect(out).toContain('TASK-abc');
    expect(out).toContain('audit'); // history row event/kind
    expect(out).toContain('watchdog-health'); // kind column un-truncated (was 14ch)
    expect(out).toContain('TASK-mtnmnp1g-f4j9'); // watchdog row surfaces its taskId
    expect(out).toContain('loop:76'); // loop-result rows identify by loop number
    expect(out).toContain('fired'); // watchdog verdict chip
    expect(out).toContain('skipped'); // loop-result short status
    const goalRow = out.split('\n').find((l) => l.includes('loop:76'));
    expect(goalRow).toBeTruthy();
    expect(goalRow!.replace(/\x1b\[[0-9;]*m/g, '').length).toBeLessThanOrEqual(140); // goal prose capped, no full-width dump
    expect(out).toContain('● working'); // status chip in the pane card
    expect(out).toContain('╭─'); // boxed panel borders (Pilot-style)
    expect(out).toContain('● queued'); // queued card chip
    expect(out).not.toContain('DAEMON UNREACHABLE');
  });

  it('aggregateStatus: live state outranks a sticky failed_recent count', () => {
    // failed_recent is taskCount().failed — never decays, so it must never
    // outrank a live run (2026-09-05 header-stale-FAILED defect).
    expect(aggregateStatus(
      { ...snap.status!, runs: { active: 1, failed_recent: 1 } },
      [],
    )).toBe('RUNNING');
    expect(aggregateStatus(
      { ...snap.status!, runs: { active: 0, failed_recent: 1 }, queue: { pending: 0, claimed: 2, done: 3 } },
      [],
    )).toBe('RUNNING');
    expect(aggregateStatus(
      { ...snap.status!, runs: { active: 0, failed_recent: 1 }, queue: { pending: 0, claimed: 0, done: 3 }, circuit: 'closed' },
      [],
    )).toBe('IDLE'); // lifetime failed count never decays — no longer FAILED
    expect(aggregateStatus(
      { ...snap.status!, runs: { active: 0, failed_recent: 0 }, queue: { pending: 0, claimed: 0, done: 3 }, circuit: 'open' },
      [],
    )).toBe('FAILED'); // live trouble: circuit open
  });

  it('renders the current iteration phase from loop-phase history rows', () => {
    const withPhase = {
      ...snap,
      history: [
        ...snap.history,
        { ts: new Date().toISOString(), kind: 'event', event: 'loop-phase', loop: 82, phase: 'task', detail: 'Ship the Q27 cross-board retry-memory' },
      ],
    };
    const out = renderDashboard(withPhase).replace(/\x1b\[[0-9;]*m/g, '');
    expect(out).toContain('iteration 82');
    expect(out).toContain('phase: task');
    expect(out).toContain('Ship the Q27 cross-board retry-memory');
    // no loop-phase rows -> no iteration line
    const plain = renderDashboard(snap).replace(/\x1b\[[0-9;]*m/g, '');
    expect(plain).not.toContain('phase:');
  });

  it('shows DAEMON UNREACHABLE when the snapshot has no status', () => {
    const out = renderDashboard({ ...snap, status: null, reachable: false });
    expect(out).toContain('DAEMON UNREACHABLE');
    expect(aggregateStatus(null, [])).toBe('IDLE');
  });

  it('sessions view lists pane id and cwd; help overlay lists keys', () => {
    const sess = renderDashboard(snap, { showSessions: true });
    expect(sess).toContain('Sessions');
    expect(sess).toContain('w1:p1');
    expect(sess).toContain('.devagent-worktrees');
    const help = renderDashboard(snap, { showHelp: true });
    expect(help).toContain('k  kill the running task');
    expect(help).toContain('y  confirm the pending kill');
    expect(help).toContain('switch view: workers / sessions / live log');
    expect(help).toContain('upgrade hint');
  });

  it('metrics line: htop-style queue meter + pilot-style activity sparkline', () => {
    const out = renderDashboard(snap, { metrics: { samples: [0, 2, 1, 3], sampleMs: 2_000 } });
    const plain = out.replace(/\x1b\[[0-9;]*m/g, '');
    expect(plain).toContain('2p/1c/3d');
    expect(plain).toContain('queue [');
    expect(plain).toMatch(/activity\(8s\) [▁▂▃▄▅▆▇█]+ 3/); // sparkline + latest sample
    expect(plain).toContain('up 5s');
    expect(plain).toContain('herdr:devagent');
    // without samples the sparkline is simply absent, never fabricated
    const bare = renderDashboard(snap).replace(/\x1b\[[0-9;]*m/g, '');
    expect(bare).not.toContain('activity(');
  });

  it('log view renders structured lines with live state + follow indicator', () => {
    const lines = [
      parseLogLine(JSON.stringify({ ts: '2026-09-05T10:00:00.000Z', level: 'warn', stage: 'clarify', runId: 'run-1234', message: 'Insufficient specification' })),
      parseLogLine('plain corruption'),
    ];
    const out = renderDashboard(snap, { view: 'log', log: { lines, scroll: 0, follow: true, state: 'live', source: 'run-1234' } });
    const plain = out.replace(/\x1b\[[0-9;]*m/g, '');
    expect(plain).toContain('▌Live log');
    expect(plain).toContain('● live');
    expect(plain).toContain('line(s) buffered');
    expect(plain).toContain('run run-123');
    expect(plain).toContain('[following tail]');
    expect(plain).toContain('warn');
    expect(plain).toContain('clarify');
    expect(plain).toContain('Insufficient specification');
    expect(plain).toContain('plain corruption');
  });

  it('log view scrolled back shows the offset and fits the terminal', () => {
    const many = Array.from({ length: 40 }, (_, i) =>
      parseLogLine(JSON.stringify({ ts: new Date(Date.now() - i * 1000).toISOString(), level: 'info', stage: 'impl', message: `event ${i}` })),
    );
    const out = renderDashboard(snap, { view: 'log', log: { lines: many, scroll: 10, follow: false, state: 'live' }, rows: 20 });
    const plain = out.replace(/\x1b\[[0-9;]*m/g, '');
    expect(plain).toContain('10 older');
    expect(plain).toContain('event 18'); // viewport starts 10+viewport lines back
    expect(plain).not.toContain('event 0'); // newest hidden while scrolled back
    expect(plain).toContain('Live log'); // the title is never cut by fitting
    expect(out.split('\n').length).toBeLessThanOrEqual(20); // htop always fits
  });

  it('detail overlay expands the selected pane; upgrade overlay shows the recipe', () => {
    const detail = renderDashboard(snap, { overlay: { kind: 'detail', item: snap.agents!.panes![0]! } });
    const d = detail.replace(/\x1b\[[0-9;]*m/g, '');
    expect(d).toContain('TASK-abc');
    expect(d).toContain('pane w1:p1');
    expect(d).toContain('workspace w1');
    expect(d).toContain('devagent attach TASK-abc');
    const up = renderDashboard(snap, { overlay: { kind: 'upgrade' } });
    const u = up.replace(/\x1b\[[0-9;]*m/g, '');
    expect(u).toContain('Upgrade');
    expect(u).toContain('git pull --ff-only');
    expect(u).toContain('npm ci && npm run build');
    expect(u).toContain('rollback');
  });

  it('selection cursor marks the selected card; small terminals still fit', () => {
    const sel = renderDashboard(snap, { selection: 1 }); // 0 = pane, 1 = queued
    const s = sel.replace(/\x1b\[[0-9;]*m/g, '');
    expect(s).toContain('▸ TASK-xyz');
    expect(s).not.toContain('▸ TASK-abc');
    const small = renderLines(snap, { rows: 12 });
    expect(small.length).toBeLessThanOrEqual(12);
    expect(small.join('\n')).toContain('DevAgent'); // header survives the trim
  });
});

describe('ledger evidence for the smoke repo', () => {
  it('has audit rows the /history endpoint can serve', () => {
    appendAuditRecord(repo, {
      taskId: 'TASK-abc',
      attempt: 1,
      verdict: { verdict: 'pass', integrity: 'ok', criteriaResults: [], summary: 'smoke' },
    });
  });
});

describe('token handling', () => {
  it('snapshot with a wrong token renders the auth-rejected header (no throw)', async () => {
    const out = await captureStdout({ url: `http://127.0.0.1:${port}`, token: 'wrong-token' });
    expect(out).toContain('DAEMON AUTH REJECTED');
  }, 8_000);
});

// Sanity: the daemon-token file the TUI reads as fallback is the one the
// daemon wrote for this test boot.
describe('token file fallback', () => {
  it('daemon-token file exists with 0600 perms and matches the boot token', () => {
    const p = join(home, 'daemon-token');
    const st = statSync(p);
    expect((st.mode & 0o777).toString(8)).toBe('600');
    expect(readFileSync(p, 'utf8').trim()).toBe(token);
  });
});

afterAll(() => {
  // Keep vitest quiet about env leakage into other suites.
  writeFileSync(join(repo, '.gitkeep'), '');
});
