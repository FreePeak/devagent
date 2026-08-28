import { describe, it, expect } from 'vitest';
import { execFile } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runCli, syncCli, spawnChild, FALLBACK_PATH_SEGMENTS } from '../src/workers/spawn-utils.js';
import { createWorktree } from '../src/git/worktree.js';

/**
 * Tests for the spawn-utils child-process helpers and the worktree.ts run()
 * helper. The motivating bug: when a parent's PATH is minimal (e.g., a
 * launchd plist, a worker sandbox, or any future env-scrubbing layer),
 * execFile('git', ...) and execFile('gh', ...) failed with `spawn git
 * ENOENT` because Node child_process does not consult a hardcoded fallback.
 *
 * runCli / syncCli / spawnChild route through buildEnv() which adds
 * FALLBACK_PATH_SEGMENTS, so children always find /usr/bin/git and
 * /usr/bin/gh even when the parent has PATH=''. The worktree helper must
 * delegate to runCli for the same reason.
 */
describe('spawn-utils child-process helpers', () => {
  it('runCli resolves git when parent PATH is empty', async () => {
    const r = await runCli('git', ['--version'], {
      cwd: process.cwd(),
      timeoutMs: 5_000,
      env: { PATH: '' },
    });
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toMatch(/^git version /);
  });

  it('runCli merges env over the parent when replaceEnv is false', async () => {
    const r = await runCli('sh', ['-c', 'echo "$FOO_FROM_TEST"'], {
      cwd: process.cwd(),
      timeoutMs: 5_000,
      env: { FOO_FROM_TEST: 'ok' },
    });
    expect(r.exitCode).toBe(0);
    expect(r.stdout.trim()).toBe('ok');
  });

  it('runCli with replaceEnv strips blocklisted vars (ANTHROPIC_MODEL etc.)', async () => {
    const r = await runCli('sh', ['-c', 'echo "model=$ANTHROPIC_MODEL small=$ANTHROPIC_SMALL_FAST_MODEL entry=$CLAUDE_CODE_ENTRYPOINT claude=$CLAUDECODE"'], {
      cwd: process.cwd(),
      timeoutMs: 5_000,
      env: { ANTHROPIC_MODEL: 'leak', ANTHROPIC_SMALL_FAST_MODEL: 'leak', CLAUDE_CODE_ENTRYPOINT: 'leak', CLAUDECODE: 'leak' },
      replaceEnv: true,
    });
    expect(r.exitCode).toBe(0);
    expect(r.stdout).toMatch(/model=\s*small=\s*entry=\s*claude=\s*/);
  });

  it('syncCli resolves ps when parent PATH is empty', () => {
    const out = syncCli('ps', ['-o', 'command=', '-p', String(process.pid)], {
      cwd: process.cwd(),
      timeoutMs: 5_000,
      env: { PATH: '' },
    });
    // ps must be discoverable via the fallback PATH even when the parent has
    // no PATH at all. We do not assert the exact command string, only that
    // ps returned a non-empty line for our pid.
    expect(out.trim().length).toBeGreaterThan(0);
  });

  it('spawnChild resolves git when parent PATH is empty and the child exits cleanly', async () => {
    const child = spawnChild('git', ['--version'], {
      cwd: process.cwd(),
      timeoutMs: 5_000,
      env: { PATH: '' },
    });
    const exitCode: number = await new Promise((resolve, reject) => {
      child.on('error', reject);
      child.on('close', resolve);
    });
    expect(exitCode).toBe(0);
  });

  it('FALLBACK_PATH_SEGMENTS include the homebrew + system bin paths', () => {
    // Regression guard: if someone removes /usr/bin or /bin from the fallback
    // list, every macOS Linux CI image breaks. The list is intentionally a
    // module-level constant, so this test documents the contract.
    expect(FALLBACK_PATH_SEGMENTS).toContain('/usr/bin');
    expect(FALLBACK_PATH_SEGMENTS).toContain('/bin');
    expect(FALLBACK_PATH_SEGMENTS).toContain('/opt/homebrew/bin');
  });
});

