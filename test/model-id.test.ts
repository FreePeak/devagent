import { describe, expect, it } from 'vitest';

import { hasDeclaredModelIdShape, validateModelId } from '../src/workers/model-id.js';
import { validateWorkerModel } from '../src/config.js';

describe('model-id registry (per-adapter predicates, PRD Q32)', () => {
  it('declares an explicit shape for every registered worker adapter', () => {
    for (const w of ['omp', 'pi', 'grok', 'claude-code', 'opencode']) {
      expect(hasDeclaredModelIdShape(w), `missing shape for ${w}`).toBe(true);
    }
  });

  it('omp: accepts provider-qualified ids including multi-segment providers', () => {
    expect(validateModelId('omp', 'omniroute/bai/glm-5.3-flash')).toBeNull();
    expect(validateModelId('omp', 'openai/gpt-4o')).toBeNull();
    expect(validateModelId('omp', ' a/b ')).toBeNull();
  });

  it('omp: rejects unqualified aliases with an actionable reason', () => {
    const problem = validateModelId('omp', 'coding');
    expect(problem).toContain('provider-qualified');
    expect(problem).toContain('"omp"');
    expect(problem).toContain('coding');
  });

  it('pi: same provider-qualified shape as omp', () => {
    expect(validateModelId('pi', 'omniroute/bai/glm-5.3-flash')).toBeNull();
    expect(validateModelId('pi', 'coding')).toMatch(/provider-qualified/);
  });

  it('grok: accepts exact xAI slugs and xai/-qualified ids, rejects tier aliases (FR-GROK-02)', () => {
    expect(validateModelId('grok', 'grok-4.6')).toBeNull();
    expect(validateModelId('grok', 'grok-build-0.1')).toBeNull();
    expect(validateModelId('grok', 'grok-4.3')).toBeNull();
    expect(validateModelId('grok', 'grok-code-fast-1')).toBeNull();
    expect(validateModelId('grok', 'grok-4.6-2026-08-14')).toBeNull();
    expect(validateModelId('grok', 'xai/grok-build-0.1')).toBeNull();
    expect(validateModelId('grok', ' xai/grok-4.6 ')).toBeNull();
    // loop-58 precedent: driver tier aliases and bare config aliases are
    // not grok ids — reject at the gate, not mid-board.
    expect(validateModelId('grok', 'coding')).toMatch(/exact xAI model slug/);
    expect(validateModelId('grok', 'coding')).toContain('grok');
    expect(validateModelId('grok', 'free')).toMatch(/exact xAI model slug/);
    expect(validateModelId('grok', 'sonnet')).toMatch(/exact xAI model slug/);
  });

  it('unset/empty/whitespace model is always valid (adapter default applies)', () => {
    for (const w of ['omp', 'pi', 'grok', 'claude-code', 'opencode'] as const) {
      expect(validateModelId(w, undefined)).toBeNull();
      expect(validateModelId(w, '')).toBeNull();
      expect(validateModelId(w, '   ')).toBeNull();
    }
  });

  it('claude-code and opencode are passthrough: adapters own id normalization', () => {
    expect(validateModelId('claude-code', 'coding')).toBeNull();
    expect(validateModelId('claude-code', 'claude-sonnet-4-5')).toBeNull();
    expect(validateModelId('opencode', 'anything-goes')).toBeNull();
  });

  it('unknown workers fall back to passthrough (registry miss never blocks dispatch)', () => {
    expect(validateModelId('not-a-worker', 'coding')).toBeNull();
    expect(hasDeclaredModelIdShape('not-a-worker')).toBe(false);
  });

  it('config.validateWorkerModel stays wired to the registry (back-compat call sites)', () => {
    expect(validateWorkerModel('omp', 'coding')).toMatch(/provider-qualified/);
    expect(validateWorkerModel('pi', 'omniroute/bai/glm-5.3-flash')).toBeNull();
    expect(validateWorkerModel('claude-code', 'coding')).toBeNull();
  });
});
