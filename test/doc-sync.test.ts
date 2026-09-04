import { afterAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { execFile } from 'node:child_process';
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
    // The operator's edit is untouched.
    const { readFileSync } = await import('node:fs');
    expect(readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8')).toContain('operator mid-edit');
  });

  it('reports diverged history as a failed sync without touching the tree', async () => {
    const { repo, origin } = await initRepoWithOrigin();
    cleanup.push(repo, origin);
    const { syncWorkSelectionDocs } = await import('../src/git/doc-sync.js');
    // Local commit that origin will never have -> ff-only merge impossible.
    mkdirSync(join(repo, 'scratch'), { recursive: true });
    writeFileSync(join(repo, 'scratch', 'x.txt'), 'local');
    await execFileP('git', ['-C', repo, 'add', '-A']);
    await execFileP('git', ['-C', repo, 'commit', '-m', 'local diverge']);
    await pushPrdUpdate(origin, 'v2');

    const r = await syncWorkSelectionDocs(repo);
    expect(r.ok).toBe(false);
    expect(r.detail).toContain('fast-forward');
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
