import { describe, expect, it, afterAll } from 'vitest';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  alreadyShipped,
  evaluateStarvation,
  goalSubjectItem,
  normalizeGoalText,
  readLedgerLines,
} from '../../src/orchestrator/selfbuild-gate.js';

// PRD:888 — the selfbuild driver's starvation gate (starved()) and Q27
// re-burn guard (already_shipped()) folded out of scripts/selfbuild-loop.sh
// into src/orchestrator/selfbuild-gate.ts, exposed as `devagent
// selfbuild-gate --starved / --already-shipped <goal>`. The exit codes are
// the contract (0 continue / 1 verdict / 2 unresolved, same as backlog-check
// in PRD:889), so the CLI tests spawn the real binary against ledger
// fixtures, mirroring test/backlog-check.test.ts.

const repoRoot = join(import.meta.dirname, '..', '..');
const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

function row(loop: number, status: string, goal: string): string {
  return `{"loop":${loop},"ts":"2026-09-06T00:00:0${loop % 10}Z","status":"${status}","goal":"${goal}"}`;
}

/** Repo fixture whose .selfbuild/ledger.jsonl holds exactly `lines`. */
function ledgerRepo(lines: string[]): string {
  const repo = mkdtempSync(join(tmpdir(), 'da-gate-'));
  dirs.push(repo);
  mkdirSync(join(repo, '.selfbuild'), { recursive: true });
  writeFileSync(join(repo, '.selfbuild', 'ledger.jsonl'), lines.map((l) => l + '\n').join(''));
  return repo;
}

function gate(repo: string, ...args: string[]): { status: number | null; out: string } {
  const r = spawnSync('npx', ['tsx', join(repoRoot, 'src/cli.ts'), 'selfbuild-gate', ...args], {
    cwd: repoRoot,
    encoding: 'utf8',
    env: { PATH: process.env.PATH, HOME: process.env.HOME },
    timeout: 60_000,
  });
  return { status: r.status, out: `${r.stdout}${r.stderr}` };
}

describe('evaluateStarvation (PRD:888 port of starved())', () => {
  it('counts consecutive non-productive rows from the tail and starves at the limit', () => {
    const lines = [row(1, 'ok', 'shipped work'), ...[2, 3, 4, 5, 6].map((n) => row(n, 'failed', 'x'))];
    expect(evaluateStarvation(lines, 5)).toMatchObject({ starved: true, count: 5 });
  });

  it('a productive row breaks the streak — one short of the limit is not starved', () => {
    const lines = [row(1, 'failed', 'x'), row(2, 'ok', 'shipped'), ...[3, 4, 5, 6].map((n) => row(n, 'failed', 'x'))];
    expect(evaluateStarvation(lines, 5)).toMatchObject({ starved: false, count: 4 });
  });

  it('every documented productive class breaks: ok | pr-open | merged | pushed', () => {
    for (const status of ['ok', 'pr-open', 'merged', 'pushed']) {
      const lines = [...[1, 2, 3, 4, 5].map((n) => row(n, 'failed', 'x')), row(6, status, 'handed off')];
      expect(evaluateStarvation(lines, 5).starved).toBe(false);
    }
  });

  it('statuses merely containing a productive word do not break (push-failed counts)', () => {
    const lines = [...[1, 2, 3, 4].map((n) => row(n, 'failed', 'x')), row(5, 'push-failed', 'x')];
    expect(evaluateStarvation(lines, 5)).toMatchObject({ starved: true, count: 5 });
  });

  it('degraded rows are exempt: they neither count nor break the streak', () => {
    // 2026-09-05 class: three provider-degraded rows must not read as strikes.
    const exempt = ['operator-degraded', 'operator-diverged', 'provider-degraded'];
    const lines = [
      row(1, 'ok', 'shipped'),
      ...[2, 3, 4].map((n) => row(n, exempt[n % 3], 'pause')),
      ...[5, 6].map((n) => row(n, 'failed', 'x')),
    ];
    expect(evaluateStarvation(lines, 5)).toMatchObject({ starved: false, count: 2 });
    // A degraded tail cannot hide an already-starved streak beneath it.
    const buried = [...[1, 2, 3, 4, 5].map((n) => row(n, 'failed', 'x')), ...[6, 7].map((n) => row(n, 'provider-degraded', 'pause'))];
    expect(evaluateStarvation(buried, 5)).toMatchObject({ starved: true, count: 5 });
  });

  it('empty ledger is not starved; limit 0 starves trivially (shell -ge parity)', () => {
    expect(evaluateStarvation([], 5)).toMatchObject({ starved: false, count: 0 });
    expect(evaluateStarvation([], 0).starved).toBe(true);
  });
});

