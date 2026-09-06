import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync, existsSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFile, execFileSync } from 'node:child_process';
import { promisify } from 'node:util';

const execFileP = promisify(execFile);

/**
 * Doc-sync (operator PRD-freshness fix): the scout and the selfbuild loop
 * select work from docs/PRD.md, but nothing refreshed the tree from origin
 * before reading it. These tests exercise syncWorkSelectionDocs against a
 * real local git repo (origin = a second bare clone) so fetch/ff/dirty logic
 * is proven end to end, not mocked.
 */

async function initRepoWithOrigin(): Promise<{ repo: string; origin: string }> {
  const base = mkdtempSync(join(tmpdir(), 'da-docsync-'));
  const seed = join(base, 'seed');
  const origin = join(base, 'origin.git');
  const repo = join(base, 'repo');
  await execFileP('git', ['init', '--bare', origin]);
  await execFileP('git', ['init', seed]);
  await execFileP('git', ['-C', seed, 'config', 'user.email', 't@t']);
  await execFileP('git', ['-C', seed, 'config', 'user.name', 't']);
  mkdirSync(join(seed, 'docs'), { recursive: true });
  writeFileSync(join(seed, 'docs', 'PRD.md'), '# PRD v1\n');
  await execFileP('git', ['-C', seed, 'add', '-A']);
  await execFileP('git', ['-C', seed, 'commit', '-m', 'v1']);
  await execFileP('git', ['-C', seed, 'branch', '-M', 'main']);
  await execFileP('git', ['-C', seed, 'push', '-q', origin, 'main']);
  // A bare repo's HEAD is unborn (refs/heads/master) until pointed at main;
  // cloning before this fix yields an empty checkout and "unknown revision HEAD".
  await execFileP('git', ['-C', origin, 'symbolic-ref', 'HEAD', 'refs/heads/main']);
  await execFileP('git', ['clone', '-q', origin, repo]);
  await execFileP('git', ['-C', repo, 'config', 'user.name', 't']);
  // Some environments carry no global git identity; the diverged scenarios
  // commit inside the clone, so pin the email locally too.
  await execFileP('git', ['-C', repo, 'config', 'user.email', 't@t']);
  return { repo, origin };
}

async function pushPrdUpdate(origin: string, version: string): Promise<void> {
  const seed = join(origin, '..', 'seed');
  writeFileSync(join(seed, 'docs', 'PRD.md'), `# PRD ${version}\n`);
  await execFileP('git', ['-C', seed, 'add', '-A']);
  await execFileP('git', ['-C', seed, 'commit', '-m', version]);
  await execFileP('git', ['-C', seed, 'push', '-q', origin, 'main']);
}

const cleanup: string[] = [];
afterAll(() => {
  for (const d of cleanup) rmSync(d, { recursive: true, force: true });
});

