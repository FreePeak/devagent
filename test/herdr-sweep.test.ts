import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { spawn } from 'node:child_process';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { findOrphanBrokerPids, findStalePanes, reapOrphanBrokers, sweepStalePanes, sweepDenyReason, SWEEP_REASON_OPERATOR_ATTACHED } from '../src/integrations/herdr.js';
import { herdrSweepConfig, loadConfig } from '../src/config.js';

// Sweep-safety stub (2026-09-05 regression: the loop's herdr-sweep closed an
// IN-FLIGHT worker pane). The stub serves `pane list` from STUB_PANES and
// answers `pane process-info` with STUB_PROCS (JSON array of foreground
// processes), mirroring the real CLI contract.
const STUB = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
if (args[0] === '--session') args.splice(0, 2);
const out = (o) => process.stdout.write(JSON.stringify(o) + '\\n');
if (args[0] === 'pane' && args[1] === 'list') {
  out({ id: 'x', result: { type: 'pane_list', panes: JSON.parse(process.env.STUB_PANES || '[]') } });
} else if (args[0] === 'pane' && args[1] === 'process-info') {
  const pane = args[args.indexOf('--pane') + 1] ?? '';
  const procs = JSON.parse(process.env.STUB_PROCS || '{}')[pane] ?? [];
  out({ id: 'x', result: { type: 'pane_process_info', process_info: { foreground_processes: procs } } });
} else if (args[0] === 'agent' && args[1] === 'list') {
  // FR-VIS-02 roster the operator-attach exemption reads: STUB_AGENTS carries
  // the same row shape as the real herdr agent list reply.
  out({ id: 'x', result: { type: 'agent_list', agents: JSON.parse(process.env.STUB_AGENTS || '[]') } });
} else if (args[0] === 'workspace' && args[1] === 'close') {
  fs.appendFileSync(process.env.STUB_CLOSED || '/tmp/stub-sweep-closed.log', args[2] + '\\n');
  out({ id: 'x', result: {} });
} else {
  console.error('stub: unsupported ' + args.join(' '));
  process.exit(2);
}
`;

let dir: string;
let bin: string;

beforeAll(() => {
  dir = mkdtempSync(join(tmpdir(), 'devagent-sweep-test-'));
  bin = join(dir, 'herdr-stub.cjs');
  writeFileSync(bin, STUB);
  chmodSync(bin, 0o755);
});

afterEach(() => {
  for (const k of [
    'DEVAGENT_HERDR_BIN', 'STUB_PANES', 'STUB_PROCS', 'STUB_AGENTS', 'STUB_CLOSED',
    'DEVAGENT_HERDR_SWEEP', 'DEVAGENT_HERDR_SWEEP_ORPHANS', 'DEVAGENT_OPERATOR_ATTACHED',
    'DEVAGENT_SWEEP_OWNER_PIDS', 'DEVAGENT_SWEEP_ANCESTRY_JSON',
  ]) {
    delete process.env[k];
  }
});

afterAll(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe('sweep safety (FR-VIS-07)', () => {
  beforeEach(() => {
    process.env.DEVAGENT_HERDR_BIN = bin;
  });

  it('never sweeps a pane whose foreground process is a worker CLI, even when agent_status says idle', async () => {
    process.env.STUB_PANES = JSON.stringify([
      {
        pane_id: 'wX:p1',
        workspace_id: 'wX',
        label: 'TASK-abc-a1',
        agent_status: 'idle',
        cwd: '/repo/.devagent-worktrees/TASK-abc-a1',
      },
    ]);
    process.env.STUB_PROCS = JSON.stringify({
      'wX:p1': [{ name: 'omp', argv0: 'omp', pid: 42 }],
    });
    const stale = await findStalePanes('devagent');
    expect(stale).toEqual([]);
  });

  it('sweeps an idle worker pane sitting in a devagent worktree when no worker runs in it', async () => {
    process.env.STUB_PANES = JSON.stringify([
      {
        pane_id: 'wX:p1',
        workspace_id: 'wX',
        label: 'TASK-abc-a1',
        agent_status: 'idle',
        cwd: '/repo/.devagent-worktrees/TASK-abc-a1',
      },
    ]);
    // Idle shell foreground (the "no live dispatch" shape).
    process.env.STUB_PROCS = JSON.stringify({
      'wX:p1': [{ name: 'zsh', argv0: 'zsh', pid: 42 }],
    });
    const stale = await findStalePanes('devagent');
    expect(stale.map((s) => s.paneId)).toEqual(['wX:p1']);
    expect(stale[0]!.reason).toBe('agent-idle');
  });

  it('never sweeps operator scratch panes (cwd outside .devagent-worktrees) even when idle or agentless', async () => {
    process.env.STUB_PANES = JSON.stringify([
      {
        pane_id: 'wV:p1',
        workspace_id: 'wV',
        agent_status: 'unknown',
        cwd: '/Users/op',
      },
      {
        pane_id: 'wV:p2',
        workspace_id: 'wV',
        label: 'scratch',
        agent_status: 'idle',
        cwd: '/tmp/somewhere-else',
      },
    ]);
    process.env.STUB_PROCS = JSON.stringify({
      'wV:p1': [{ name: 'zsh', argv0: 'zsh', pid: 7 }],
      'wV:p2': [{ name: 'zsh', argv0: 'zsh', pid: 8 }],
    });
    const stale = await findStalePanes('devagent');
    expect(stale).toEqual([]);
  });

  it('sweeps an agentless leftover shell inside a devagent worktree (true leftover)', async () => {
    process.env.STUB_PANES = JSON.stringify([
      {
        pane_id: 'wT:p1',
        workspace_id: 'wT',
        agent_status: undefined,
        cwd: '/repo/.devagent-worktrees/TASK-old-a1',
      },
    ]);
    process.env.STUB_PROCS = JSON.stringify({
      'wT:p1': [{ name: 'zsh', argv0: 'zsh', pid: 9 }],
    });
    const stale = await findStalePanes('devagent');
    expect(stale.map((s) => s.reason)).toEqual(['no-agent']);
  });

  describe('orphaned-driver sweep (2026-09-07: v11 OOM left live omp pane uncollected)', () => {
    afterEach(() => {
      delete process.env.DEVAGENT_SWEEP_OWNER_PIDS;
      delete process.env.DEVAGENT_SWEEP_ANCESTRY_JSON;
    });

    const liveWorkerPane = {
      pane_id: 'wX:p1',
      workspace_id: 'wX',
      label: 'TASK-orphan-a1',
      agent_status: 'idle',
      cwd: '/repo/.devagent-worktrees/TASK-orphan-a1',
    };
    const liveProcs = { 'wX:p1': [{ name: 'omp', argv0: 'omp', pid: 42 }] };

    it('default sweep still leaves a live worker pane alone (no --orphans)', async () => {
      process.env.STUB_PANES = JSON.stringify([liveWorkerPane]);
      process.env.STUB_PROCS = JSON.stringify(liveProcs);
      process.env.DEVAGENT_SWEEP_OWNER_PIDS = '4242\n'; // owner exists, ancestry unknown
      process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({
        4242: ['timeout 7200 npx tsx src/cli.ts task'],
      });
      const stale = await findStalePanes('devagent');
      expect(stale).toEqual([]);
    });

    it('closes a live worker pane whose owner CLI detached from any loop driver', async () => {
      process.env.STUB_PANES = JSON.stringify([liveWorkerPane]);
      process.env.STUB_PROCS = JSON.stringify(liveProcs);
      // Owner CLI alive but its ancestry has no selfbuild-loop.sh (driver died).
      process.env.DEVAGENT_SWEEP_OWNER_PIDS = '4242\n';
      process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({
        4242: ['timeout 7200 npx tsx src/cli.ts task', 'launchd'],
      });
      const stale = await findStalePanes('devagent', { orphans: true });
      expect(stale.map((s) => s.reason)).toEqual(['orphaned-driver']);
      expect(stale[0]!.paneId).toBe('wX:p1');
    });

    it('spares a live worker pane whose ancestry still carries the loop driver', async () => {
      process.env.STUB_PANES = JSON.stringify([liveWorkerPane]);
      process.env.STUB_PROCS = JSON.stringify(liveProcs);
      process.env.DEVAGENT_SWEEP_OWNER_PIDS = '4242\n';
      process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({
        4242: [
          'bash /repo/scripts/selfbuild-loop.sh',
          'timeout 7200 npx tsx src/cli.ts task',
        ],
      });
      const stale = await findStalePanes('devagent', { orphans: true });
      expect(stale).toEqual([]);
    });

    it('closes a live worker pane with NO pane-run owner CLI at all (poller died)', async () => {
      process.env.STUB_PANES = JSON.stringify([liveWorkerPane]);
      process.env.STUB_PROCS = JSON.stringify(liveProcs);
      process.env.DEVAGENT_SWEEP_OWNER_PIDS = '';
      const stale = await findStalePanes('devagent', { orphans: true });
      expect(stale.map((s) => s.reason)).toEqual(['orphaned-driver']);
    });
  });
 });

// FR-VIS-10 (PRD §18 Q23): the sweep's blast radius is bounded from the
// operator's side — a master toggle, a managed deny list, and an exemption for
// panes an operator is attached to — not only by the per-pane checks above.
describe('sweep deny toggle (FR-VIS-10)', () => {
  beforeEach(() => {
    process.env.DEVAGENT_HERDR_BIN = bin;
  });

  /** The shape FR-VIS-07 lets through: idle agent in a devagent worktree. */
  const sweepable = {
    pane_id: 'wX:p1',
    workspace_id: 'wX',
    label: 'TASK-abc-a1',
    agent_status: 'idle',
    cwd: '/repo/.devagent-worktrees/TASK-abc-a1',
  };

  it('DEVAGENT_HERDR_SWEEP=0 stops the sweep before it lists a pane', async () => {
    process.env.STUB_PANES = JSON.stringify([sweepable]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'zsh', argv0: 'zsh' }] });
    expect((await findStalePanes('devagent')).map((s) => s.reason)).toEqual(['agent-idle']);
    process.env.DEVAGENT_HERDR_SWEEP = '0';
    expect(await findStalePanes('devagent')).toEqual([]);
    expect(await sweepStalePanes('devagent')).toEqual([]);
  });

  it('herdr.sweep.enabled=false is the same switch from config', async () => {
    process.env.STUB_PANES = JSON.stringify([sweepable]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'zsh', argv0: 'zsh' }] });
    expect(await findStalePanes('devagent', { sweep: { enabled: false, denySessions: [] } })).toEqual([]);
  });

  it('a denied session is never listed, while every other session still sweeps', async () => {
    process.env.STUB_PANES = JSON.stringify([sweepable]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'zsh', argv0: 'zsh' }] });
    const sweep = { enabled: true, denySessions: ['devagent'] };
    expect(await findStalePanes('devagent', { sweep })).toEqual([]);
    expect(await sweepStalePanes('devagent', { sweep })).toEqual([]);
    expect((await findStalePanes('devagent-ci', { sweep })).map((s) => s.paneId)).toEqual(['wX:p1']);
  });

  it('sweepDenyReason names which bound fired', () => {
    expect(sweepDenyReason('devagent', { enabled: true, denySessions: [] })).toBeNull();
    expect(sweepDenyReason('devagent', { enabled: false, denySessions: ['devagent'] })).toBe('disabled');
    expect(sweepDenyReason('devagent', { enabled: true, denySessions: ['devagent', 'other'] })).toBe('session-denied');
    expect(sweepDenyReason('other', { enabled: true, denySessions: ['devagent'] })).toBeNull();
  });

  it('herdr.sweep.orphans arms the orphan class without the caller passing --orphans', async () => {
    process.env.STUB_PANES = JSON.stringify([
      { ...sweepable, agent_status: 'idle', cwd: '/repo/.devagent-worktrees/TASK-orphan-a1' },
    ]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'omp', argv0: 'omp' }] });
    process.env.DEVAGENT_SWEEP_OWNER_PIDS = ''; // no pane-run owner at all
    expect(await findStalePanes('devagent')).toEqual([]);
    expect((await findStalePanes('devagent', { sweep: { enabled: true, orphans: true, denySessions: [] } }))
      .map((s) => s.reason)).toEqual(['orphaned-driver']);
  });
});

describe('operator-attach exemption (FR-VIS-10)', () => {
  beforeEach(() => {
    process.env.DEVAGENT_HERDR_BIN = bin;
    process.env.STUB_CLOSED = join(dir, 'closed.log');
  });

  afterEach(() => {
    rmSync(join(dir, 'closed.log'), { force: true });
  });

  /** Idle worker pane: sweepable on the FR-VIS-07 checks alone. */
  const idleWorktreePane = {
    pane_id: 'wX:p1',
    workspace_id: 'wX',
    label: 'TASK-abc-a1',
    agent_status: 'idle',
    cwd: '/repo/.devagent-worktrees/TASK-abc-a1',
  };

  /** Roster row for the same pane, as `herdr agent list` reports it. */
  const rosterRow = (agentStatus: string) => ({
    name: 'TASK-abc-a1',
    label: 'TASK-abc-a1',
    agent: 'omp',
    pane_id: 'wX:p1',
    workspace_id: 'wX',
    agent_status: agentStatus,
    cwd: '/repo/.devagent-worktrees/TASK-abc-a1',
  });

  function closedWorkspaces(): string[] {
    const p = process.env.STUB_CLOSED;
    if (!p || !existsSync(p)) return [];
    return readFileSync(p, 'utf8').split('\n').filter((l) => l.trim() !== '');
  }

  it('spares a pane the roster reports as running and reports why', async () => {
    process.env.STUB_PANES = JSON.stringify([idleWorktreePane]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'zsh', argv0: 'zsh' }] });
    process.env.STUB_AGENTS = JSON.stringify([rosterRow('working')]);
    const found = await findStalePanes('devagent');
    expect(found.map((s) => s.reason)).toEqual([SWEEP_REASON_OPERATOR_ATTACHED]);
    const swept = await sweepStalePanes('devagent');
    expect(swept.map((s) => s.reason)).toEqual([SWEEP_REASON_OPERATOR_ATTACHED]);
    expect(closedWorkspaces()).toEqual([]);
  });

  it('a roster entry that is not live does not exempt the pane', async () => {
    process.env.STUB_PANES = JSON.stringify([idleWorktreePane]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'zsh', argv0: 'zsh' }] });
    process.env.STUB_AGENTS = JSON.stringify([rosterRow('idle')]);
    expect((await findStalePanes('devagent')).map((s) => s.reason)).toEqual(['agent-idle']);
    await sweepStalePanes('devagent');
    expect(closedWorkspaces()).toEqual(['wX']);
  });

  it('DEVAGENT_OPERATOR_ATTACHED spares every candidate in the session', async () => {
    process.env.STUB_PANES = JSON.stringify([
      idleWorktreePane,
      { ...idleWorktreePane, pane_id: 'wY:p1', workspace_id: 'wY', label: 'TASK-def-a1', cwd: '/repo/.devagent-worktrees/TASK-def-a1' },
    ]);
    process.env.STUB_PROCS = JSON.stringify({
      'wX:p1': [{ name: 'zsh', argv0: 'zsh' }],
      'wY:p1': [{ name: 'zsh', argv0: 'zsh' }],
    });
    process.env.DEVAGENT_OPERATOR_ATTACHED = '1';
    const swept = await sweepStalePanes('devagent');
    expect(swept.map((s) => s.reason)).toEqual([SWEEP_REASON_OPERATOR_ATTACHED, SWEEP_REASON_OPERATOR_ATTACHED]);
    expect(closedWorkspaces()).toEqual([]);
    // A false-ish flag value is not an exemption.
    process.env.DEVAGENT_OPERATOR_ATTACHED = '0';
    expect((await findStalePanes('devagent')).map((s) => s.reason)).toEqual(['agent-idle', 'agent-idle']);
  });

  it('the exemption outranks the orphan class: a running live worker is never closed', async () => {
    process.env.STUB_PANES = JSON.stringify([idleWorktreePane]);
    process.env.STUB_PROCS = JSON.stringify({ 'wX:p1': [{ name: 'omp', argv0: 'omp' }] });
    process.env.DEVAGENT_SWEEP_OWNER_PIDS = ''; // owner gone -> orphan candidate
    process.env.STUB_AGENTS = JSON.stringify([rosterRow('working')]);
    const stale = await findStalePanes('devagent', { orphans: true });
    expect(stale.map((s) => s.reason)).toEqual([SWEEP_REASON_OPERATOR_ATTACHED]);
  });
});

describe('herdr.sweep config (FR-VIS-10)', () => {
  const repoWithConfig = (cfg: Record<string, unknown>): string => {
    const dir = mkdtempSync(join(tmpdir(), 'devagent-sweep-cfg-'));
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify(cfg));
    return dir;
  };

  it('defaults keep today\'s behavior: sweep on, orphan class left to the caller', () => {
    const repo = repoWithConfig({ worker: 'omp', herdr: { enabled: true } });
    expect(herdrSweepConfig(loadConfig(repo))).toEqual({ enabled: true, denySessions: [] });
    rmSync(repo, { recursive: true, force: true });
  });

  it('parses the sweep block and lets the env overrides win', () => {
    const repo = repoWithConfig({
      worker: 'omp',
      herdr: { enabled: true, sweep: { enabled: false, orphans: true, denySessions: ['personal'] } },
    });
    const cfg = loadConfig(repo);
    expect(herdrSweepConfig(cfg)).toEqual({ enabled: false, orphans: true, denySessions: ['personal'] });
    process.env.DEVAGENT_HERDR_SWEEP = '1';
    process.env.DEVAGENT_HERDR_SWEEP_ORPHANS = '0';
    expect(herdrSweepConfig(cfg)).toEqual({ enabled: true, orphans: false, denySessions: ['personal'] });
    rmSync(repo, { recursive: true, force: true });
  });

  it('ignores an unrecognized env value instead of arming the sweep', () => {
    const repo = repoWithConfig({ worker: 'omp', herdr: { sweep: { enabled: false } } });
    process.env.DEVAGENT_HERDR_SWEEP = 'maybe';
    process.env.DEVAGENT_HERDR_SWEEP_ORPHANS = 'yes';
    expect(herdrSweepConfig(loadConfig(repo))).toEqual({ enabled: false, denySessions: [] });
    rmSync(repo, { recursive: true, force: true });
  });

  it('rejects malformed toggles and deny entries', () => {
    const cases: Array<[Record<string, unknown>, RegExp]> = [
      [{ herdr: { sweep: { enabled: 'no' } } }, /Invalid herdr\.sweep\.enabled/],
      [{ herdr: { sweep: { orphans: 1 } } }, /Invalid herdr\.sweep\.orphans/],
      [{ herdr: { sweep: { denySessions: 'personal' } } }, /Invalid herdr\.sweep\.denySessions/],
      [{ herdr: { sweep: { denySessions: ['Personal Session'] } } }, /Invalid herdr\.sweep\.denySessions entry/],
    ];
    for (const [cfg, expected] of cases) {
      const repo = repoWithConfig(cfg);
      expect(() => loadConfig(repo)).toThrow(expected);
      rmSync(repo, { recursive: true, force: true });
    }
  });
});

// 2026-09-08 memory-pressure diagnosis: four orphaned omp daemon_broker
// processes (ppid 1, etime 13h..8d) pinned an lsp_mux and a gopls fleet
// (~1.3 GiB) after their sessions died. The pane sweep cannot see them — they
// are not panes — so the sweep grew a process-scoped class of its own.
describe('orphaned daemon_broker reap (2026-09-08 memory-pressure class)', () => {
  const BROKER_CMD = 'node /Users/op/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js __omp_worker_daemon_broker';

  it('reports a broker reparented to launchd (ancestry is only itself)', async () => {
    process.env.DEVAGENT_SWEEP_OWNER_PIDS = '47537\n';
    process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({ 47537: [BROKER_CMD] });
    expect(await findOrphanBrokerPids()).toEqual([{ pid: 47537, command: BROKER_CMD }]);
  });

  it('spares a broker that still has its omp session above it', async () => {
    process.env.DEVAGENT_SWEEP_OWNER_PIDS = '14727\n';
    process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({
      14727: [BROKER_CMD, 'omp', '-zsh', '/Users/op/.local/bin/herdr server'],
    });
    expect(await findOrphanBrokerPids()).toEqual([]);
  });

  it('ignores a pgrep hit that is not a broker', async () => {
    process.env.DEVAGENT_SWEEP_OWNER_PIDS = '99\n';
    process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({ 99: ['grep daemon_broker'] });
    expect(await findOrphanBrokerPids()).toEqual([]);
  });

  it('dry-run lists orphans without signalling them', async () => {
    process.env.DEVAGENT_SWEEP_OWNER_PIDS = '47537\n';
    process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({ 47537: [BROKER_CMD] });
    const { orphans, killed } = await reapOrphanBrokers({ dryRun: true });
    expect(orphans.map((o) => o.pid)).toEqual([47537]);
    expect(killed).toEqual([]);
  });

  it('escalates to SIGKILL for an orphan that ignores SIGTERM', async () => {
    // A real process is the only honest proof of the escalation path: this one
    // traps SIGTERM, exactly like the 13476 orphan that shrugged it off.
    // Real-clock exception (ts-no-test-timers): the reap's SIGTERM grace period
    // IS the behavior under test, and the signal must actually reach the child
    // before the escalation check runs. Fake timers would fire the check first,
    // so the test would pass without proving escalation.
    const child = spawn(process.execPath, ['-e', "process.on('SIGTERM', () => {}); process.send?.('trapped'); setInterval(() => {}, 1000);"], { stdio: ['ignore', 'ignore', 'ignore', 'ipc'] });
    // Await the trap, not 'spawn': 'spawn' only means a pid exists, and a
    // signal delivered before the child's script installs the handler takes the
    // default action (the first version of this test killed it with SIGTERM).
    const { promise: trapped, resolve: onTrapped } = Promise.withResolvers<void>();
    child.once('message', onTrapped);
    await trapped;
    try {
      process.env.DEVAGENT_SWEEP_OWNER_PIDS = `${child.pid}\n`;
      process.env.DEVAGENT_SWEEP_ANCESTRY_JSON = JSON.stringify({ [child.pid]: [BROKER_CMD] });
      const { killed } = await reapOrphanBrokers({});
      expect(killed).toEqual([child.pid]);
      if (child.signalCode === null) {
        const { promise: exited, resolve: onExit } = Promise.withResolvers<void>();
        child.once('exit', onExit);
        await exited;
      }
      expect(child.signalCode).toBe('SIGKILL');
    } finally {
      if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
    }
  }, 20_000);
});
