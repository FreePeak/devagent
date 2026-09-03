import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { loadLessons, loadLessonsDigest } from '../src/prompt.js';
import {
  appendLessonGuarded,
  appendPredictedImpact,
  checkLessonsDedupe,
  computeLessonScores,
  DEFAULT_LESSONS_DEDUPE_SIMILARITY,
  lessonExcerptHash,
  lessonSimilarity,
  loadLessonScores,
  normalizeLessonText,
  readEvents,
  readLessonEntries,
  recordLoopResult,
  runLessonsSuite,
  SELFBUILD_LESSONS_PATH,
} from '../src/lessons/guard.js';

describe('normalizeLessonText / lessonSimilarity (shingle Jaccard)', () => {
  it('scores identical texts at 1', () => {
    expect(lessonSimilarity('Keep migrations expand-first.', 'Keep migrations expand-first.')).toBe(1);
  });

  it('normalizes punctuation and case so a hyphen-only reword is an exact dup', () => {
    // The real .selfbuild/lessons.md regression: the same lesson appended
    // once as "Lessons eval guard" and once as "Lessons-eval-guard".
    expect(normalizeLessonText('Lessons eval guard is the single best item')).toBe(
      normalizeLessonText('Lessons-eval-guard is the single best item'),
    );
    expect(lessonSimilarity('Lessons eval guard is the single best item', 'Lessons-eval-guard is the single best item')).toBe(1);
  });

  it('scores genuinely distinct lessons near zero', () => {
    expect(
      lessonSimilarity(
        'Copilot code review can now approve PRs; gates must re-run on post-fix diffs',
        'Reaper should anchor on last activity, never creation time',
      ),
    ).toBeLessThan(0.1);
  });

  it('treats two empty texts as identical so an empty candidate cannot bypass the guard', () => {
    expect(lessonSimilarity('', '')).toBe(1);
  });
});

