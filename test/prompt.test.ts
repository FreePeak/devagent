import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { buildImplementationPrompt, buildRepairPrompt, DEFAULT_LESSONS_FILE, loadLessons, loadLessonsDigest } from '../src/prompt.js';
import { lessonExcerptHash } from '../src/lessons/guard.js';
import { planFromTicket } from '../src/planner.js';

const plan = planFromTicket({
  id: 'ENG-7',
  title: 'Add GET /health endpoint',
  description: 'Returns service status JSON with uptime.',
  labels: [],
  acceptanceCriteria: ['returns 200', 'includes uptime'],
});

describe('buildImplementationPrompt', () => {
  it('embeds title, criteria and plan tasks', () => {
    const p = buildImplementationPrompt(plan);
    expect(p).toContain('GET /health');
    expect(p).toContain('- returns 200');
    expect(p).toContain('1. Define route/handler');
  });

  it('instructs expand-first migrations for migration tickets', () => {
    const p = buildImplementationPrompt(planFromTicket({
      id: 'ENG-8',
      title: 'Alter table users add column',
      description: 'Schema change adding a nullable column to users table.',
      labels: [],
      acceptanceCriteria: [],
    }));
    expect(p).toContain('down-migration');
    expect(p).toContain('expand-first');
  });
});

describe('buildRepairPrompt', () => {
  it('carries failure evidence back to the worker', () => {
    const p = buildRepairPrompt(plan, 2, 'FAIL src/health.test.ts\n  expected 200 got 500');
    expect(p).toContain('attempt (2)');
    expect(p).toContain('expected 200 got 500');
    expect(p).toContain('Fix the issues');
  });

  it('appends lessons when provided', () => {
    const p = buildRepairPrompt(plan, 2, 'tests failed', 'Never drop columns without a down-migration.');
    expect(p).toContain('## Lessons from previous runs');
    expect(p).toContain('Never drop columns without a down-migration.');
  });
});

describe('lessons feedback loop (PRD Phase 4)', () => {
  let dir: string;

  afterEach(() => {
    if (dir) rmSync(dir, { recursive: true, force: true });
  });

  it('loadLessons returns empty string when the lessons file is absent', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    expect(loadLessons(dir)).toBe('');
  });

  it('loadLessons reads the default .devagent/lessons.md path', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    mkdirSync(join(dir, '.devagent'));
    writeFileSync(join(dir, DEFAULT_LESSONS_FILE), 'Keep migrations expand-first.\n');
    expect(loadLessons(dir)).toBe('Keep migrations expand-first.');
  });

  it('loadLessons honors the config override and caps to the last 40 lines', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    const lines = Array.from({ length: 50 }, (_, i) => `lesson line ${i}`);
    writeFileSync(join(dir, 'custom-lessons.md'), `${lines.join('\n')}\n`);
    const loaded = loadLessons(dir, 'custom-lessons.md');
    expect(loaded.split('\n')).toHaveLength(40);
    expect(loaded).toContain('lesson line 49');
    expect(loaded).not.toContain('lesson line 0\n');
  });

  it('loadLessons drops oldest entries whole until under the character budget', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    const lines = Array.from({ length: 10 }, (_, i) => `x`.repeat(900) + ` entry-${i}`);
    writeFileSync(join(dir, 'custom-lessons.md'), `${lines.join('\n')}\n`);
    const loaded = loadLessons(dir, 'custom-lessons.md', 2000);
    expect(loaded.length).toBeLessThanOrEqual(2000 + ' entry-9'.length);
    expect(loaded.split('\n')).toHaveLength(2); // two whole entries fit; a third would overflow
    expect(loaded).toContain('entry-9');
    expect(loaded).toContain('entry-8');
    expect(loaded).not.toContain('entry-7');
  });

  it('loadLessons drops an old oversized entry whole in favor of newer ones', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    const huge = 'y'.repeat(5000) + ' tail-marker';
    writeFileSync(join(dir, 'custom-lessons.md'), `${huge}\nsmall note\n`);
    expect(loadLessons(dir, 'custom-lessons.md', 100)).toBe('small note');
  });

  it('loadLessons keeps a single newest oversized line whole rather than splitting or emptying', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    const huge = 'z'.repeat(5000) + ' keep-marker';
    writeFileSync(join(dir, 'custom-lessons.md'), `${huge}\n`);
    expect(loadLessons(dir, 'custom-lessons.md', 100)).toBe(huge);
  });

  it('loadLessons keeps short files untouched by the char budget', () => {
    dir = mkdtempSync(join(tmpdir(), 'devagent-lessons-'));
    writeFileSync(join(dir, 'custom-lessons.md'), 'a\nb\nc\n');
    expect(loadLessons(dir, 'custom-lessons.md', 4000)).toBe('a\nb\nc');
  });

  it('buildImplementationPrompt injects the lessons section only when non-empty', () => {
    const withLessons = buildImplementationPrompt(plan, 'Run npm test before claiming done.');
    const withoutLessons = buildImplementationPrompt(plan);
    expect(withLessons).toContain('## Lessons from previous runs');
    expect(withLessons).toContain('Run npm test before claiming done.');
    expect(withoutLessons).not.toContain('Lessons from previous runs');
  });
});

