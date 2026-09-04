import { execFile, spawn } from 'node:child_process';
import { existsSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, join } from 'node:path';
import { buildEnv, type SpawnCliOptions, type SpawnCliResult } from '../workers/spawn-utils.js';
import { appendWatchdogHealthRecord } from '../orchestrator/ledger.js';

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

/** Vars unset in every pane before env.sh is sourced (mirrors NESTED_ENV_BLOCKLIST). */
const NESTED_PANE_UNSETS = [
  'unset ANTHROPIC_MODEL',
  'unset ANTHROPIC_SMALL_FAST_MODEL',
  'unset CLAUDE_CODE_ENTRYPOINT',
  'unset CLAUDECODE',
];

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
    // Panes inherit the daemon's environment, so the daemon must start with a
    // scrubbed env too — a parent's ANTHROPIC_MODEL would otherwise leak into
    // every worker pane (env.sh only overrides, it never unsets).
    const child = spawn(bin, ['--session', session, 'server'], {
      stdio: 'ignore',
      detached: true,
      env: buildEnv({ cwd: process.cwd(), timeoutMs: 0 }),
    });
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

/**
 * Render a sourceable env file from an env record. Keys that are not valid
 * shell identifiers (npm injects `npm_config_node_pre_gyp:cache`, and any
 * var with a dash or colon) are skipped: `export KEY=val` for those aborts
 * the whole `source` under zsh ("not valid in this context"), which left the
 * worker pane without PATH/HOME and the process was killed. The pane shell
 * already carries such vars in its own environment when they were set, so
 * dropping them from the override file loses nothing.
 */
export function renderEnvFile(env: NodeJS.ProcessEnv): string {
  return Object.entries(env)
    .filter(([, v]) => v !== undefined)
    .filter(([k]) => /^[A-Za-z_][A-Za-z0-9_]*$/.test(k))
    .map(([k, v]) => `export ${k}=${shQuote(String(v))}`)
    .join('\n') + '\n';
}

interface PaneRefs {
  workspaceId: string;
  paneId: string;
}

/**
 * Pane/workspace name: the cwd's basename. Worktree checkouts encode the
 * task in their directory name (.devagent-worktrees/T1-a1), so the name
 * identifies what is running without any per-call label plumbing.
 */
function paneNameForCwd(cwd: string): string {
  const base = basename(cwd).trim();
  return base.length > 0 ? base : 'devagent worker';
}