/**
 * Worktree helper tests. The run() helper in src/git/worktree.ts is private;
 * we exercise it indirectly via createWorktree, which calls it for every
 * git operation. The fix is to delegate run() to runCli so env is patched.
 *
 * The failure scenario: parent process with PATH=''. Without the fix,
 * createWorktree throws `spawn git ENOENT` on the first git invocation.
 * With the fix, it succeeds.
 */
describe('worktree.ts run() helper — env safety', () => {
  it('createWorktree does not throw spawn git ENOENT when parent PATH is empty', async () => {
    // Spin up a fresh temp git repo as the parent (mimics the devagent
    // top-level checkout) and run createWorktree under PATH=''.
    const tmp = mkdtempSync(join(tmpdir(), 'devagent-wt-test-'));
    try {
      // Init a real git repo with one commit so worktree add can branch.
      await runCli('git', ['init', '-q', '--initial-branch', 'main', tmp], {
        cwd: tmp,
        timeoutMs: 5_000,
      });
      await runCli('git', ['-C', tmp, 'config', 'user.email', 'wt-test@devagent'], {
        cwd: tmp,
        timeoutMs: 5_000,
      });
      await runCli('git', ['-C', tmp, 'config', 'user.name', 'wt-test'], {
        cwd: tmp,
        timeoutMs: 5_000,
      });
      writeFileSync(join(tmp, 'README.md'), 'init\n');
      await runCli('git', ['-C', tmp, 'add', '-A'], { cwd: tmp, timeoutMs: 5_000 });
      await runCli('git', ['-C', tmp, 'commit', '-q', '-m', 'init'], {
        cwd: tmp,
        timeoutMs: 5_000,
        env: { GIT_AUTHOR_NAME: 'wt-test', GIT_AUTHOR_EMAIL: 'wt-test@devagent', GIT_COMMITTER_NAME: 'wt-test', GIT_COMMITTER_EMAIL: 'wt-test@devagent' },
      });

      // Now invoke createWorktree under a scrubbed PATH. The pre-fix code
      // would throw `spawn git ENOENT` on the first `git rev-parse` call.
      const savedPath = process.env.PATH;
      try {
        process.env.PATH = '';
        // The helper delegates to git via the (now-fixed) run() in
        // src/git/worktree.ts. The createWorktree signature takes (repoPath,
        // ticketId) and returns the worktree path/branch.
        const wt = await createWorktree(tmp, 'TICKET-A1');
        expect(wt.worktreePath).toBeTruthy();
        expect(wt.branch).toMatch(/TICKET-A1/);
      } finally {
        process.env.PATH = savedPath;
      }
    } finally {
      rmSync(tmp, { recursive: true, force: true });
    }
  });
});

/**
 * Negative regression: confirm the *unfixed* baseline would fail. This is
 * what we observed in the selfbuild loop's loop-50 failure log:
 * `spawn git ENOENT` from execFile('git', ...) with PATH=''. The test
 * exercises a direct execFile call (NOT through runCli) and asserts the
 * ENOENT error happens. It exists to document the bug shape and to fail
 * loudly if the test environment changes such that the repro no longer
 * triggers (e.g. macOS moves git to /opt/homebrew/bin which is also in the
 * fallback list — in that case this test should be updated to scrub harder).
 */
describe('regression shape: bare execFile with empty PATH errors with ENOENT', () => {
  it('documents the spawn git ENOENT failure mode that the fix prevents', async () => {
    const result = await new Promise<{ code: string | null; msg: string }>((resolve) => {
      execFile('git', ['--version'], { cwd: process.cwd(), env: { PATH: '' } }, (err) => {
        if (err) resolve({ code: (err as { code?: string }).code ?? null, msg: err.message });
        else resolve({ code: null, msg: 'unexpected success — git was found despite PATH empty' });
      });
    });
    expect(result.code).toBe('ENOENT');
    expect(result.msg).toMatch(/spawn git ENOENT/);
  });
});
