import { afterEach, describe, expect, it } from 'vitest';
import { mkdtempSync, readFileSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import {
  appendTrailSignature,
  duplicateTrailingSignatures,
  evaluateTrailInterrupt,
  failureSignature,
  readTrailSignatures,
  taskTrailPath,
} from '../src/orchestrator/executor.js';

/**
 * Executor failure surface (PRD:775): after the task exhausts attempts with
 * N+ identical trailing trail.jsonl failure signatures, the executor marks
 * taskInterrupt and aborts the worker instead of burning another attempt on
 * the same wall. These cases exercise the trail-signature machinery directly
 * (no worker subprocess) — the same helpers `executeTask` calls on every
 * failed attempt.
 */

describe('failureSignature (trail.jsonl failure identity)', () => {
  it('collides for identical excerpts (same failure = same signature)', () => {
    expect(failureSignature('npm test: 3 failed\n  suite A: broken')).toBe(
      failureSignature('npm test: 3 failed\n  suite A: broken'),
    );
  });

  it('is insensitive to whitespace/case churn and embedded ISO timestamps', () => {
    // Same failure, different minute / quoting / line wrapping: still one signature.
    const a = failureSignature('2026-09-01T08:00:12Z  "TEST FAILED: 3 failed"');
    const b = failureSignature('2026-09-01T08:00:13Z "test   failed: 3  failed"');
    expect(a).toBe(b);
  });

  it('separates genuinely different failures', () => {
    expect(failureSignature('npm test: 3 failed')).not.toBe(failureSignature('npm test: 0 failed'));
    expect(failureSignature('worker exited 1')).not.toBe(failureSignature('worker exited 2'));
  });
});

describe('duplicateTrailingSignatures (N+ identical trailing signatures)', () => {
  it('returns the trailing slice when the last N signatures are identical', () => {
    const trail = [
      { ts: 't1', attempt: 1, signature: 'a', failureClass: 'test-gate' as const, excerpt: 'x' },
      { ts: 't2', attempt: 2, signature: 'b', failureClass: 'test-gate' as const, excerpt: 'y' },
      { ts: 't3', attempt: 3, signature: 'b', failureClass: 'test-gate' as const, excerpt: 'y' },
      { ts: 't4', attempt: 4, signature: 'b', failureClass: 'test-gate' as const, excerpt: 'y' },
    ];
    const hit = duplicateTrailingSignatures(trail, 3);
    expect(hit).not.toBeNull();
    expect(hit!.map((s) => s.signature)).toEqual(['b', 'b', 'b']);
    expect(hit![0]!.failureClass).toBe('test-gate');
  });

  it('returns null when fewer than N trailing signatures exist', () => {
    const trail = [
      { ts: 't1', attempt: 1, signature: 'a', failureClass: 'worker-error' as const, excerpt: 'x' },
      { ts: 't2', attempt: 2, signature: 'a', failureClass: 'worker-error' as const, excerpt: 'x' },
    ];
    expect(duplicateTrailingSignatures(trail, 3)).toBeNull();
  });

  it('returns null when the trailing signatures are not all identical', () => {
    const trail = [
      { ts: 't1', attempt: 1, signature: 'a', failureClass: 'test-gate' as const, excerpt: 'x' },
      { ts: 't2', attempt: 2, signature: 'b', failureClass: 'test-gate' as const, excerpt: 'y' },
      { ts: 't3', attempt: 3, signature: 'c', failureClass: 'test-gate' as const, excerpt: 'z' },
    ];
    expect(duplicateTrailingSignatures(trail, 3)).toBeNull();
  });

  it('honors a custom threshold', () => {
    const trail = [
      { ts: 't1', attempt: 1, signature: 'a', failureClass: 'test-gate' as const, excerpt: 'x' },
      { ts: 't2', attempt: 2, signature: 'a', failureClass: 'test-gate' as const, excerpt: 'x' },
    ];
    expect(duplicateTrailingSignatures(trail, 2)).not.toBeNull();
    expect(duplicateTrailingSignatures(trail, 3)).toBeNull();
  });
});

describe('evaluateTrailInterrupt (taskInterrupt decision)', () => {
  const dirs: string[] = [];
  const tempRepo = () => {
    const d = mkdtempSync(join(tmpdir(), 'da-exec-interrupt-'));
    dirs.push(d);
    return d;
  };
  afterEach(() => {
    for (const d of dirs.splice(0)) rmSync(d, { recursive: true, force: true });
  });

  it('returns null until N+ identical trailing signatures accumulate', () => {
    const repo = tempRepo();
    // First failure: trail has 1 signature -> keep retrying.
    expect(evaluateTrailInterrupt(repo, 'T1', 1, 'test gate failed: suite A broken', 'test-gate')).toBeNull();
    // Second identical failure: trail has 2 -> still retrying (threshold 3).
    expect(evaluateTrailInterrupt(repo, 'T1', 2, 'test gate failed: suite A broken', 'test-gate')).toBeNull();
    const trail = readTrailSignatures(repo, 'T1');
    expect(trail).toHaveLength(2);
    expect(trail[0]!.signature).toBe(trail[1]!.signature);
  });

  it('marks taskInterrupt on the 3rd identical trailing signature and aborts', () => {
    const repo = tempRepo();
    evaluateTrailInterrupt(repo, 'T1', 1, 'test gate failed: suite A broken', 'test-gate');
    evaluateTrailInterrupt(repo, 'T1', 2, 'test gate failed: suite A broken', 'test-gate');
    const decision = evaluateTrailInterrupt(repo, 'T1', 3, 'test gate failed: suite A broken', 'test-gate');
    expect(decision).not.toBeNull();
    expect(decision!.interrupted).toBe(true);
    expect(decision!.failureClass).toBe('test-gate');
    expect(decision!.attempts).toBe(3);
    // Last gate excerpt carried through for the ledger post-mortem
    expect(decision!.lastGateExcerpt).toContain('suite A broken');
    expect(decision!.trailHash).toMatch(/^[0-9a-f]{16}$/);
    expect(decision!.detail).toContain('identical');
    // Full trail was persisted (survives worktree cleanup for the post-mortem)
    expect(readTrailSignatures(repo, 'T1')).toHaveLength(3);
  });

  it('resets the streak when a different failure interrupts the identical run', () => {
    const repo = tempRepo();
    evaluateTrailInterrupt(repo, 'T1', 1, 'test gate failed: suite A broken', 'test-gate');
    evaluateTrailInterrupt(repo, 'T1', 2, 'test gate failed: suite A broken', 'test-gate');
    // A NEW failure (different signature) breaks the run: not terminal yet.
    expect(evaluateTrailInterrupt(repo, 'T1', 3, 'worker crashed: out of memory', 'worker-error')).toBeNull();
    // Two more of the NEW signature -> interrupt on the worker-error class.
    const decision = evaluateTrailInterrupt(repo, 'T1', 4, 'worker crashed: out of memory', 'worker-error');
    expect(decision).toBeNull();
    const d2 = evaluateTrailInterrupt(repo, 'T1', 5, 'worker crashed: out of memory', 'worker-error');
    expect(d2).not.toBeNull();
    expect(d2!.failureClass).toBe('worker-error');
  });

  it('persists the trail.jsonl under the repo and is readable back', () => {
    const repo = tempRepo();
    appendTrailSignature(repo, 'T9', {
      attempt: 1,
      signature: failureSignature('boom'),
      failureClass: 'test-gate',
      excerpt: 'boom',
    });
    const file = taskTrailPath(repo, 'T9');
    const raw = readFileSync(file, 'utf8').trim().split('\n');
    expect(raw).toHaveLength(1);
    const row = JSON.parse(raw[0]!);
    expect(row).toMatchObject({ attempt: 1, signature: failureSignature('boom'), failureClass: 'test-gate' });
    expect(readTrailSignatures(repo, 'T9')).toHaveLength(1);
    // Missing task -> empty trail
    expect(readTrailSignatures(repo, 'TX')).toEqual([]);
  });
});
