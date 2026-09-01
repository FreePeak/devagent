import { existsSync, mkdirSync, readFileSync, writeFileSync, readdirSync, realpathSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { tmpdir } from 'node:os';
import { ensureQueueDirs, queueDir, prdsDir } from './queue.js';
import { loadConfig, type DevAgentConfig } from './config.js';
import { spawnCli } from './workers/spawn-utils.js';

export interface CreateOptions {
  repoPath: string;
  scout?: boolean;
  /** Progress-tracker agent: devagent track --interval (role 2 of self-build factory) */
  tracker?: boolean;
  /** Builder agent: scripts/build-loop.sh consuming the queue (role 3) */
  builder?: boolean;
  /** Orchestrator agent: scripts/orchestrate-loop.sh driving the DAG board (role 4) */
  orchestrator?: boolean;
  /** Goal text handed to the orchestrator loop for planning when no board exists */
  orchestratorGoal?: string;
  workers?: number;
  autoMerge?: boolean;
  selfUpdate?: boolean;
  dryRun?: boolean;
  intervalMinutes?: number;
  scoutWorker?: 'opencode' | 'claude-code' | 'omp' | 'pi';
  trackIntervalMinutes?: number;
}

export interface CreateResult {
  ok: boolean;
  detail: string;
  dirs: string[];
  configPath?: string;
  launchAgentPlist?: string;
  /** All installed LaunchAgent plists (scout/tracker/builder), when requested */
  launchAgentPlists?: string[];
  orcaWorktrees?: string[];
}

type CliRunner = (cmd: string, args: string[], opts: { cwd: string; timeoutMs: number }) => Promise<{ exitCode: number; stdout: string; timedOut: boolean }>;
const defaultRunner: CliRunner = (cmd, args, opts) => spawnCli(cmd, args, opts);

function resolveConfigPath(repoPath: string): string {
  // Prefer devagent.json when both exist; otherwise devagent.json is the write target
  const a = join(repoPath, 'devagent.json');
  const b = join(repoPath, '.devagent.json');
  if (existsSync(b) && !existsSync(a)) return b;
  return a;
}

/** Best-effort Orca repo registration: `orca repo add --path <repoPath> --json`. */
async function ensureOrcaRepo(repoPath: string, runner: CliRunner): Promise<boolean> {
  try {
    const r = await runner('orca', ['repo', 'add', '--path', repoPath, '--json'], { cwd: repoPath, timeoutMs: 15_000 });
    return !r.timedOut && r.exitCode === 0;
  } catch {
    return false;
  }
}

/** Create an Orca worktree for a worker slot. Returns worktree path or null. */
export async function createOrcaWorktree(
  repoPath: string,
  name: string,
  runner: CliRunner = defaultRunner,
): Promise<string | null> {
  try {
    const r = await runner('orca', ['worktree', 'create', '--name', name, '--repo', `path:${repoPath}`, '--json'], { cwd: repoPath, timeoutMs: 30_000 });
    if (r.timedOut || r.exitCode !== 0) return null;
    const obj = JSON.parse(r.stdout.slice(r.stdout.indexOf('{'))) as { result?: { path?: string; worktree?: { path?: string } } };
    return obj.result?.path ?? obj.result?.worktree?.path ?? null;
  } catch {
    return null;
  }
}

interface PlistSpec {
  label: string;
  logName: string;
  programArgs: string[];
  repoPath: string;
  /** Extra EnvironmentVariables entries merged into the plist (e.g. ORCHESTRATOR_GOAL) */
  env?: Record<string, string>;
}

export function buildLaunchAgentPlist(spec: PlistSpec): string {
  const { label, logName, programArgs, repoPath, env } = spec;
  const logPath = join(process.env.HOME ?? '/tmp', 'Library/Logs', logName);
  // launchd default PATH is minimal; embed the current PATH so agents can
  // find opencode/claude/git/node installed in user locations.
  const installPath = process.env.PATH ?? '/usr/bin:/bin';
  const xmlEsc = (s: string) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<plist version="1.0">',
    '<dict>',
    `  <key>Label</key><string>${label}</string>`,
    '  <key>EnvironmentVariables</key>',
    '  <dict>',
    `    <key>PATH</key><string>${xmlEsc(installPath)}</string>`,
    `    <key>HOME</key><string>${process.env.HOME ?? ''}</string>`,
    ...Object.entries(env ?? {}).map(([k, v]) => `    <key>${xmlEsc(k)}</key><string>${xmlEsc(v)}</string>`),
    '  </dict>',
    '  <key>ProgramArguments</key>',
    '  <array>',
    ...programArgs.map((a) => `    <string>${xmlEsc(a)}</string>`),
    '  </array>',
    `  <key>WorkingDirectory</key><string>${repoPath}</string>`,
    '  <key>RunAtLoad</key><true/>',
    '  <key>KeepAlive</key><true/>',
    '  <key>StandardOutPath</key><string>' + logPath + '</string>',
    '  <key>StandardErrorPath</key><string>' + logPath + '</string>',
    '  <key>ThrottleInterval</key><integer>60</integer>',
    '</dict>',
    '</plist>',
    '',
  ].join('\n');
}

