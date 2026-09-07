import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// The consume wiring under test ends in `gh pr merge` and the repo suite; both
// are replaced so the tests exercise only the merge-path evidence gate.
vi.mock('../src/integrations/github.js', () => ({
  autoMergePr: vi.fn().mockResolvedValue(undefined),
}));
vi.mock('../src/workers/spawn-utils.js', () => ({
  spawnCli: vi.fn(),
  runCli: vi.fn(),
}));
vi.mock('../src/pipeline.js', () => ({
  runPipeline: vi.fn(),
}));

import { autoMergePr } from '../src/integrations/github.js';
import { runCli } from '../src/workers/spawn-utils.js';
import { runPipeline } from '../src/pipeline.js';
import { consumeOnce, recordMergedKgEvidence } from '../src/consume.js';
import { enqueueTask } from '../src/queue.js';
import { LESSONS_PATH, lessonExcerptHash } from '../src/lessons/guard.js';
import { captureKgEvidence, createLeanKgProvider, isFreshKgEvidence } from '../src/leankg.js';
import type { KgEvidence } from '../src/leankg.js';
import { buildKnowledgeContext } from '../src/prompt.js';

const mockAutoMerge = vi.mocked(autoMergePr);
const mockRun = vi.mocked(runCli);
const mockRunPipeline = vi.mocked(runPipeline);

/**
 * PRD Q28: the KG portion of the run's digest is persisted into `lessons.md`
 * on a successful merge, gated on LeanKG's own `freshness` stamp (FR-CTX-05).
 * Every test below drives a fake `leankg` binary, so no live index is needed.
 */

const dirs: string[] = [];

function tmpDir(prefix: string): string {
  const d = mkdtempSync(join(tmpdir(), prefix));
  dirs.push(d);
  return d;
}

/**
 * Write an executable `#!/bin/sh` stub that prints one canned KG reply.
 * `printf` is a shell builtin, so the reply costs no child process.
 */
function kgStub(json: string): string {
  const p = join(tmpDir('da-kg-stub-'), 'leankg');
  writeFileSync(p, `#!/bin/sh\nprintf '%s\\n' '${json}'\n`);
  chmodSync(p, 0o755);
  return p;
}

/** Stub that exits non-zero: the client's degraded path (KG layer omitted). */
function kgStubFailing(): string {
  const p = join(tmpDir('da-kg-stub-'), 'leankg');
  writeFileSync(p, '#!/bin/sh\necho "index unavailable" >&2\nexit 3\n');
  chmodSync(p, 0o755);
  return p;
}

/**
 * Run the digest build exactly as the dispatch sites do, then capture. The
 * wall-clock budget is widened: these tests pin provenance capture, and the
 * production 1s budget is covered by test/leankg.test.ts (a loaded machine must
 * not turn a budget expiry into a capture failure here).
 */
function evidenceFromStubRepo(bin: string): KgEvidence | undefined {
  const repo = tmpDir('da-kg-repo-');
  const provider = createLeanKgProvider({ repoPath: repo, query: 'merge gate ownership', bin, timeoutMs: 15_000 });
  buildKnowledgeContext(repo, { kg: 'leankg', kgProvider: provider });
  return captureKgEvidence(provider);
}

const ledgerRows = (repo: string): Record<string, unknown>[] => {
  const p = join(repo, '.devagent', 'runs', 'orchestration', 'events.jsonl');
  if (!existsSync(p)) return [];
  return readFileSync(p, 'utf8')
    .split('\n')
    .filter(Boolean)
    .map((l) => JSON.parse(l) as Record<string, unknown>);
};

