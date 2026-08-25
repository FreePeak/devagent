import { spawnCli } from './workers/spawn-utils.js';
import type { RunLogger } from './logger.js';

export interface SelfUpdateResult {
  ok: boolean;
  detail: string;
}

type CliRunner = (
  cmd: string,
  args: string[],
  opts: { cwd: string; timeoutMs: number },
) => Promise<{ exitCode: number; stdout: string; stderr: string; timedOut: boolean }>;

const defaultRunner: CliRunner = (cmd, args, opts) => spawnCli(cmd, args, opts);

/**
 * Redact credential-looking substrings (tokens, basic-auth URLs) before they
 * reach logs or returned details. Git errors can embed authenticated remote URLs.
 */
export function redactSecrets(text: string): string {
  return text
    .replace(/(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{10,}/g, '<redacted>')
    .replace(/github_pat_[A-Za-z0-9_]{10,}/g, '<redacted>')
    .replace(/glpat-[A-Za-z0-9_\-]{10,}/g, '<redacted>')
    .replace(/(https?:\/\/)[^/@\s]+:[^@\s]+@/g, '$1<redacted>@')
    .replace(/x-access-token:[^\s@]+@/gi, 'x-access-token:<redacted>@');
}

/**
 * Pull latest main, rebuild, and (when scout LaunchAgent is installed) kickstart it.
 * Never runs with a dirty worktree; callers should check before invoking.
 */
export async function runSelfUpdate(
  repoPath: string,
  log?: RunLogger,
  runner: CliRunner = defaultRunner,
): Promise<SelfUpdateResult> {
  const steps: string[] = [];

  // 1) git status must be clean (no staged/unstaged changes); untracked .devagent/* is ok
  const status = await runner('git', ['status', '--porcelain'], { cwd: repoPath, timeoutMs: 10_000 });
  if (status.timedOut || status.exitCode !== 0) {
    return { ok: false, detail: 'self-update: git status failed' };
  }
  const dirty = status.stdout.split('\n').filter((l) => l.trim() && !l.includes('.devagent/') && !l.includes('.selfbuild/'));
  if (dirty.length > 0) {
    return { ok: false, detail: `self-update skipped: dirty worktree (${dirty.length} file(s))` };
  }

  // 2) git pull --ff-only
  const pull = await runner('git', ['pull', '--ff-only'], { cwd: repoPath, timeoutMs: 30_000 });
  if (pull.timedOut || pull.exitCode !== 0) {
    const msg = redactSecrets((pull.stdout + pull.stderr).slice(0, 400));
    log?.warn('self-update', `pull failed: ${msg}`);
    return { ok: false, detail: `self-update: pull failed: ${msg.trim().slice(0, 200)}` };
  }
  steps.push('pull');

  // 3) npm ci (or npm install fallback) + build
  let install = await runner('npm', ['ci', '--ignore-scripts'], { cwd: repoPath, timeoutMs: 120_000 });
  if (install.timedOut || install.exitCode !== 0) {
    install = await runner('npm', ['install', '--ignore-scripts'], { cwd: repoPath, timeoutMs: 120_000 });
    if (install.timedOut || install.exitCode !== 0) {
      return { ok: false, detail: `self-update: npm install failed` };
    }
  }
  steps.push('install');

  const build = await runner('npm', ['run', 'build'], { cwd: repoPath, timeoutMs: 60_000 });
  if (build.timedOut || build.exitCode !== 0) {
    return { ok: false, detail: `self-update: build failed` };
  }
  steps.push('build');

  // 4) kickstart scout LaunchAgent if present (macOS)
  if (process.platform === 'darwin') {
    const label = 'com.devagent.scout';
    const uid = process.getuid?.() ?? 501;
    const kick = await runner('launchctl', ['kickstart', `gui/${uid}/${label}`], { cwd: repoPath, timeoutMs: 5_000 });
    if (!kick.timedOut && kick.exitCode === 0) steps.push('scout kickstart');
  }

  const detail = `self-update ok: ${steps.join(' -> ')}`;
  log?.info('self-update', detail);
  return { ok: true, detail };
}
