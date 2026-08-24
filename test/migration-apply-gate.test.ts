import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

vi.mock('../src/workers/spawn-utils.js', () => ({ spawnCli: vi.fn() }));

import { spawnCli } from '../src/workers/spawn-utils.js';
import { detectComposeFile, extractG2Config, runMigrationApplyGate } from '../src/validation/migration-apply-gate.js';
import { DEFAULT_CONFIG } from '../src/config.js';

const mockSpawn = vi.mocked(spawnCli);

const compose = `services:
  db:
    image: postgres:16
`;

function tempRepo(files: Record<string, string> = {}): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-g2-'));
  for (const [name, content] of Object.entries(files)) writeFileSync(join(dir, name), content);
  return dir;
}

describe('detectComposeFile', () => {
  it('prefers the devagent-specific compose file', () => {
    const dir = tempRepo({ 'docker-compose.devagent.yml': compose, 'docker-compose.yml': compose });
    try {
      expect(detectComposeFile(dir)).toMatch(/docker-compose\.devagent\.yml$/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('returns null without any compose file', () => {
    expect(detectComposeFile(tempRepo())).toBeNull();
  });
});

describe('extractG2Config', () => {
  it('returns null without g2 section', () => {
    expect(extractG2Config(DEFAULT_CONFIG)).toBeNull();
  });

  it('reads g2 section', () => {
    const cfg = { ...DEFAULT_CONFIG, g2: { dbService: 'db', migrationUp: 'npm run migrate:up' } };
    expect(extractG2Config(cfg as never)).toEqual({
      dbService: 'db',
      migrationUp: 'npm run migrate:up',
      migrationDown: undefined,
    });
  });
});

describe('runMigrationApplyGate', () => {
  beforeEach(() => mockSpawn.mockReset());

  it('skips when no compose file exists', async () => {
    const r = await runMigrationApplyGate(tempRepo(), 1000);
    expect(r.passed).toBe(true);
    expect(r.skipped).toBe(true);
    expect(r.detail).toContain('skipped');
    expect(mockSpawn).not.toHaveBeenCalled();
  });

  it('skips when docker daemon is unavailable', async () => {
    const dir = tempRepo({ 'docker-compose.yml': compose });
    try {
      mockSpawn.mockResolvedValue({ exitCode: 1, stdout: '', stderr: 'cannot connect', timedOut: false });
      const r = await runMigrationApplyGate(dir, 1000);
      expect(r.passed).toBe(true);
      expect(r.detail).toContain('docker not available');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('fails when up-migration command fails and tears down', async () => {
    const dir = tempRepo({
      'docker-compose.yml': compose,
      'devagent.json': JSON.stringify({ g2: { dbService: 'db', migrationUp: 'false', migrationDown: 'true' } }),
    });
    try {
      mockSpawn.mockImplementation(async (cmd, args) => {
        if (cmd === 'docker' && args![0] === 'info')
          return { exitCode: 0, stdout: '24.0', stderr: '', timedOut: false };
        if (cmd === 'sh') return { exitCode: 1, stdout: '', stderr: 'boom', timedOut: false };
        return { exitCode: 0, stdout: '', stderr: '', timedOut: false }; // compose up/down
      });
      const r = await runMigrationApplyGate(dir, 5000);
      expect(r.passed).toBe(false);
      expect(r.detail).toContain('up-migration: exit 1');
      // teardown (compose down -v) was invoked
      expect(mockSpawn.mock.calls.some(([c, a]) => c === 'docker' && a!.includes('down'))).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('passes when up and down migrations succeed', async () => {
    const dir = tempRepo({
      'docker-compose.yml': compose,
      'devagent.json': JSON.stringify({ g2: { dbService: 'db', migrationUp: 'migrate up', migrationDown: 'migrate down' } }),
    });
    try {
      mockSpawn.mockImplementation(async (cmd, args) => {
        if (cmd === 'docker' && args![0] === 'info')
          return { exitCode: 0, stdout: '24.0', stderr: '', timedOut: false };
        if (cmd === 'sh') return { exitCode: 0, stdout: 'ok', stderr: '', timedOut: false };
        return { exitCode: 0, stdout: '', stderr: '', timedOut: false };
      });
      const r = await runMigrationApplyGate(dir, 5000);
      expect(r.passed).toBe(true);
      expect(r.skipped).toBeUndefined();
      expect(r.detail).toContain('down-migration: exit 0');
      expect(r.detail).toContain('rollback round-trip re-up: exit 0');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

  it('fails when the rollback round-trip re-up breaks after a successful down', async () => {
    const dir = tempRepo({
      'docker-compose.yml': compose,
      'devagent.json': JSON.stringify({ g2: { dbService: 'db', migrationUp: 'migrate up', migrationDown: 'migrate down' } }),
    });
    try {
      let shCalls = 0;
      mockSpawn.mockImplementation(async (cmd, args) => {
        if (cmd === 'docker' && args![0] === 'info')
          return { exitCode: 0, stdout: '24.0', stderr: '', timedOut: false };
        if (cmd === 'sh') {
          shCalls += 1;
          // up ok, down ok, re-up broken (e.g. non-idempotent migration)
          return shCalls <= 2
            ? { exitCode: 0, stdout: 'ok', stderr: '', timedOut: false }
            : { exitCode: 1, stdout: '', stderr: 'column already exists', timedOut: false };
        }
        return { exitCode: 0, stdout: '', stderr: '', timedOut: false }; // compose up/down
      });
      const r = await runMigrationApplyGate(dir, 5000);
      expect(shCalls).toBe(3);
      expect(r.passed).toBe(false);
      expect(r.detail).toContain('rollback round-trip re-up: exit 1');
      expect(mockSpawn.mock.calls.some(([c, a]) => c === 'docker' && a!.includes('down'))).toBe(true);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
