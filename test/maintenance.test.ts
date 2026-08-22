import { describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, utimesSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { findStaleWorktrees } from '../src/maintenance.js';

function tempRepo(): string {
  const repo = mkdtempSync(join(tmpdir(), 'da-clean-'));
  return repo;
}

describe('findStaleWorktrees', () => {
  it('returns only worktrees older than the cutoff', () => {
    const repo = tempRepo();
    try {
      const root = join(repo, '.devagent-worktrees');
      mkdirSync(join(root, 'ENG-1'), { recursive: true });
      mkdirSync(join(root, 'ENG-2'), { recursive: true });
      // ENG-1: 10 days old; ENG-2: fresh
      const old = new Date(Date.now() - 10 * 86_400_000);
      utimesSync(join(root, 'ENG-1'), old, old);

      const stale = findStaleWorktrees(repo, 7 * 86_400_000);
      expect(stale).toHaveLength(1);
      expect(stale[0]!.path.endsWith('ENG-1')).toBe(true);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('returns empty when no worktree dir exists', () => {
    expect(findStaleWorktrees(tempRepo(), 0)).toEqual([]);
  });
});
