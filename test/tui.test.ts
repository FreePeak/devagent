import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { chmodSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { startDaemon } from '../src/server/daemon.js';
import { runTui, renderDashboard, aggregateStatus } from '../src/tui/tui.js';
import type { Snapshot } from '../src/tui/tui.js';
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
    expect(out).toContain('[r] refresh [s] sessions [k] kill [?] help [q] quit');
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