describe('captureKgEvidence (verbatim provenance excerpt)', () => {
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('captures the excerpt the run actually consumed from a fresh reply', () => {
    const bin = kgStub(
      '{"answer":"src/consume.ts owns the merge gate","retrieval":{"rung":"graph-query","reason":"exact match"},"freshness":"fresh"}',
    );
    expect(evidenceFromStubRepo(bin)).toEqual({
      excerpt: 'retrieval: graph-query (exact match) | freshness: fresh',
      freshness: 'fresh',
    });
  });

  it('captures a stale stamp verbatim (the gate, not the capture, omits it)', () => {
    const bin = kgStub('{"answer":"x","retrieval":{"rung":"query"},"freshness":"possibly_stale"}');
    expect(evidenceFromStubRepo(bin)).toEqual({
      excerpt: 'retrieval: query | freshness: possibly_stale',
      freshness: 'possibly_stale',
    });
  });

  it('yields nothing when the client degraded (no reply, no provenance)', () => {
    expect(evidenceFromStubRepo(kgStubFailing())).toBeUndefined();
  });

  it('yields nothing when the reply carries no provenance fields', () => {
    expect(evidenceFromStubRepo(kgStub('{"answer":"bare answer"}'))).toBeUndefined();
  });
});

describe('isFreshKgEvidence (FR-CTX-05 freshness gate)', () => {
  it('admits only `fresh`', () => {
    expect(isFreshKgEvidence({ excerpt: 'e', freshness: 'fresh' })).toBe(true);
  });

  it('omits stale, possibly_stale, cold, and an absent stamp', () => {
    for (const freshness of ['stale', 'possibly_stale', 'cold', 'unknown']) {
      expect(isFreshKgEvidence({ excerpt: 'e', freshness })).toBe(false);
    }
    expect(isFreshKgEvidence({ excerpt: 'e' })).toBe(false);
    expect(isFreshKgEvidence(undefined)).toBe(false);
  });
});

describe('recordMergedKgEvidence (lessons.md persistence)', () => {
  /** Temp repo whose `npm test` exits `code` (0 = green evaluate step). */
  function tempRepo(code = 0): string {
    const d = tmpDir('da-kg-lessons-');
    writeFileSync(
      join(d, 'package.json'),
      JSON.stringify({ name: 'fixture', scripts: { test: `node -e "process.exit(${code})"` } }),
    );
    return d;
  }

  const fresh = { excerpt: 'retrieval: graph-query (exact match) | freshness: fresh', freshness: 'fresh' };
  const lessonsOf = (repo: string) => readFileSync(join(repo, LESSONS_PATH), 'utf8');

  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('appends the verbatim excerpt through the eval guard when fresh', () => {
    const repo = tempRepo();
    const r = recordMergedKgEvidence(repo, fresh);
    expect(r?.reason).toBe('accepted');
    expect(r?.suite).toBe('green');
    expect(lessonsOf(repo)).toBe(
      'KG digest evidence persisted on merge: retrieval: graph-query (exact match) | freshness: fresh [predictedImpact: cuts repeat KG re-queries on future runs: fresh structural evidence is already in the digest]\n',
    );
    expect(ledgerRows(repo)).toHaveLength(1);
    expect(ledgerRows(repo)[0]).toMatchObject({
      event: 'lessons-eval',
      accepted: true,
      reason: 'accepted',
      excerptHash: lessonExcerptHash(`KG digest evidence persisted on merge: ${fresh.excerpt}`),
    });
  });

  it('writes nothing for stale, possibly_stale, cold, or missing freshness', () => {
    for (const freshness of ['stale', 'possibly_stale', 'cold', undefined]) {
      const repo = tempRepo();
      const r = recordMergedKgEvidence(repo, { excerpt: 'retrieval: query', ...(freshness ? { freshness } : {}) });
      expect(r).toBeUndefined();
      expect(existsSync(join(repo, LESSONS_PATH))).toBe(false);
      expect(ledgerRows(repo)).toEqual([]);
    }
  });

  it('rejects the second persist of the same excerpt as a duplicate', () => {
    const repo = tempRepo();
    expect(recordMergedKgEvidence(repo, fresh)?.reason).toBe('accepted');
    const second = recordMergedKgEvidence(repo, fresh);
    expect(second?.ok).toBe(false);
    expect(second?.reason).toBe('duplicate');
    expect(second?.similarity).toBe(1);
    expect(lessonsOf(repo).split('\n').filter(Boolean)).toHaveLength(1);
    expect(ledgerRows(repo)).toHaveLength(2);
    expect(ledgerRows(repo)[1]).toMatchObject({ accepted: false, reason: 'duplicate', suite: 'skipped' });
  });

  it('reverts the lessons file when the evaluate step goes red', () => {
    const repo = tempRepo(1);
    const r = recordMergedKgEvidence(repo, fresh);
    expect(r?.reason).toBe('suite-red');
    expect(existsSync(join(repo, LESSONS_PATH))).toBe(false);
  });
});

