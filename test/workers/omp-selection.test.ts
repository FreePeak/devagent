import { describe, expect, it } from 'vitest';
import { getWorker, workers } from '../../src/workers/index.js';
import type { WorkerName } from '../../src/types.js';

describe('omp adapter - Seam C: worker selection', () => {
  it('exposes omp in the workers registry', () => {
    expect(Object.keys(workers)).toContain('omp');
  });

  it('resolves WorkerName "omp" via getWorker', () => {
    const w = getWorker('omp' as WorkerName);
    expect(w.name).toBe('omp');
  });

  it('still rejects truly unknown worker names', () => {
    expect(() => getWorker('definitely-not-a-worker' as WorkerName)).toThrow(/Unknown worker/);
  });
});
