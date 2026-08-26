import { afterAll, afterEach, beforeAll, describe, expect, it } from 'vitest';
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  ensureHerdrServer,
  herdrServerUp,
  runCommandInHerdrPane,
} from '../src/integrations/herdr.js';
import { runWorkerCli, shouldUseHerdr } from '../src/workers/herdr-runtime.js';
import { herdrEnabled, herdrSessionName, loadConfig } from '../src/config.js';

// Functional stub of the `herdr` CLI: implements just enough of the surface
// devagent uses (workspace create/list/close, pane rename/run/send-keys) and
// actually executes `pane run` commands through a real shell, so the whole
// env-file + redirect + marker protocol is exercised end to end.
const STUB = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
if (args[0] === '--session') args.splice(0, 2);
const out = (o) => process.stdout.write(JSON.stringify(o) + '\\n');
const log = (line) => fs.appendFileSync(process.env.STUB_LOG || '/tmp/stub-herdr.log', line + '\\n');
log(args.join(' '));
if (args[0] === 'workspace' && args[1] === 'list') {
  if (process.env.STUB_SERVER_DOWN === '1') { console.error('server_not_running'); process.exit(1); }
  out({ id: 'x', result: { type: 'workspace_list', workspaces: [] } });
} else if (args[0] === 'workspace' && args[1] === 'create') {
  out({ id: 'x', result: { type: 'workspace_created',
    workspace: { workspace_id: 'w1' }, tab: { tab_id: 'w1:t1' }, root_pane: { pane_id: 'w1:p1' } } });
} else if (args[0] === 'pane' && args[1] === 'rename') {
  out({ id: 'x', result: {} });
} else if (args[0] === 'pane' && args[1] === 'run') {
  // Like the real server: type the command into the pane and return
  // immediately; the pane shell owns the process lifetime.
  const c = require('node:child_process').spawn('/bin/sh', ['-c', args[3]], { stdio: 'ignore', detached: true });
  c.unref();
  out({ id: 'x', result: {} });
} else if (args[0] === 'pane' && args[1] === 'send-keys') {
  out({ id: 'x', result: {} });
} else if (args[0] === 'workspace' && args[1] === 'close') {
  out({ id: 'x', result: {} });
} else {
  console.error('stub-herdr: unsupported command: ' + args.join(' '));
  process.exit(2);
}
`;

let stubDir: string;
let stubBin: string;
let stubLog: string;

beforeAll(() => {
  stubDir = mkdtempSync(join(tmpdir(), 'devagent-herdr-test-'));
  stubBin = join(stubDir, 'herdr-stub.cjs');
  stubLog = join(stubDir, 'calls.log');
  writeFileSync(stubBin, STUB);
  chmodSync(stubBin, 0o755);
});

afterEach(() => {
  delete process.env.DEVAGENT_HERDR;
  delete process.env.DEVAGENT_HERDR_SESSION;
  // The stub inherits this process's env; never leak failure simulation.
  delete process.env.STUB_SERVER_DOWN;
  try { rmSync(stubLog, { force: true }); } catch {}
});

afterAll(() => {
  rmSync(stubDir, { recursive: true, force: true });
});

function calls(): string[] {
  try {
    return readFileSync(stubLog, 'utf8').trim().split('\n');
  } catch {
    return [];
  }
}

describe('herdr integration', () => {
  it('detects the session server via workspace list', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    await expect(herdrServerUp('t')).resolves.toBe(true);
  });

  it('ensureHerdrServer reports false quickly when the server never comes up', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    process.env.STUB_SERVER_DOWN = '1';
    const ok = await ensureHerdrServer('t', stubBin, { attempts: 2, delayMs: 10 });
    expect(ok).toBe(false);
  });

  it('runs a command in a pane and captures stdout and exit code', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    const r = await runCommandInHerdrPane('echo', ['hello'], {
      cwd: stubDir,
      timeoutMs: 10_000,
      label: 'test worker',
    });
    expect(r).not.toBeNull();
    expect(r!.timedOut).toBe(false);
    expect(r!.exitCode).toBe(0);
    expect(r!.stdout).toBe('hello\n');
    // hygiene default: workspace closed after capture
    expect(calls().some((l) => l.startsWith('workspace close w1'))).toBe(true);
  });

  it('propagates nonzero exit codes through the marker file', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    const r = await runCommandInHerdrPane('sh', ['-c', 'echo boom >&2; exit 3'], {
      cwd: stubDir,
      timeoutMs: 10_000,
    });
    expect(r!.exitCode).toBe(3);
    expect(r!.stderr).toContain('boom');
  });

  it('injects opts.env into the pane environment without leaking it on the command line', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    const target = join(stubDir, 'secret.out');
    const r = await runCommandInHerdrPane('sh', ['-c', `printf %s "$SECRET_V" > '${target}'; echo ok`], {
      cwd: stubDir,
      timeoutMs: 10_000,
      env: { SECRET_V: 's3cret-value' },
    });
    expect(r!.exitCode).toBe(0);
    expect(readFileSync(target, 'utf8')).toBe('s3cret-value');
    // The secret reaches the pane via a sourced env file, never argv/scrollback.
    expect(calls().join('\n')).not.toContain('s3cret-value');
  }, 20_000);

  it('maps wall-clock timeout to timedOut=true and tears the workspace down', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.STUB_LOG = stubLog;
    const r = await runCommandInHerdrPane('sleep', ['30'], {
      cwd: stubDir,
      timeoutMs: 700,
    });
    expect(r!.timedOut).toBe(true);
    expect(r!.exitCode).toBe(-1);
    expect(calls().some((l) => l.includes('send-keys w1:p1 ctrl+c'))).toBe(true);
    expect(calls().some((l) => l.startsWith('workspace close w1'))).toBe(true);
  }, 20_000);

  it('keeps completed panes open when DEVAGENT_HERDR_KEEP_PANES=1', async () => {
    process.env.DEVAGENT_HERDR_BIN = stubBin;
    process.env.DEVAGENT_HERDR_KEEP_PANES = '1';
    process.env.STUB_LOG = stubLog;
    await runCommandInHerdrPane('echo', ['kept'], { cwd: stubDir, timeoutMs: 10_000 });
    expect(calls().filter((l) => l.startsWith('workspace close')).length).toBe(0);
    delete process.env.DEVAGENT_HERDR_KEEP_PANES;
  });
});

describe('herdr-runtime fallback', () => {
  it('falls back to direct spawn when herdr cannot serve', async () => {
    process.env.DEVAGENT_HERDR_BIN = join(stubDir, 'does-not-exist');
    const r = await runWorkerCli('echo', ['direct-fallback'], {
      cwd: stubDir,
      timeoutMs: 10_000,
      herdr: true,
    });
    expect(r.exitCode).toBe(0);
    expect(r.stdout).toBe('direct-fallback\n');
  });

  it('shouldUseHerdr honors explicit flag over env; env decides when unset', () => {
    process.env.DEVAGENT_HERDR = '0';
    expect(shouldUseHerdr(undefined)).toBe(false);
    expect(shouldUseHerdr(true)).toBe(true);
    process.env.DEVAGENT_HERDR = '1';
    expect(shouldUseHerdr(undefined)).toBe(true);
    expect(shouldUseHerdr(false)).toBe(false);
  });
});

describe('herdr config', () => {
  const repoWithConfig = (cfg: Record<string, unknown>): string => {
    const dir = mkdtempSync(join(tmpdir(), 'devagent-cfg-'));
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify(cfg));
    return dir;
  };

  it('parses herdr block and validates session name', () => {
    const dir = repoWithConfig({ worker: 'claude-code', herdr: { enabled: true, session: 'my-agents' } });
    const cfg = loadConfig(dir);
    expect(cfg.herdr?.enabled).toBe(true);
    expect(cfg.herdr?.session).toBe('my-agents');
    rmSync(dir, { recursive: true, force: true });
  });

  it('rejects invalid herdr.session', () => {
    const dir = repoWithConfig({ worker: 'claude-code', herdr: { session: 'Bad Name!' } });
    expect(() => loadConfig(dir)).toThrow(/Invalid herdr\.session/);
    rmSync(dir, { recursive: true, force: true });
  });

  it('env DEVAGENT_HERDR overrides config; session name resolution order holds', () => {
    const dir = repoWithConfig({ worker: 'claude-code', herdr: { enabled: true, session: 'from-config' } });
    const cfg = loadConfig(dir);
    delete process.env.DEVAGENT_HERDR;
    expect(herdrEnabled(cfg)).toBe(true);
    expect(herdrSessionName(cfg)).toBe('from-config');
    process.env.DEVAGENT_HERDR_SESSION = 'from-env';
    expect(herdrSessionName(cfg)).toBe('from-env');
    process.env.DEVAGENT_HERDR = '0';
    expect(herdrEnabled(cfg)).toBe(false);
    delete process.env.DEVAGENT_HERDR_SESSION;
    rmSync(dir, { recursive: true, force: true });
  });
});
