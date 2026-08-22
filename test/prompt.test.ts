import { describe, expect, it } from 'vitest';
import { buildImplementationPrompt, buildRepairPrompt } from '../src/prompt.js';
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
});