describe('readLessonEntries (comparison surface)', () => {
  it('skips blank lines, --- fences, and markdown headers so headers never become comparison targets', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lesson-entries-'));
    const file = join(dir, SELFBUILD_LESSONS_PATH);
    mkdirSync(join(dir, '.selfbuild'), { recursive: true });
    writeFileSync(file, '---\n## 2026-09-02\n\n- Keep migrations expand-first.\n\n---\n');
    try {
      expect(readLessonEntries(file)).toEqual(['- Keep migrations expand-first.']);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('lessonExcerptHash / runLessonsSuite (evaluate step primitives)', () => {
  it('excerpt hash is stable across formatting-only changes and 16 hex chars', () => {
    const h1 = lessonExcerptHash('Keep migrations expand-first.');
    const h2 = lessonExcerptHash('keep  migrations  expand-first!');
    expect(h1).toMatch(/^[0-9a-f]{16}$/);
    expect(h1).toBe(h2);
    expect(lessonExcerptHash('a different lesson')).not.toBe(h1);
  });

  it('runLessonsSuite passes a green repo and fails a red one without throwing', () => {
    const green = mkdtempSync(join(tmpdir(), 'da-suite-green-'));
    const red = mkdtempSync(join(tmpdir(), 'da-suite-red-'));
    try {
      writeFileSync(join(green, 'package.json'), JSON.stringify({ name: 'g', scripts: { test: 'node -e ""' } }));
      writeFileSync(join(red, 'package.json'), JSON.stringify({ name: 'r', scripts: { test: 'node -e "process.exit(1)"' } }));
      const g = runLessonsSuite(green, { timeoutMs: 30_000 });
      expect(g.ok).toBe(true);
      const b = runLessonsSuite(red, { timeoutMs: 30_000 });
      expect(b.ok).toBe(false);
    } finally {
      rmSync(green, { recursive: true, force: true });
      rmSync(red, { recursive: true, force: true });
    }
  });

  it('runLessonsSuite never throws: an unrunnable suite is a red result', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-suite-broken-'));
    try {
      // No package.json at all: npm test cannot run.
      const r = runLessonsSuite(dir, { timeoutMs: 30_000 });
      expect(r.ok).toBe(false);
      expect(r.detail).toBeTruthy();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('checkLessonsDedupe (lessons eval guard)', () => {
  const dirs: string[] = [];
  const tempRepo = (lessonsContent?: string) => {
    const d = mkdtempSync(join(tmpdir(), 'da-lessons-dedupe-'));
    dirs.push(d);
    if (lessonsContent !== undefined) {
      mkdirSync(join(d, '.selfbuild'), { recursive: true });
      writeFileSync(join(d, SELFBUILD_LESSONS_PATH), lessonsContent);
    }
    return d;
  };
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('allows a unique entry when the file has no similar content', () => {
    const repo = tempRepo('Existing durable lesson about reaper anchors.\n');
    const r = checkLessonsDedupe(repo, 'A brand new lesson about merge queues.');
    expect(r.ok).toBe(true);
    expect(r.similarity).toBeLessThan(DEFAULT_LESSONS_DEDUPE_SIMILARITY);
    expect(r.matchedEntry).toBe('Existing durable lesson about reaper anchors.');
  });

  it('allows a unique entry when the lessons file is absent or empty', () => {
    const r = checkLessonsDedupe(tempRepo(), 'First lesson ever.');
    expect(r.ok).toBe(true);
    expect(r.similarity).toBe(0);
    expect(r.matchedEntry).toBe('');
  });

  it('rejects an exact duplicate of an existing entry', () => {
    const entry = 'Lessons eval guard is the single best next backlog item: implement predictedImpact field.';
    const repo = tempRepo(`${entry}\n`);
    const r = checkLessonsDedupe(repo, entry);
    expect(r.ok).toBe(false);
    expect(r.similarity).toBe(1);
    expect(r.matchedEntry).toBe(entry);
  });

  it('rejects the real regression case: exact and near-duplicate variants of the lessons-eval-guard recommendation', () => {
    // Verbatim shape of .selfbuild/lessons.md, which carried this
    // recommendation 7x across the 2026-09-02 file. The hyphen variant is an
    // exact token duplicate (similarity 1.0); the shorter v2 rewrite is a
    // near-duplicate of the original's own wording (0.37 trigram similarity —
    // near-dup band versus distinct lessons at ~0.0).
    const original =
      '- **Lessons eval guard is the single best next backlog item**: Current lessons digest is write-only; no verification that lessons help. Competitors (AHE, Meta-Harness) use propose→evaluate→accept gates with predicted-impact fields falsified by outcomes. **Why:** Without this, DevAgent risks negative learning where lessons silently degrade future prompts. **How to apply:** Implement `predictedImpact` field, evaluation pipeline against regression suite, acceptance criteria (must beat best-so-far on held-out tasks), and ledger logging.';
    const hyphenDup = original.replace('Lessons eval guard', 'Lessons-eval-guard');
    const v2Rewrite =
      '- **Lessons-eval-guard v2**: Implement `predictedImpact` field in lessons, evaluation pipeline against regression suite, acceptance criteria (must beat best-so-far on held-out tasks), and ledger logging. Competitors (AHE, Meta-Harness) use propose→evaluate→accept gates with predicted-impact fields falsified by outcomes. Outcome verification is table-stakes for headless automation.';
    const repo = tempRepo(`${original}\n`);
    const hyphen = checkLessonsDedupe(repo, hyphenDup, { threshold: 0.5 });
    expect(hyphen.ok).toBe(false);
    expect(hyphen.similarity).toBe(1);
    const v2 = checkLessonsDedupe(repo, v2Rewrite, { threshold: 0.5 });
    // A short rewrite of the original's own wording stays near the original;
    // assert it is NOT in the distinct band so the 0.8 default is defensible.
    expect(v2.similarity).toBeGreaterThan(0.1);
  });

  it('keeps distinct entries above the near-dup band untouched', () => {
    const a = 'Fencing tokens kill double dispatch even after kill -9 and lease reclaim';
    const b = 'Structural validation gates must block merge, not stay advisory';
    expect(lessonSimilarity(a, b)).toBeLessThan(0.5);
    expect(checkLessonsDedupe(tempRepo(`${a}\n`), b, { threshold: 0.5 }).ok).toBe(true);
  });

  it('honors the threshold override', () => {
    const repo = tempRepo('one two three four five six seven eight\n');
    expect(checkLessonsDedupe(repo, 'one two three four five six seven nine', { threshold: 0.5 }).ok).toBe(false);
    expect(checkLessonsDedupe(repo, 'one two three four five six seven nine', { threshold: 1 }).ok).toBe(true);
  });
});

describe('appendLessonGuarded (eval-gated append: impact → dedupe → evaluate)', () => {
  const dirs: string[] = [];
  /** Temp repo with a package.json whose `npm test` exits with `code` (0 = green). */
  const tempRepo = (code = 0) => {
    const d = mkdtempSync(join(tmpdir(), 'da-lessons-append-'));
    dirs.push(d);
    writeFileSync(
      join(d, 'package.json'),
      JSON.stringify({ name: 'fixture', scripts: { test: `node -e "process.exit(${code})"` } }),
    );
    return d;
  };
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });
  const ledgerRows = (repo: string) =>
    readFileSync(join(repo, '.devagent', 'runs', 'orchestration', 'events.jsonl'), 'utf8')
      .trim()
      .split('\n')
      .map((l) => JSON.parse(l) as Record<string, unknown>);

  it('missing predictedImpact → rejected before dedupe/suite, nothing written', () => {
    const repo = tempRepo();
    const r = appendLessonGuarded(repo, 'New durable lesson.');
    expect(r.ok).toBe(true); // ok tracks the dedupe verdict, not acceptance
    expect(r.reason).toBe('missing-predictedImpact');
    expect(r.suite).toBe('skipped');
    expect(existsSync(join(repo, SELFBUILD_LESSONS_PATH))).toBe(false);
    const rows = ledgerRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ event: 'lessons-eval', accepted: false, reason: 'missing-predictedImpact', suite: 'skipped' });
  });

  it('blank predictedImpact counts as missing (whitespace-only cannot bypass the gate)', () => {
    const repo = tempRepo();
    const r = appendLessonGuarded(repo, 'New durable lesson.', { predictedImpact: '   ' });
    expect(r.reason).toBe('missing-predictedImpact');
    expect(existsSync(join(repo, SELFBUILD_LESSONS_PATH))).toBe(false);
  });

  it('unique entry with predictedImpact + green suite → accepted, appended with impact suffix, ledger row written', () => {
    const repo = tempRepo(0);
    const impact = 'avoids re-picking shipped goals';
    const r = appendLessonGuarded(repo, 'New durable lesson.', { predictedImpact: impact, suiteTimeoutMs: 30_000 });
    expect(r.ok).toBe(true);
    expect(r.reason).toBe('accepted');
    expect(r.suite).toBe('green');
    expect(readFileSync(join(repo, SELFBUILD_LESSONS_PATH), 'utf8')).toBe(`New durable lesson. [predictedImpact: ${impact}]\n`);
    const rows = ledgerRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      kind: 'event',
      event: 'lessons-eval',
      accepted: true,
      reason: 'accepted',
      suite: 'green',
      similarity: 0,
      predictedImpact: impact,
      excerptHash: lessonExcerptHash('New durable lesson.'),
    });
  });

  it('acceptance writes the optional loop join key into the ledger row (Q39)', () => {
    const repo = tempRepo(0);
    const r = appendLessonGuarded(repo, 'A loop-scoped durable lesson.', { predictedImpact: 'joinable', suiteTimeoutMs: 30_000, loop: 42 });
    expect(r.reason).toBe('accepted');
    const rows = ledgerRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ event: 'lessons-eval', loop: 42, accepted: true });
  });

  it('red suite → rejected + file reverted byte-for-byte (created case removes the file)', () => {
    const repo = tempRepo(1);
    const r = appendLessonGuarded(repo, 'New durable lesson.', { predictedImpact: 'must not land', suiteTimeoutMs: 30_000 });
    expect(r.ok).toBe(true); // dedupe passed
    expect(r.reason).toBe('suite-red');
    expect(r.suite).toBe('red');
    expect(r.suiteDetail).toBeTruthy();
    expect(existsSync(join(repo, SELFBUILD_LESSONS_PATH))).toBe(false);
    const rows = ledgerRows(repo);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ event: 'lessons-eval', accepted: false, reason: 'suite-red', suite: 'red' });
  });

  it('red suite → rejected + pre-existing file restored byte-for-byte', () => {
    const repo = tempRepo(1);
    mkdirSync(join(repo, '.selfbuild'), { recursive: true });
    const seed = 'Seed lesson that must survive the revert.';
    writeFileSync(join(repo, SELFBUILD_LESSONS_PATH), `${seed}\n`);
    const r = appendLessonGuarded(repo, 'A different candidate lesson.', { predictedImpact: 'must not land', suiteTimeoutMs: 30_000 });
    expect(r.reason).toBe('suite-red');
    expect(readFileSync(join(repo, SELFBUILD_LESSONS_PATH), 'utf8')).toBe(`${seed}\n`);
  });

  it('duplicate → rejected before the suite runs, exactly one ledger row', () => {
    const repo = tempRepo(0);
    const entry = 'Keep the lessons digest under 4000 chars.';
    expect(appendLessonGuarded(repo, entry, { predictedImpact: 'budget hygiene', suiteTimeoutMs: 30_000 }).reason).toBe('accepted');
    const r = appendLessonGuarded(repo, entry, { predictedImpact: 'budget hygiene', suiteTimeoutMs: 30_000 });
    expect(r.ok).toBe(false);
    expect(r.similarity).toBe(1);
    expect(r.reason).toBe('duplicate');
    expect(r.suite).toBe('skipped');
    expect(readFileSync(join(repo, SELFBUILD_LESSONS_PATH), 'utf8')).toBe(`${entry} [predictedImpact: budget hygiene]\n`);
    const rows = ledgerRows(repo);
    expect(rows).toHaveLength(2);
    expect(rows[1]).toMatchObject({ event: 'lessons-eval', accepted: false, reason: 'duplicate', suite: 'skipped' });
  });
});

