import { describe, expect, it } from 'vitest';
import { evaluateReadiness, G0_READINESS_THRESHOLD } from '../src/validation/readiness-gate.js';
import type { TicketSpec } from '../src/types.js';

const ticket = (over: Partial<TicketSpec> = {}): TicketSpec => ({
  id: 'ENG-1',
  title: 'Add GET /health endpoint',
  description: 'Endpoint returns service status as JSON including uptime and version.',
  labels: [],
  acceptanceCriteria: ['returns 200', 'has integration test'],
  ...over,
});

describe('evaluateReadiness (acceptance)', () => {
  it('(a) passes a well-specified endpoint ticket at or above threshold', () => {
    const r = evaluateReadiness({
      ticket: ticket(),
      classification: 'endpoint-only',
    });
    expect(r.gate).toBe('G0-readiness');
    expect(r.passed).toBe(true);
    expect(r.score).toBeGreaterThanOrEqual(G0_READINESS_THRESHOLD);
    expect(r.findings).toEqual([]);
  });

  it('(b) rejects an under-specified ticket and lists every unmet criterion', () => {
    const r = evaluateReadiness({
      ticket: ticket({
        title: 'Fix it',
        description: '',
        acceptanceCriteria: [],
      }),
      classification: 'endpoint-only',
    });
    expect(r.passed).toBe(false);
    expect(r.score).toBeLessThan(G0_READINESS_THRESHOLD);
    const ids = r.findings.map((f) => f.ruleId);
    expect(ids).toContain('G0-TITLE');
    expect(ids).toContain('G0-DESCRIPTION');
    expect(ids).toContain('G0-ACCEPTANCE');
    expect(ids).toContain('G0-ENDPOINT-SURFACE');
    expect(ids).toContain('G0-VERIFICATION');
    // Highest-severity common gaps are 'high'
    expect(r.findings.some((f) => f.severity === 'high')).toBe(true);
  });

  it('(c) applies type-specific criteria per classification', () => {
    // Migration ticket: schema entities named, but no rollback/down-migration
    // signal anywhere in title/description/criteria.
    const migration = evaluateReadiness({
      ticket: ticket({
        title: 'Add profile column to users table',
        description:
          'Alter table users to add a nullable profile_url column. Migration required for the new schema entity.',
        acceptanceCriteria: ['column exists after up', 'schema matches expectation'],
      }),
      classification: 'migration-required',
    });
    expect(migration.findings.map((f) => f.ruleId)).toContain('G0-REVERSIBILITY');
    expect(migration.findings.map((f) => f.ruleId)).not.toContain('G0-SCHEMA-ENTITY');

    // Same rules must not leak into endpoint classification.
    const endpoint = evaluateReadiness({
      ticket: ticket(),
      classification: 'migration-required',
    });
    expect(endpoint.findings.map((f) => f.ruleId)).toContain('G0-SCHEMA-ENTITY');
  });

  it('(d) tiered acceptance-criteria scoring: one criterion scores below two', () => {
    const two = evaluateReadiness({ ticket: ticket(), classification: 'endpoint-only' });
    const one = evaluateReadiness({
      ticket: ticket({ acceptanceCriteria: ['returns 200'] }),
      classification: 'endpoint-only',
    });
    expect(one.score).toBeLessThan(two.score);
    // Zero criteria is the only finding tier; one criterion earns partial
    // credit without a finding.
    expect(two.findings.map((f) => f.ruleId)).not.toContain('G0-ACCEPTANCE');
    expect(one.findings.map((f) => f.ruleId)).not.toContain('G0-ACCEPTANCE');
  });
  it('(e) skips honestly on unknown classification instead of failing', () => {
    const r = evaluateReadiness({
      ticket: ticket(),
      classification: 'nonsense' as never,
    });
    expect(r.skipped).toBe(true);
    expect(r.passed).toBe(true);
    expect(r.findings).toEqual([]);
    expect(r.detail).toContain('skipped');
  });

  it('(f) tolerates missing optional fields without throwing', () => {
    const r = evaluateReadiness({
      ticket: {
        id: 'X',
        title: '',
        description: '',
        labels: undefined as never,
        acceptanceCriteria: undefined as never,
      },
      classification: 'consumer-only',
    });
    expect(typeof r.score).toBe('number');
    expect(r.passed).toBe(false);
    expect(r.findings.length).toBeGreaterThanOrEqual(4);
  });
  it('(g) includes a deterministic detail block with score, threshold, and findings', () => {
    const r = evaluateReadiness({
      ticket: ticket({ title: 'No', acceptanceCriteria: [] }),
      classification: 'endpoint-only',
    });
    expect(r.passed).toBe(false);
    expect(r.detail).toMatch(/G0 readiness \d+\/100 \(threshold 60\) for endpoint-only: rejected/);
    expect(r.detail).toContain('G0-TITLE');
    expect(r.detail).toContain('G0-ACCEPTANCE');
    // G0-DESCRIPTION absent because the fixture description is substantive
    expect(r.detail).not.toContain('G0-DESCRIPTION');
  });
});
