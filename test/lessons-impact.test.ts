import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { loadLessonsDigest } from '../src/prompt.js';
import {
  fitDigestBudget,
  parseLessonsEvalRow,
  rankDigestLines,
  readLessonsEvalRows,
  readLoopOutcomes,
  scoreLessons,
  type LessonsEvalRow,
  type LoopOutcome,
} from '../src/lessons/impact.js';
import { lessonExcerptHash } from '../src/lessons/guard.js';

/** Fixture ledger row for one gated append. */
const evalRow = (over: Partial<LessonsEvalRow> & { entry: string }): LessonsEvalRow => ({
  ts: over.ts ?? '2026-09-02T00:00:00.000Z',
  excerptHash: over.excerptHash ?? lessonExcerptHash(over.entry),
  similarity: 0,
  threshold: 0.8,
  predictedImpact: 'avoids re-picking shipped goals',
  suite: 'green',
  accepted: true,
  reason: 'accepted',
  entry: over.entry,
  ...over,
});

const loop = (over: Partial<LoopOutcome> & { ts: string }): LoopOutcome => ({
  loop: 1,
  status: 'ok',
  goal: 'ship something',
  ...over,
});

describe('scoreLessons (accept rate + repeat-failure delta)', () => {
  it('accept path: green-suite acceptance with improved loop outcomes scores highest', () => {
    const rows = [evalRow({ entry: 'Keep migrations expand-first.', ts: '2026-09-02T01:00:00.000Z' })];
    const outcomes = [
      loop({ loop: 1, ts: '2026-09-01T00:00:00.000Z', status: 'failed' }),
      loop({ loop: 2, ts: '2026-09-02T02:00:00.000Z', status: 'ok' }),
      loop({ loop: 3, ts: '2026-09-02T03:00:00.000Z', status: 'merged' }),
    ];
    const scores = scoreLessons(rows, outcomes);
    const s = scores.get(rows[0]!.excerptHash)!;
    expect(s.totalAppends).toBe(1);
    expect(s.acceptedAppends).toBe(1);
    expect(s.acceptRate).toBe(1);
    expect(s.beforeFailRate).toBe(1); // one loop before acceptance, failed
    expect(s.afterFailRate).toBe(0); // two loops after, both productive
    expect(s.repeatFailureDelta).toBe(1);
    expect(s.score).toBe(2);
  });

  it('suite-red revert path: rejected append has acceptRate 0 and no delta (no acceptance to anchor on)', () => {
    const rows = [evalRow({ entry: 'A lesson that regressed the suite.', ts: '2026-09-02T01:00:00.000Z', accepted: false, suite: 'red', reason: 'suite-red' })];
    const outcomes = [
      loop({ loop: 1, ts: '2026-09-01T00:00:00.000Z', status: 'failed' }),
      loop({ loop: 2, ts: '2026-09-02T02:00:00.000Z', status: 'ok' }),
    ];
    const s = scoreLessons(rows, outcomes).get(rows[0]!.excerptHash)!;
    expect(s.acceptRate).toBe(0);
    expect(s.firstAcceptedTs).toBeUndefined();
    expect(s.beforeFailRate).toBe(0);
    expect(s.afterFailRate).toBe(0);
    expect(s.repeatFailureDelta).toBe(0);
    expect(s.score).toBe(0);
  });

  it('duplicate-skip path: later duplicate drags acceptRate down without changing the delta', () => {
    const entry = 'Keep the lessons digest under 4000 chars.';
    const hash = lessonExcerptHash(entry);
    const rows = [
      evalRow({ entry, ts: '2026-09-02T01:00:00.000Z' }),
      evalRow({ entry, ts: '2026-09-02T02:00:00.000Z', accepted: false, suite: 'skipped', reason: 'duplicate', similarity: 1 }),
    ];
    const outcomes = [
      loop({ loop: 1, ts: '2026-09-01T00:00:00.000Z', status: 'failed' }),
      loop({ loop: 2, ts: '2026-09-02T03:00:00.000Z', status: 'ok' }),
    ];
    const s = scoreLessons(rows, outcomes).get(hash)!;
    expect(s.totalAppends).toBe(2);
    expect(s.acceptedAppends).toBe(1);
    expect(s.acceptRate).toBe(0.5);
    expect(s.firstAcceptedTs).toBe('2026-09-02T01:00:00.000Z');
    expect(s.beforeFailRate).toBe(1);
    expect(s.afterFailRate).toBe(0);
    expect(s.repeatFailureDelta).toBe(1);
    expect(s.score).toBe(1.5);
  });

  it('missing-predictedImpact skip counts as a non-accepted append', () => {
    const rows = [
      evalRow({ entry: 'Lesson without impact cannot land.', accepted: false, suite: 'skipped', reason: 'missing-predictedImpact' }),
    ];
    const s = scoreLessons(rows, []).get(rows[0]!.excerptHash)!;
    expect(s.acceptRate).toBe(0);
    expect(s.score).toBe(0);
  });

  it('no loop outcomes → delta 0, score is acceptRate alone', () => {
    const rows = [evalRow({ entry: 'Fresh lesson with no loop history yet.' })];
    const s = scoreLessons(rows, []).get(rows[0]!.excerptHash)!;
    expect(s.repeatFailureDelta).toBe(0);
    expect(s.beforeFailRate).toBe(0);
    expect(s.afterFailRate).toBe(0);
    expect(s.score).toBe(1);
  });

  it('empty inputs → empty map', () => {
    expect(scoreLessons([], []).size).toBe(0);
  });

  it('scores distinct excerpts independently', () => {
    const a = evalRow({ entry: 'Lesson A.', ts: '2026-09-02T01:00:00.000Z' });
    const b = evalRow({ entry: 'Lesson B.', ts: '2026-09-02T01:00:00.000Z' });
    const outcomes = [loop({ loop: 1, ts: '2026-09-01T00:00:00.000Z', status: 'failed' })];
    const scores = scoreLessons([a, b], outcomes);
    expect(scores.get(a.excerptHash)!.score).toBe(2);
    expect(scores.get(b.excerptHash)!.score).toBe(2);
    expect(a.excerptHash).not.toBe(b.excerptHash);
  });
});