describe('alreadyShipped (PRD:888 port of already_shipped())', () => {
  const Q41_CANDIDATE =
    'Goal: Q41 — surface consecutive cross-role provider degradation. Loops 106–108 logged three `provider-degraded` rows in 75s; those rows are starvation-gate-exempt';
  // Real loop-100 row: mentions "+ Q41" only inside the parenthetical.
  const LOOP100_ROW = row(
    100,
    'ok',
    'Goal: Divergence-resilient doc-sync (PRD §17 doc-sync defect + Q41 degradation surface; loops 95–99 burned on generic divergence misclassification). In `src/git',
  );

  it('matches the real re-burn class: goal prefix on a productive row', () => {
    const goal = 'Cross-board retry memory beyond the SHA guard so the scout deprioritizes until the root-cause fix lands (Q27).';
    const lines = [row(53, 'ok', `Goal: ${goal}`)];
    expect(alreadyShipped(`Goal: ${goal}`, lines)).toEqual({ shipped: true, reason: 'goal-prefix' });
  });

  it('normalizes before matching: quotes and newlines in the candidate still hit', () => {
    const recorded = row(70, 'ok', 'Goal: fold selfbuild gates — starvation + Q27 re-burn guard into typed code with fixtures and CLI smoke coverage');
    const candidate = 'Goal: fold selfbuild gates\n\t— "starvation" + Q27 re-burn guard into typed code with fixtures and CLI smoke coverage';
    expect(normalizeGoalText(candidate)).toContain(' starvation ');
    expect(alreadyShipped(candidate, [recorded]).shipped).toBe(true);
  });

  it('subject-id match survives goal rewrites (loop-58/71 Q35 class)', () => {
    const lines = [row(110, 'ok', 'Goal: Q41 — surface consecutive cross-role provider degradation. Loops 106–108 logged three `provider-degraded` rows in 75s, and those rows are starvation-gate-')];
    expect(alreadyShipped('Goal: Q41 — surface provider degradation across roles (follow-up slice)', lines)).toEqual({
      shipped: true,
      reason: 'subject-id',
    });
  });

  it('loop-100 guard: an incidental "+ Q41" inside a parenthetical is NOT a match', () => {
    expect(alreadyShipped(Q41_CANDIDATE, [LOOP100_ROW])).toEqual({ shipped: false, reason: null });
  });

  it('non-productive rows never match (skipped/failed rows are not evidence)', () => {
    const lines = [row(109, 'skipped', Q41_CANDIDATE), row(110, 'failed', Q41_CANDIDATE)];
    expect(alreadyShipped(Q41_CANDIDATE, lines).shipped).toBe(false);
  });

  it('unrelated goal, empty goal, and missing goal marker all read as not shipped', () => {
    // Empty goal is the one intentional divergence from the awk original:
    // index($0, "") == 1 made the shell read an empty goal as shipped-on-
    // everything; the driver's ^Goal: gate makes it unreachable anyway.
    expect(alreadyShipped('Goal: something entirely new (Q99)', [LOOP100_ROW]).shipped).toBe(false);
    expect(alreadyShipped('', [LOOP100_ROW]).shipped).toBe(false);
    expect(alreadyShipped('Goal: Q41 x', []).shipped).toBe(false);
  });

  it('goalSubjectItem: id must sit before the first paren, capped at 80 chars', () => {
    expect(goalSubjectItem('Goal: Q35 cross-board retry memory (shipped as #100)')).toBe('Q35');
    expect(goalSubjectItem('Goal: doc-sync surface (PRD §17 + Q41 degradation surface)')).toBe('');
    expect(goalSubjectItem(`Goal: ${'x'.repeat(90)} Q42`)).toBe('');
    expect(goalSubjectItem('Goal: plain goal, no id')).toBe('');
  });
});

