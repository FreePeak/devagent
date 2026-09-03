import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { lessonExcerptHash, SELFBUILD_LESSONS_PATH } from '../src/lessons/guard.js';
import {
  aggregateEvalRows,
  computeLessonImpact,
  parseLessonsEvalRow,
  parseLoopLedgerRow,
  rankLinesByImpact,
  type LessonImpact,
} from '../src/lessons/impact.js';
import { loadLessonsDigest } from '../src/prompt.js';

const iso = (d: string) => new Date(d).toISOString();

/** Synthetic lessons-eval row fixture. */
const evalRow = (hash: string, ts: string, accepted: boolean, predictedImpact = 'flips a future outcome') => ({
  ts: iso(ts),
  excerptHash: hash,
  accepted,
  predictedImpact,
});

/** Synthetic loop-ledger row fixture. */
const loopRow = (ts: string, status: string) => ({ ts: iso(ts), status });

describe('parseLessonsEvalRow / parseLoopLedgerRow (fixture decoders)', () => {
  it('parses a lessons-eval events-ledger row and ignores other events', () => {
    const row = parseLessonsEvalRow(
      JSON.stringify({ ts: '2026-09-01T00:00:00.000Z', kind: 'event', event: 'lessons-eval', excerptHash: 'abc123', accepted: true, predictedImpact: 'x' }),
    );
    expect(row).toEqual({ ts: '2026-09-01T00:00:00.000Z', excerptHash: 'abc123', accepted: true, predictedImpact: 'x' });
    expect(parseLessonsEvalRow(JSON.stringify({ event: 'audit', excerptHash: 'abc' }))).toBeNull();
    expect(parseLessonsEvalRow('not json')).toBeNull();
    expect(parseLessonsEvalRow('')).toBeNull();
  });

  it('parses a loop-ledger row and rejects rows without ts/status', () => {
    expect(parseLoopLedgerRow(JSON.stringify({ loop: 61, ts: '2026-08-24T14:05:00.000Z', status: 'pr-open', goal: 'x' }))).toEqual({
      ts: '2026-08-24T14:05:00.000Z',
      status: 'pr-open',
    });
    expect(parseLoopLedgerRow(JSON.stringify({ loop: 1 }))).toBeNull();
    expect(parseLoopLedgerRow('garbage')).toBeNull();
  });
});

