import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { appendFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

import {
  DEGRADE_BREACH_SOURCES,
  isDegradeBreachSource,
  pageDegradeBreach,
} from '../src/resilience/degrade-pager.js';
import type { DegradeBreachNotifier } from '../src/resilience/degrade-pager.js';
import { DEGRADE_STREAK_THRESHOLD } from '../src/resilience/degradation.js';
import { EVENTS_FILE } from '../src/lessons/guard.js';

/**
 * PRD §18 Q41 paging gap. The breach POST lived inside `runPreflightGate`, so
 * only preflight probe failures could page — yet the selfbuild loop's doc-sync
 * branch mirrors `loop-result` rows into the same orchestration ledger, and
 * `readDegradationStreak` counts `operator-diverged` / `provider-degraded`
 * among them. Loops 145–154 logged ten consecutive operator-diverged syncs and
 * paged nobody. `src/resilience/degrade-pager.ts` is now the caller-neutral
 * write side both surfaces page through; these tests pin the doc-sync caller's
 * contract: one page at the threshold, silence mid-streak, and a broken config
 * that can never throw into the loop.
 */

const repoRoot = join(import.meta.dirname, '..');

/** Row builder mirroring selfbuild-loop.sh's `record` (the doc-sync producer). */
const loopResult = (loop: number, status: string, ts: string): Record<string, unknown> => ({
  ts,
  kind: 'event',
  event: 'loop-result',
  loop,
  status,
  goal: `Goal: close Q41 paging gap (loop ${loop})`,
});

const dirs: string[] = [];

/** Repo with the paging knob set; no config file = paging disabled (opt-in). */
function tempRepo(webhookUrl?: string): string {
  const dir = mkdtempSync(join(tmpdir(), 'da-pager-'));
  dirs.push(dir);
  mkdirSync(join(dir, '.devagent', 'runs', 'orchestration'), { recursive: true });
  if (webhookUrl) {
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ resilience: { degradeWebhookUrl: webhookUrl } }));
  }
  return dir;
}

function appendRow(repo: string, row: Record<string, unknown>): void {
  appendFileSync(join(repo, EVENTS_FILE), `${JSON.stringify(row)}\n`, 'utf8');
}

/** ts 30s apart, starting 2026-09-06T00:00:00Z (the live 145–154 cadence). */
const tsAt = (i: number): string => new Date(Date.parse('2026-09-06T00:00:00Z') + i * 30_000).toISOString();

/** One doc-sync pause: the loop records its row, then asks the pager. */
async function docSyncCycle(repo: string, i: number, notify: DegradeBreachNotifier, status = 'operator-diverged'): Promise<boolean> {
  appendRow(repo, loopResult(145 + i, status, tsAt(i)));
  return pageDegradeBreach({
    repoPath: repo,
    source: 'doc-sync',
    role: 'selfbuild',
    worker: 'omp',
    model: 'omniroute/dev',
    detail: `doc-sync rc=3: fatal: the current branch ${status}`,
    notify,
  });
}

beforeEach(() => {
  // Hermetic vs an operator-exported DEVAGENT_DEGRADE_WEBHOOK_URL.
  delete process.env.DEVAGENT_DEGRADE_WEBHOOK_URL;
});
afterEach(() => {
  while (dirs.length) rmSync(dirs.pop()!, { recursive: true, force: true });
});