describe('selfbuild-gate CLI (exit-code contract: 0 continue / 1 verdict / 2 unresolved)', () => {
  it('--starved: exit 1 with the STARVED verdict word once the limit is reached', () => {
    const repo = ledgerRepo([1, 2, 3, 4, 5].map((n) => row(n, 'failed', 'x')));
    const r = gate(repo, '--starved', '--repo', repo);
    expect(r.status).toBe(1);
    expect(r.out).toContain('starved: 5 consecutive non-productive iterations >= limit 5');
  });

  it('--starved: exit 0 when the tail is productive; --limit overrides the default 5', () => {
    const repo = ledgerRepo([...[1, 2, 3].map((n) => row(n, 'failed', 'x')), row(4, 'pr-open', 'handed to review')]);
    expect(gate(repo, '--starved', '--repo', repo).status).toBe(0);
    const streak = ledgerRepo([1, 2, 3].map((n) => row(n, 'failed', 'x')));
    expect(gate(streak, '--starved', '--limit', '3', '--repo', streak).status).toBe(1);
    expect(gate(streak, '--starved', '--repo', streak).out).toContain('productive: 3 consecutive');
  });

  it('--already-shipped: exit 1 + "already shipped" on a productive-row match, exit 0 otherwise', () => {
    const shipped = row(110, 'ok', 'Goal: Q41 — surface consecutive cross-role provider degradation. Loops 106–108 logged three rows and those rows are starvation-gate-exempt');
    const repo = ledgerRepo([shipped]);
    const hit = gate(repo, '--already-shipped', 'Goal: Q41 — surface provider degradation across roles (follow-up slice)', '--repo', repo);
    expect(hit.status).toBe(1);
    expect(hit.out).toContain('already shipped (subject-id match on a productive ledger row)');
    const miss = gate(repo, '--already-shipped', 'Goal: Q42 brand-new work nobody recorded', '--repo', repo);
    expect(miss.status).toBe(0);
    expect(miss.out).toContain('not shipped — dispatch ok');
  });

  it('missing ledger reads as continue for both gates (shell `[ -f ] || return 1` parity)', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-gate-empty-'));
    dirs.push(repo);
    expect(gate(repo, '--starved', '--repo', repo).status).toBe(0);
    expect(gate(repo, '--already-shipped', 'Goal: anything', '--repo', repo).status).toBe(0);
  });

  it('unresolved (exit 2): no gate flag, or both flags at once', () => {
    const repo = ledgerRepo([row(1, 'failed', 'x')]);
    expect(gate(repo, '--repo', repo).status).toBe(2);
    expect(gate(repo, '--starved', '--already-shipped', 'Goal: x', '--repo', repo).status).toBe(2);
  });

  it('readLedgerLines: trailing newline yields no phantom empty row', () => {
    const repo = ledgerRepo([row(1, 'ok', 'x')]);
    const lines = readLedgerLines(join(repo, '.selfbuild', 'ledger.jsonl'));
    expect(lines).toHaveLength(1);
    expect(readLedgerLines(join(repo, 'nope.jsonl'))).toEqual([]);
  });
});

describe('selfbuild-loop.sh wiring (PRD:888 thin caller)', () => {
  const script = readFileSync(join(repoRoot, 'scripts', 'selfbuild-loop.sh'), 'utf8');

  it('dispatches the CLI gates instead of embedding awk decision logic', () => {
    expect(script).toContain(`out="$("${'${DEVAGENT[@]}'}" selfbuild-gate --starved --limit "$STARVATION_LIMIT" --repo "$REPO" 2>&1)" || rc=$?`);
    expect(script).toContain(`out="$("${'${DEVAGENT[@]}'}" selfbuild-gate --already-shipped "$1" --repo "$REPO" 2>&1)" || rc=$?`);
    expect(script).not.toContain('awk -v lim="$STARVATION_LIMIT"');
    expect(script).not.toContain('gsub(/.*goal:/');
  });

  it('honors rc 1 only with the CLI verdict word — a crashed gate must not halt/skip', () => {
    expect(script).toContain(`[ "$rc" -eq 1 ] && [[ "$out" == *"starved:"* ]]`);
    expect(script).toContain(`[ "$rc" -eq 1 ] && [[ "$out" == *"already shipped"* ]]`);
  });

  it('call sites keep the documented semantics: halt on starvation, Q27 guard after backlog-check', () => {
    expect(script).toContain('if starved; then');
    expect(script).toContain('if [ "$GUARD_RESOLVED" != 1 ] && already_shipped "$GOAL"; then');
  });
});
