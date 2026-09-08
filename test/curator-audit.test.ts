import { describe, expect, it, afterAll } from 'vitest';
import { mkdtempSync, rmSync, mkdirSync, writeFileSync, readFileSync, utimesSync, existsSync, readdirSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { enqueueTask, setTaskStatus, updateTask } from '../src/queue.js';
import { auditPrdCoverage, formatAuditReport, DEFAULT_STALE_AFTER_MS } from '../src/curator/audit.js';

/**
 * Q15 (PRD:912) — the curator's PRD-coverage audit is ADVISORY ONLY: it reads
 * the queue and warns, it never enqueues. These tests pin both halves of that
 * contract: the finding semantics (unqueued / stale, and the cases that must
 * stay silent) and the read-only guarantee (a run leaves the queue directory
 * untouched, and the CLI exits 0 even when it found something).
 *
 * Determinism: the clock is injected via `now` and every PRD mtime is set with
 * utimesSync, so ages are exact integers rather than wall-clock races.
 */

const NOW = Date.parse('2026-09-07T00:00:00.000Z');
const DAY = 24 * 60 * 60 * 1000;

const dirs: string[] = [];
afterAll(() => {
  for (const d of dirs) rmSync(d, { recursive: true, force: true });
});

/** Temp repo with docs/prds/, each PRD's mtime set to `daysOld` before NOW. */
function tmpRepo(prds: Record<string, number> = {}): string {
  const repo = mkdtempSync(join(tmpdir(), 'da-curator-audit-'));
  dirs.push(repo);
  mkdirSync(join(repo, 'docs', 'prds'), { recursive: true });
  for (const [name, daysOld] of Object.entries(prds)) {
    const p = join(repo, 'docs', 'prds', `${name}.md`);
    writeFileSync(p, `# ${name}\n`);
    const t = new Date(NOW - daysOld * DAY);
    utimesSync(p, t, t);
  }
  return repo;
}

const audit = (repo: string, opts = {}) => auditPrdCoverage(repo, { now: () => NOW, ...opts });

/** Every queue file and its bytes, so any write by the audit is detectable. */
function queueState(repo: string): Record<string, string> {
  const dir = join(repo, '.devagent', 'queue');
  if (!existsSync(dir)) return {};
  return Object.fromEntries(readdirSync(dir).map((f) => [f, readFileSync(join(dir, f), 'utf8')]));
}

describe('curator audit: coverage findings', () => {
  it('reports a docs/prds file no queue task covers', () => {
    const repo = tmpRepo({ 'PRD-orphan': 3 });
    const r = audit(repo);
    expect(r.scanned).toBe(1);
    expect(r.findings).toHaveLength(1);
    expect(r.findings[0]).toMatchObject({ kind: 'unqueued', stem: 'PRD-orphan', taskIds: [] });
    expect(r.findings[0]!.file).toBe(join('docs', 'prds', 'PRD-orphan.md'));
  });

  it('counts a PRD as covered by the task id', () => {
    const repo = tmpRepo({ 'PRD-queued': 3 });
    enqueueTask(repo, { id: 'PRD-queued', title: 'Queued idea', goal: 'Goal: ship it' });
    expect(audit(repo).findings).toEqual([]);
  });

  it('counts a PRD as covered via prdPath when the task id differs', () => {
    const repo = tmpRepo({ 'PRD-renamed': 3 });
    const q = enqueueTask(repo, {
      id: 'SCOUT-2026-09-01-aaaa',
      title: 'Renamed idea',
      goal: 'Goal: ship it',
      prdMarkdown: '# PRD-renamed\n',
    });
    // The queue copy is named after the task id; point prdPath at the docs/prds
    // file to model a PRD authored under one name and enqueued under another.
    updateTask(repo, q.id, { prdPath: join(repo, 'docs', 'prds', 'PRD-renamed.md') });
    expect(audit(repo).findings).toEqual([]);
  });

  it('reports a still-open covered PRD past the mtime threshold as stale', () => {
    const repo = tmpRepo({ 'PRD-stuck': 20 });
    enqueueTask(repo, { id: 'PRD-stuck', title: 'Stuck idea', goal: 'Goal: ship it' });
    const r = audit(repo);
    expect(r.findings).toHaveLength(1);
    expect(r.findings[0]).toMatchObject({ kind: 'stale', stem: 'PRD-stuck', taskIds: ['PRD-stuck'] });
    expect(r.findings[0]!.ageMs).toBe(20 * DAY);
  });

  it('stays silent on a covered PRD inside the threshold', () => {
    const repo = tmpRepo({ 'PRD-fresh': 2 });
    enqueueTask(repo, { id: 'PRD-fresh', title: 'Fresh idea', goal: 'Goal: ship it' });
    expect(audit(repo, { staleAfterMs: DEFAULT_STALE_AFTER_MS }).findings).toEqual([]);
  });

  it('treats a PRD whose covering tasks are all terminal as retired, not stale', () => {
    const repo = tmpRepo({ 'PRD-shipped': 90 });
    enqueueTask(repo, { id: 'PRD-shipped', title: 'Shipped idea', goal: 'Goal: ship it' });
    setTaskStatus(repo, 'PRD-shipped', 'done');
    expect(audit(repo).findings).toEqual([]);
  });

  it('reports unqueued once for an old uncovered PRD, not both kinds', () => {
    const repo = tmpRepo({ 'PRD-neglected': 90 });
    const r = audit(repo);
    expect(r.findings.map((f) => f.kind)).toEqual(['unqueued']);
    expect(r.findings[0]!.ageMs).toBe(90 * DAY);
  });

  it('scans nothing and never throws when docs/prds is absent', () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-curator-audit-'));
    dirs.push(repo);
    rmSync(join(repo, 'docs'), { recursive: true, force: true });
    const r = audit(repo);
    expect(r).toMatchObject({ scanned: 0, tasks: 0, findings: [], warnings: [], enqueued: 0 });
  });

  it('ignores non-markdown files and orders findings by file name', () => {
    const repo = tmpRepo({ 'b-second': 1, 'a-first': 1 });
    writeFileSync(join(repo, 'docs', 'prds', 'notes.txt'), 'not a prd\n');
    const r = audit(repo);
    expect(r.scanned).toBe(2);
    expect(r.findings.map((f) => f.stem)).toEqual(['a-first', 'b-second']);
  });
});