describe('computeLessonImpact (score computation over synthetic ledger+loop fixtures)', () => {
  it('scores an accepted lesson by the success rate of subsequent loops', () => {
    const h = 'hash-A';
    const scores = computeLessonImpact([evalRow(h, '2026-08-01T00:00:00Z', true)], [
      loopRow('2026-08-02T00:00:00Z', 'merged'),
      loopRow('2026-08-03T00:00:00Z', 'failed'),
      loopRow('2026-08-04T00:00:00Z', 'ok'),
    ]);
    const s = scores.get(h)!;
    expect(s.accepted).toBe(true);
    expect(s.loops).toBe(3);
    expect(s.successes).toBe(2);
    expect(s.measuredEffect).toBe(2 / 3);
  });

  it('counts all productive statuses as successes and all failure statuses as failures', () => {
    const h = 'hash-all-statuses';
    const s = computeLessonImpact([evalRow(h, '2026-08-01T00:00:00Z', true)], [
      loopRow('2026-08-02T00:00:00Z', 'ok'),
      loopRow('2026-08-02T01:00:00Z', 'pr-open'),
      loopRow('2026-08-02T02:00:00Z', 'merged'),
      loopRow('2026-08-02T03:00:00Z', 'pushed'),
      loopRow('2026-08-03T00:00:00Z', 'failed'),
      loopRow('2026-08-03T01:00:00Z', 'failed-tests'),
      loopRow('2026-08-03T02:00:00Z', 'invalid'),
      loopRow('2026-08-03T03:00:00Z', 'push-failed'),
    ]).get(h)!;
    expect(s.loops).toBe(8);
    expect(s.successes).toBe(4);
    expect(s.measuredEffect).toBe(0.5);
  });

  it('excludes skipped and unknown loop statuses from both numerator and denominator', () => {
    const h = 'hash-skipped';
    const s = computeLessonImpact([evalRow(h, '2026-08-01T00:00:00Z', true)], [
      loopRow('2026-08-02T00:00:00Z', 'merged'),
      loopRow('2026-08-02T01:00:00Z', 'skipped'),
      loopRow('2026-08-02T02:00:00Z', 'mystery-status'),
    ]).get(h)!;
    expect(s.loops).toBe(1);
    expect(s.successes).toBe(1);
    expect(s.measuredEffect).toBe(1);
  });

  it('zero-effect when no loop ran after the lesson was accepted', () => {
    const h = 'hash-no-loops';
    const s = computeLessonImpact([evalRow(h, '2026-08-05T00:00:00Z', true)], [
      loopRow('2026-08-01T00:00:00Z', 'merged'), // before acceptance: not subsequent
    ]).get(h)!;
    expect(s.loops).toBe(0);
    expect(s.successes).toBe(0);
    expect(s.measuredEffect).toBe(0);
  });

  it('never-accepted lessons have zero measured effect regardless of later loops', () => {
    const h = 'hash-rejected';
    const s = computeLessonImpact([evalRow(h, '2026-08-01T00:00:00Z', false)], [
      loopRow('2026-08-02T00:00:00Z', 'merged'),
      loopRow('2026-08-03T00:00:00Z', 'ok'),
    ]).get(h)!;
    expect(s.accepted).toBe(false);
    expect(s.loops).toBe(0);
    expect(s.measuredEffect).toBe(0);
  });

  it('entry point is the earliest accepted row: a later accepted attempt only counts loops after acceptance', () => {
    const h = 'hash-retry';
    // Rejected on 08-01 (red suite), accepted on 08-02. A merged loop on
    // 08-01 ran before the lesson entered the digest and must not count.
    const scores = computeLessonImpact(
      [evalRow(h, '2026-08-01T00:00:00Z', false), evalRow(h, '2026-08-02T00:00:00Z', true)],
      [
        loopRow('2026-08-01T12:00:00Z', 'merged'),
        loopRow('2026-08-03T00:00:00Z', 'failed'),
      ],
    );
    const s = scores.get(h)!;
    expect(s.accepted).toBe(true);
    expect(s.loops).toBe(1);
    expect(s.successes).toBe(0);
    expect(s.measuredEffect).toBe(0);
  });

  it('a later rejected duplicate does not reset the entry point of an accepted lesson', () => {
    const h = 'hash-dup';
    const scores = computeLessonImpact(
      [evalRow(h, '2026-08-01T00:00:00Z', true), evalRow(h, '2026-08-04T00:00:00Z', false)],
      [loopRow('2026-08-02T00:00:00Z', 'merged')],
    );
    expect(scores.get(h)!.accepted).toBe(true);
    expect(scores.get(h)!.loops).toBe(1);
    expect(scores.get(h)!.measuredEffect).toBe(1);
  });

  it('loop rows at exactly the entry-point ts are not subsequent', () => {
    const h = 'hash-boundary';
    const s = computeLessonImpact([evalRow(h, '2026-08-02T00:00:00Z', true)], [
      loopRow('2026-08-02T00:00:00Z', 'merged'),
    ]).get(h)!;
    expect(s.loops).toBe(0);
    expect(s.measuredEffect).toBe(0);
  });

  it('aggregateEvalRows keeps the earliest accepted ts across out-of-order rows', () => {
    const verdicts = aggregateEvalRows([
      evalRow('h', '2026-08-03T00:00:00Z', true, 'later-accept'),
      evalRow('h', '2026-08-01T00:00:00Z', true, 'first-accept'),
    ]);
    expect(verdicts.get('h')).toEqual({ accepted: true, ts: iso('2026-08-01T00:00:00Z'), predictedImpact: 'first-accept' });
  });
});

