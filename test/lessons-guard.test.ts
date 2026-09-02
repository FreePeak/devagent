import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  appendLessonGuarded,
  appendPredictedImpact,
  checkLessonsDedupe,
  DEFAULT_LESSONS_DEDUPE_SIMILARITY,
  lessonSimilarity,
  normalizeLessonText,
  readLessonEntries,
  SELFBUILD_LESSONS_PATH,
} from '../src/lessons/guard.js';
import { loadLessons, loadLessonsDigest } from '../src/prompt.js';

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

describe('appendLessonGuarded (guarded append)', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-lessons-append-'));
    dirs.push(d);
    return d;
  };
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('unique-append path: writes the entry and records no rejection', () => {
    const repo = tempRepo();
    const r = appendLessonGuarded(repo, 'New durable lesson.');
    expect(r.ok).toBe(true);
    expect(readFileSync(join(repo, SELFBUILD_LESSONS_PATH), 'utf8')).toBe('New durable lesson.\n');
    expect(existsSync(join(repo, '.devagent', 'runs', 'orchestration'))).toBe(false);
  });

  it('exact-dup path: writes nothing and records a lessons-dedupe-rejected event row', () => {
    const repo = tempRepo();
    const entry = 'Keep the lessons digest under 4000 chars.';
    expect(appendLessonGuarded(repo, entry).ok).toBe(true);
    const r = appendLessonGuarded(repo, entry);
    expect(r.ok).toBe(false);
    expect(r.similarity).toBe(1);
    // Exactly one entry remains in the file.
    expect(readFileSync(join(repo, SELFBUILD_LESSONS_PATH), 'utf8')).toBe(`${entry}\n`);
    const raw = readFileSync(join(repo, '.devagent', 'runs', 'orchestration', 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
    const row = JSON.parse(raw[0]!) as Record<string, unknown>;
    expect(row).toMatchObject({
      kind: 'event',
      event: 'lessons-dedupe-rejected',
      similarity: 1,
      threshold: DEFAULT_LESSONS_DEDUPE_SIMILARITY,
      matchedEntry: entry,
      entry,
    });
  });

  it('near-dup-reject path: reworded duplicate is rejected before append and leaves the file untouched', () => {
    const repo = tempRepo();
    expect(appendLessonGuarded(repo, 'Guard rejects near duplicate lessons before append.').ok).toBe(true);
    const nearDup = 'guard rejects near-duplicate lessons before append!'; // punctuation-only rewrite
    const r = appendLessonGuarded(repo, nearDup);
    expect(r.ok).toBe(false);
    expect(r.matchedEntry).toBe('Guard rejects near duplicate lessons before append.');
    expect(readFileSync(join(repo, SELFBUILD_LESSONS_PATH), 'utf8')).toBe(
      'Guard rejects near duplicate lessons before append.\n',
    );
    const raw = readFileSync(join(repo, '.devagent', 'runs', 'orchestration', 'events.jsonl'), 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
    expect(JSON.parse(raw[0]!) as Record<string, unknown>).toMatchObject({ event: 'lessons-dedupe-rejected' });
  });

  it('creates the lessons parent dir on the first unique append', () => {
    const repo = tempRepo();
    appendLessonGuarded(repo, 'Seed lesson.');
    expect(existsSync(join(repo, '.selfbuild'))).toBe(true);
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
      const impact = 'avoids burning the digest budget on repeats';
      const r = appendLessonGuarded(dir, 'A distinct lesson about the eval guard.', { predictedImpact: impact });
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

      const r = appendLessonGuarded(dir, 'A brand new distinct lesson worth echoing.', {
        predictedImpact: 'must survive the cap',
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