describe('rankDigestLines (score-ordered, newest-first tiebreak)', () => {
  it('orders high-score lessons before low-score ones and keeps unranked lines last', () => {
    const weak = evalRow({ entry: 'Weak lesson.' });
    const strong = evalRow({ entry: 'Strong lesson.' });
    const scores = scoreLessons([weak, strong], [loop({ loop: 1, ts: '2026-09-01T00:00:00.000Z', status: 'failed' })]);
    // Give weak a lower score: reject one of its appends.
    scores.set(weak.excerptHash, { ...scores.get(weak.excerptHash)!, totalAppends: 2, acceptedAppends: 1, acceptRate: 0.5, score: 1.5 });

    const lines = ['## 2026-09-02', weak.entry, strong.entry];
    const ranked = rankDigestLines(lines, scores);
    expect(ranked[0]).toBe(strong.entry);
    expect(ranked[1]).toBe(weak.entry);
    expect(ranked[2]).toBe('## 2026-09-02'); // no score → sinks
  });

  it('newest line wins the tiebreak when scores are equal', () => {
    const older = evalRow({ entry: 'First lesson.' });
    const newer = evalRow({ entry: 'Second lesson.' });
    const scores = scoreLessons([older, newer], []);
    const ranked = rankDigestLines([older.entry, newer.entry], scores);
    expect(ranked[0]).toBe(newer.entry);
    expect(ranked[1]).toBe(older.entry);
  });

  it('all-zero scores still tiebreak newest-first', () => {
    const ranked = rankDigestLines(['old line', 'middle line', 'new line'], new Map());
    expect(ranked).toEqual(['new line', 'middle line', 'old line']);
  });
});

describe('fitDigestBudget (4000-char cap, never split a line)', () => {
  it('drops low-priority lines whole until the block fits the budget', () => {
    const lines = Array.from({ length: 20 }, (_, i) => `line ${i} ` + 'x'.repeat(200)); // ~206 chars each
    const out = fitDigestBudget(lines, 1000);
    expect(out.length).toBeLessThanOrEqual(1000);
    expect(out.split('\n').length).toBeGreaterThan(1);
  });

  it('keeps a single oversized line whole rather than returning nothing', () => {
    const huge = 'huge_'.repeat(1000); // ~5000 chars, no trailing whitespace
    expect(fitDigestBudget([huge], 100)).toBe(huge);
  });

  it('trims to exactly the joined lines (no trailing newline)', () => {
    const out = fitDigestBudget(['a', 'b', 'c'], 100);
    expect(out).toBe('a\nb\nc');
  });
});