/** Role → plist spec for the self-build factory (scout / tracker / builder / orchestrator). */
export function rolePlistSpecs(opts: {
  repoPath: string;
  scout?: boolean;
  tracker?: boolean;
  builder?: boolean;
  orchestrator?: boolean;
  orchestratorGoal?: string;
  intervalMinutes: number;
  scoutWorker: string;
  trackIntervalMinutes: number;
}): PlistSpec[] {
  const specs: PlistSpec[] = [];
  const nodePath = process.execPath;
  const devagentBin = join(opts.repoPath, 'dist/src/cli.js');
  if (opts.scout) {
    specs.push({
      label: 'com.devagent.scout',
      logName: 'devagent-scout.log',
      repoPath: opts.repoPath,
      programArgs: [nodePath, devagentBin, 'scout', '--repo', opts.repoPath, '--interval', String(opts.intervalMinutes), '--worker', opts.scoutWorker],
    });
  }
  if (opts.tracker) {
    specs.push({
      label: 'com.devagent.tracker',
      logName: 'devagent-tracker.log',
      repoPath: opts.repoPath,
      programArgs: [nodePath, devagentBin, 'track', '--repo', opts.repoPath, '--interval', String(opts.trackIntervalMinutes)],
    });
  }
  if (opts.builder) {
    specs.push({
      label: 'com.devagent.builder',
      logName: 'devagent-builder.log',
      repoPath: opts.repoPath,
      programArgs: ['/bin/bash', join(opts.repoPath, 'scripts', 'build-loop.sh')],
    });
  }
  if (opts.orchestrator) {
    const env: Record<string, string> = { ORCHESTRATOR_REPO: opts.repoPath };
    if (opts.orchestratorGoal) env.ORCHESTRATOR_GOAL = opts.orchestratorGoal;
    specs.push({
      label: 'com.devagent.orchestrator',
      logName: 'devagent-orchestrator.log',
      repoPath: opts.repoPath,
      programArgs: ['/bin/bash', join(opts.repoPath, 'scripts', 'orchestrate-loop.sh')],
      env,
    });
  }
  return specs;
}

