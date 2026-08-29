import { describe, expect, it } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

// next-version.mjs is a script with side-effectful top-level code, so tests
// run it as a subprocess against a scratch repo with controlled history.
const SCRIPT = new URL('../scripts/release/next-version.mjs', import.meta.url).pathname;

function runScript(subjects: string[], tag?: string): { prev: string; next: string; bump: string } {
  const dir = mkdtempSync(join(tmpdir(), 'next-version-'));
  try {
    const g = (args: string[]) => execFileSync('git', ['-C', dir, ...args], { encoding: 'utf8' });
    g(['init', '-q']);
    g(['config', 'user.email', 't@t']);
    g(['config', 'user.name', 't']);
    for (const s of subjects) {
      writeFileSync(join(dir, 'f.txt'), s);
      g(['add', '-A']);
      g(['commit', '-q', '-m', s]);
    }
    if (tag) {
      // Tag the first commit so the range <tag>..HEAD covers the rest.
      g(['tag', tag, 'HEAD~' + (subjects.length - 1)]);
    }
    const out = execFileSync('node', [SCRIPT], { encoding: 'utf8', cwd: dir });
    return JSON.parse(out.trim().split('\n').pop()!);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

describe('next-version', () => {
  it('seeds 0.1.0 (minor) when no tag exists and feat commits are present', () => {
    expect(runScript(['feat(validation): add gate'])).toMatchObject({ prev: '0.0.0', next: '0.1.0', bump: 'minor' });
  });

  it('bumps patch for fix-only history since the last tag', () => {
    expect(runScript(['feat: base', 'fix(loop): repair'], 'v0.1.0')).toMatchObject({ prev: '0.1.0', next: '0.1.1', bump: 'patch' });
  });

  it('bumps minor for feat since the last tag', () => {
    expect(runScript(['feat: base', 'feat(concurrency): governor'], 'v0.1.0')).toMatchObject({ next: '0.2.0', bump: 'minor' });
  });

  it('bumps major on breaking-change titles (bang)', () => {
    expect(runScript(['feat: base', 'feat!: drop legacy cli'], 'v0.1.0')).toMatchObject({ next: '1.0.0', bump: 'major' });
  });

  it('floors to patch for docs/chore/config-only history', () => {
    expect(runScript(['feat: base', 'docs: curation', 'config: switch model'], 'v0.1.0')).toMatchObject({ next: '0.1.1', bump: 'patch' });
  });

  it('ignores commits before the last tag', () => {
    // Breaking change before the tag must not leak into the bump decision.
    expect(runScript(['feat!: drop legacy', 'fix: after'], 'v0.1.0')).toMatchObject({ next: '0.1.1', bump: 'patch' });
  });
});