describe('curator audit: advisory-only contract', () => {
  it('creates no queue entries and mutates no existing task', () => {
    const repo = tmpRepo({ 'PRD-orphan': 30, 'PRD-stuck': 30 });
    enqueueTask(repo, { id: 'PRD-stuck', title: 'Stuck idea', goal: 'Goal: ship it' });
    const before = queueState(repo);

    const r = audit(repo);
    expect(r.findings.map((f) => f.kind)).toEqual(['unqueued', 'stale']);
    expect(r.enqueued).toBe(0);
    expect(queueState(repo)).toEqual(before);
    // It must not have closed the gap itself by writing the missing PRD file.
    expect(existsSync(join(repo, '.devagent', 'prds', 'PRD-orphan.md'))).toBe(false);
  });

  it('renders a summary plus one warning line per finding', () => {
    const repo = tmpRepo({ 'PRD-orphan': 30 });
    const lines = formatAuditReport(audit(repo)).split('\n');
    expect(lines).toHaveLength(2);
    expect(lines[0]).toContain('1 warning(s)');
    expect(lines[0]).toContain('advisory only (Q15: no enqueue)');
    expect(lines[1]).toMatch(/^\[prd-audit\] warn unqueued docs\/prds\/PRD-orphan\.md/);
  });
});

describe('curator audit: devagent prd-audit CLI', () => {
  const repoRoot = join(import.meta.dirname, '..');

  function prdAudit(repo: string, ...args: string[]): { status: number | null; out: string } {
    const r = spawnSync('npx', ['tsx', join(repoRoot, 'src/cli.ts'), 'prd-audit', '--repo', repo, ...args], {
      cwd: repoRoot,
      encoding: 'utf8',
      env: { PATH: process.env.PATH, HOME: process.env.HOME, DEVAGENT_SUPPRESS_DEPRECATION: '1' },
      timeout: 90_000,
    });
    return { status: r.status, out: `${r.stdout}${r.stderr}` };
  }

  it('exits 0 with warnings present — a finding is never a cycle failure', () => {
    const repo = tmpRepo({ 'PRD-orphan': 30 });
    const r = prdAudit(repo);
    expect(r.status).toBe(0);
    expect(r.out).toContain('[prd-audit] warn unqueued docs/prds/PRD-orphan.md');
    expect(queueState(repo)).toEqual({});
  });

  it('--json emits the machine report with the enqueued: 0 invariant', () => {
    const repo = tmpRepo({ 'PRD-orphan': 30 });
    const r = prdAudit(repo, '--json');
    expect(r.status).toBe(0);
    const payload = JSON.parse(r.out) as {
      scanned: number;
      tasks: number;
      enqueued: number;
      staleAfterMs: number;
      findings: { kind: string; stem: string }[];
    };
    expect(payload).toMatchObject({
      scanned: 1,
      tasks: 0,
      enqueued: 0,
      staleAfterMs: DEFAULT_STALE_AFTER_MS,
    });
    expect(payload.findings[0]).toMatchObject({ kind: 'unqueued', stem: 'PRD-orphan' });
  });
});