/** Install one plist + best-effort launchctl bootstrap. Returns the plist path or null when skipped. */
async function installPlist(spec: PlistSpec, repoPath: string): Promise<string | null> {
  const home = process.env.HOME ?? '/tmp';
  const plistPath = join(home, 'Library/LaunchAgents', `${spec.label}.plist`);
  mkdirSync(join(home, 'Library/LaunchAgents'), { recursive: true });
  mkdirSync(join(home, 'Library/Logs'), { recursive: true });
  writeFileSync(plistPath, buildLaunchAgentPlist(spec));
  try {
    const uid = process.getuid?.() ?? 501;
    await spawnCli('launchctl', ['bootout', `gui/${uid}/${spec.label}`], { cwd: repoPath, timeoutMs: 5_000 });
  } catch { /* not loaded yet — fine */ }
  try {
    const uid = process.getuid?.() ?? 501;
    await spawnCli('launchctl', ['bootstrap', `gui/${uid}`, plistPath], { cwd: repoPath, timeoutMs: 5_000 });
  } catch { /* best-effort; user can bootstrap manually */ }
  return plistPath;
}

export async function runCreate(opts: CreateOptions, runner: CliRunner = defaultRunner): Promise<CreateResult> {
  // Normalize early: LaunchAgent plists embed repoPath (ProgramArguments +
  // WorkingDirectory); a relative --repo would produce broken agents.
  const repoPath = resolve(opts.repoPath);
  if (!existsSync(repoPath)) {
    return { ok: false, detail: `repoPath does not exist: ${opts.repoPath}`, dirs: [] };
  }

  const intervalMinutes = opts.intervalMinutes ?? 30;
  const scoutWorker = opts.scoutWorker ?? 'omp';
  const dirs: string[] = [];

  if (opts.dryRun) {
    const plan: string[] = [];
    plan.push(`would ensure ${queueDir(repoPath)} and ${prdsDir(repoPath)}`);
    plan.push(`would merge ${resolveConfigPath(repoPath)} with ${JSON.stringify({ scout: opts.scout ? { enabled: true, worker: scoutWorker, intervalMinutes } : undefined, autoMerge: opts.autoMerge, selfUpdate: opts.selfUpdate })}`);
    if (opts.scout) plan.push(`would install LaunchAgent plist for scout (${scoutWorker} every ${intervalMinutes}m)`);
    if (opts.tracker) plan.push(`would install LaunchAgent plist for tracker (every ${opts.trackIntervalMinutes ?? 15}m)`);
    if (opts.builder) plan.push(`would install LaunchAgent plist for builder (scripts/build-loop.sh, poll 300s)`);
    if (opts.orchestrator) plan.push(`would install LaunchAgent plist for orchestrator (scripts/orchestrate-loop.sh, goal: ${opts.orchestratorGoal ? `"${opts.orchestratorGoal.slice(0, 80)}"` : 'repo default'})`);
    if (opts.workers && opts.workers > 0) plan.push(`would provision ${opts.workers} Orca worktree(s) via orca worktree create`);
    return { ok: true, detail: plan.join('; '), dirs: [queueDir(repoPath), prdsDir(repoPath)] };
  }

  // 1) ensure queue/prd dirs
  ensureQueueDirs(repoPath);
  dirs.push(queueDir(repoPath), prdsDir(repoPath));
  mkdirSync(join(repoPath, '.devagent'), { recursive: true });

  // 2) merge config
  const cfgPath = resolveConfigPath(repoPath);
  let fileConfig: Record<string, unknown> = {};
  if (existsSync(cfgPath)) {
    try { fileConfig = JSON.parse(readFileSync(cfgPath, 'utf8')) as Record<string, unknown>; } catch { fileConfig = {}; }
  }
  if (opts.scout) {
    const scout = (fileConfig.scout as Record<string, unknown> | undefined) ?? {};
    fileConfig.scout = { ...scout, enabled: true, worker: scoutWorker, intervalMinutes };
  }
  if (opts.autoMerge) fileConfig.autoMerge = true;
  if (opts.selfUpdate) fileConfig.selfUpdate = true;
  if (opts.orchestrator) {
    const orch = (fileConfig.orchestrator as Record<string, unknown> | undefined) ?? {};
    fileConfig.orchestrator = { ...orch, enabled: true, ...(opts.orchestratorGoal ? { goal: opts.orchestratorGoal } : {}) };
  }
  // Validate by loading (throws on bad values) before writing
  try {
    // Write then validate via loadConfig
    writeFileSync(cfgPath, JSON.stringify(fileConfig, null, 2) + '\n');
    loadConfig(repoPath);
  } catch (err) {
    return { ok: false, detail: `config write/validate failed: ${(err as Error).message}`, dirs };
  }

  // 3) Orca repo registration (best-effort)
  let orcaWorktrees: string[] | undefined;
  if (opts.workers && opts.workers > 0) {
    // Register repo so worktree create can use path: selector
    await ensureOrcaRepo(repoPath, runner);
    orcaWorktrees = [];
    for (let i = 0; i < opts.workers; i++) {
      const name = `devagent-worker-${i + 1}`;
      const p = await createOrcaWorktree(repoPath, name, runner);
      if (p) orcaWorktrees.push(p);
    }
  }

  // 4) LaunchAgents (macOS only; skip gracefully elsewhere). Never install the
  // persistent user agents for an ephemeral repo: tmp-dir factories would
  // hijack com.devagent.* slots and crash-loop (EX_CONFIG) once the temp
  // checkout is deleted.
  const launchAgentPlists: string[] = [];
  let plistPath: string | undefined;
  if ((opts.scout || opts.tracker || opts.builder || opts.orchestrator) && process.platform === 'darwin' && shouldInstallLaunchAgent(repoPath)) {
    const specs = rolePlistSpecs({
      repoPath,
      scout: opts.scout,
      tracker: opts.tracker,
      builder: opts.builder,
      orchestrator: opts.orchestrator,
      orchestratorGoal: opts.orchestratorGoal,
      intervalMinutes,
      scoutWorker,
      trackIntervalMinutes: opts.trackIntervalMinutes ?? 15,
    });
    try {
      for (const spec of specs) {
        const p = await installPlist(spec, repoPath);
        if (p) launchAgentPlists.push(p);
      }
    } catch (err) {
      return { ok: false, detail: `LaunchAgent install failed: ${(err as Error).message}`, dirs, configPath: cfgPath };
    }
  }
  plistPath = launchAgentPlists.find((p) => p.includes('scout'));

  const detail = `factory ready: queue at ${queueDir(repoPath)}, prds at ${prdsDir(repoPath)}${opts.scout ? `, scout ${scoutWorker}/${intervalMinutes}m` : ''}${opts.tracker ? ', tracker agent' : ''}${opts.builder ? ', builder agent' : ''}${opts.orchestrator ? `, orchestrator agent${opts.orchestratorGoal ? ' (goal set)' : ''}` : ''}${orcaWorktrees?.length ? `, ${orcaWorktrees.length} orca worktree(s)` : ''}${launchAgentPlists.length ? ` [${launchAgentPlists.length} LaunchAgent(s)]` : ''}`;
  return { ok: true, detail, dirs, configPath: cfgPath, launchAgentPlist: plistPath, launchAgentPlists: launchAgentPlists.length ? launchAgentPlists : undefined, orcaWorktrees };
}

export function launchAgentPlistContent(repoPath: string, intervalMinutes = 30, worker: string = 'omp'): string {
  return buildLaunchAgentPlist(rolePlistSpecs({ repoPath, scout: true, intervalMinutes, scoutWorker: worker, trackIntervalMinutes: 15 })[0]!);
}

/**
 * Guard for the shared com.devagent.scout LaunchAgent slot: only repos that
 * live outside the OS temp dir (and exist) may claim it.
 */
export function shouldInstallLaunchAgent(repoPath: string): boolean {
  if (!existsSync(repoPath)) return false;
  const resolved = realpathSync(repoPath);
  // macOS tmpdir() (/var/folders/...) resolves to /private/var/folders/...
  let realTmp = '';
  try { realTmp = realpathSync(tmpdir()); } catch { realTmp = tmpdir(); }
  return !resolved.startsWith(realTmp) && !resolved.startsWith(tmpdir());
}