describe('rankLinesByImpact (digest ordering)', () => {
  const impactOf = (entries: Array<[string, Partial<LessonImpact>]>): Map<string, LessonImpact> => {
    const m = new Map<string, LessonImpact>();
    for (const [hash, over] of entries) {
      m.set(hash, {
        excerptHash: hash,
        accepted: true,
        predictedImpact: '',
        ts: iso('2026-08-01T00:00:00Z'),
        measuredEffect: 0,
        loops: 0,
        successes: 0,
        ...over,
      } as LessonImpact);
    }
    return m;
  };

  it('orders by measured effect desc and sinks zero-effect lessons to the bottom', () => {
    const high = 'High-effect lesson about merge gates.';
    const mid = 'Mid-effect lesson about fanout.';
    const zero = 'Zero-effect legacy lesson with no ledger row.';
    const impact = impactOf([
      [lessonExcerptHash(high), { measuredEffect: 1, ts: iso('2026-08-01T00:00:00Z') }],
      [lessonExcerptHash(mid), { measuredEffect: 0.5, ts: iso('2026-08-01T00:00:00Z') }],
    ]);
    // File order deliberately opposite to effect order (zero newest).
    const ranked = rankLinesByImpact([zero, mid, high], impact);
    expect(ranked).toEqual([high, mid, zero]);
  });

  it('breaks effect ties by recency: newer accepted ts first, regardless of file position', () => {
    const old = 'Older-accepted lesson with full effect.';
    const fresh = 'Freshly-accepted lesson with full effect.';
    const impact = impactOf([
      // File order: fresh lesson first (older position), old lesson second.
      [lessonExcerptHash(fresh), { measuredEffect: 1, ts: iso('2026-08-10T00:00:00Z') }],
      [lessonExcerptHash(old), { measuredEffect: 1, ts: iso('2026-08-01T00:00:00Z') }],
    ]);
    const ranked = rankLinesByImpact([fresh, old], impact);
    expect(ranked).toEqual([fresh, old]);
  });

  it('orders unscored lines by file position among equal effects', () => {
    const a = 'Legacy lesson alpha.';
    const b = 'Legacy lesson beta.';
    const impact = impactOf([[lessonExcerptHash(a), { measuredEffect: 1 }]]);
    const ranked = rankLinesByImpact([b, a], impact);
    expect(ranked).toEqual([a, b]);
  });

  it('returns null when no line carries an accepted verdict', () => {
    const scored = 'Scored lesson.';
    const rows = [evalRow('stale-hash', '2026-08-01T00:00:00Z', true)];
    const impact = computeLessonImpact(rows, []);
    expect(rankLinesByImpact([scored, '## 2026-09-01'], impact)).toBeNull();
    // Rejected-only ledger rows do not activate ranking either.
    const rejected = computeLessonImpact([evalRow(lessonExcerptHash(scored), '2026-08-01T00:00:00Z', false)], []);
    expect(rankLinesByImpact([scored], rejected)).toBeNull();
  });

  it('never matches structural lines (headers, fences, blanks) against ledger hashes', () => {
    const header = '## 2026-09-01';
    const fence = '---';
    const impact = impactOf([
      // A ledger row exists for the header text — a header must not be scored
      // as a lesson even when its hash would match.
      [lessonExcerptHash(header), { measuredEffect: 1 }],
    ]);
    const ranked = rankLinesByImpact([header, 'A real lesson.'], impact);
    expect(ranked).toBeNull();
    expect(rankLinesByImpact([fence], impact)).toBeNull();
    expect(rankLinesByImpact([''], impact)).toBeNull();
  });
});

