import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { findStalePanes } from '../src/integrations/herdr.js';

// Sweep-safety stub (2026-09-05 regression: the loop's herdr-sweep closed an
// IN-FLIGHT worker pane). The stub serves `pane list` from STUB_PANES and
// answers `pane process-info` with STUB_PROCS (JSON array of foreground
// processes), mirroring the real CLI contract.
const STUB = `#!/usr/bin/env node
const args = process.argv.slice(2);
if (args[0] === '--session') args.splice(0, 2);
const out = (o) => process.stdout.write(JSON.stringify(o) + '\\n');
if (args[0] === 'pane' && args[1] === 'list') {
  out({ id: 'x', result: { type: 'pane_list', panes: JSON.parse(process.env.STUB_PANES || '[]') } });
} else if (args[0] === 'pane' && args[1] === 'process-info') {
  const pane = args[args.indexOf('--pane') + 1] ?? '';
  const procs = JSON.parse(process.env.STUB_PROCS || '{}')[pane] ?? [];
  out({ id: 'x', result: { type: 'pane_process_info', process_info: { foreground_processes: procs } } });
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
  delete process.env.DEVAGENT_HERDR_BIN;
  delete process.env.STUB_PANES;
  delete process.env.STUB_PROCS;
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
});
