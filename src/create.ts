import { existsSync, mkdirSync, readFileSync, writeFileSync, readdirSync, realpathSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { ensureQueueDirs, queueDir, prdsDir } from './queue.js';
import { loadConfig, type DevAgentConfig } from './config.js';
import { spawnCli } from './workers/spawn-utils.js';

export interface CreateOptions {
  repoPath: string;
  scout?: boolean;
  workers?: number;
  autoMerge?: boolean;
  selfUpdate?: boolean;
  dryRun?: boolean;
  intervalMinutes?: number;
  scoutWorker?: 'opencode' | 'claude-code';
}

export interface CreateResult {
  ok: boolean;
  detail: string;
  dirs: string[];
  configPath?: string;
  launchAgentPlist?: string;
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

function buildLaunchAgentPlist(opts: { repoPath: string; intervalMinutes: number; worker: string }): string {
  const label = 'com.devagent.scout';
  const logPath = join(process.env.HOME ?? '/tmp', 'Library/Logs/devagent-scout.log');
  const devagentBin = join(opts.repoPath, 'dist/src/cli.js');
  // Use node to run the built CLI so LaunchAgent does not depend on npx
  const nodePath = process.execPath;
  // launchd default PATH is minimal; embed the current PATH so the scout can
  // find opencode/claude/git installed in user locations.
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
    '  </dict>',
    '  <key>ProgramArguments</key>',
    '  <array>',
    `    <string>${nodePath}</string>`,
    `    <string>${devagentBin}</string>`,
    '    <string>scout</string>',
    `    <string>--repo</string><string>${opts.repoPath}</string>`,
    `    <string>--interval</string><string>${String(opts.intervalMinutes)}</string>`,
    `    <string>--worker</string><string>${opts.worker}</string>`,
    '  </array>',
    `  <key>WorkingDirectory</key><string>${opts.repoPath}</string>`,
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

export async function runCreate(opts: CreateOptions, runner: CliRunner = defaultRunner): Promise<CreateResult> {
  const repoPath = opts.repoPath;
  if (!existsSync(repoPath)) {
    return { ok: false, detail: `repoPath does not exist: ${repoPath}`, dirs: [] };
  }

  const intervalMinutes = opts.intervalMinutes ?? 30;
  const scoutWorker = opts.scoutWorker ?? 'opencode';
  const dirs: string[] = [];

  if (opts.dryRun) {
    const plan: string[] = [];
    plan.push(`would ensure ${queueDir(repoPath)} and ${prdsDir(repoPath)}`);
    plan.push(`would merge ${resolveConfigPath(repoPath)} with ${JSON.stringify({ scout: opts.scout ? { enabled: true, worker: scoutWorker, intervalMinutes } : undefined, autoMerge: opts.autoMerge, selfUpdate: opts.selfUpdate })}`);
    if (opts.scout) plan.push(`would install LaunchAgent plist for scout (${scoutWorker} every ${intervalMinutes}m)`);
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

  // 4) LaunchAgent (macOS only; skip gracefully elsewhere)
  // Never install the persistent user agent for an ephemeral repo: tmp-dir
  // factories would hijack com.devagent.scout and crash-loop (EX_CONFIG)
  // once the temp checkout is deleted.
  let plistPath: string | undefined;
  if (opts.scout && process.platform === 'darwin' && shouldInstallLaunchAgent(repoPath)) {
    const plist = buildLaunchAgentPlist({ repoPath, intervalMinutes, worker: scoutWorker });
    const home = process.env.HOME ?? '/tmp';
    plistPath = join(home, 'Library/LaunchAgents/com.devagent.scout.plist');
    try {
      mkdirSync(join(home, 'Library/LaunchAgents'), { recursive: true });
      mkdirSync(join(home, 'Library/Logs'), { recursive: true });
      writeFileSync(plistPath, plist);
      // Best-effort bootstrap (requires orca not needed)
      try {
        const { spawnCli: sc } = await import('./workers/spawn-utils.js');
        await sc('launchctl', ['bootout', `gui/${process.getuid?.() ?? 501}/${'com.devagent.scout'}`], { cwd: repoPath, timeoutMs: 5_000 }).catch(() => null);
        await sc('launchctl', ['bootstrap', `gui/${process.getuid?.() ?? 501}`, plistPath], { cwd: repoPath, timeoutMs: 5_000 });
      } catch { /* best-effort */ }
    } catch (err) {
      return { ok: false, detail: `LaunchAgent install failed: ${(err as Error).message}`, dirs, configPath: cfgPath };
    }
  }

  const detail = `factory ready: queue at ${queueDir(repoPath)}, prds at ${prdsDir(repoPath)}${opts.scout ? `, scout ${scoutWorker}/${intervalMinutes}m` : ''}${orcaWorktrees?.length ? `, ${orcaWorktrees.length} orca worktree(s)` : ''}`;
  return { ok: true, detail, dirs, configPath: cfgPath, launchAgentPlist: plistPath, orcaWorktrees };
}

export function launchAgentPlistContent(repoPath: string, intervalMinutes = 30, worker: string = 'opencode'): string {
  return buildLaunchAgentPlist({ repoPath, intervalMinutes, worker });
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
