import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { detectTestCommand, runTestGate } from '../src/validation/test-gate.js';

vi.mock('../src/workers/spawn-utils.js', () => ({
  spawnCli: vi.fn(),
}));

const mockSpawn = vi.mocked(spawnCli);
import { spawnCli } from '../src/workers/spawn-utils.js';

function tempRepo(files: Record<string, string> = {}): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-g1-'));
  for (const [name, content] of Object.entries(files)) {
    const p = join(dir, name);
    mkdirSync(join(p, '..'), { recursive: true });
    writeFileSync(p, content);
  }
  return dir;
}

describe('detectTestCommand', () => {
  it('detects npm test when package.json has a test script', () => {
    const dir = tempRepo({ 'package.json': JSON.stringify({ scripts: { test: 'vitest run' } }) });
    try {
      expect(detectTestCommand(dir)).toEqual({ cmd: 'npm', args: ['test'] });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('falls back to go test on go.mod', () => {
    const dir = tempRepo({ 'go.mod': 'module x\n' });
    try {
      expect(detectTestCommand(dir)).toEqual({ cmd: 'go', args: ['test', './...'] });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('returns null with no conventions', () => {
    expect(detectTestCommand(tempRepo())).toBeNull();
  });
});

describe('runTestGate', () => {
  beforeEach(() => mockSpawn.mockReset());

  it('passes when the suite exits zero', async () => {
    const dir = tempRepo({ 'package.json': JSON.stringify({ scripts: { test: 'vitest run' } }) });
    try {
      mockSpawn.mockResolvedValue({ exitCode: 0, stdout: 'all good\n', stderr: '', timedOut: false });
      const r = await runTestGate(dir, 60_000);
      expect(r.passed).toBe(true);
      expect(mockSpawn).toHaveBeenCalledWith('npm', ['test'], expect.objectContaining({ cwd: dir }));
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('fails with output tail on non-zero exit', async () => {
    const dir = tempRepo({ 'go.mod': 'module x\n' });
    try {
      mockSpawn.mockResolvedValue({ exitCode: 1, stdout: 'a\nb\nc\nFAILED\n', stderr: '', timedOut: false });
      const r = await runTestGate(dir, 60_000);
      expect(r.passed).toBe(false);
      expect(r.detail).toContain('FAILED');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('fails on timeout', async () => {
    const dir = tempRepo({ 'package.json': JSON.stringify({ scripts: { test: 'x' } }) });
    try {
      mockSpawn.mockResolvedValue({ exitCode: -1, stdout: '', stderr: '', timedOut: true });
      expect((await runTestGate(dir, 1)).passed).toBe(false);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('skips cleanly without a detected command', async () => {
    const empty = tempRepo();
    try {
      mockSpawn.mockResolvedValue({ exitCode: -1, stdout: '', stderr: '', timedOut: true });
      const skipped = await runTestGate(empty, 1000);
      expect(skipped.passed).toBe(true);
      expect(skipped.detail).toContain('skipped');
      expect(mockSpawn).not.toHaveBeenCalled();
    } finally {
      rmSync(empty, { recursive: true, force: true });
    }
  });
});