async function openPane(cwd: string, session: string): Promise<PaneRefs | null> {
  const name = paneNameForCwd(cwd);
  const r = await herdrCli([
    '--session', session,
    'workspace', 'create',
    '--label', name,
    '--cwd', cwd,
  ]);
  const parsed = parseCliJson(r.stdout);
  const result = parsed?.result as { workspace?: { workspace_id?: string }; root_pane?: { pane_id?: string } } | undefined;
  if (r.code !== 0 || !result?.workspace?.workspace_id || !result?.root_pane?.pane_id) return null;
  const refs = { workspaceId: result.workspace.workspace_id, paneId: result.root_pane.pane_id };
  // Best-effort name on the pane itself so it reads well in the tab bar.
  await herdrCli(['--session', session, 'pane', 'rename', refs.paneId, name]);
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
  opts: SpawnCliOptions & HerdrRuntimeOptions,
): Promise<SpawnCliResult | null> {
  const session = resolveSession(opts.session);
  if (!(await ensureHerdrServer(session))) return null;

  const refs = await openPane(opts.cwd, session);
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
    writeFileSync(envFile, renderEnvFile(env), { mode: 0o600 });

    const script = [
      // Unset harness-injected vars the daemon may have leaked before env.sh
      // overrides — claude rejects a nested ANTHROPIC_MODEL outright.
      ...NESTED_PANE_UNSETS,
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
    let watchdogFired = false;
    let clockResets = 0;

    // Progress = NEW TOOLCALL OR TEXT output, not raw byte growth. glm-style
    // models stream thinking_delta continuously (2026-08-31 live evidence:
    // 60k+ thinking deltas, 8-11 MB, while making zero tool calls for the
    // full hour), so byte-counting treats deliberation as progress and the
    // no-progress watchdog never fires. Strip thinking_delta lines before
    // counting: a run that only thinks is a hang in headless mode.
    // PRD Q33: progress via the shared classifier — only lines evidencing new
    // work (tool calls, answer text) count; thinking-only lines never do.
    const { isNdjsonProgressLine } = await import('../workers/progress.js');
    const meaningfulBytes = (p: string): number => {
      let n = 0;
      try {
        for (const line of readFileSync(p, 'utf8').split('\n')) {
          if (!isNdjsonProgressLine(line)) continue;
          n += line.length + 1;
        }
      } catch {
        // File not there yet / mid-rename: zero meaningful bytes.
      }
      return n;
    };
    const seededBytes = meaningfulBytes(outFile) + meaningfulBytes(errFile);
    lastBytes = seededBytes;
    // Q34: the pre-loop seed counts as one clock reset when the pane already
    // carried meaningful output.
    if (seededBytes > 0) clockResets++;
    const graceMs = Math.min(noProgressMs, 60_000);

    while (true) {
      // Sample output BEFORE the done-check: a fast pane (echo → exit) can
      // finish between polls, and checking the marker first would skip the
      // final progress snapshot — the row would claim zero clock resets and
      // zero meaningful bytes for a perfectly healthy run.
      const now = Date.now();
      const bytes = meaningfulBytes(outFile) + meaningfulBytes(errFile);
      if (bytes !== lastBytes) {
        lastBytes = bytes;
        lastProgressAt = now;
        clockResets++;
      }
      if (existsSync(doneFile)) break;
      if (now >= start + opts.timeoutMs) {
        timedOut = true;
        break;
      }
      if (noProgressMs > 0 && now - lastProgressAt >= noProgressMs && now - start >= graceMs) {
        // Q34: watchdogFired is recorded only for the no-progress branch —
        // wall-clock expiry is a different outcome and the row must not conflate them.
        watchdogFired = true;
        timedOut = true;
        break;
      }
      await sleep(POLL_MS);
    }

    // Q34: exactly one watchdog-health row per pane launch with a clock armed.
    if (opts.watchdogLedger && noProgressMs > 0) {
      const ctx = opts.watchdogLedger;
      appendWatchdogHealthRecord(ctx.repoPath, {
        ts: new Date().toISOString(),
        kind: 'event',
        event: 'watchdog-health',
        taskId: ctx.taskId,
        attempt: ctx.attempt,
        worker: ctx.worker,
        site: 'herdr-pane',
        // FR-VIS: pane launches are operator-visible by definition.
        runtime: 'herdr-pane',
        visible: true,
        visibility: 'herdr-pane',
        noProgressTimeoutMs: noProgressMs,
        watchdogFired,
        wallClockMs: Date.now() - start,
        clockResets,
        meaningfulBytes: lastBytes,
        idleMs: Date.now() - lastProgressAt,
      });
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

/**
 * Session-scoped stale-pane sweep. The named session is the trust boundary:
 * everything in it was spawned by this automation, so idle/unknown agents are
 * leftovers the per-run teardown missed (e.g. orchestrator died mid-run) and
 * are safe to close. Panes with no agent (bare shells) count as stale too.
 * Sessions other than the devagent one are never listed, let alone closed,
 * and the reaper path is untouched — interactive user sessions outside herdr
 * remain out of reach by construction.
 */
export interface StalePane {
  workspaceId: string;
  paneId: string;
  label: string;
  agentStatus: string;
  reason: string;
}

/** Agent statuses that mean "not doing work right now". */
const IDLE_STATUSES = new Set(['idle', 'unknown', 'done']);

export async function findStalePanes(session: string): Promise<StalePane[]> {
  const res = await herdrCli(['--session', session, 'pane', 'list'], { timeoutMs: 10_000 });
  if (res.code !== 0) return [];
  let panes: Array<{ pane_id?: string; workspace_id?: string; label?: string; agent_status?: string }>;
  try {
    panes = JSON.parse(res.stdout)?.result?.panes ?? [];
  } catch {
    return [];
  }
  const stale: StalePane[] = [];
  for (const p of panes) {
    const status = p.agent_status ?? 'unknown';
    // No agent at all (bare shell) => leftover; known agent => only idle ones.
    if (p.agent_status === undefined) {
      stale.push({
        workspaceId: p.workspace_id ?? '',
        paneId: p.pane_id ?? '',
        label: p.label ?? '(no label)',
        agentStatus: status,
        reason: 'no-agent',
      });
    } else if (IDLE_STATUSES.has(status)) {
      stale.push({
        workspaceId: p.workspace_id ?? '',
        paneId: p.pane_id ?? '',
        label: p.label ?? '(no label)',
        agentStatus: status,
        reason: `agent-${status}`,
      });
    }
  }
  return stale;
}

/** Close every stale pane workspace in `session`. Returns what was closed. */
export async function sweepStalePanes(
  session: string,
  opts: { dryRun?: boolean } = {},
): Promise<StalePane[]> {
  const stale = await findStalePanes(session);
  if (opts.dryRun) return stale;
  for (const s of stale) {
    if (!s.workspaceId) continue;
    await closeWorkspace(s.workspaceId, session);
  }
  return stale;
}

// ---------- Operator visibility (FR-VIS): pane roster + attach ----------

/**
 * Env var a pane may set to flag that an operator is attached (herdr-side
 * attach detection); exported so the daemon/TUI can reference it without a
 * magic string. Detection stays herdr-side; devagent only records/traces it.
 */
export const PANE_ENV_OP_ATTACH = 'DEVAGENT_OPERATOR_ATTACHED';

/** One operator-observable worker pane (FR-VIS-02). */
export interface SessionPaneInfo {
  taskId: string;
  role: string;
  worker: string;
  paneId: string;
  workspaceId: string;
  label: string;
  cwd: string;
  agentStatus: string;
  state: 'running' | 'idle' | 'stale';
  /** Pane creation timestamp as reported by herdr; "" when unavailable. */
  startedAt: string;
}


function mapPaneState(agentStatus: string, cwd: string): 'running' | 'idle' | 'stale' {
  // Stale (sweepable) = idle/unknown agent sitting in a .devagent-worktrees
  // checkout — the exact shape sweepStalePanes closes.
  if (IDLE_STATUSES.has(agentStatus) && cwd.includes('.devagent-worktrees')) return 'stale';
  if (agentStatus === 'working') return 'running';
  return 'idle';
}

interface HerdrAgentRow {
  name?: string;
  label?: string;
  pane_id?: string;
  workspace_id?: string;
  agent_status?: string;
  cwd?: string;
  created_at?: string;
}

/**
 * Strip the worktree attempt suffix (`-a1`, `-a1r2` — src/orchestrator/types.ts
 * attemptSuffix shape) from a worktree basename to recover the task id.
 */
function taskIdFromWorktreeBase(base: string): string {
  return base.replace(/-a\d+(?:r\d+)?$/, '');
}

/**
 * Task id from a pane cwd: only worktree checkouts
 * (.devagent-worktrees/<taskId>-a<attempt>) carry one; scratch panes have no
 * task semantics.
 */
function paneTaskIdFromCwd(cwd: string): string {
  if (!cwd.includes('.devagent-worktrees')) return '';
  const base = basename(cwd).trim();
  return base ? taskIdFromWorktreeBase(base) : '';
}

/**
 * Worker identity: the pane label is the worktree basename
 * (<taskId>-a<attempt>); the prefix before the first attempt suffix is the
 * best available worker name. 'unknown' when the label carries no suffix.
 */
function workerFromLabel(label: string): string {
  const idx = label.indexOf('-a');
  return idx > 0 ? label.slice(0, idx) : 'unknown';
}

function parseAgentRows(stdout: string): HerdrAgentRow[] {
  try {
    const parsed = JSON.parse(stdout) as { result?: { agents?: HerdrAgentRow[] } };
    return Array.isArray(parsed?.result?.agents) ? parsed.result.agents : [];
  } catch {
    return [];
  }
}

/**
 * Roster of worker panes in a herdr session (FR-VIS-02): one SessionPaneInfo
 * per agent row, tolerating missing fields. Empty/failed list -> [].
 */
export async function listSessionPanes(session?: string): Promise<SessionPaneInfo[]> {
  const s = resolveSession(session);
  const res = await herdrCli(['--session', s, 'agent', 'list'], { timeoutMs: 10_000 });
  if (res.code !== 0) return [];
  const rows = parseAgentRows(res.stdout);
  const out: SessionPaneInfo[] = [];
  for (const a of rows) {
    const cwd = a.cwd ?? '';
    const label = a.label ?? a.name ?? '';
    const agentStatus = a.agent_status ?? 'unknown';
    out.push({
      taskId: paneTaskIdFromCwd(cwd),
      role: 'worker',
      worker: workerFromLabel(label),
      paneId: a.pane_id ?? '',
      workspaceId: a.workspace_id ?? '',
      label,
      cwd,
      agentStatus,
      state: mapPaneState(agentStatus, cwd),
      // Never fabricated: only what herdr reports.
      startedAt: a.created_at ?? '',
    });
  }
  return out;
}

/**
 * Shell command an operator runs to jump into the task's pane (FR-VIS-03);
 * null when no pane is rostered for the task.
 */
export async function attachCommandFor(taskId: string, session?: string): Promise<string | null> {
  const s = resolveSession(session);
  const panes = await listSessionPanes(s);
  const pane = panes.find((p) => p.taskId === taskId && p.paneId !== '');
  if (!pane) return null;
  return `herdr --session ${s} agent attach ${pane.paneId}`;
}

/**
 * True when a live (running) pane for the task is rostered — the cheap
 * one-call probe later used to suppress watchdog auto-kill while an operator
 * is attached (FR-VIS-03).
 */
export async function operatorAttachTrace(taskId: string, session?: string): Promise<boolean> {
  const panes = await listSessionPanes(session);
  return panes.some((p) => p.taskId === taskId && p.state === 'running');
}
