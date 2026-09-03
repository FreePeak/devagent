import { describe, expect, it } from 'vitest';
import { checkBacklogPick, defaultTaskId, parseBacklogItems, runTask, strikeBacklogItems, syntheticTicketFromPrompt } from '../src/task.js';
import type { TaskDeps } from '../src/task.js';
import { RunLogger } from '../src/logger.js';

describe('defaultTaskId', () => {
  it('produces sanitize-safe TASK-prefixed ids unique per invocation', () => {
    const a = defaultTaskId();
    const b = defaultTaskId();
    expect(a).toMatch(/^TASK-[a-z0-9]+-[a-z0-9]{2,6}$/);
    expect(b).toMatch(/^TASK-[a-z0-9]+-[a-z0-9]{2,6}$/);
    expect(a).not.toBe(b);
  });

  it('uses the injected clock for the epoch segment', () => {
    expect(defaultTaskId(() => 0)).toMatch(/^TASK-0-[a-z0-9]{2,6}$/);
  });
});

describe('syntheticTicketFromPrompt', () => {
  it('splits first line as title, rest as description', () => {
    const t = syntheticTicketFromPrompt('Add rate limiting\n\nUse a token bucket per client IP.');
    expect(t.title).toBe('Add rate limiting');
    expect(t.description).toBe('Use a token bucket per client IP.');
    expect(t.labels).toContain('orchestrated');
  });

  it('uses the whole prompt as description when single-line', () => {
    const t = syntheticTicketFromPrompt('Only one line');
    expect(t.title).toBe('Only one line');
    expect(t.description).toBe('Only one line');
  });

  it('caps title at 80 chars', () => {
    expect(syntheticTicketFromPrompt('x'.repeat(200)).title).toHaveLength(80);
  });

  it('honors an explicit taskId for id and trackerInternalId', () => {
    const t = syntheticTicketFromPrompt('do thing', 'loop-66');
    expect(t.id).toBe('loop-66');
    expect(t.trackerInternalId).toBe('loop-66');
  });

  it('defaults to a unique collision-free id per call', () => {
    const a = syntheticTicketFromPrompt('one');
    const b = syntheticTicketFromPrompt('two');
    expect(a.id).not.toBe('TASK');
    expect(b.id).not.toBe(a.id);
  });
});

function fakeDeps(ok: boolean, prUrl?: string): TaskDeps {
  return {
    runPipelineDeps: {
      fetchTicket: async () => ({ id: 'TASK', title: '', description: '', labels: [], acceptanceCriteria: [] }),
      runGateG3: () => ({ passed: true, findings: [], detail: '' }),
    },
    implementStage: async (_cfg, ticket) => {
      seenTicket = ticket;
      return { ok, worker: 'claude-code', attempts: 1, worktreePath: ok ? '/wt' : undefined };
    },
    publishStage: async () => prUrl,
  };
}

let seenTicket: { id: string } | undefined;

describe('runTask', () => {
  const log = new RunLogger();

  it('threads opts.taskId into the dispatched ticket', async () => {
    seenTicket = undefined;
    await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: false, maxLoops: 1, timeoutMs: 1000, log, taskId: 'loop-66-x' },
      fakeDeps(true),
    );
    expect(seenTicket?.id).toBe('loop-66-x');
  });

  it('reports failure without publishing when implementation fails', async () => {
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: true, maxLoops: 1, timeoutMs: 1000, log },
      fakeDeps(false),
    );
    expect(r.ok).toBe(false);
    expect(r.prUrl).toBeUndefined();
  });

  it('skips publish without --auto-pr and reports worktree', async () => {
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: false, maxLoops: 1, timeoutMs: 1000, log },
      fakeDeps(true, 'https://example/pr/1'),
    );
    expect(r.ok).toBe(true);
    expect(r.prUrl).toBeUndefined(); // never called publisher
    expect(r.note).toContain('/wt');
  });

  it('publishes with --auto-pr and returns PR url', async () => {
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: true, maxLoops: 1, timeoutMs: 1000, log },
      fakeDeps(true, 'https://example/pr/2'),
    );
    expect(r.ok).toBe(true);
    expect(r.prUrl).toBe('https://example/pr/2');
  });

  it('handles missing credentials gracefully in auto-pr mode', async () => {
    const deps = fakeDeps(true);
    deps.publishStage = async () => undefined;
    const r = await runTask(
      { prompt: 'do thing', repoPath: '.', autoPr: true, maxLoops: 1, timeoutMs: 1000, log },
      deps,
    );
    expect(r.ok).toBe(true);
    expect(r.note).toContain('no remote credentials');
  });
});

