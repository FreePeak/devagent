import { describe, expect, it } from 'vitest';
import { analyzeAsyncHazards, collectChangedSourceFiles, type AsyncSourceFile } from '../src/validation/async-review.js';

const file = (path: string, content: string): AsyncSourceFile => ({ path, content });

describe('analyzeAsyncHazards', () => {
  it('flags .then() without .catch()', () => {
    const findings = analyzeAsyncHazards([file('a.ts', 'fetch(url).then(r => r.json());\n')]);
    expect(findings.map((f) => f.ruleId)).toContain('DA101');
  });

  it('accepts .then() chains that catch', () => {
    const findings = analyzeAsyncHazards([
      file('a.ts', 'fetch(url).then(r => r.json()).catch(console.error);\n'),
    ]);
    expect(findings.filter((f) => f.ruleId === 'DA101')).toHaveLength(0);
  });

  it('ignores commented-out hazards', () => {
    const findings = analyzeAsyncHazards([file('a.ts', '// fetch().then(x => x)\n')]);
    expect(findings).toHaveLength(0);
  });

  it('flags async forEach callbacks', () => {
    const findings = analyzeAsyncHazards([file('a.ts', 'ids.forEach(async id => {\n  await save(id);\n});\n')]);
    expect(findings.map((f) => f.ruleId)).toContain('DA102');
  });

  it('flags setInterval without clearInterval in the same file', () => {
    const findings = analyzeAsyncHazards([
      file('poll.ts', 'setInterval(poll, 1000);\n'),
      file('other.ts', 'setInterval(tick, 500);\nclearInterval(handle);\n'),
    ]);
    const da103 = findings.filter((f) => f.ruleId === 'DA103');
    expect(da103).toHaveLength(1);
    expect(da103[0]!.file).toBe('poll.ts');
  });

  it('flags void-cast fire-and-forget calls', () => {
    const findings = analyzeAsyncHazards([file('a.ts', 'void sendMetrics();\n')]);
    expect(findings.map((f) => f.ruleId)).toContain('DA104');
  });

  it('reports line numbers', () => {
    const findings = analyzeAsyncHazards([file('a.ts', 'const a = 1;\nconst b = 2;\np.then(x => x);\n')]);
    expect(findings[0]!.line).toBe(3);
  });
});

describe('collectChangedSourceFiles', () => {
  it('reads only changed source files and skips deleted ones', async () => {
    const runGit = async (args: string[]) => {
      if (args[0] === 'merge-base') return { exitCode: 0, stdout: 'abc123\n' };
      return { exitCode: 0, stdout: 'src/a.ts\nREADME.md\ndeleted.ts\n' };
    };
    // Only src/a.ts exists on disk in this fake layout
    const { mkdtempSync, mkdirSync, writeFileSync, rmSync } = await import('node:fs');
    const { tmpdir } = await import('node:os');
    const { join } = await import('node:path');
    const dir = mkdtempSync(join(tmpdir(), 'da-g4-'));
    try {
      mkdirSync(join(dir, 'src'), { recursive: true });
      writeFileSync(join(dir, 'src', 'a.ts'), 'export {};\n');
      const files = await collectChangedSourceFiles(dir, 'main', runGit);
      expect(files.map((f) => f.path)).toEqual(['src/a.ts']);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('returns empty when merge-base fails (no common ancestor)', async () => {
    const files = await collectChangedSourceFiles('/x', 'main', async () => ({ exitCode: 128, stdout: '' }));
    expect(files).toHaveLength(0);
  });
});
