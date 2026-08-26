import { execFile, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildEnv, type SpawnCliOptions, type SpawnCliResult } from '../workers/spawn-utils.js';

/**
 * Herdr runtime integration (https://github.com/herdrdev/herdr).
 *
 * Herdr is a persistent terminal workspace manager for coding agents. When
 * enabled, worker launches execute inside a pane of a dedicated named herdr
 * session ("devagent" by default) instead of a direct child process:
 * runs stay visible in a reattachable TUI, survive client disconnects, and
 * leave per-run workspaces behind for inspection.
 *
 * Output contract: the worker command's stdout/stderr are redirected to temp
 * files inside the pane and completion is signaled with an exit-code marker
 * file. Devagent polls those files, so the exact stdout JSON parsing the
 * adapters rely on is preserved (no pty scraping). Progress for the
 * no-progress watchdog is derived from captured-file growth.
 */

const POLL_MS = 250;

export interface HerdrRuntimeOptions {
  /** Named persistent session; defaults to DEVAGENT_HERDR_SESSION or "devagent". */
  session?: string;
  /** Herdr binary override (tests inject a stub); defaults to "herdr". */
  bin?: string;
}

export function herdrBin(): string {
  return process.env.DEVAGENT_HERDR_BIN || 'herdr';
}

export function resolveSession(explicit?: string): string {
  return explicit || process.env.DEVAGENT_HERDR_SESSION || 'devagent';
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

export interface HerdrCliResult {
  code: number;
  stdout: string;
  stderr: string;
}

/** Run one herdr CLI command to completion (5s default cap — these are local RPCs). */
export function herdrCli(args: string[], opts: { timeoutMs?: number } = {}): Promise<HerdrCliResult> {
  return new Promise((resolve) => {
    execFile(
      herdrBin(),
      args,
      { timeout: opts.timeoutMs ?? 5_000, encoding: 'utf8', maxBuffer: 8 * 1024 * 1024 },
      (error, stdout, stderr) => {
        const rawCode = (error as { code?: unknown } | null)?.code;
        resolve({
          code: error === null ? 0 : typeof rawCode === 'number' ? rawCode : -1,
          stdout: String(stdout ?? ''),
          stderr: String(stderr ?? ''),
        });
      },
    );
  });
}

function parseCliJson(stdout: string): Record<string, unknown> | null {
  try {
    return JSON.parse(stdout) as Record<string, unknown>;
  } catch {
    return null;
  }
}

/** True when the named session's server answers `workspace list`. */
export async function herdrServerUp(session: string): Promise<boolean> {
  const r = await herdrCli(['--session', session, 'workspace', 'list'], { timeoutMs: 5_000 });
  const parsed = parseCliJson(r.stdout);
  return r.code === 0 && (parsed?.result as { type?: string } | undefined)?.type === 'workspace_list';
}

/**
 * Ensure the dedicated session's headless server is running, starting it
 * detached when necessary. Returns false when herdr cannot serve at all.
 */
export async function ensureHerdrServer(
  session: string,
  bin = herdrBin(),
  wait: { attempts?: number; delayMs?: number } = {},
): Promise<boolean> {
  if (await herdrServerUp(session)) return true;
  let spawnFailed = false;
  try {
    const child = spawn(bin, ['--session', session, 'server'], { stdio: 'ignore', detached: true });
    child.unref();
    // ENOENT etc: no point polling a server that can never start.
    child.on('error', () => {
      spawnFailed = true;
    });
  } catch {
    return false;
  }
  const attempts = wait.attempts ?? 24;
  const delayMs = wait.delayMs ?? 500;
  for (let i = 0; i < attempts; i++) {
    await sleep(delayMs);
    if (spawnFailed) return false;
    if (await herdrServerUp(session)) return true;
  }
  return false;
}

function shQuote(s: string): string {
  return `'${s.replaceAll("'", `'\\''`)}'`;
}

interface PaneRefs {
  workspaceId: string;
  paneId: string;
}

async function openPane(label: string, cwd: string, session: string): Promise<PaneRefs | null> {
  const r = await herdrCli([
    '--session', session,
    'workspace', 'create',
    '--label', label,
    '--cwd', cwd,
  ]);
  const parsed = parseCliJson(r.stdout);
  const result = parsed?.result as { workspace?: { workspace_id?: string }; root_pane?: { pane_id?: string } } | undefined;
  if (r.code !== 0 || !result?.workspace?.workspace_id || !result?.root_pane?.pane_id) return null;
  const refs = { workspaceId: result.workspace.workspace_id, paneId: result.root_pane.pane_id };
  // Best-effort label on the pane itself so it reads well in the tab bar.
  await herdrCli(['--session', session, 'pane', 'rename', refs.paneId, label]);
  return refs;
}

async function closeWorkspace(workspaceId: string, session: string): Promise<void> {
  await herdrCli(['--session', session, 'workspace', 'close', workspaceId], { timeoutMs: 10_000 });
}

function fileSize(p: string): number {
  try {
    return statSync(p).size;
  } catch {
    return 0;
  }
}

/**
 * Execute `cmd args...` inside a new herdr pane of the dedicated session.
 * Returns the same SpawnCliResult shape as spawnCli, or null when herdr is
 * unavailable/misbehaving so callers can fall back to direct execution.
 */
export async function runCommandInHerdrPane(
  cmd: string,
  args: string[],
  opts: SpawnCliOptions & HerdrRuntimeOptions & { label?: string },
): Promise<SpawnCliResult | null> {
  const session = resolveSession(opts.session);
  if (!(await ensureHerdrServer(session))) return null;

  const refs = await openPane(opts.label ?? 'devagent worker', opts.cwd, session);
  if (!refs) return null;

  const dir = mkdtempSync(join(tmpdir(), 'devagent-herdr-'));
  const outFile = join(dir, 'out');
  const errFile = join(dir, 'err');
  const doneFile = join(dir, 'done');

  try {
    // Panes inherit the server daemon's environment, not this process', so the
    // full computed child env is materialized into a source-only env file.
    // It is removed by the pane script immediately after sourcing and never
    // appears on the command line (nothing leaks into pane scrollback).
    const env = buildEnv(opts);
    const envFile = join(dir, 'env.sh');
    writeFileSync(
      envFile,
      Object.entries(env)
        .filter(([, v]) => v !== undefined)
        .map(([k, v]) => `export ${k}=${shQuote(String(v))}`)
        .join('\n') + '\n',
      { mode: 0o600 },
    );

    const script = [
      `set -a; . ${shQuote(envFile)}; set +a`,
      `rm -f ${shQuote(envFile)}`,
      `cd ${shQuote(opts.cwd)} || exit 125`,
      `${shQuote(cmd)} ${args.map(shQuote).join(' ')} > ${shQuote(outFile)} 2> ${shQuote(errFile)}`,
      `echo $? > ${shQuote(doneFile)}`,
    ].join('; ');
    const run = await herdrCli(['--session', session, 'pane', 'run', refs.paneId, script], { timeoutMs: 30_000 });
    if (run.code !== 0) return null;

    const noProgressMs = opts.noProgressTimeoutMs ?? 0;
    const start = Date.now();
    let lastBytes = -1;
    let lastProgressAt = Date.now();
    let timedOut = false;

    while (true) {
      if (existsSync(doneFile)) break;
      const now = Date.now();
      const bytes = fileSize(outFile) + fileSize(errFile);
      if (bytes !== lastBytes) {
        lastBytes = bytes;
        lastProgressAt = now;
      }
      if (now >= start + opts.timeoutMs || (noProgressMs > 0 && now - lastProgressAt >= noProgressMs)) {
        timedOut = true;
        break;
      }
      await sleep(POLL_MS);
    }

    if (timedOut) {
      // Match spawnCli's SIGKILL semantics: interrupt the foreground process
      // hard, then tear the workspace down.
      await herdrCli(['--session', session, 'pane', 'send-keys', refs.paneId, 'ctrl+c']);
      await herdrCli(['--session', session, 'pane', 'send-keys', refs.paneId, 'ctrl+c'], { timeoutMs: 3_000 });
      await closeWorkspace(refs.workspaceId, session);
      return {
        exitCode: -1,
        stdout: readIfExists(outFile),
        stderr: readIfExists(errFile),
        timedOut: true,
      };
    }

    let exitCode = -1;
    try {
      exitCode = Number.parseInt(readFileSync(doneFile, 'utf8').trim(), 10);
      if (!Number.isFinite(exitCode)) exitCode = -1;
    } catch {
      // marker vanished between existsSync and read — treat as spawn failure
    }

    const result: SpawnCliResult = {
      exitCode,
      stdout: readIfExists(outFile),
      stderr: readIfExists(errFile),
      timedOut: false,
    };

    // Hygiene default: tear the run workspace down once its output is
    // captured. Set DEVAGENT_HERDR_KEEP_PANES=1 to leave completed runs open
    // in the session for inspection.
    if (process.env.DEVAGENT_HERDR_KEEP_PANES !== '1') {
      await closeWorkspace(refs.workspaceId, session);
    }
    return result;
  } finally {
    // Captured output lives in the returned strings; the scratch dir is done.
    rmSync(dir, { recursive: true, force: true });
  }
}

function readIfExists(p: string): string {
  try {
    return readFileSync(p, 'utf8');
  } catch {
    return '';
  }
}
