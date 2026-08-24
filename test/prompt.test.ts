import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import { buildImplementationPrompt, buildRepairPrompt, DEFAULT_LESSONS_FILE, loadLessons } from '../src/prompt.js';
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

  it('buildImplementationPrompt injects the lessons section only when non-empty', () => {
    const withLessons = buildImplementationPrompt(plan, 'Run npm test before claiming done.');
    const withoutLessons = buildImplementationPrompt(plan);
    expect(withLessons).toContain('## Lessons from previous runs');
    expect(withLessons).toContain('Run npm test before claiming done.');
    expect(withoutLessons).not.toContain('Lessons from previous runs');
  });
});
