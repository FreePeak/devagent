import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  attachCommandFor,
  listSessionPanes,
  operatorAttachTrace,
  PANE_ENV_OP_ATTACH,
} from '../src/integrations/herdr.js';
import { appendOperatorAttachRecord, readLedger } from '../src/orchestrator/ledger.js';
import { runWorkerCli, setFallbackSink, shouldUseHerdr } from '../src/workers/herdr-runtime.js';
import { spawnVisibility } from '../src/config.js';
import type { SpawnCliResult } from '../src/workers/spawn-utils.js';

// Functional stub of the `herdr` CLI exposing just the `agent list` surface
// needed for the FR-VIS roster. Panes are declared via STUB_PANES env (JSON).
const STUB = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
if (args[0] === '--session') args.splice(0, 2);
const out = (o) => process.stdout.write(JSON.stringify(o) + '\\n');
fs.appendFileSync(process.env.STUB_LOG || '/tmp/stub-herdr-vis.log', args.join(' ') + '\\n');
if (args[0] === 'agent' && args[1] === 'list') {
  if (process.env.STUB_FAIL === '1') { console.error('server_not_running'); process.exit(1); }
  const agents = JSON.parse(process.env.STUB_PANES || '[]');
  out({ id: 'x', result: { agents } });
} else if (args[0] === 'workspace' && args[1] === 'list') {
  out({ id: 'x', result: { type: 'workspace_list', workspaces: [] } });
} else {
  console.error('stub-herdr: unsupported command: ' + args.join(' '));
  process.exit(2);
}
`;

let stubDir: string;
let stubBin: string;

beforeAll(() => {
  stubDir = mkdtempSync(join(tmpdir(), 'devagent-herdr-vis-test-'));
  stubBin = join(stubDir, 'herdr-stub.cjs');
  writeFileSync(stubBin, STUB);
  chmodSync(stubBin, 0o755);
});

beforeEach(() => {
  // Roster tests run against the stub by default; DEVAGENT_HERDR=1 forces the
  // fallback tests onto the (failing) pane path so the sink fires. The stub
  // answers `workspace list` with an empty roster, so ensureHerdrServer
  // "starts a server" but no pane is ever created -> runCommandInHerdrPane
  // returns null -> loud fallback.
  process.env.DEVAGENT_HERDR_BIN = stubBin;
  process.env.DEVAGENT_HERDR_SESSION = 'testsession';
  process.env.DEVAGENT_HERDR = '1';
});
afterAll(() => {
  rmSync(stubDir, { recursive: true, force: true });
});

afterEach(() => {
  for (const k of ['DEVAGENT_HERDR_BIN', 'DEVAGENT_HERDR', 'DEVAGENT_HERDR_SESSION', 'DEVAGENT_VISIBILITY', 'STUB_PANES', 'STUB_FAIL']) {
    delete process.env[k];
  }
  setFallbackSink(null);
});

/** Two-pane roster: TASK-abc running, TASK-old idle in a stale worktree. */
function setPanestub(): void {
  process.env.STUB_PANES = JSON.stringify([
    {
      name: 'TASK-abc-a1',
      label: 'TASK-abc-a1',
      pane_id: 'pane-abc',
      workspace_id: 'ws-abc',
      agent_status: 'working',
      cwd: '/tmp/devagent/.devagent-worktrees/TASK-abc-a1',
      created_at: '2026-09-04T10:00:00Z',
    },
    {
      name: 'TASK-old-a1',
      label: 'TASK-old-a1',
      pane_id: 'pane-old',
      workspace_id: 'ws-old',
      agent_status: 'idle',
      cwd: '/tmp/devagent/.devagent-worktrees/TASK-old-a1',
    },
    {
      name: 'shell',
      label: 'scratch shell',
      pane_id: 'pane-shell',
      agent_status: 'idle',
      cwd: '/tmp/devagent',
    },
  ]);
}

describe('FR-VIS visibility resolution', () => {
  it('shouldUseHerdr: explicit flag wins over every env', () => {
    process.env.DEVAGENT_HERDR = '0';
    process.env.DEVAGENT_VISIBILITY = 'visible';
    expect(shouldUseHerdr(true)).toBe(true);
    expect(shouldUseHerdr(false)).toBe(false);
  });

  it('shouldUseHerdr: DEVAGENT_HERDR beats DEVAGENT_VISIBILITY', () => {
    process.env.DEVAGENT_HERDR = '1';
    process.env.DEVAGENT_VISIBILITY = 'headless';
    expect(shouldUseHerdr()).toBe(true);
    process.env.DEVAGENT_HERDR = '0';
    process.env.DEVAGENT_VISIBILITY = 'visible';
    expect(shouldUseHerdr()).toBe(false);
  });

  it('shouldUseHerdr: DEVAGENT_VISIBILITY=headless forces direct spawn', () => {
    delete process.env.DEVAGENT_HERDR;
    process.env.DEVAGENT_VISIBILITY = 'headless';
    expect(shouldUseHerdr()).toBe(false);
  });

  it('shouldUseHerdr: DEVAGENT_VISIBILITY=visible routes to panes', () => {
    delete process.env.DEVAGENT_HERDR;
    process.env.DEVAGENT_VISIBILITY = 'visible';
    expect(shouldUseHerdr()).toBe(true);
  });

  it('shouldUseHerdr: defaults to visible when nothing is set (FR-VIS-01 flip)', () => {
    delete process.env.DEVAGENT_HERDR;
    expect(shouldUseHerdr()).toBe(true);
  });

  it('spawnVisibility: env wins over config, config wins over default', () => {
    delete process.env.DEVAGENT_VISIBILITY;
    expect(spawnVisibility({ spawn: { visibility: 'headless' } } as never)).toBe('headless');
    expect(spawnVisibility({} as never)).toBe('visible');
    process.env.DEVAGENT_VISIBILITY = 'headless';
    expect(spawnVisibility({ spawn: { visibility: 'visible' } } as never)).toBe('headless');
  });

  it('PANE_ENV_OP_ATTACH is the documented env name', () => {
    expect(PANE_ENV_OP_ATTACH).toBe('DEVAGENT_OPERATOR_ATTACHED');
  });
});

describe('FR-VIS loud fallbacks', () => {
  it('warns once per spawn site (worker CLI name), not per launch', async () => {
    const warnings: Array<{ site: string; message: string }> = [];
    setFallbackSink((site, message) => warnings.push({ site, message }));
    // Two omp launches + one claude-code launch, all falling back.
    const run = (cmd: string): Promise<SpawnCliResult> =>
      runWorkerCli(cmd, ['--version'], { herdr: true, cwd: stubDir, timeoutMs: 5_000 });
    await run('true');
    await run('true');
    await run('cat');
    const sites = warnings.map((w) => w.site);
    expect(sites.filter((s) => s === 'true').length).toBe(1);
    expect(sites.filter((s) => s === 'cat').length).toBe(1);
    expect(warnings.length).toBe(2);
    expect(warnings[0]!.message).toContain('directly');
  });

  it('clears dedupe on sink reinstall so a second fallback re-warns', async () => {
    const warnings: string[] = [];
    setFallbackSink((_site, message) => warnings.push(message));
    await runWorkerCli('true', ['--version'], { herdr: true, cwd: stubDir, timeoutMs: 5_000 });
    expect(warnings.length).toBe(1);
    setFallbackSink((_site, message) => warnings.push(message));
    await runWorkerCli('true', ['--version'], { herdr: true, cwd: stubDir, timeoutMs: 5_000 });
    expect(warnings.length).toBe(2);
  });
});

describe('FR-VIS session pane roster', () => {
  // DEVAGENT_HERDR_BIN/SESSION come from the global beforeEach; the fallback
  // tests' DEVAGENT_HERDR=1 is irrelevant here (no herdr:true is passed).

  it('maps agent rows to SessionPaneInfo with task ids and states', async () => {
    setPanestub();
    const panes = await listSessionPanes('testsession');
    expect(panes.length).toBe(3);
    const abc = panes.find((p) => p.paneId === 'pane-abc')!;
    expect(abc.taskId).toBe('TASK-abc');
    // Worker = label prefix before the FIRST '-a' ('TASK-abc-a1' -> 'TASK');
    // contract-literal parsing, even when the task id itself is hyphenated.
    expect(abc.worker).toBe('TASK');
    expect(abc.role).toBe('worker');
    expect(abc.state).toBe('running');
    expect(abc.agentStatus).toBe('working');
    expect(abc.cwd).toContain('.devagent-worktrees');
    expect(abc.startedAt).toBe('2026-09-04T10:00:00Z');
    const old = panes.find((p) => p.paneId === 'pane-old')!;
    expect(old.taskId).toBe('TASK-old');
    const shell = panes.find((p) => p.paneId === 'pane-shell')!;
    // Scratch (non-worktree) pane: no task semantics -> empty taskId,
    // worker falls back to 'unknown' (label carries no attempt suffix).
    expect(shell.taskId).toBe('');
    expect(shell.worker).toBe('unknown');
    expect(shell.state).toBe('idle');
  });

  it('strips recovery attempt suffixes (-a1r2) from task ids', async () => {
    process.env.STUB_PANES = JSON.stringify([
      {
        label: 'TASK-x-a2r1',
        pane_id: 'p',
        agent_status: 'working',
        cwd: '/repo/.devagent-worktrees/TASK-x-a2r1',
      },
    ]);
    const panes = await listSessionPanes();
    expect(panes[0]!.taskId).toBe('TASK-x');
  });

  it('returns [] when the herdr CLI fails or emits junk', async () => {
    process.env.STUB_FAIL = '1';
    expect(await listSessionPanes()).toEqual([]);
    process.env.STUB_FAIL = '';
    process.env.STUB_PANES = 'not json at all';
    expect(await listSessionPanes()).toEqual([]);
  });

  it('attachCommandFor returns the herdr attach command for a rostered task', async () => {
    setPanestub();
    expect(await attachCommandFor('TASK-abc')).toBe('herdr --session testsession agent attach pane-abc');
    expect(await attachCommandFor('TASK-missing')).toBeNull();
  });

  it('operatorAttachTrace is true only for a live pane of the task', async () => {
    setPanestub();
    expect(await operatorAttachTrace('TASK-abc')).toBe(true);
    expect(await operatorAttachTrace('TASK-old')).toBe(false); // stale, not live
    expect(await operatorAttachTrace('TASK-missing')).toBe(false);
  });
});

describe('FR-VIS operator-attach ledger roundtrip', () => {
  it('appendOperatorAttachRecord persists a row readLedger can filter by task', () => {
    const repo = mkdtempSync(join(tmpdir(), 'devagent-attach-ledger-'));
    try {
      appendOperatorAttachRecord(repo, {
        ts: '2026-09-04T12:00:00Z',
        kind: 'event',
        event: 'operator-attached',
        taskId: 'TASK-abc',
        attempt: 1,
        paneId: 'pane-abc',
        session: 'devagent',
      });
      // readLedger is audit-shaped; raw JSONL is the storage contract.
      const raw = readFileSync(join(repo, '.devagent', 'runs', 'orchestration', 'events.jsonl'), 'utf8').trim();
      const row = JSON.parse(raw) as Record<string, unknown>;
      expect(row.event).toBe('operator-attached');
      expect(row.taskId).toBe('TASK-abc');
      expect(row.paneId).toBe('pane-abc');
      expect(row.session).toBe('devagent');
      expect(row.kind).toBe('event');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});
