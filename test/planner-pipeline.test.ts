import { describe, expect, it } from 'vitest';
import { classifyTicket, checkSpec, planFromTicket } from '../src/planner.js';
import { RunLogger, redact } from '../src/logger.js';
import { loadConfig, loadCredentials, credentialStatus } from '../src/config.js';
import type { TicketSpec } from '../src/types.js';
import { mkdtempSync, rmSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const ticket = (over: Partial<TicketSpec> = {}): TicketSpec => ({
  id: 'LINEAR-1',
  title: 'Add health endpoint',
  description: 'Add a GET /health endpoint returning service status as JSON.',
  labels: [],
  acceptanceCriteria: ['returns 200', 'includes uptime'],
  ...over,
});

describe('classifyTicket', () => {
  it('classifies migration tickets by keywords', () => {
    expect(classifyTicket(ticket({ title: 'Add column to users' }))).toBe('migration-required');
    expect(classifyTicket(ticket({ title: 'New schema index' }))).toBe('migration-required');
  });

  it('classifies consumer tickets', () => {
    expect(classifyTicket(ticket({ title: 'Kafka consumer for orders' }))).toBe('consumer-only');
  });

  it('defaults to endpoint-only', () => {
    expect(classifyTicket(ticket())).toBe('endpoint-only');
  });
});

describe('checkSpec', () => {
  it('accepts a sufficient spec', () => {
    expect(checkSpec(ticket()).sufficient).toBe(true);
  });

  it('refuses vague specs with a question', () => {
    const r = checkSpec(ticket({ description: 'fix it', acceptanceCriteria: [] }));
    expect(r.sufficient).toBe(false);
    expect(r.question).toMatch(/acceptance criteria/i);
  });
});

describe('planFromTicket', () => {
  it('emits migration tasks including down-migration', () => {
    const plan = planFromTicket(ticket({ title: 'Alter table users add column' }));
    expect(plan.classification).toBe('migration-required');
    expect(plan.tasks.some((t) => /down-migration/i.test(t))).toBe(true);
  });
});

describe('logger redaction', () => {
  it('redacts sensitive keys', () => {
    const out = redact({ LINEAR_API_KEY: 'x', safe: 1, githubToken: 'y' });
    expect(out.LINEAR_API_KEY).toBe('[REDACTED]');
    expect(out.githubToken).toBe('[REDACTED]');
    expect(out.safe).toBe(1);
  });

  it('writes JSONL entries to a run file', () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-test-'));
    try {
      const log = new RunLogger(dir);
      log.info('fetch', 'hello', { LINEAR_API_KEY: 'secret' });
      const lines = readFileSync(log.path, 'utf8').trim().split('\n');
      expect(lines).toHaveLength(1);
      const parsed = JSON.parse(lines[0]!);
      expect(parsed.stage).toBe('fetch');
      expect(parsed.data.LINEAR_API_KEY).toBe('[REDACTED]');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe('config', () => {
  it('applies defaults', () => {
    const cfg = loadConfig('/nonexistent-path-for-sure');
    expect(cfg.maxLoops).toBe(3);
    expect(cfg.worker).toBe('omp');
  });

  it('credential status never exposes values', () => {
    const s = credentialStatus(loadCredentials({ GITHUB_TOKEN: 'abc' } as NodeJS.ProcessEnv));
    expect(s.GITHUB_TOKEN).toBe(true);
    expect(s.LINEAR_API_KEY).toBe(false);
    expect(JSON.stringify(s)).not.toContain('abc');
  });
});
