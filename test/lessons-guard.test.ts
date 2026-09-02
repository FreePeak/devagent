import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { loadLessons, loadLessonsDigest } from '../src/prompt.js';
import {
  appendLessonGuarded,
  appendPredictedImpact,
  checkLessonsDedupe,
  DEFAULT_LESSONS_DEDUPE_SIMILARITY,
  lessonExcerptHash,
  lessonSimilarity,
  normalizeLessonText,
  readLessonEntries,
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