describe('syncWorkSelectionDocs', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it('fast-forwards a stale repo so docs/PRD.md reflects origin (the scout bug)', async () => {
    const { repo, origin } = await initRepoWithOrigin();
    cleanup.push(repo, origin);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');

    const before = await execFileP('git', ['-C', repo, 'rev-parse', 'HEAD']);
    const up1 = await syncWorkSelectionDocs(repo);
    expect(up1.ok).toBe(true);
    expect(up1.alreadyUpToDate).toBe(true);

    // Operator pushes a manual PRD edit from "another machine" (the seed clone).
    await pushPrdUpdate(origin, 'v2');

    const up2 = await syncWorkSelectionDocs(repo);
    expect(up2.ok).toBe(true);
    expect(up2.alreadyUpToDate).toBe(false);

    const after = await execFileP('git', ['-C', repo, 'rev-parse', 'HEAD']);
    expect(after.stdout).not.toBe(before.stdout);
    expect(await execFileP('git', ['-C', repo, 'rev-parse', 'HEAD:docs/PRD.md']).then(r => r.stdout.trim()))
      .toBe(await execFileP('git', ['-C', origin, 'rev-parse', 'main:docs/PRD.md']).then(r => r.stdout.trim()));
  });

  it('refuses to sync when docs/PRD.md is locally modified (operator mid-edit)', async () => {
    const { repo, origin } = await initRepoWithOrigin();
    cleanup.push(repo, origin);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');
    await pushPrdUpdate(origin, 'v2');
    mkdirSync(join(repo, 'docs'), { recursive: true });
    writeFileSync(join(repo, 'docs', 'PRD.md'), '# operator mid-edit\n');

    const r = await syncWorkSelectionDocs(repo);
    expect(r.ok).toBe(false);
    expect(r.detail).toContain('refusing sync');
    expect(readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8')).toContain('operator mid-edit');
  });

  it('diverged-clean: rebases a local-only commit onto origin and flags diverged', async () => {
    const { repo, origin } = await initRepoWithOrigin();
    cleanup.push(repo, origin);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');
    // Local commit origin will never have -> histories diverge once v2 lands.
    mkdirSync(join(repo, 'scratch'), { recursive: true });
    writeFileSync(join(repo, 'scratch', 'x.txt'), 'local');
    await execFileP('git', ['-C', repo, 'add', '-A']);
    await execFileP('git', ['-C', repo, 'commit', '-m', 'local diverge']);
    await pushPrdUpdate(origin, 'v2');

    const r = await syncWorkSelectionDocs(repo);
    expect(r.ok).toBe(true);
    expect(r.diverged).toBe(true);
    // The local commit survived the rebase and the PRD matches origin.
    expect(await execFileP('git', ['-C', repo, 'log', '--format=%s', '-2']).then(x => x.stdout))
      .toContain('local diverge');
    expect(await execFileP('git', ['-C', repo, 'rev-parse', 'HEAD:docs/PRD.md']).then(x => x.stdout.trim()))
      .toBe(await execFileP('git', ['-C', origin, 'rev-parse', 'main:docs/PRD.md']).then(x => x.stdout.trim()));
  });

  it('diverged-conflict: rebase conflict aborts cleanly, tree left untouched', async () => {
    const { repo, origin } = await initRepoWithOrigin();
    cleanup.push(repo, origin);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');
    // Both sides rewrite docs/PRD.md -> rebase must hit a conflict.
    writeFileSync(join(repo, 'docs', 'PRD.md'), '# local divergent PRD\n');
    await execFileP('git', ['-C', repo, 'add', '-A']);
    await execFileP('git', ['-C', repo, 'commit', '-m', 'conflicting local PRD']);
    await pushPrdUpdate(origin, 'v2 conflicting');

    const r = await syncWorkSelectionDocs(repo);
    expect(r.ok).toBe(false);
    expect(r.diverged).toBe(true);
    expect(r.dirty).toBe(false);
    expect(r.detail).toContain('aborted cleanly');
    // Clean abort: no rebase left in progress, HEAD still the local commit.
    expect(existsSync(join(repo, '.git', 'rebase-merge'))).toBe(false);
    expect(existsSync(join(repo, '.git', 'rebase-apply'))).toBe(false);
    expect(await execFileP('git', ['-C', repo, 'log', '--format=%s', '-1']).then(x => x.stdout.trim()))
      .toBe('conflicting local PRD');
    // The PRD still reflects the local commit, not origin's.
    expect(readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8')).toContain('local divergent PRD');
  });

  it('diverged-dirty: refuses with the distinct diverged+dirty outcome, edit untouched', async () => {
    const { repo, origin } = await initRepoWithOrigin();
    cleanup.push(repo, origin);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');
    // Diverge via a committed local change, then dirty the PRD on top.
    mkdirSync(join(repo, 'scratch'), { recursive: true });
    writeFileSync(join(repo, 'scratch', 'x.txt'), 'local');
    await execFileP('git', ['-C', repo, 'add', '-A']);
    await execFileP('git', ['-C', repo, 'commit', '-m', 'local diverge']);
    await pushPrdUpdate(origin, 'v2');
    writeFileSync(join(repo, 'docs', 'PRD.md'), '# operator mid-edit on diverged tree\n');

    const r = await syncWorkSelectionDocs(repo);
    expect(r.ok).toBe(false);
    expect(r.diverged).toBe(true);
    expect(r.dirty).toBe(true);
    expect(r.detail).toContain('reconcile by hand');
    // The operator's uncommitted edit is untouched and no rebase started.
    expect(readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8')).toContain('operator mid-edit on diverged tree');
    expect(existsSync(join(repo, '.git', 'rebase-merge'))).toBe(false);
  });

  it('unreachable origin is a failed sync, not a throw', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-docsync-bad-'));
    cleanup.push(dir);
    const repo = join(dir, 'repo');
    await execFileP('git', ['init', '-q', repo]);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');
    const r = await syncWorkSelectionDocs(repo);
    expect(r.ok).toBe(false);
    expect(r.detail).toContain('git fetch failed');
  });
});

// ---------- CLI smoke: real devagent binary against a real repo ----------

describe('devagent sync-docs CLI (smoke)', () => {
  interface CliResult {
    code: number;
    stdout: string;
    stderr: string;
  }

  function runCli(args: string[]): CliResult {
    try {
      const out = execFileSync('npx', ['tsx', 'src/cli.ts', 'sync-docs', ...args], {
        cwd: join(import.meta.dirname, '..'),
        stdio: 'pipe',
        encoding: 'utf8',
        timeout: 60_000,
      });
      return { code: 0, stdout: out, stderr: '' };
    } catch (err) {
      const e = err as { status?: number; stdout?: string; stderr?: string };
      return { code: e.status ?? 1, stdout: e.stdout ?? '', stderr: e.stderr ?? '' };
    }
  }

  async function initCliRepo(): Promise<{ repo: string; origin: string }> {
    const base = mkdtempSync(join(tmpdir(), 'da-docsync-cli-'));
    const seed = join(base, 'seed');
    const origin = join(base, 'origin.git');
    const repo = join(base, 'repo');
    await execFileP('git', ['init', '--bare', origin]);
    await execFileP('git', ['init', seed]);
    await execFileP('git', ['-C', seed, 'config', 'user.email', 't@t']);
    await execFileP('git', ['-C', seed, 'config', 'user.name', 't']);
    mkdirSync(join(seed, 'docs'), { recursive: true });
    writeFileSync(join(seed, 'docs', 'PRD.md'), '# PRD v1\n');
    await execFileP('git', ['-C', seed, 'add', '-A']);
    await execFileP('git', ['-C', seed, 'commit', '-m', 'v1']);
    await execFileP('git', ['-C', seed, 'branch', '-M', 'main']);
    await execFileP('git', ['-C', seed, 'push', '-q', origin, 'main']);
    await execFileP('git', ['-C', origin, 'symbolic-ref', 'HEAD', 'refs/heads/main']);
    await execFileP('git', ['clone', '-q', origin, repo]);
    await execFileP('git', ['-C', repo, 'config', 'user.name', 't']);
    await execFileP('git', ['-C', repo, 'config', 'user.email', 't@t']);
    cleanup.push(repo, origin);
    return { repo, origin };
  }

  it('up-to-date repo: exit 0, json {ok:true, upToDate:true}', async () => {
    const { repo } = await initCliRepo();
    const r = runCli(['--json', '--repo', repo]);
    expect(r.code).toBe(0);
    const parsed = JSON.parse(r.stdout) as Record<string, unknown>;
    expect(parsed).toMatchObject({ ok: true, upToDate: true, diverged: false, dirty: false });
    expect(typeof parsed.detail).toBe('string');
  });

  it('stale repo: exit 0 after fast-forward, json {ok:true, upToDate:false}', async () => {
    const { repo, origin } = await initCliRepo();
    await pushPrdUpdate(origin, 'v2');
    const r = runCli(['--json', '--repo', repo]);
    expect(r.code).toBe(0);
    const parsed = JSON.parse(r.stdout) as Record<string, unknown>;
    expect(parsed).toMatchObject({ ok: true, upToDate: false, diverged: false, dirty: false });
  });

  it('dirty PRD: exit 2 (dirty refusal), json {ok:false, dirty:true}', async () => {
    const { repo, origin } = await initCliRepo();
    await pushPrdUpdate(origin, 'v2');
    mkdirSync(join(repo, 'docs'), { recursive: true });
    writeFileSync(join(repo, 'docs', 'PRD.md'), '# operator mid-edit\n');
    const r = runCli(['--json', '--repo', repo]);
    expect(r.code).toBe(2);
    const parsed = JSON.parse(r.stdout) as Record<string, unknown>;
    expect(parsed).toMatchObject({ ok: false, diverged: false, dirty: true });
  });

  it('diverged clean: exit 0 after rebase, json {ok:true, diverged:true}', async () => {
    const { repo, origin } = await initCliRepo();
    mkdirSync(join(repo, 'scratch'), { recursive: true });
    writeFileSync(join(repo, 'scratch', 'x.txt'), 'local');
    await execFileP('git', ['-C', repo, 'add', '-A']);
    await execFileP('git', ['-C', repo, 'commit', '-m', 'local diverge']);
    await pushPrdUpdate(origin, 'v2');
    const r = runCli(['--json', '--repo', repo]);
    expect(r.code).toBe(0);
    const parsed = JSON.parse(r.stdout) as Record<string, unknown>;
    expect(parsed).toMatchObject({ ok: true, upToDate: false, diverged: true, dirty: false });
  });

  it('diverged conflict: exit 3 (diverged), json {ok:false, diverged:true}', async () => {
    const { repo, origin } = await initCliRepo();
    writeFileSync(join(repo, 'docs', 'PRD.md'), '# local divergent PRD\n');
    await execFileP('git', ['-C', repo, 'add', '-A']);
    await execFileP('git', ['-C', repo, 'commit', '-m', 'conflicting local PRD']);
    await pushPrdUpdate(origin, 'v2 conflicting');
    const r = runCli(['--json', '--repo', repo]);
    expect(r.code).toBe(3);
    const parsed = JSON.parse(r.stdout) as Record<string, unknown>;
    expect(parsed).toMatchObject({ ok: false, diverged: true, dirty: false });
    // Clean abort: the repo is not left mid-rebase for the next iteration.
    expect(existsSync(join(repo, '.git', 'rebase-merge'))).toBe(false);
  });
});
