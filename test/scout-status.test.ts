import { describe, expect, it } from 'vitest';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { writeHeartbeat, readHeartbeat } from '../src/scout.js';

const repoRoot = join(import.meta.dirname, '..');

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-scoutstatus-'));
  // minimal repo shape scout reads during a cycle
  mkdirSync(join(d, 'docs'), { recursive: true });
  writeFileSync(join(d, 'docs', 'PRD.md'), '# PRD\n## 4 Competitive Landscape\nfoo\n## 17 Roadmap\nbar\n');
  mkdirSync(join(d, '.selfbuild'), { recursive: true });
  writeFileSync(join(d, '.selfbuild', 'ledger.jsonl'), '{"loop":1,"status":"ok"}\n');
  writeFileSync(join(d, '.selfbuild', 'lessons.md'), '# Lessons\n');
  return d;
}

interface CliResult {
  out: string;
  status: number;
}

function runCli(args: string[]): CliResult {
  try {
    const out = execFileSync('npx', ['tsx', 'src/cli.ts', ...args], {
      cwd: repoRoot,
      stdio: 'pipe',
      env: {
        PATH: process.env.PATH,
        HOME: process.env.HOME,
        DEVAGENT_HOME: process.env.DEVAGENT_HOME ?? process.env.HOME ?? '.',
      },
      timeout: 30_000,
    }).toString();
    return { out, status: 0 };
  } catch (err) {
    const e = err as { status?: number; stdout?: Buffer; stderr?: Buffer };
    return {
      out: `${e.stdout?.toString() ?? ''}${e.stderr?.toString() ?? ''}`,
      status: e.status ?? 1,
    };
  }
}

describe('devagent scout-status (human output)', () => {
  it('reports worker, interval, status, fresh age, last task, and queue depth after a dry-run scout cycle', () => {
    const repo = tmpRepo();
    try {
      const cycle = runCli(['scout', '--once', '--dry-run', '--repo', repo]);
      expect(cycle.status).toBe(0);

      const out = runCli(['scout-status', '--repo', repo]);
      expect(out.status).toBe(0);
      expect(out.out).toContain('Scout: omp interval 30m last ok');
      expect(out.out).toMatch(/last ok (\d+s|\dm) ago/); // heartbeat age is reported
      expect(out.out).toMatch(/lastTask: SCOUT-\d{8}-fallback/);
      expect(out.out).toContain('detail: dry-run produced');
      expect(out.out).toContain(`at: `);
      expect(out.out).toContain('Queue: total 1 (pending:1 claimed:0 done:0 failed:0)');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('reports heartbeat age in minutes plus detail and timestamp for an older heartbeat', () => {
    const repo = tmpRepo();
    try {
      const twoMinutesAgo = new Date(Date.now() - 120_000).toISOString();
      writeHeartbeat(repo, {
        lastRunAt: twoMinutesAgo,
        lastStatus: 'failed',
        lastDetail: 'scout timed out after 300000ms',
        worker: 'pi',
        intervalMinutes: 15,
      });
      const out = runCli(['scout-status', '--repo', repo]);
      expect(out.status).toBe(0);
      expect(out.out).toContain('Scout: pi interval 15m last failed 2m ago');
      expect(out.out).toContain('lastTask: (none)');
      expect(out.out).toContain('detail: scout timed out after 300000ms');
      expect(out.out).toContain(`at: ${twoMinutesAgo}`);
      expect(out.out).toContain('Queue: total 0');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('reports a missing heartbeat cleanly with a hint and zero queue counts', () => {
    const repo = tmpRepo();
    try {
      const out = runCli(['scout-status', '--repo', repo]);
      expect(out.status).toBe(0);
      expect(out.out).toContain('No scout heartbeat yet. Run: devagent scout --once --dry-run');
      expect(out.out).toContain('Queue: total 0 (pending:0 claimed:0 done:0 failed:0)');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('treats a corrupt heartbeat file as missing instead of crashing', () => {
    const repo = tmpRepo();
    try {
      mkdirSync(join(repo, '.devagent'), { recursive: true });
      writeFileSync(join(repo, '.devagent', 'scout.heartbeat.json'), 'not json');
      expect(readHeartbeat(repo)).toBeNull();
      const out = runCli(['scout-status', '--repo', repo]);
      expect(out.status).toBe(0);
      expect(out.out).toContain('No scout heartbeat yet');
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});

describe('devagent scout-status --json', () => {
  it('emits the heartbeat object and queue counts', () => {
    const repo = tmpRepo();
    try {
      const cycle = runCli(['scout', '--once', '--dry-run', '--repo', repo]);
      expect(cycle.status).toBe(0);

      const out = runCli(['scout-status', '--repo', repo, '--json']);
      expect(out.status).toBe(0);
      const parsed = JSON.parse(out.out) as {
        heartbeat: {
          lastRunAt: string;
          lastTaskId?: string;
          lastStatus: string;
          lastDetail: string;
          worker: string;
          intervalMinutes: number;
        } | null;
        queue: { total: number; pending: number; done: number };
      };
      expect(parsed.heartbeat).not.toBeNull();
      expect(parsed.heartbeat!.lastStatus).toBe('ok');
      expect(parsed.heartbeat!.lastTaskId).toMatch(/^SCOUT-\d{8}-fallback$/);
      expect(parsed.heartbeat!.worker).toBe('omp');
      expect(parsed.heartbeat!.intervalMinutes).toBe(30);
      expect(parsed.heartbeat!.lastDetail).toContain('dry-run produced');
      expect(typeof parsed.heartbeat!.lastRunAt).toBe('string');
      expect(parsed.queue).toMatchObject({ total: 1, pending: 1, done: 0 });
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it('emits null heartbeat and zeroed queue counts when no scout has run', () => {
    const repo = tmpRepo();
    try {
      const out = runCli(['scout-status', '--repo', repo, '--json']);
      expect(out.status).toBe(0);
      const parsed = JSON.parse(out.out) as { heartbeat: unknown; queue: { total: number } };
      expect(parsed.heartbeat).toBeNull();
      expect(parsed.queue.total).toBe(0);
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });
});
