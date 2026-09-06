import { describe, expect, it, afterAll } from 'vitest';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// PRD:889 — `devagent backlog-check` is the machine-readable pick-time
// reconciliation the selfbuild driver calls before dispatch. Exit codes are
// the contract (0 current / 1 shipped / 2 unresolved), so the tests spawn the
// real CLI against real git fixtures, mirroring test/scan-text.test.ts.

const repoRoot = join(import.meta.dirname, '..');
const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

const PRD_FIXTURE = `## 17. Roadmap

### Phase 4 — Expansion (post-v1)

#### Phase 4 — current backlog (2026-09-02, curation run 23)

- **Cross-board retry memory beyond the SHA guard** — carry the prior board's failure class onto the re-bridged goal so the scout deprioritizes until the root-cause fix lands (Q27).
- **Operator-role provider preflight** — apply the cheap probe + isTransientProviderError gate to curator/warroom/PO loops and emit a ledger row so a degraded factory is visible, not silent (Q40).
- **Lessons impact telemetry** — aggregate accept/reject outcomes against loop results so the lessonsMaxChars digest is ranked by measured effect (Q39).

## 18. Open Questions
`;

/** Real repo with docs/PRD.md + squash-merged PR subjects on origin/main. */
function fixture(opts: { subjects: string[]; withOrigin?: boolean; prd?: string }): string {
  const repo = mkdtempSync(join(tmpdir(), 'da-backlog-'));
  dirs.push(repo);
  const g = (...args: string[]) => spawnSync('git', args, { cwd: repo, encoding: 'utf8' });
  g('init', '-b', 'main');
  g('config', 'user.email', 't@t');
  g('config', 'user.name', 't');
  mkdirSync(join(repo, 'docs'), { recursive: true });
  writeFileSync(join(repo, 'docs', 'PRD.md'), opts.prd ?? PRD_FIXTURE);
  g('add', '-A');
  for (const s of opts.subjects) {
    writeFileSync(join(repo, 'CHANGELOG.md'), s + '\n', { flag: 'a' });
    g('add', '-A');
    g('commit', '-m', s);
  }
  if (opts.withOrigin !== false) g('update-ref', 'refs/remotes/origin/main', 'HEAD');
  return repo;
}

function backlogCheck(repo: string, ...args: string[]): { status: number | null; out: string } {
  const r = spawnSync('npx', ['tsx', join(repoRoot, 'src/cli.ts'), 'backlog-check', ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: { PATH: process.env.PATH, HOME: process.env.HOME },
    timeout: 60_000,
  });
  return { status: r.status, out: `${r.stdout}${r.stderr}` };
}

describe('backlog-check CLI (PRD:889 driver pick guard)', () => {
  it('exit 1: rejects an id whose merged PR title matches, so the driver skips before dispatch', () => {
    const repo = fixture({
      subjects: ['feat(preflight): Operator-role provider preflight — probe stdin + circuit advance (#120)'],
    });
    const r = backlogCheck(repo, 'Q40', '--repo', repo);
    expect(r.status).toBe(1);
    expect(r.out).toContain('Q40 already shipped');
  });

  it('exit 1: rejects an id already struck in docs/PRD.md', () => {
    const struck = PRD_FIXTURE
      .replace('- **Lessons impact telemetry**', '~~- **Lessons impact telemetry**')
      .replace('(Q39).', '(Q39).~~');
    const repo = fixture({ subjects: ['chore: unrelated'], prd: struck });
    const r = backlogCheck(repo, 'Q39', '--repo', repo);
    expect(r.status).toBe(1);
    expect(r.out).toContain('already shipped (struck in docs/PRD.md)');
  });

  it('exit 0: accepts a current-backlog pick when merged-title evidence exists', () => {
    const repo = fixture({ subjects: ['feat(lessons): eval-guard dedupe gate before any append (#116)'] });
    const r = backlogCheck(repo, 'Q27', '--repo', repo);
    expect(r.status).toBe(0);
    expect(r.out).toContain('Q27 is current backlog — dispatch ok');
  });

  it('exit 2 (unresolved, caller falls back): no origin/main evidence for a current pick', () => {
    const repo = fixture({ subjects: ['init'], withOrigin: false });
    const r = backlogCheck(repo, 'Q27', '--repo', repo);
    expect(r.status).toBe(2);
  });

  it('exit 2 (unresolved): id not found in the Phase 4 backlog', () => {
    const repo = fixture({ subjects: ['init commit'] });
    const r = backlogCheck(repo, 'Q99', '--repo', repo);
    expect(r.status).toBe(2);
    expect(r.out).toContain('not found');
  });

  it('--strike writes confirmed-shipped items struck into docs/PRD.md and reports them', () => {
    const repo = fixture({
      subjects: ['Operator-role provider preflight: probe stdin + circuit advance (#120)'],
    });
    const r = backlogCheck(repo, 'Q27', '--repo', repo, '--strike');
    // The pick itself is current (dispatch ok) but the reconciled shipped
    // item is struck in the same run — the task --pick behavior, driver-side.
    expect(r.status).toBe(0);
    expect(r.out).toContain('struck: Q40');
    const prd = readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8');
    expect(prd).toContain('~~- **Operator-role provider preflight**');
    expect(prd).toContain('(Q40).~~');
    expect(prd).not.toContain('~~- **Cross-board retry memory');
  });
});

describe('selfbuild-loop.sh wiring (PRD:889)', () => {
  const script = readFileSync(join(repoRoot, 'scripts', 'selfbuild-loop.sh'), 'utf8');

  it('dispatches backlog-check with --strike on the goal subject id before phases 4-7', () => {
    // Subject extraction mirrors already_shipped: text before the first "(",
    // capped at 80 chars, first Q-token.
    expect(script).toContain("BACKLOG_PICK=$(printf '%s' \"${GOAL%%(*}\" | cut -c1-80 | grep -oE 'Q[0-9]+' | head -1 || true)");
    expect(script).toContain('BC_ARGS=(backlog-check "$BACKLOG_PICK" --repo "$REPO")');
    // Dry-run must never write the PRD.
    expect(script).toContain('[ "$DRY_RUN" != 1 ] && BC_ARGS+=(--strike)');
    // rc 1 skips the iteration before dispatch as a ledger-visible skip —
    // but only with the CLI's verdict line present: a crashed check also
    // exits 1 and must fall through to the ledger guard, not false-skip.
    expect(script).toContain('if [ "$BC_RC" -eq 1 ] && [[ "$BC_OUT" == *"already shipped"* ]]; then');
    expect(script).toContain('record "$N" skipped "$GOAL"');
    expect(script).toContain('already shipped — skipping before dispatch (PRD:889 pick reconciliation)');
  });

  it('supersedes already_shipped only when the check resolved the id', () => {
    expect(script).toContain('[ "$BC_RC" -eq 0 ] && GUARD_RESOLVED=1');
    expect(script).toContain('if [ "$GUARD_RESOLVED" != 1 ] && already_shipped "$GOAL"; then');
  });

  it('commits a written strike locally so the next sync-docs does not refuse on a dirty PRD', () => {
    expect(script).toContain('git commit -m "self-build loop $N: strike confirmed-shipped backlog items (PRD:889 pick reconciliation)"');
  });
});