describe('pageDegradeBreach (shared Q41 pager, doc-sync source)', () => {
  it('pages exactly once when the doc-sync streak reaches the threshold, then stays silent', async () => {
    const repo = tempRepo('https://pager.invalid/hook');
    const calls: Array<{ url: string; alert: DegradeBreachAlertLike }> = [];
    const notify: DegradeBreachNotifier = async (url, alert) => void calls.push({ url, alert });

    const results: boolean[] = [];
    for (let i = 0; i <= DEGRADE_STREAK_THRESHOLD; i++) results.push(await docSyncCycle(repo, i, notify));

    // One POST for the whole episode: the breach cycle pages, every later
    // cycle of the same outage stays silent (Q41 once-per-episode rule).
    expect(calls).toHaveLength(1);
    expect(results).toEqual(Array.from({ length: DEGRADE_STREAK_THRESHOLD + 1 }, (_, i) => i === DEGRADE_STREAK_THRESHOLD - 1));
    expect(calls[0]!.url).toBe('https://pager.invalid/hook');
    expect(calls[0]!.alert).toMatchObject({
      event: 'provider-degraded-breach',
      source: 'doc-sync',
      repo,
      role: 'selfbuild',
      worker: 'omp',
      model: 'omniroute/dev',
      count: DEGRADE_STREAK_THRESHOLD,
      threshold: DEGRADE_STREAK_THRESHOLD,
      detail: 'doc-sync rc=3: fatal: the current branch operator-diverged',
    });
    // Outage window carried off the streak rows: 3 rows, 30s apart.
    expect(calls[0]!.alert.oldestTs).toBe(tsAt(0));
    expect(calls[0]!.alert.latestTs).toBe(tsAt(DEGRADE_STREAK_THRESHOLD - 1));
    expect(calls[0]!.alert.windowMs).toBe((DEGRADE_STREAK_THRESHOLD - 1) * 30_000);
    expect(typeof calls[0]!.alert.ts).toBe('string');
  });

  it('stays silent while the streak is below the threshold', async () => {
    const repo = tempRepo('https://pager.invalid/hook');
    let calls = 0;
    for (let i = 0; i < DEGRADE_STREAK_THRESHOLD - 1; i++) {
      expect(await docSyncCycle(repo, i, async () => void (calls += 1))).toBe(false);
    }
    expect(calls).toBe(0);
  });

  it('treats a dirty-PRD refusal as productive, so a paused streak never pages', async () => {
    const repo = tempRepo('https://pager.invalid/hook');
    let calls = 0;
    const notify: DegradeBreachNotifier = async () => void (calls += 1);
    // rc=2 records status `operator-degraded`, which is NOT a degraded
    // loop-result status: it stops the streak walk.
    await docSyncCycle(repo, 0, notify);
    await docSyncCycle(repo, 1, notify);
    expect(await docSyncCycle(repo, 2, notify, 'operator-degraded')).toBe(false);
    expect(calls).toBe(0);
  });

  it('never throws when the config file is broken', async () => {
    const repo = tempRepo();
    writeFileSync(join(repo, 'devagent.json'), '{ not json at all');
    for (let i = 0; i < DEGRADE_STREAK_THRESHOLD; i++) appendRow(repo, loopResult(145 + i, 'provider-degraded', tsAt(i)));
    let calls = 0;
    // loadConfig throws on the malformed file; the pager swallows it.
    expect(await pageDegradeBreach({ repoPath: repo, source: 'doc-sync', notify: async () => void (calls += 1) })).toBe(false);
    expect(calls).toBe(0);
  });

  it('never throws when the paging transport fails', async () => {
    const repo = tempRepo('https://pager.invalid/hook');
    for (let i = 0; i < DEGRADE_STREAK_THRESHOLD; i++) appendRow(repo, loopResult(145 + i, 'operator-diverged', tsAt(i)));
    const paged = await pageDegradeBreach({
      repoPath: repo,
      source: 'doc-sync',
      notify: async () => {
        throw new Error('connect ECONNREFUSED 127.0.0.1:443');
      },
    });
    expect(paged).toBe(false);
  });

  it('does not page when resilience.degradeWebhookUrl is unset (opt-in)', async () => {
    const repo = tempRepo();
    let calls = 0;
    for (let i = 0; i <= DEGRADE_STREAK_THRESHOLD; i++) {
      expect(await docSyncCycle(repo, i, async () => void (calls += 1))).toBe(false);
    }
    expect(calls).toBe(0);
  });

  it('declares exactly the two streak surfaces', () => {
    expect([...DEGRADE_BREACH_SOURCES]).toEqual(['preflight', 'doc-sync']);
    expect(isDegradeBreachSource('doc-sync')).toBe(true);
    expect(isDegradeBreachSource('preflight')).toBe(true);
    expect(isDegradeBreachSource('board-archived')).toBe(false);
  });
});

/** Alert shape as received by the injected transport (structural, for the CLI smoke). */
type DegradeBreachAlertLike = {
  event: string;
  source: string;
  repo: string;
  role: string;
  worker: string;
  model: string;
  count: number;
  threshold: number;
  latestTs: string | null;
  oldestTs: string | null;
  windowMs: number | null;
  roles: string[];
  detail?: string;
};