const PRD_FIXTURE = `## 17. Roadmap

### Phase 4 — Expansion (post-v1)

#### Phase 4 — current backlog (2026-09-02, curation run 23)

- **Cross-board retry memory beyond the SHA guard** — carry the prior board's failure class onto the re-bridged goal so the scout deprioritizes until the root-cause fix lands (Q27).
- **Operator-role provider preflight** — apply the cheap probe + isTransientProviderError gate to curator/warroom/PO loops and emit a ledger row so a degraded factory is visible, not silent (Q40).
- **Lessons impact telemetry** — aggregate accept/reject outcomes against loop results so the lessonsMaxChars digest is ranked by measured effect (Q39).

## 18. Open Questions

> **Completed post-v0.3 (2026-09-02, curation run 23):** Operator hardening — research moved to local-evidence-only (5d8a319), NDJSON assistant-text extraction (d3adf17), all roles defaulted to omp (799fd86).
`;

/** Same fixture with the Q40 bullet wrapped in ~~ (as the curator strikes shipped items). */
function struckQ40Fixture(): string {
  return PRD_FIXTURE.replace(
    '- **Operator-role provider preflight**',
    '~~- **Operator-role provider preflight**',
  ).replace('(Q40).', '(Q40).~~');
}

describe('parseBacklogItems', () => {
  it('extracts id and bold title from each Phase 4 backlog bullet', () => {
    const items = parseBacklogItems(PRD_FIXTURE);
    expect(items.map((i) => i.id)).toEqual(['Q27', 'Q40', 'Q39']);
    expect(items[1]).toMatchObject({ title: 'Operator-role provider preflight', struck: false });
  });

  it('marks already-struck lines as struck', () => {
    const items = parseBacklogItems(struckQ40Fixture());
    expect(items.find((i) => i.id === 'Q40')?.struck).toBe(true);
  });
});

describe('checkBacklogPick', () => {
  it('rejects a pick whose id appears in a merged PR title', () => {
    const r = checkBacklogPick('Q40', PRD_FIXTURE, ['Q40 is shipped — preflight CLI at src/cli.ts (#120)']);
    expect(r.ok).toBe(false);
    expect(r.shipped).toBe(true);
    expect(r.message).toContain('already shipped');
    expect(r.message).toContain('Q40');
    expect(r.struckIds).toContain('Q40');
  });

  it('rejects a pick whose bold title text matches a merged PR title (curator title-match rule)', () => {
    const r = checkBacklogPick('Q40', PRD_FIXTURE, ['Operator-role provider preflight: probe stdin + circuit advance (#120)']);
    expect(r.shipped).toBe(true);
    expect(r.message).toContain('already shipped');
  });

  it('accepts a pick still in the current backlog when no merged PR matches', () => {
    const r = checkBacklogPick('Q27', PRD_FIXTURE, ['feat(lessons): eval-guard dedupe gate before any append (#116)']);
    expect(r.ok).toBe(true);
    expect(r.shipped).toBe(false);
    expect(r.prompt).toContain('(Q27)');
  });

  it('rejects a pick already struck from the backlog', () => {
    const r = checkBacklogPick('Q40', struckQ40Fixture(), []);
    expect(r.shipped).toBe(true);
    expect(r.message).toContain('already shipped');
  });

  it('rejects a pick not found in the current backlog section', () => {
    const r = checkBacklogPick('Q99', PRD_FIXTURE, []);
    expect(r.ok).toBe(false);
    expect(r.shipped).toBe(false);
    expect(r.message).toContain('not found');
  });
});

describe('strikeBacklogItems', () => {
  it('wraps confirmed-shipped lines in ~~ and leaves others untouched', () => {
    const updated = strikeBacklogItems(PRD_FIXTURE, ['Q40', 'Q39']);
    expect(updated).toContain('~~- **Operator-role provider preflight**');
    expect(updated).toContain('~~- **Lessons impact telemetry**');
    expect(updated).toContain('- **Cross-board retry memory beyond the SHA guard**');
  });

  it('does not double-strike an already-struck line', () => {
    const once = strikeBacklogItems(PRD_FIXTURE, ['Q40']);
    const twice = strikeBacklogItems(once, ['Q40']);
    expect(twice).toBe(once);
  });
});
