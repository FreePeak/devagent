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
function fixture(opts: { subjects: string[]; withOrigin?: boolean; prd?: string; ledger?: string[] }): string {
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
  if (opts.ledger) {
    mkdirSync(join(repo, '.selfbuild'), { recursive: true });
    writeFileSync(join(repo, '.selfbuild', 'ledger.jsonl'), opts.ledger.join('\n') + '\n');
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

describe('selfbuild-loop.sh wiring (2026-09-07 issue-first tracker)', () => {
  const script = readFileSync(join(repoRoot, 'scripts', 'selfbuild-loop.sh'), 'utf8');

  it('claims the highest-priority selfbuild issue deterministically before LLM selection', () => {
    expect(script).toContain('pick_issue()');
    expect(script).toContain("gh issue list --repo \"$GH_REPO\" --state open --label \"$ISSUE_LABEL\"");
    // Deterministic order: priority rank, then oldest issue number.
    expect(script).toContain('items.sort((a, b) => rank(a.labels) - rank(b.labels) || a.number - b.number)');
    // The claim outranks the LLM selection path: PO runs only when the
    // tracker AND the queue are empty.
    expect(script).toContain('if [ -z "${QUEUED_TASK_ID:-}" ] && [ -z "${ISSUE_NUM:-}" ]; then');
    expect(script).toContain('[issue] no open selfbuild issue found — falling back to LLM selection');
  });

  it('closes the claimed issue when the iteration ships or the guard skips it', () => {
    expect(script).toContain('close_issue "$ISSUE_NUM" "self-build loop $N shipped this issue: $GOAL" || true');
    expect(script).toContain('[ -n "${ISSUE_NUM:-}" ] && close_issue "$ISSUE_NUM" "self-build loop $N: goal already shipped (Q27 no re-burn guard) — closing as done" || true');
  });

  it('keeps docs/PRD.md as a state document: dirty PRD pauses the loop, PRD-per-PR rides the dispatch', () => {
    expect(script).toContain('if [ "$DRY_RUN" != 1 ] && ! git diff --quiet -- docs/PRD.md; then');
    expect(script).toContain('record "$N" operator-degraded "PRD dirty: operator mid-edit, state doc must land clean"');
    expect(script).toContain('PRD-per-PR policy (2026-09-07): every PR ships with its state update.');
    expect(script).toContain('TASK_ARGS=(task --prompt "${GOAL}');
  });
});

// PRD:889 residual — goals name backlog bullets by docs/PRD.md line
// ("PRD:889"), and when merged PR titles are generic auto-cleanup snapshots
// the productive ledger rows are the shipped evidence. Mirrors the real
// docs/PRD.md:884/:889 collision: the struck bullet and the open one both
// parse to Q27 (extractItemId keys on the last [A-Z]+\d+ token, and the open
// bullet ends "(Q27 family; …)"), so a naive id lookup resolves onto the
// struck twin and the guard never fires.
describe('backlog-check line refs + ledger fallback (PRD:889 residual)', () => {
  const COLLIDE_FIXTURE = `## 17. Roadmap

#### Phase 4 — current backlog (2026-09-07, collision fixture)

~~- **Cross-board retry memory beyond the SHA guard** — the shipped twin (Q27).~~
- **PRD-backlog reconciliation at pick time** — the open twin (Q27 family; trailing).
- **Consolidate the loop scripts** — fold recovery into src/orchestrator/ (Q19).

## 18. Open Questions
`;
  // Line 5 = struck Q27 twin, line 6 = open Q27 twin, line 7 = Q19.
  const goalRow = (loop: number, status: string, goal: string) =>
    JSON.stringify({ loop, ts: '2026-09-07T00:00:00Z', status, goal });

  it('exit 1: a PRD:<line> pick resolves to the bullet; a productive goal naming the line is shipped evidence', () => {
    const repo = fixture({
      subjects: ['devagent(TASK-x): auto-cleanup snapshot (#162)'],
      prd: COLLIDE_FIXTURE,
      ledger: [goalRow(120, 'ok', 'Goal: PRD:6 — wire checkBacklogPick into the selfbuild driver')],
    });
    const r = backlogCheck(repo, 'PRD:6', '--repo', repo);
    expect(r.status).toBe(1);
    expect(r.out).toContain('PRD:6 already shipped');
  });

  it('--strike with ledger evidence strikes only the referenced line, not its struck-id twin', () => {
    const repo = fixture({
      subjects: ['chore: unrelated'],
      prd: COLLIDE_FIXTURE,
      ledger: [goalRow(120, 'ok', 'Goal: PRD:6 — shipped the reconciliation wiring')],
    });
    // Pick :7 (Q19, current) while :6 is confirmed-shipped via the ledger goal.
    const r = backlogCheck(repo, 'PRD:7', '--repo', repo, '--strike');
    expect(r.status).toBe(0);
    expect(r.out).toContain('struck: Q27');
    const prd = readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8');
    expect(prd).toContain('~~- **PRD-backlog reconciliation at pick time**');
    expect(prd).toContain('(Q27 family; trailing).~~');
    expect(prd).not.toContain('~~- **Consolidate the loop scripts**');
  });

  it('partial-completion guard: a "remainder" goal referencing the line keeps the item open', () => {
    const repo = fixture({
      subjects: ['chore: unrelated'],
      prd: COLLIDE_FIXTURE,
      ledger: [
        goalRow(121, 'ok', 'Goal: PRD:7 — fold starved() out of shell into typed code'),
        goalRow(122, 'ok', 'Goal: PRD:7 remainder — migrate the three sibling drivers'),
      ],
    });
    const r = backlogCheck(repo, 'PRD:7', '--repo', repo, '--strike');
    expect(r.status).toBe(0);
    expect(r.out).toContain('PRD:7 is current backlog');
    const prd = readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8');
    expect(prd).not.toContain('~~- **Consolidate the loop scripts**');
  });

  it('a failed ledger row is not shipped evidence', () => {
    const repo = fixture({
      subjects: ['chore: unrelated'],
      prd: COLLIDE_FIXTURE,
      ledger: [goalRow(120, 'failed', 'Goal: PRD:6 — attempted the wiring')],
    });
    const r = backlogCheck(repo, 'PRD:6', '--repo', repo);
    expect(r.status).toBe(0);
    expect(r.out).toContain('PRD:6 is current backlog');
  });

  it('ledger evidence is the strict PRD:<line> ref only — a goal quoting the title without the ref cannot strike', () => {
    const repo = fixture({
      subjects: ['chore: unrelated'],
      prd: COLLIDE_FIXTURE,
      ledger: [
        goalRow(105, 'ok', 'Goal: Consolidate stuck-board recovery into typed code per backlog item Consolidate the loop scripts'),
      ],
    });
    const r = backlogCheck(repo, 'PRD:7', '--repo', repo, '--strike');
    expect(r.status).toBe(0);
    const prd = readFileSync(join(repo, 'docs', 'PRD.md'), 'utf8');
    expect(prd).not.toContain('~~- **Consolidate the loop scripts**');
  });

  it('id collision resolves toward the unstruck twin: pick Q27 targets the open bullet, not the struck one', () => {
    const repo = fixture({ subjects: ['chore: unrelated'], prd: COLLIDE_FIXTURE });
    const r = backlogCheck(repo, 'Q27', '--repo', repo);
    // Pre-fix this read `already shipped (struck in docs/PRD.md)` off the twin.
    expect(r.status).toBe(0);
    expect(r.out).toContain('Q27 is current backlog');
  });

  it('exit 2: a PRD:<line> ref pointing outside the backlog section is unresolved', () => {
    const repo = fixture({ subjects: ['chore: unrelated'], prd: COLLIDE_FIXTURE });
    const r = backlogCheck(repo, 'PRD:1', '--repo', repo);
    expect(r.status).toBe(2);
    expect(r.out).toContain('not found');
  });
});