describe('devagent page-degrade-breach (CLI surface)', () => {
  /** Run the real binary the way selfbuild-loop.sh does (DEVAGENT=(npx tsx src/cli.ts)). */
  function runCli(args: string[]): Promise<{ code: number | null; out: string }> {
    const { promise, resolve } = Promise.withResolvers<{ code: number | null; out: string }>();
    const child = spawn('npx', ['tsx', join(repoRoot, 'src', 'cli.ts'), ...args], {
      cwd: repoRoot,
      stdio: ['ignore', 'pipe', 'pipe'],
      env: { PATH: process.env.PATH, HOME: process.env.HOME },
    });
    let out = '';
    child.stdout.on('data', (c) => (out += c));
    child.stderr.on('data', (c) => (out += c));
    child.on('error', () => resolve({ code: -1, out }));
    child.on('close', (code) => resolve({ code, out }));
    return promise;
  }

  /** Local webhook sink: the POST the loop would have paged a human with. */
  async function webhook(): Promise<{ url: string; received: DegradeBreachAlertLike[]; close: () => void }> {
    const received: DegradeBreachAlertLike[] = [];
    const server = createServer((req, res) => {
      let body = '';
      req.on('data', (c) => (body += c));
      req.on('end', () => {
        try {
          received.push(JSON.parse(body) as DegradeBreachAlertLike);
        } catch {
          /* ignore a malformed probe */
        }
        res.statusCode = 200;
        res.end('ok');
      });
    });
    const { promise, resolve } = Promise.withResolvers<void>();
    server.listen(0, '127.0.0.1', resolve);
    await promise;
    const { port } = server.address() as { port: number };
    return {
      url: `http://127.0.0.1:${port}/hook`,
      received,
      close: () => server.close(),
    };
  }

  it('POSTs one doc-sync breach at the threshold and exits 0', async () => {
    const hook = await webhook();
    const repo = tempRepo(hook.url);
    for (let i = 0; i < DEGRADE_STREAK_THRESHOLD; i++) appendRow(repo, loopResult(145 + i, 'operator-diverged', tsAt(i)));
    try {
      const r = await runCli([
        'page-degrade-breach',
        '--repo',
        repo,
        '--source',
        'doc-sync',
        '--role',
        'selfbuild',
        '--detail',
        'doc-sync rc=3: operator must reconcile',
      ]);
      expect(r.code).toBe(0);
      expect(r.out).toContain('paged operator webhook');
      expect(hook.received).toHaveLength(1);
      expect(hook.received[0]).toMatchObject({
        event: 'provider-degraded-breach',
        source: 'doc-sync',
        repo,
        role: 'selfbuild',
        count: DEGRADE_STREAK_THRESHOLD,
        threshold: DEGRADE_STREAK_THRESHOLD,
        detail: 'doc-sync rc=3: operator must reconcile',
      });
    } finally {
      hook.close();
    }
  });

  it('exits 0 without posting below the threshold, so the loop keeps running', async () => {
    const hook = await webhook();
    const repo = tempRepo(hook.url);
    for (let i = 0; i < DEGRADE_STREAK_THRESHOLD - 1; i++) appendRow(repo, loopResult(145 + i, 'provider-degraded', tsAt(i)));
    try {
      const r = await runCli(['page-degrade-breach', '--repo', repo, '--source', 'doc-sync']);
      expect(r.code).toBe(0);
      expect(r.out).toContain('no page');
      expect(hook.received).toHaveLength(0);
    } finally {
      hook.close();
    }
  });

  it('exits 0 on a broken config instead of throwing at the caller', async () => {
    const repo = tempRepo();
    writeFileSync(join(repo, 'devagent.json'), '{ not json at all');
    for (let i = 0; i < DEGRADE_STREAK_THRESHOLD; i++) appendRow(repo, loopResult(145 + i, 'operator-diverged', tsAt(i)));
    const r = await runCli(['page-degrade-breach', '--repo', repo, '--source', 'doc-sync']);
    expect(r.code).toBe(0);
    expect(r.out).toContain('no page');
  });

  it('rejects an unknown --source (argv contract, not a paging failure)', async () => {
    const repo = tempRepo('https://pager.invalid/hook');
    const r = await runCli(['page-degrade-breach', '--repo', repo, '--source', 'board-archived']);
    expect(r.code).toBe(1);
    expect(r.out).toContain('unknown source "board-archived"');
    expect(r.out).toContain('preflight, doc-sync');
  });
});

describe('selfbuild-loop.sh wiring (Q41 doc-sync paging surface)', () => {
  const script = readFileSync(join(repoRoot, 'scripts', 'selfbuild-loop.sh'), 'utf8');

  it('pages beside the sync-failure records, guarded so paging cannot fail the cycle', () => {
    // The pager call must sit in the sync-failure branch, after the record
    // arms (so this cycle's row is in the streak it reads) and before the
    // breaker exit (so a dying driver still pages).
    const branch = script.slice(script.indexOf('PRD refresh failed'), script.indexOf('sleep "${SELFBUILD_SYNC_RETRY_SECS:-60}"'));
    expect(branch.indexOf('record "$N" operator-diverged')).toBeLessThan(branch.indexOf('page-degrade-breach'));
    expect(branch.indexOf('page-degrade-breach')).toBeLessThan(branch.indexOf('circuit breaker'));
    expect(branch).toContain('--source doc-sync');
    expect(branch).toContain('--role selfbuild');
    expect(branch).toContain('|| true');
    expect(branch).toContain('"${DEVAGENT[@]}" page-degrade-breach');
  });
});