describe('lessons digest ranking by measured impact (Q39)', () => {
  let dir: string;

  afterEach(() => {
    if (dir) rmSync(dir, { recursive: true, force: true });
  });

  const writeLessons = (lines: string[]) => {
    dir = mkdtempSync(join(tmpdir(), 'da-lessons-rank-'));
    writeFileSync(join(dir, 'lessons.md'), `${lines.join('\n')}\n`);
  };

  const writeEvents = (events: Array<Record<string, unknown>>) => {
    const eventsDir = join(dir, '.devagent', 'runs', 'orchestration');
    mkdirSync(eventsDir, { recursive: true });
    writeFileSync(join(eventsDir, 'events.jsonl'), events.map((e) => JSON.stringify(e)).join('\n') + '\n');
  };

  it('ranks a high-score lesson ahead of a low-score one when the budget is tight', () => {
    const high = 'First lesson that prevents failures.';
    const low = 'Second lesson that correlates with failures.';
    const unscored = 'Third lesson with no ledger rows yet.';
    writeLessons([high, low, unscored]);
    writeEvents([
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(high), accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(low), accepted: false, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'failed' },
    ]);
    // Budget fits roughly one line: the highest-scored lesson survives whole.
    const digest = loadLessonsDigest(dir, 'lessons.md', 50);
    expect(digest).toContain(high);
    expect(digest).not.toContain(low);
    expect(digest).not.toContain(unscored);
  });

  it('outputs surviving lines in file order regardless of score order', () => {
    const low = 'Low score lesson first in file.';
    const high = 'High score lesson later in file.';
    writeLessons([low, high]);
    writeEvents([
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(high), accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(low), accepted: false, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'failed' },
    ]);
    const digest = loadLessonsDigest(dir, 'lessons.md', 4000);
    expect(digest.split('\n')).toEqual([low, high]);
  });

  it('breaks score ties oldest-first', () => {
    const older = 'Older lesson with a perfect record.';
    const newer = 'Newer lesson with a perfect record.';
    writeLessons([older, newer]);
    writeEvents([
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(older), accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(newer), accepted: true, loop: 2 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'ok' },
    ]);
    // Both score 1 (accepted, in ok loops, no overall failures). Budget fits one line.
    const digest = loadLessonsDigest(dir, 'lessons.md', older.length + 5);
    expect(digest).toBe(older);
  });

  it('keeps newest-first recency behavior when no scores exist', () => {
    const lines = Array.from({ length: 10 }, (_, i) => `x`.repeat(900) + ` entry-${i}`);
    writeLessons(lines);
    const digest = loadLessonsDigest(dir, 'lessons.md', 2000);
    expect(digest).toContain('entry-9');
    expect(digest).toContain('entry-8');
    expect(digest).not.toContain('entry-7');
  });

  it('held-out tier: digest order matches held-out ranking — a lesson cannot rank on its informing loop', () => {
    // rider: accepted on its informing loop 1 only, never re-evaluated → no
    // held-out evidence → score 0.
    // earner: accepted on informing loop 1 AND on held-out loop 2 → score 1.
    const rider = 'Rider lesson accepted on its own informing loop.';
    const earner = 'Earner lesson re-validated on a later loop.';
    writeLessons([rider, earner]);
    writeEvents([
      { ts: '2026-01-01T01:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(rider), accepted: true, loop: 1 },
      { ts: '2026-01-01T02:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(earner), accepted: true, loop: 1 },
      { ts: '2026-01-01T03:00:00Z', kind: 'event', event: 'lessons-eval', excerptHash: lessonExcerptHash(earner), accepted: true, loop: 2 },
      { ts: '2026-01-01T04:00:00Z', kind: 'event', event: 'loop-result', loop: 1, status: 'ok' },
      { ts: '2026-01-01T05:00:00Z', kind: 'event', event: 'loop-result', loop: 2, status: 'ok' },
    ]);
    // Budget fits one line: under in-sample scoring both lessons would tie at
    // 1 and the rider (older) would win; held-out ranking keeps the earner.
    const digest = loadLessonsDigest(dir, 'lessons.md', earner.length + 5);
    expect(digest).toBe(earner);
  });
});