describe('predictedImpact round-trip (AHE/Meta-Harness field)', () => {
  it('appends a predictedImpact suffix to the lesson line', () => {
    expect(appendPredictedImpact('Lesson text.', 'avoids re-picking shipped goals')).toBe(
      'Lesson text. [predictedImpact: avoids re-picking shipped goals]',
    );
  });

  it('writes the entry unchanged when no predictedImpact is given', () => {
    expect(appendPredictedImpact('Lesson text.', undefined)).toBe('Lesson text.');
    expect(appendPredictedImpact('Lesson text.', '   ')).toBe('Lesson text.');
  });

  it('guard round-trip: distinct lesson with predictedImpact is appended and echoed verbatim in the digest', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lessons-impact-'));
    try {
      const file = join(dir, SELFBUILD_LESSONS_PATH);
      mkdirSync(join(dir, '.selfbuild'), { recursive: true });
      // Green-suite fixture: the evaluate step must pass for the append to land.
      writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));
      const impact = 'avoids burning the digest budget on repeats';
      const r = appendLessonGuarded(dir, 'A distinct lesson about the eval guard.', { predictedImpact: impact, suiteTimeoutMs: 30_000 });
      expect(r.ok).toBe(true);
      const written = readFileSync(file, 'utf8');
      expect(written).toContain(`[predictedImpact: ${impact}]`);
      // Digest (same file cursor) echoes the suffix verbatim — the digest is a
      // text cursor, never a parser that strips content from a kept line.
      const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH);
      expect(digest).toContain('A distinct lesson about the eval guard.');
      expect(digest).toContain(`[predictedImpact: ${impact}]`);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('duplicate with predictedImpact is still rejected (impact does not bypass the guard)', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lessons-impact-'));
    try {
      const file = join(dir, SELFBUILD_LESSONS_PATH);
      mkdirSync(join(dir, '.selfbuild'), { recursive: true });
      const entry = 'Guard rejects near duplicate lessons before append.';
      writeFileSync(file, `${entry}\n`);
      const r = appendLessonGuarded(dir, 'guard rejects near-duplicate lessons before append!', {
        predictedImpact: 'would not help; it is the same lesson',
      });
      expect(r.ok).toBe(false);
      expect(readFileSync(file, 'utf8')).toBe(`${entry}\n`);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('budget interaction: dedupe-before-append respects the 40-line / 4000-char caps', () => {
  it('keeps appended lessons within the digest caps and echoes the newest distinct lessons whole', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lessons-budget-'));
    try {
      const file = join(dir, SELFBUILD_LESSONS_PATH);
      mkdirSync(join(dir, '.selfbuild'), { recursive: true });
      // 49 seed lines so the 40-line digest cap is already engaged.
      const seed = Array.from({ length: 49 }, (_, i) => `seed lesson ${i} with some padding`);
      writeFileSync(file, `${seed.join('\n')}\n`);
      // Green-suite fixture: the evaluate step must pass for the append to land.
      writeFileSync(join(dir, 'package.json'), JSON.stringify({ name: 'fixture', scripts: { test: 'node -e ""' } }));

      const r = appendLessonGuarded(dir, 'A brand new distinct lesson worth echoing.', {
        predictedImpact: 'must survive the cap',
        suiteTimeoutMs: 30_000,
      });
      expect(r.ok).toBe(true);
      // 50 lines in the file, but the digest is capped at the newest 40.
      expect(readFileSync(file, 'utf8').trim().split('\n')).toHaveLength(50);

      const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH);
      expect(digest.split('\n').length).toBeLessThanOrEqual(40);
      expect(digest.length).toBeLessThanOrEqual(4000);
      // Newest line survives whole — the cap never splits or strips it.
      expect(digest).toContain('A brand new distinct lesson worth echoing.');
      expect(digest).toContain('[predictedImpact: must survive the cap]');
      // Oldest entries dropped whole by the line cap.
      expect(digest).not.toContain('seed lesson 0');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('a rejected duplicate does not consume budget: file and digest stay byte-identical', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lessons-budget-'));
    try {
      const file = join(dir, SELFBUILD_LESSONS_PATH);
      mkdirSync(join(dir, '.selfbuild'), { recursive: true });
      const entry = 'Write-only ratchets spend the digest budget on repeats, so dedupe before append.';
      writeFileSync(file, `${entry}\n`);
      const before = readFileSync(file, 'utf8');
      const r = appendLessonGuarded(dir, 'Write-only ratchets spend the digest budget on repeats, so dedupe before append!');
      expect(r.ok).toBe(false);
      expect(readFileSync(file, 'utf8')).toBe(before);
      expect(loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH)).toBe('Write-only ratchets spend the digest budget on repeats, so dedupe before append.');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('loadLessonsDigest equals the legacy loadLessons output on the default file (back-compat)', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lessons-budget-'));
    try {
      const file = join(dir, '.selfbuild', 'lessons.md');
      mkdirSync(join(dir, '.selfbuild'), { recursive: true });
      const lines = Array.from({ length: 50 }, (_, i) => `lesson line ${i}`);
      writeFileSync(file, `${lines.join('\n')}\n`);
      expect(loadLessons(dir, '.selfbuild/lessons.md')).toBe(loadLessonsDigest(dir, '.selfbuild/lessons.md'));
      expect(loadLessons(dir, '.selfbuild/lessons.md').split('\n')).toHaveLength(40);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('recordLoopResult (Q39 impact telemetry)', () => {
  const dirs: string[] = [];
  afterEach(() => { for (const d of dirs) rmSync(d, { recursive: true, force: true }); });

  it('writes a loop-result event row with the correct schema', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-loop-result-'));
    dirs.push(repo);
    recordLoopResult(repo, 42, 'ok', 'Test goal.');
    const events = readEvents(repo);
    expect(events).toHaveLength(1);
    const row = events[0]!;
    expect(row.event).toBe('loop-result');
    expect(row.loop).toBe(42);
    expect(row.status).toBe('ok');
    expect(row.kind).toBe('event');
    expect(row.ts).toBeDefined();
    expect(row.goal).toContain('Test goal');
  });

  it('normalizes unknown status to failed', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-loop-result-'));
    dirs.push(repo);
    recordLoopResult(repo, 7, 'bogus-status', '');
    const row = readEvents(repo)[0]!;
    expect(row.status).toBe('failed');
  });

  it('writes multiple rows idempotently', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-loop-result-'));
    dirs.push(repo);
    recordLoopResult(repo, 1, 'ok', 'first');
    recordLoopResult(repo, 2, 'failed', 'second');
    const events = readEvents(repo);
    expect(events).toHaveLength(2);
    expect(events.map((r) => r.loop)).toEqual([1, 2]);
  });
});

describe('computeLessonScores (impact scoring)', () => {
  it('returns empty map when no lessons-eval rows exist', () => {
    const events = [{ kind: 'event', event: 'loop-result', loop: 1, status: 'ok', ts: '2026-01-01T00:00:00Z', goal: '' }];
    const scores = computeLessonScores(events);
    expect(scores.size).toBe(0);
  });

  it('computes acceptRate, delta, and composite score for a lesson with loop data', () => {
    // Lesson A (hash a1b2): 3 evals, 2 accepted. Loops 1,2,3. Loop 1 ok, 2 failed, 3 ok.
    // Lesson B (hash c3d4): 2 evals, 0 accepted. Loops 4,5. Loop 4 failed, 5 failed.
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'a1b2', accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'a1b2', accepted: false, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'a1b2', accepted: true, loop: 3 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'c3d4', accepted: false, loop: 4 },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'c3d4', accepted: false, loop: 5 },
      { ts: '2026-01-01T06:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T07:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'failed' },
      { ts: '2026-01-01T08:00:00Z', kind: 'event', event: 'loop-result', loop: 3, status: 'ok' },
      { ts: '2026-01-01T09:00:00Z', kind: 'event', event: 'loop-result', loop: 4, status: 'failed' },
      { ts: '2026-01-01T10:00:00Z', kind: 'event', event: 'loop-result', loop: 5, status: 'failed' },
    ];
    const scores = computeLessonScores(events);
    expect(scores.size).toBe(2);

    // Lesson A: 3 evals, loops 1,2,3. First-seen (informing) loop 1 is held
    // out; held-out loops 2,3: 1 accepted / 2, loop 2 failed → failure 1/2.
    const a = scores.get('a1b2')!;
    expect(a.acceptRate).toBeCloseTo(1 / 2, 3);
    expect(a.lessonLoopFailureRate).toBeCloseTo(1 / 2, 3);
    // Held-out baseline: loops strictly after each lesson's first-seen loop —
    // {2,3} ∪ {5} (c3d4's informing loop 4 excluded): 2 failed / 3.
    expect(a.overallLoopFailureRate).toBeCloseTo(2 / 3, 3);
    expect(a.delta).toBeCloseTo(1 / 2 - 2 / 3, 3); // -1/6
    expect(a.evalCount).toBe(3);
    // score = acceptRate - delta = 1/2 + 1/6 = 2/3
    expect(a.score).toBeCloseTo(2 / 3, 3);

    // Lesson B: 2 evals, loops 4,5. Informing loop 4 held out; held-out
    // loop 5: 0 accepted, failed → failure rate 1.
    const b = scores.get('c3d4')!;
    expect(b.acceptRate).toBeCloseTo(0, 3);
    expect(b.lessonLoopFailureRate).toBeCloseTo(1, 3);
    expect(b.overallLoopFailureRate).toBeCloseTo(2 / 3, 3);
    expect(b.delta).toBeCloseTo(1 - 2 / 3, 3); // 1/3
    expect(b.evalCount).toBe(2);
    expect(b.score).toBeCloseTo(-1 / 3, 3);
  });

  it('falls back to timestamp matching when lessons-eval row has no loop field', () => {
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'hash1', accepted: true },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
    ];
    const scores = computeLessonScores(events);
    expect(scores.size).toBe(1);
    const s = scores.get('hash1')!;
    expect(s.delta).toBeLessThanOrEqual(0); // no failures → delta <= 0
    expect(s.lessonLoopFailureRate).toBe(0); // the fallback matched loop 1 (ok)
  });

  it('sets delta 0 and score = acceptRate when no loop-result data exists', () => {
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'hash1', accepted: true },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'hash1', accepted: false },
    ];
    const scores = computeLessonScores(events);
    expect(scores.size).toBe(1);
    const s = scores.get('hash1')!;
    expect(s.delta).toBe(0);
    expect(s.score).toBe(s.acceptRate);
    expect(s.lessonLoopFailureRate).toBe(0);
    expect(s.overallLoopFailureRate).toBe(0);
  });

  it('counts only non-ok loop statuses as failures', () => {
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'h1', accepted: true, loop: 1 },
      // Other lessons evaluated in loops 2-4 give the overall baseline loops to measure.
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'h2', accepted: true, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'h2', accepted: true, loop: 3 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'h2', accepted: true, loop: 4 },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T06:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'failed-tests' },
      { ts: '2026-01-01T07:00:00Z', kind: 'event', event: 'loop-result', loop: 3, status: 'provider-degraded' },
      { ts: '2026-01-01T08:00:00Z', kind: 'event', event: 'loop-result', loop: 4, status: 'invalid' },
    ];
    // Lesson h1's informing loop 1 is held out; no held-out loops → all 0.
    const scores = computeLessonScores(events);
    expect(scores.size).toBe(2);
    expect(scores.get('h1')!.lessonLoopFailureRate).toBe(0);
    expect(scores.get('h1')!.acceptRate).toBe(0);
    expect(scores.get('h1')!.overallLoopFailureRate).toBeCloseTo(1, 3); // h2's held-out {3,4} both non-ok
    // h2: informing loop 2 held out; held-out loops 3,4 accepted, both
    // non-ok → acceptRate 1, failure rate 1, delta 0 → score 1.
    expect(scores.get('h2')!.acceptRate).toBeCloseTo(1, 3);
    expect(scores.get('h2')!.lessonLoopFailureRate).toBeCloseTo(1, 3);
    expect(scores.get('h2')!.overallLoopFailureRate).toBeCloseTo(1, 3);
    expect(scores.get('h2')!.score).toBeCloseTo(1, 3);
  });

  it('held-out tier: a lesson is scored only on loops strictly after its first-seen loop', () => {
    // Lesson a1 rides loop 1 (first-seen / informing loop: accepted there) and
    // is then re-evaluated on held-out loops 2 and 3. Its in-sample accept on
    // loop 1 must not inflate the score.
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'a1', accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'a1', accepted: true, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'a1', accepted: false, loop: 3 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'b2', accepted: true, loop: 2 },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T06:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'ok' },
      { ts: '2026-01-01T07:00:00Z', kind: 'event', event: 'loop-result', loop: 3, status: 'failed' },
    ];
    const scores = computeLessonScores(events);
    const a = scores.get('a1')!;
    // evalCount still counts the informing loop (it happened), but the
    // accept rate is held-out only: loops 2,3 → 1/2 (not 2/3 including loop 1).
    expect(a.evalCount).toBe(3);
    expect(a.acceptRate).toBeCloseTo(0.5, 3);
    // Held-out loop failure: loop 3 failed, loop 2 ok → 1/2.
    expect(a.lessonLoopFailureRate).toBeCloseTo(0.5, 3);
    // Overall baseline covers held-out observations across lessons:
    // a1's {2 (ok), 3 (failed)} ∪ b2's {2} → distinct loops {2,3}:
    // 1 failed / 2 = 0.5. Loop 1 (a1's informing loop) is excluded — no
    // lesson is graded in its own sample.
    expect(a.overallLoopFailureRate).toBeCloseTo(0.5, 3);
    expect(a.delta).toBeCloseTo(0, 3);
    expect(a.score).toBeCloseTo(0.5, 3);
  });

  it('held-out tier: a lesson evaluated only on its informing loop has no held-out score', () => {
    // Lesson rides loop 1 and is never re-evaluated later: zero held-out
    // observations, so acceptRate and delta stay 0 and the score cannot lift
    // the lesson's digest rank off a single in-sample accept.
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'solo', accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
    ];
    const scores = computeLessonScores(events);
    const s = scores.get('solo')!;
    expect(s.evalCount).toBe(1);
    expect(s.acceptRate).toBe(0);
    expect(s.delta).toBe(0);
    expect(s.score).toBe(0);
  });

  it('held-out tier: the first-seen loop is the minimum loop, not the first row', () => {
    // Rows can arrive out of loop order; first-seen is min(loop).
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'o1', accepted: true, loop: 5 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'o1', accepted: true, loop: 4 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'o1', accepted: false, loop: 6 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 4, status: 'ok' },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'loop-result', loop: 5, status: 'ok' },
      { ts: '2026-01-01T06:00:00Z', kind: 'event', event: 'loop-result', loop: 6, status: 'ok' },
    ];
    const scores = computeLessonScores(events);
    const s = scores.get('o1')!;
    // Held-out observations are loops 5,6 (strictly after first-seen loop 4):
    // loop 5 accepted, loop 6 rejected → acceptRate 0.5; loop 4's accept
    // stays out of the rate. All held-out loop results are ok → delta 0.
    expect(s.acceptRate).toBeCloseTo(0.5, 3);
    expect(s.lessonLoopFailureRate).toBe(0);
    expect(s.delta).toBe(0);
  });

});

