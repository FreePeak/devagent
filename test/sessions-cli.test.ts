import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runAttach, runSessions } from '../src/commands/sessions.js';
import { LEDGER_DIR } from '../src/orchestrator/ledger.js';

// Stub of the `herdr` CLI exposing just the `agent list` / `pane list`
// surface the sessions/attach operator commands consume. STUB_AGENTS /
// STUB_PANES carry the fixture JSON; STUB_SERVER_DOWN=1 simulates a session
// with no server so listSessionPanes must degrade to [] without throwing.
const STUB = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
if (args[0] === '--session') args.splice(0, 2);
const out = (o) => process.stdout.write(JSON.stringify(o) + '\\n');
const log = (line) => fs.appendFileSync(process.env.STUB_LOG || '/tmp/stub-sessions.log', line + '\\n');
log(args.join(' '));
if (process.env.STUB_SERVER_DOWN === '1') { console.error('server_not_running'); process.exit(1); }
if (args[0] === 'agent' && args[1] === 'list') {
  out({ id: 'x', result: { type: 'agent_list', agents: JSON.parse(process.env.STUB_AGENTS || '[]') } });
} else if (args[0] === 'pane' && args[1] === 'list') {
  out({ id: 'x', result: { type: 'pane_list', panes: JSON.parse(process.env.STUB_PANES || '[]') } });
} else {
  console.error('stub-herdr: unsupported command: ' + args.join(' '));
  process.exit(2);
}
`;

let stubDir: string;
let stubBin: string;
let stubLog: string;
let repoPath: string;
let logs: string[];
let errs: string[];
let priorExitCode: string | number | undefined;

beforeAll(() => {
  stubDir = mkdtempSync(join(tmpdir(), 'devagent-sessions-test-'));
  stubBin = join(stubDir, 'herdr-stub.cjs');
  stubLog = join(stubDir, 'calls.log');
  writeFileSync(stubBin, STUB);
  chmodSync(stubBin, 0o755);
});

afterAll(() => {
  rmSync(stubDir, { recursive: true, force: true });
});

beforeEach(() => {
  repoPath = mkdtempSync(join(tmpdir(), 'devagent-sessions-repo-'));
  process.env.DEVAGENT_HERDR_BIN = stubBin;
  process.env.DEVAGENT_HERDR_SESSION = 'test-session';
  process.env.STUB_LOG = stubLog;
  logs = [];
  errs = [];
  priorExitCode = process.exitCode;
  vi.spyOn(console, 'log').mockImplementation((...a: unknown[]) => {
    logs.push(a.map(String).join(' '));
  });
  vi.spyOn(console, 'error').mockImplementation((...a: unknown[]) => {
    errs.push(a.map(String).join(' '));
  });
});

afterEach(() => {
  vi.restoreAllMocks();
  delete process.env.DEVAGENT_HERDR_BIN;
  delete process.env.DEVAGENT_HERDR_SESSION;
  delete process.env.STUB_SERVER_DOWN;
  delete process.env.STUB_AGENTS;
  delete process.env.STUB_PANES;
  delete process.env.STUB_LOG;
  process.exitCode = priorExitCode;
  rmSync(repoPath, { recursive: true, force: true });
});

function ledgerRows(): Array<Record<string, unknown>> {
  const file = join(repoPath, LEDGER_DIR, 'events.jsonl');
  if (!existsSync(file)) return [];
  return readFileSync(file, 'utf8')
    .split('\n')
    .filter((l) => l.trim())
    .map((l) => JSON.parse(l) as Record<string, unknown>);
}

describe('devagent sessions CLI', () => {
  it('runSessions --json prints [] when the herdr server is down', async () => {
    process.env.STUB_SERVER_DOWN = '1';
    await runSessions({ json: true, repoPath });
    expect(logs.join('\n').trim()).toBe('[]');
    expect(errs).toEqual([]);
  });

  it('runSessions prints a table and runAttach records the operator attach', async () => {
    process.env.STUB_LOG = stubLog;
    process.env.STUB_AGENTS = JSON.stringify([
      {
        name: 'TASK-x-a1',
        label: 'TASK-x-a1',
        pane_id: 'w1:p9',
        workspace_id: 'w1',
        agent_status: 'working',
        cwd: join(repoPath, '.devagent-worktrees', 'TASK-x-a1'),
      },
    ]);

    await runSessions({ repoPath });
    expect(logs[0]?.startsWith('PANE')).toBe(true);
    expect(logs.join('\n')).toContain('w1:p9');

    logs = [];
    await runAttach('TASK-x', { repoPath });
    expect(logs.join('\n')).toContain('agent attach');
    expect(errs).toEqual([]);

    const rows = ledgerRows();
    const attach = rows.find((r) => r.event === 'operator-attached');
    expect(attach).toMatchObject({ taskId: 'TASK-x', paneId: 'w1:p9', session: 'test-session' });
  });

  it('runAttach exits 1 when no pane matches the task', async () => {
    process.env.STUB_AGENTS = '[]';
    await runAttach('TASK-missing', { repoPath });
    expect(process.exitCode).toBe(1);
    expect(errs.join('\n')).toContain('TASK-missing');
    expect(ledgerRows()).toEqual([]);
  });
});