describe('loadLessonsDigest × lessons impact (ranked digest over synthetic fixtures)', () => {
  const dirs: string[] = [];
  /** Temp repo with a lessons file plus optional synthetic events/loop ledgers. */
  const tempRepo = (lessons: string[], events?: unknown[], loops?: unknown[]) => {
    const d = mkdtempSync(join(tmpdir(), 'da-lessons-digest-impact-'));
    dirs.push(d);
    mkdirSync(join(d, '.selfbuild'), { recursive: true });
    mkdirSync(join(d, '.devagent', 'runs', 'orchestration'), { recursive: true });
    writeFileSync(join(d, SELFBUILD_LESSONS_PATH), `${lessons.join('\n')}\n`);
    if (events) {
      writeFileSync(join(d, '.devagent', 'runs', 'orchestration', 'events.jsonl'), `${events.map((e) => JSON.stringify(e)).join('\n')}\n`);
    }
    if (loops) {
      writeFileSync(join(d, '.selfbuild', 'ledger.jsonl'), `${loops.map((l) => JSON.stringify(l)).join('\n')}\n`);
    }
    return d;
  };
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('ranks the digest by measured effect: high-effect first, zero-effect sinking to the bottom', () => {
    const high = 'Guard dedupe stopped the repeat burn.';
    const mid = 'Fanout needs one flaky rerun before condemning.';
    const zero = 'Unmeasured legacy lesson from before the eval guard.';
    const dir = tempRepo(
      [zero, mid, high],
      [
        // high: accepted 08-01 → 4 loops after, all merged → 4/4 = 1.0
        { ...evalRow(lessonExcerptHash(high), '2026-08-01T00:00:00Z', true), kind: 'event', event: 'lessons-eval' },
        // mid: accepted 08-04 → 2 loops after (ok + failed) → 1/2 = 0.5
        { ...evalRow(lessonExcerptHash(mid), '2026-08-04T00:00:00Z', true), kind: 'event', event: 'lessons-eval' },
      ],
      [
        { loop: 1, ts: iso('2026-08-02T00:00:00Z'), status: 'merged', goal: 'g' },
        { loop: 2, ts: iso('2026-08-03T00:00:00Z'), status: 'merged', goal: 'g' },
        { loop: 3, ts: iso('2026-08-05T00:00:00Z'), status: 'ok', goal: 'g' },
        { loop: 4, ts: iso('2026-08-06T00:00:00Z'), status: 'failed', goal: 'g' },
      ],
    );
    // zero: no ledger row → 0 → last.
    const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH);
    const lines = digest.split('\n');
    expect(lines[0]).toBe(high);
    expect(lines[1]).toBe(mid);
    expect(lines[2]).toBe(zero);
  });

  it('drops the lowest-ranked (zero-effect) lines first when the char budget is tight', () => {
    const high = 'Expand-first migrations are the rule.';
    const mid = 'Never trust a green suite without a rerun.';
    const zero = 'Unmeasured lesson that should be cut first.';
    const dir = tempRepo(
      [zero, high, mid],
      [
        { ...evalRow(lessonExcerptHash(high), '2026-08-02T00:00:00Z', true), kind: 'event', event: 'lessons-eval' },
        { ...evalRow(lessonExcerptHash(mid), '2026-08-03T00:00:00Z', true), kind: 'event', event: 'lessons-eval' },
      ],
      [
        { loop: 1, ts: iso('2026-08-04T00:00:00Z'), status: 'merged', goal: 'g' },
        { loop: 2, ts: iso('2026-08-05T00:00:00Z'), status: 'failed', goal: 'g' },
      ],
    );
    // high: 1/2 → 0.5; mid: 1/2 → 0.5 (tie → recency: mid accepted later);
    // zero: 0. Budget 86 fits mid(42)+high(37) but not zero(43) too.
    const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH, 86);
    expect(digest).not.toContain(zero);
    expect(digest).toContain(high);
    expect(digest).toContain(mid);
  });

  it('keeps the 40-line / char caps in impact mode and echoes predictedImpact suffixes verbatim', () => {
    const scored: string[] = [];
    for (let i = 0; i < 45; i++) scored.push(`Scored seed lesson ${i} with some padding text.`);
    // The file line carries the predictedImpact suffix; lessonExcerptHash
    // normalizes it away so the ledger row (bare hash) still matches.
    const newest = 'A brand new measured lesson worth echoing. [predictedImpact: must survive the cap]';
    const dir = tempRepo(
      [...scored, newest],
      [
        ...scored.map((l, i) => ({ ...evalRow(lessonExcerptHash(l), `2026-08-01T00:00:0${i % 10}Z`, true), kind: 'event', event: 'lessons-eval' })),
        { ...evalRow(lessonExcerptHash(newest), '2026-08-02T00:00:00Z', true, 'must survive the cap'), kind: 'event', event: 'lessons-eval' },
      ],
      [
        { loop: 1, ts: iso('2026-08-03T00:00:00Z'), status: 'merged', goal: 'g' },
        { loop: 2, ts: iso('2026-08-04T00:00:00Z'), status: 'ok', goal: 'g' },
      ],
    );
    const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH);
    expect(digest.split('\n').length).toBeLessThanOrEqual(40);
    expect(digest.length).toBeLessThanOrEqual(4000);
    // Newest measured line survives whole with its predictedImpact suffix.
    expect(digest).toContain(newest);
    expect(digest).toContain('[predictedImpact: must survive the cap]');
  });

  it('with no lessons-eval ledger data the digest is the pre-Q39 recency cursor (no regression)', () => {
    const lines = Array.from({ length: 50 }, (_, i) => `lesson line ${i}`);
    const dir = tempRepo(lines);
    // No events.jsonl / ledger.jsonl in this repo.
    const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH);
    expect(digest.split('\n')).toHaveLength(40);
    expect(digest).toContain('lesson line 49');
    expect(digest).not.toContain('lesson line 0\n');
    // Exact legacy equality: newest lines in file order.
    const expected = lines.slice(-40).join('\n');
    expect(digest).toBe(expected);
  });

  it('digest ranking survives when ledger rows exist for lessons no longer in the file', () => {
    const live = 'Live lesson still in the file.';
    const staleHash = lessonExcerptHash('A lesson that was rolled off the ratchet.');
    const dir = tempRepo(
      ['A legacy unmeasured line.', live],
      [{ ...evalRow(staleHash, '2026-08-01T00:00:00Z', true), kind: 'event', event: 'lessons-eval' }],
      [{ loop: 1, ts: iso('2026-08-02T00:00:00Z'), status: 'merged', goal: 'g' }],
    );
    const digest = loadLessonsDigest(dir, SELFBUILD_LESSONS_PATH);
    // No file line matches the stale ledger hash → pre-Q39 cursor applies.
    expect(digest).toBe(['A legacy unmeasured line.', live].join('\n'));
  });
});