describe('consumeOnce merge wiring (KG evidence -> lessons.md)', () => {
  let repo: string;

  /** Real git repo with a green `npm test` so the guard's evaluate step passes. */
  function gitRepo(): string {
    const dir = mkdtempSync(join(tmpdir(), 'da-kg-merge-'));
    dirs.push(dir);
    const git = (args: string[]) => execFileSync('git', args, { cwd: dir, stdio: 'pipe' });
    git(['init', '-q']);
    git(['config', 'user.email', 't@t']);
    git(['config', 'user.name', 't']);
    writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));
    git(['add', '-A']);
    git(['commit', '-qm', 'init']);
    git(['branch', 'devagent/pull-1']);
    return dir;
  }

  /** Mocked runCli: real git worktree plumbing, scripted suite run. */
  function mockRunCli(exitCode: number): void {
    mockRun.mockImplementation(async (cmd, args, opts) => {
      if (cmd === 'git' && args[0] === 'worktree') {
        try {
          execFileSync('git', args, { cwd: opts.cwd, stdio: 'pipe' });
          return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
        } catch (err) {
          return { exitCode: 1, stdout: '', stderr: String((err as Error).message), timedOut: false };
        }
      }
      return { exitCode, stdout: '', stderr: '', timedOut: false };
    });
  }

  function stubPipeline(kgEvidence?: { excerpt: string; freshness?: string }): void {
    mockRunPipeline.mockResolvedValue([
      { stage: 'implement', worker: 'omp', attempts: 1, ok: true, branch: 'devagent/pull-1', kgEvidence },
      { stage: 'validate', passed: true },
      { stage: 'publish', prUrl: 'https://github.com/acme/repo/pull/1', note: 'auto-pr enabled' },
    ]);
  }

  beforeEach(() => {
    mockRun.mockReset();
    mockAutoMerge.mockReset();
    mockAutoMerge.mockResolvedValue(undefined);
    mockRunPipeline.mockReset();
  });
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  const fresh = { excerpt: 'retrieval: graph-query (exact match) | freshness: fresh', freshness: 'fresh' };

  it('persists the excerpt on a successful merge', async () => {
    repo = gitRepo();
    stubPipeline(fresh);
    mockRunCli(0);
    enqueueTask(repo, { id: 'TASK-kg1', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.merged).toBe(true);
    expect(readFileSync(join(repo, LESSONS_PATH), 'utf8')).toContain(
      'KG digest evidence persisted on merge: retrieval: graph-query (exact match) | freshness: fresh',
    );
  });

  it('persists nothing when the run had no KG layer', async () => {
    repo = gitRepo();
    stubPipeline(undefined);
    mockRunCli(0);
    enqueueTask(repo, { id: 'TASK-kg2', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.merged).toBe(true);
    expect(existsSync(join(repo, LESSONS_PATH))).toBe(false);
  });

  it('persists nothing when the run\'s evidence was stale', async () => {
    repo = gitRepo();
    stubPipeline({ excerpt: 'retrieval: query | freshness: stale', freshness: 'stale' });
    mockRunCli(0);
    enqueueTask(repo, { id: 'TASK-kg3', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.merged).toBe(true);
    expect(existsSync(join(repo, LESSONS_PATH))).toBe(false);
  });

  it('persists nothing when the merge was blocked before it happened', async () => {
    repo = gitRepo();
    stubPipeline(fresh);
    mockRunCli(1);
    enqueueTask(repo, { id: 'TASK-kg4', title: 't', goal: 'g' });
    const r = await consumeOnce({ repoPath: repo, autoPr: false, autoMerge: true, maxLoops: 1, timeoutMs: 10_000 });
    expect(r.merged).toBe(false);
    expect(mockAutoMerge).not.toHaveBeenCalled();
    expect(existsSync(join(repo, LESSONS_PATH))).toBe(false);
  });
});