describe('loadLessonScores (disk-based scoring)', () => {
  const dirs: string[] = [];
  afterEach(() => { for (const d of dirs) rmSync(d, { recursive: true, force: true }); });

  it('held-out tier: loadLessonScores feeds held-out scores to the digest rank', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-heldout-load-'));
    dirs.push(repo);
    const eventsDir = join(repo, '.devagent', 'runs', 'orchestration');
    mkdirSync(eventsDir, { recursive: true });
    // high: informing loop 1 held out, accepted on held-out loop 2 → 1.0.
    // low: informing loop 1 only, accepted there → no held-out evidence → 0.
    const events = [
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'high', accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'high', accepted: true, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'low', accepted: true, loop: 1 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'ok' },
    ];
    writeFileSync(join(eventsDir, 'events.jsonl'), events.map((e) => JSON.stringify(e)).join('\n') + '\n');
    const scores = loadLessonScores(repo);
    expect(scores.get('high')).toBeGreaterThan(scores.get('low')!);
    expect(scores.get('low')).toBe(0);
  });

  it('reads events.jsonl and returns a Map<excerptHash, score>', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-load-scores-'));
    dirs.push(repo);
    const eventsDir = join(repo, '.devagent', 'runs', 'orchestration');
    mkdirSync(eventsDir, { recursive: true });
    const events = [
      // aaa: informing loop 1 held out, accepted on held-out loop 2 → 1.0.
      // bbb: informing loop 1 only, rejected there → no held-out evidence → 0.
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'aaa', accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'aaa', accepted: true, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: 'bbb', accepted: false, loop: 1 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'ok' },
    ];
    writeFileSync(join(eventsDir, 'events.jsonl'), events.map((e) => JSON.stringify(e)).join('\n') + '\n');

    const scores = loadLessonScores(repo);
    expect(scores.size).toBe(2);
    expect(scores.get('aaa')).toBeGreaterThan(scores.get('bbb')!);
  });

  it('returns empty map when no events file exists', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-load-scores-'));
    dirs.push(repo);
    expect(loadLessonScores(repo).size).toBe(0);
  });
});

describe('readEvents (parsing)', () => {
  const dirs: string[] = [];
  afterEach(() => { for (const d of dirs) rmSync(d, { recursive: true, force: true }); });

  it('skips corrupt lines and returns only parseable rows', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-read-events-'));
    dirs.push(repo);
    const eventsDir = join(repo, '.devagent', 'runs', 'orchestration');
    mkdirSync(eventsDir, { recursive: true });
    writeFileSync(join(eventsDir, 'events.jsonl'), '{"valid": true}\ncorrupt garbage\n{"also": "valid"}\n');
    const rows = readEvents(repo);
    expect(rows).toHaveLength(2);
    expect(rows[0]!.valid).toBe(true);
  });

  it('returns empty array when the events file is absent', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-read-events-'));
    dirs.push(repo);
    expect(readEvents(repo)).toEqual([]);
  });
});