describe('ledger readers + parseLessonsEvalRow', () => {
  it('parseLessonsEvalRow accepts a ledger-shaped record and rejects non-eval events', () => {
    const row = parseLessonsEvalRow({
      kind: 'event',
      event: 'lessons-eval',
      ts: '2026-09-02T00:00:00.000Z',
      excerptHash: 'abc123',
      similarity: 0,
      threshold: 0.8,
      predictedImpact: 'impact',
      suite: 'green',
      accepted: true,
      reason: 'accepted',
      entry: 'Lesson.',
    });
    expect(row).toMatchObject({ excerptHash: 'abc123', accepted: true, suite: 'green' });
    expect(parseLessonsEvalRow({ event: 'taskInterrupt' })).toBeNull();
    expect(parseLessonsEvalRow({ event: 'lessons-eval', ts: 1 })).toBeNull();
  });

  it('readLessonsEvalRows / readLoopOutcomes round-trip fixture files', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-lessons-impact-'));
    try {
      mkdirSync(join(dir, '.devagent', 'runs', 'orchestration'), { recursive: true });
      mkdirSync(join(dir, '.selfbuild'), { recursive: true });
      writeFileSync(
        join(dir, '.devagent', 'runs', 'orchestration', 'events.jsonl'),
        `${JSON.stringify({ kind: 'event', event: 'lessons-eval', ts: '2026-09-02T00:00:00.000Z', excerptHash: 'abc123', similarity: 0, threshold: 0.8, predictedImpact: 'p', suite: 'green', accepted: true, reason: 'accepted', entry: 'L.' })}\nnot json\n`,
      );
      writeFileSync(join(dir, '.selfbuild', 'ledger.jsonl'), `${JSON.stringify({ loop: 3, ts: '2026-09-01T00:00:00.000Z', status: 'failed', goal: 'g' })}\n`);
      const rows = readLessonsEvalRows(dir);
      expect(rows).toHaveLength(1);
      expect(rows[0]).toMatchObject({ excerptHash: 'abc123', accepted: true });
      const outcomes = readLoopOutcomes(dir);
      expect(outcomes).toHaveLength(1);
      expect(outcomes[0]).toMatchObject({ loop: 3, status: 'failed' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('loadLessonsDigest ranking integration (fixture ledger drives order)', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-lessons-digest-rank-'));
    dirs.push(d);
    mkdirSync(join(d, '.devagent', 'runs', 'orchestration'), { recursive: true });
    mkdirSync(join(d, '.selfbuild'), { recursive: true });
    return d;
  };
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('older high-score lesson outranks a newer low-score one once the ledger has evidence', () => {
    const repo = tempRepo();
    const weak = 'Newer lesson that never helped: repeated duplicates.';
    const strong = 'Older lesson with a green accept and improved loops.';
    writeFileSync(join(repo, '.selfbuild', 'lessons.md'), `${strong}\n${weak}\n`);

    const weakHash = lessonExcerptHash(weak);
    const strongHash = lessonExcerptHash(strong);
    const row = (h: string, accepted: boolean, ts: string, suite: 'green' | 'skipped', reason: 'accepted' | 'duplicate') =>
      JSON.stringify({ kind: 'event', event: 'lessons-eval', ts, excerptHash: h, similarity: accepted ? 0 : 1, threshold: 0.8, predictedImpact: 'p', suite, accepted, reason, entry: '' });
    writeFileSync(
      join(repo, '.devagent', 'runs', 'orchestration', 'events.jsonl'),
      [
        row(weakHash, true, '2026-09-01T10:00:00.000Z', 'green', 'accepted'),
        row(weakHash, false, '2026-09-02T10:00:00.000Z', 'skipped', 'duplicate'),
        row(strongHash, true, '2026-09-01T11:00:00.000Z', 'green', 'accepted'),
      ].join('\n') + '\n',
    );
    writeFileSync(
      join(repo, '.selfbuild', 'ledger.jsonl'),
      [
        JSON.stringify({ loop: 1, ts: '2026-09-01T00:00:00.000Z', status: 'failed', goal: 'a' }),
        JSON.stringify({ loop: 2, ts: '2026-09-01T12:00:00.000Z', status: 'ok', goal: 'b' }),
        JSON.stringify({ loop: 3, ts: '2026-09-02T12:00:00.000Z', status: 'merged', goal: 'c' }),
      ].join('\n') + '\n',
    );

    const digest = loadLessonsDigest(repo, '.selfbuild/lessons.md');
    const lines = digest.split('\n');
    // strong: acceptRate 1 + delta 1 → 2.0; weak: acceptRate 0.5 + delta 0 → 0.5.
    expect(lines[0]).toBe(strong);
    expect(lines[1]).toBe(weak);
  });

  it('no ledger → legacy newest-first output preserved exactly', () => {
    const repo = tempRepo();
    const lines = Array.from({ length: 45 }, (_, i) => `lesson line ${i}`);
    writeFileSync(join(repo, '.selfbuild', 'lessons.md'), `${lines.join('\n')}\n`);
    const digest = loadLessonsDigest(repo, '.selfbuild/lessons.md');
    expect(digest.split('\n')).toHaveLength(40);
    expect(digest).toContain('lesson line 44');
    expect(digest).not.toContain('lesson line 0');
  });
});