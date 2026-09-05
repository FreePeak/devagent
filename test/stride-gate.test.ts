import { describe, expect, it } from 'vitest';
import {
  parseUnifiedDiff,
  runStrideGate,
  type ParsedDiffHunk,
} from '../src/validation/stride-gate.js';

/** Build one parsed diff hunk; added/removed mirror the unified-diff shapes. */
function hunk(file: string, added: string[], removed: string[] = [], line?: number): ParsedDiffHunk {
  return {
    file,
    ...(line !== undefined ? { line } : {}),
    added: added.map((l) => `${l}\n`).join(''),
    removed: removed.map((l) => `${l}\n`).join(''),
  };
}

// Synthetic credential fixture, assembled at runtime so this file never
// contains a usable credential-shaped literal (the gate under test still
// sees the exact same string).
const CRED_LINE = 'const api_key = "sk-live-' + 'abcd1234";';

describe('runStrideGate (acceptance)', () => {
  it('(a) blocks on a high-severity hardcoded API key', async () => {
    const r = await runStrideGate(
      [hunk('src/handler.ts', [CRED_LINE], [], 10)],
      '/tmp/worktree',
    );
    expect(r.passed).toBe(false);
    expect(r.severityMax).toBe('high');
    expect(r.findings[0]?.category).toBe('S');
    expect(r.gate).toBe('G5-stride');
  });

  it('(b) passes medium findings as advisory with a STRIDE detail block', async () => {
    const r = await runStrideGate(
      [hunk('src/handler.ts', ['console.log("debug:", password);'], [], 20)],
      '/tmp/worktree',
    );
    expect(r.passed).toBe(true);
    expect(r.severityMax).toBe('medium');
    expect(r.findings.length).toBeGreaterThanOrEqual(1);
    expect(r.detail).toContain('STRIDE');
    expect(r.detail).toContain('## STRIDE G5 findings');
  });

  it('(c) passes on null, undefined, and empty diffs without throwing', async () => {
    for (const parsed of [null, undefined, []]) {
      const r = await runStrideGate(parsed, '/tmp/wt');
      expect(r.passed).toBe(true);
      expect(r.findings).toEqual([]);
      expect(r.severityMax).toBeNull();
    }
  });

  it('(d) categorizes one finding per STRIDE letter', async () => {
    const hunks = [
      hunk('src/s.ts', [CRED_LINE], [], 1),
      hunk('src/t.ts', ['db.query(`SELECT * FROM u WHERE id = ${req.params.id}`)'], [], 2),
      hunk('src/r.ts', ['// audit log call removed'], [], 3),
      hunk('src/i.ts', ['console.log(req.body)'], [], 4),
      hunk('src/d.ts', ['setInterval(tick, 1000)'], [], 5),
      hunk('src/e.ts', [], ['if (user.role !== "admin") return;'], 6),
    ];
    const r = await runStrideGate(hunks, '/tmp/worktree');
    const categories = new Set(r.findings.map((f) => f.category));
    for (const letter of ['S', 'T', 'R', 'I', 'D', 'E'] as const) {
      expect(categories.has(letter), `missing category ${letter}`).toBe(true);
    }
  });

  it('(e) returns a Promise so it composes with the awaited gate pipeline', () => {
    const parsed = [hunk('src/x.ts', ['plain line'])];
    const p = runStrideGate(parsed, '/tmp');
    expect(typeof p.then).toBe('function');
    return p;
  });

  it('(f) gives duplicate offending lines distinct finding ids', async () => {
    const line = 'console.log("debug:", password);';
    const r = await runStrideGate([hunk('src/dup.ts', [line, line], [], 7)], '/tmp/worktree');
    expect(r.findings.length).toBe(2);
    expect(r.findings[0]?.id).not.toBe(r.findings[1]?.id);
  });
});

describe('parseUnifiedDiff', () => {
  it('splits a unified diff into per-file hunks with added and removed lines', () => {
    const diff = [
      'diff --git a/src/a.ts b/src/a.ts',
      '--- a/src/a.ts',
      '+++ b/src/a.ts',
      '@@ -1,3 +1,3 @@',
      ' context line',
      '-const old = 1;',
      '+' + CRED_LINE,
    ].join('\n');
    const hunks = parseUnifiedDiff(diff);
    expect(hunks).toHaveLength(1);
    expect(hunks[0]?.file).toBe('src/a.ts');
    expect(hunks[0]?.line).toBe(1);
    expect(hunks[0]?.added).toBe(CRED_LINE + '\n');
    expect(hunks[0]?.removed).toBe('const old = 1;\n');
  });
});
