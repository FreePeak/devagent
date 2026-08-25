import { describe, expect, it } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runSelfUpdate } from '../src/self-update.js';

type Runner = (
  cmd: string,
  args: string[],
  opts: { cwd: string; timeoutMs: number },
) => Promise<{ exitCode: number; stdout: string; stderr: string; timedOut: boolean }>;

const dirs: string[] = [];

function initRepo(): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-selfupd-'));
  dirs.push(dir);
  const repo = join(dir, 'repo');
  mkdirSync(repo);
  execFileSync('git', ['init', '-b', 'main'], { cwd: repo });
  execFileSync('git', ['config', 'user.email', 'test@example.com'], { cwd: repo });
  execFileSync('git', ['config', 'user.name', 'test'], { cwd: repo });
  writeFileSync(join(repo, 'f.txt'), 'x\n');
  execFileSync('git', ['add', '.'], { cwd: repo });
  execFileSync('git', ['commit', '-m', 'init'], { cwd: repo });
  return repo;
}

function scripted(steps: Record<string, { exitCode?: number; stdout?: string; stderr?: string; timedOut?: boolean }>): {
  runner: Runner;
  calls: Array<{ cmd: string; args: string[] }>;
} {
  const calls: Array<{ cmd: string; args: string[] }> = [];
  const runner: Runner = async (cmd, args) => {
    calls.push({ cmd, args });
    const key = `${cmd} ${args[0] ?? ''}`;
    const s = steps[key] ?? { exitCode: 0, stdout: '', stderr: '' };
    return { exitCode: s.exitCode ?? 0, stdout: s.stdout ?? '', stderr: s.stderr ?? '', timedOut: s.timedOut ?? false };
  };
  return { runner, calls };
}

describe('runSelfUpdate', () => {
  it('skips on a dirty worktree without pulling', async () => {
    const repo = initRepo();
    writeFileSync(join(repo, 'f.txt'), 'modified\n');
    const { runner, calls } = scripted({
      'git status': { exitCode: 0, stdout: ' M f.txt\n' },
    });
    const r = await runSelfUpdate(repo, undefined, runner);
    expect(r.ok).toBe(false);
    expect(r.detail).toContain('dirty worktree');
    expect(calls.filter((c) => c.cmd === 'git' && c.args[0] === 'pull')).toHaveLength(0);
  });

  it('treats untracked .devagent noise as clean and proceeds', async () => {
    const repo = initRepo();
    mkdirSync(join(repo, '.devagent'), { recursive: true });
    writeFileSync(join(repo, '.devagent', 'noise.txt'), 'ok\n');
    const { runner, calls } = scripted({
      'git status': { exitCode: 0, stdout: '?? .devagent/noise.txt\n' },
    });
    const r = await runSelfUpdate(repo, undefined, runner);
    expect(r.ok).toBe(true);
    expect(calls.some((c) => c.cmd === 'git' && c.args[0] === 'pull')).toBe(true);
  });

  it('fails cleanly when git pull fails and never leaks remote credentials in the detail', async () => {
    const repo = initRepo();
    const secret = 'ghp_SUPERSECRET123';
    const { runner } = scripted({
      'git pull': { exitCode: 128, stdout: `fatal: unable to access using token ${secret}`, stderr: '' },
    });
    const r = await runSelfUpdate(repo, undefined, runner);
    expect(r.ok).toBe(false);
    expect(r.detail).toContain('pull failed');
    expect(r.detail).not.toContain(secret);
  });

  it('runs the full pull -> install -> build sequence on a clean tree', async () => {
    const repo = initRepo();
    const { runner, calls } = scripted({});
    const r = await runSelfUpdate(repo, undefined, runner);
    expect(r.ok).toBe(true);
    expect(r.detail).toContain('pull');
    expect(r.detail).toContain('build');
    const cmds = calls.map((c) => c.cmd);
    expect(cmds).toEqual(expect.arrayContaining(['git', 'npm', 'npm']));
    const npmArgs = calls.filter((c) => c.cmd === 'npm').map((c) => c.args.join(' '));
    expect(npmArgs).toContain('ci --ignore-scripts');
    expect(npmArgs).toContain('run build');
  });

  it('falls back to npm install when npm ci fails', async () => {
    const repo = initRepo();
    const { runner, calls } = scripted({
      'npm ci': { exitCode: 1 },
    });
    const r = await runSelfUpdate(repo, undefined, runner);
    expect(r.ok).toBe(true);
    const npmArgs = calls.filter((c) => c.cmd === 'npm').map((c) => c.args.join(' '));
    expect(npmArgs).toContain('install --ignore-scripts');
  });

  it('reports failure when build fails', async () => {
    const repo = initRepo();
    const { runner } = scripted({
      'npm run': { exitCode: 2, stderr: 'tsc error' },
    });
    const r = await runSelfUpdate(repo, undefined, runner);
    expect(r.ok).toBe(false);
    expect(r.detail).toContain('build failed');
  });
});
