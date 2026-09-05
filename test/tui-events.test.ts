import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { randomUUID } from 'node:crypto';
import { startDaemon } from '../src/server/daemon.js';
import { subscribeEvents } from '../src/tui/transport.js';

/**
 * FR-TUI-03 live tail: subscribeEvents against a real daemon whose run-log
 * follower was seeded with two JSONL lines before boot. Bounded (<8s): the
 * file never grows, so exactly the replayed tail must arrive.
 */

let home = '';
let repo = '';
let stop: (() => Promise<void>) | null = null;
let port = 0;
let token = '';
const runId = randomUUID();

beforeAll(async () => {
  home = mkdtempSync(join(tmpdir(), 'devagent-sse-home-'));
  repo = mkdtempSync(join(tmpdir(), 'devagent-sse-repo-'));
  process.env.DEVAGENT_HOME = home;
  delete process.env.DEVAGENT_DAEMON_TOKEN;
  // Seed before boot: RunLogFollower picks the newest runs/*.jsonl at construction.
  mkdirSync(join(home, 'runs'), { recursive: true });
  const seeded = [
    { ts: new Date().toISOString(), runId, stage: 'clarify', level: 'info', message: 'seed-one' },
    { ts: new Date().toISOString(), runId, stage: 'plan', level: 'warn', message: 'seed-two' },
  ];
  writeFileSync(join(home, 'runs', `${runId}.jsonl`), `${seeded.map((l) => JSON.stringify(l)).join('\n')}\n`);
  const d = await startDaemon({ port: 0, repoPath: repo });
  stop = d.stop;
  port = d.port ?? 0;
  token = d.token;
});

afterAll(async () => {
  await stop?.();
  delete process.env.DEVAGENT_HOME;
  try {
    rmSync(home, { recursive: true, force: true });
    rmSync(repo, { recursive: true, force: true });
  } catch {
    /* tmp cleanup best-effort */
  }
});

function until(cond: () => boolean, ms: number): Promise<void> {
  return new Promise((resolve, reject) => {
    const t0 = Date.now();
    const iv = setInterval(() => {
      if (cond()) {
        clearInterval(iv);
        resolve();
      } else if (Date.now() - t0 > ms) {
        clearInterval(iv);
        reject(new Error('condition timeout'));
      }
    }, 25);
  });
}

describe('subscribeEvents (SSE /events)', () => {
  it('replays the seeded run-log tail and reports live state', async () => {
    const got: string[] = [];
    const states: string[] = [];
    const sub = subscribeEvents(
      { url: `http://127.0.0.1:${port}`, token },
      (_id, data) => got.push(data),
      (st) => states.push(st),
    );
    try {
      await until(() => got.length >= 2, 4_000);
    } finally {
      sub.stop();
    }
    expect(got.length).toBe(2); // file does not grow: replay only
    expect(got[0]).toContain('seed-one');
    expect(got[1]).toContain('seed-two');
    expect(states).toContain('live');
  }, 8_000);

  it('resumes from Last-Event-ID without replaying earlier lines', async () => {
    const got: string[] = [];
    const sub = subscribeEvents({ url: `http://127.0.0.1:${port}`, token }, (_id, data) => got.push(data), undefined, 0);
    try {
      await until(() => got.length >= 1, 4_000);
    } finally {
      sub.stop();
    }
    expect(got.length).toBe(1); // id 0 skipped
    expect(got[0]).toContain('seed-two');
  }, 8_000);

  it('stop() tears the subscription down without throwing', () => {
    const sub = subscribeEvents({ url: `http://127.0.0.1:${port}`, token }, () => {});
    sub.stop();
    expect(sub.lastEventId()).toBe(-1);
  });
});
