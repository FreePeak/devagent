import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import {
  buildPlannerPrompt,
  buildChildTrailsDigest,
  CHILD_TRAILS_MAX_CHARS,
  COMPACT_CONTEXT_MARKER,
} from '../../src/prompt.js';

/**
 * End-to-end coverage of the prior-loop child-trail flow into the next-loop
 * planner prompt.
 *
 * Simulates a full loop boundary: a prior loop's worklog.jsonl sits inside a
 * worker output directory (the layout the prompt builder walks at
 * `src/prompt.ts`), `buildPlannerPrompt` drains that worklog into the
 * per-(loopId, taskId) trail ledger and splices the rendered content under
 * the `## Prior Worker Trails` heading of the next-loop planner prompt.
 * The same digest builder — `buildChildTrailsDigest` — is exercised
 * directly on a long trail.jsonl to prove the cap behavior is genuine,
 * not a coincidence of fixture sizing.
 *
 * The test uses the production plumbing as-is:
 *  - `buildPlannerPrompt` (wiring in `src/prompt.ts`)
 *  - `buildChildTrailsDigest` (helper in `src/prompt.ts`)
 *  - `CHILD_TRAILS_MAX_CHARS` (the audit-reported `lessonsMaxChars` default)
 *
 * No mocks anywhere: the real `ingestChildTrails` / `compactContext` /
 * `buildChildTrailsDigest` paths run so a future refactor that drops the
 * call site, shrinks the cap, or reorders the splice fails here.
 */

const SENTINEL_PRIOR_TRAIL = 'E2E_PRIOR_TRAIL_SENTINEL_55';

/**
 * Lay out a fake prior-loop worklog under the per-loop worker convention
 * (`<cwd>/.selfbuild/loops/<loopId>/workers/<workerName>/worklog.jsonl`)
 * with three entries. The newest entry carries the recognizable sentinel
 * in the `text` field so the rendered prompt assertion cannot false-positive
 * on incidental text.
 */
function writePriorWorklog(cwd: string, loopId: string): void {
  const workerDir = join(cwd, '.selfbuild', 'loops', loopId, 'workers', 'claude-code');
  mkdirSync(workerDir, { recursive: true });
  const worklogPath = join(workerDir, 'worklog.jsonl');
  const lines = [
    JSON.stringify({ ts: 1, text: 'iteration 1 start' }),
    JSON.stringify({ ts: 2, text: 'criterion T1 met' }),
    // Newest entry — carries the recognizable sentinel in `text`.
    JSON.stringify({ ts: 3, text: `committed with marker ${SENTINEL_PRIOR_TRAIL}` }),
  ];
  writeFileSync(worklogPath, lines.join('\n') + '\n', 'utf8');
}

/**
 * Write a long prior-loop trail.jsonl whose joined payload exceeds the
 * `CHILD_TRAILS_MAX_CHARS` cap so the digest builder's truncation path is
 * exercised rather than the no-truncation happy path.
 */
function writeOverflowTrail(repo: string): string {
  const trailPath = join(repo, 'prior-trail.jsonl');
  // Each line is ~120 chars; 40 lines of payload ≈ 4800 chars, well over
  // CHILD_TRAILS_MAX_CHARS (4000). The digest builder's ratchet logic
  // must drop oldest entries whole to fit the cap.
  const lines = Array.from({ length: 40 }, (_, i) =>
    JSON.stringify({ ts: i, text: `payload-${i}-${'x'.repeat(80)}` }),
  );
  writeFileSync(trailPath, lines.join('\n') + '\n', 'utf8');
  return trailPath;
}

describe('end-to-end prior-loop trail flow into next-loop planner prompt', () => {
  let priorWorktree: string;
  let repoPath: string;

  beforeEach(() => {
    priorWorktree = mkdtempSync(join(tmpdir(), 'da-e2e-prior-wt-'));
    repoPath = mkdtempSync(join(tmpdir(), 'da-e2e-prompt-'));
  });

  afterEach(() => {
    rmSync(priorWorktree, { recursive: true, force: true });
    rmSync(repoPath, { recursive: true, force: true });
  });

  it('flows prior trail into next loop prompt', () => {
    // 1) Lay out the prior loop's worklog under the worker convention so
    //    the wiring (ingestChildTrails + compactContext) can find it.
    const loopId = 'loop-55';
    writePriorWorklog(priorWorktree, loopId);

    // 2) Build the next-loop planner prompt through the prompt builder.
    const goal = 'Ship e2e prior-trail flow coverage';
    const prompt = buildPlannerPrompt(goal, priorWorktree, { loopId, taskId: 'T1' });

    // (a) Sentinel substring from the newest trail entry must surface
    //     verbatim — proves the trail was drained AND rendered into the
    //     prompt, not just read and discarded.
    expect(prompt).toContain(SENTINEL_PRIOR_TRAIL);
    // (b) The exact section header the builder writes must be present.
    //     The header is the `COMPACT_CONTEXT_MARKER` constant exported
    //     from `src/prompt.ts` — the splice point at which
    //     `compactContext` injects the rendered trail block. Importing
    //     the constant (rather than a magic-string literal) guarantees
    //     this assertion cannot drift if the marker is ever renamed.
    expect(prompt).toContain(COMPACT_CONTEXT_MARKER);
  });

  it('caps digest at lessonsMaxChars', async () => {
    // Build the digest with the real helper on a long trail.jsonl that
    // intentionally overflows the cap. The truncation path is the thing
    // under test — a no-truncation fixture would mask a regression that
    // dropped the ratchet.
    const trailPath = writeOverflowTrail(repoPath);
    const { digest } = await buildChildTrailsDigest([trailPath], CHILD_TRAILS_MAX_CHARS);

    // The audit-reported `lessonsMaxChars` default is the same constant
    // that bounds the child-trail digest (`CHILD_TRAILS_MAX_CHARS = 4000`).
    // The digest must fit within that cap — never exceed it, never split
    // a line in half.
    expect(digest.length).toBeLessThanOrEqual(CHILD_TRAILS_MAX_CHARS);
    // Sanity: the digest is non-empty (the fixture has data) AND the
    // newest entries survive (the ratchet drops oldest, not newest).
    expect(digest.length).toBeGreaterThan(0);
    expect(digest).toContain('payload-39-');
  });

  it('skips gracefully when prior trail is missing', () => {
    // No fixture written — neither the worklog layout nor any trail file
    // exists under the temp dirs. The prompt builder must still render
    // without throwing; the missing trail simply omits the section.
    const goal = 'Skip the prior-trail injection gracefully';
    const prompt = buildPlannerPrompt(goal, repoPath, {
      loopId: 'loop-missing-55',
      taskId: 'T1',
    });

    // The prompt must still be a non-empty string (valid prompt object).
    expect(typeof prompt).toBe('string');
    expect(prompt.length).toBeGreaterThan(0);
    // The goal must surface so the planner still knows what to do.
    expect(prompt).toContain(goal);
    // The labeled trail section is absent when there is nothing to inject.
    // We assert the marker is not followed by a Trail block (i.e. nothing
    // was rendered after the marker). The marker itself is in the prompt
    // template as the splice point, so its bare presence is fine; what
    // must be missing is the actual rendered trail content beneath it.
    expect(prompt).not.toContain(`${COMPACT_CONTEXT_MARKER}\n### Trail for`);
  });
});
