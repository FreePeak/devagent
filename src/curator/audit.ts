import { existsSync, readdirSync, statSync } from 'node:fs';
import { basename, join } from 'node:path';
import { listTasks } from '../queue.js';
import type { QueuedTask } from '../queue.js';

/**
 * Curator PRD-coverage audit (PRD:912 Q15) — ADVISORY ONLY.
 *
 * Q15 asked whether the curator's audit step should be authoritative (the
 * curator enqueues directly) or advisory-only. Resolved advisory-only: this
 * module READS the queue through `listTasks()` (src/queue.ts:126) and prints
 * warnings, and it never calls `enqueueTask`/`writePrd`/`setTaskStatus`. The
 * reason is coupling: the queue schema (`QueuedTask`, id sanitization, the
 * scout-owned PRD files under .devagent/prds/) belongs to the scout/bridge
 * side, and a curator that writes it directly becomes a second owner of that
 * schema — every future queue change then needs a curator change with it. The
 * 2026-08-29 gap behind PR #64 (a `docs/prds/` file that "sat in docs/prds/
 * for weeks" because the curator kept omitting it from the queue) is a
 * visibility problem, so it gets a visibility fix: the warnings surface in
 * the curation cycle log (scripts/prd-curator.sh) where the next scout cycle
 * and the operator both read them.
 *
 * Coverage model: a `docs/prds/<stem>.md` file counts as queued when any task
 * names that stem, either through its `id` or through the basename of its
 * `prdPath` (the queue keeps its own copy under .devagent/prds/, so the file
 * stem is the join key between the two directories).
 *
 * Finding kinds, mutually exclusive per file (unqueued wins):
 *   unqueued  no queue task covers the PRD at all
 *   stale     a covering task is still open (pending/claimed) and the PRD's
 *             mtime is older than the threshold — i.e. it is queued but not
 *             moving. A PRD whose covering tasks are all terminal (done/
 *             failed) is shipped or retired state, not a stale backlog, so it
 *             stays silent.
 *
 * Determinism: file order is the sorted `docs/prds` listing, and the clock is
 * injectable (`now`), so a temp-repo test controls both the listing and every
 * mtime age without touching system time.
 */

/** PRD directory the curator writes, relative to the target repo. */
export const CURATOR_PRD_DIR = join('docs', 'prds');

/** Default mtime age after which a still-open covered PRD reads as stale. */
export const DEFAULT_STALE_AFTER_MS = 14 * 24 * 60 * 60 * 1000;

export type PrdAuditKind = 'unqueued' | 'stale';

export interface CuratorAuditOptions {
  /** Stale threshold in ms of PRD mtime age (default DEFAULT_STALE_AFTER_MS). */
  staleAfterMs?: number;
  /** Injectable clock (epoch ms); tests pass a fixed value for determinism. */
  now?: () => number;
}

export interface PrdAuditFinding {
  kind: PrdAuditKind;
  /** PRD path relative to the repo, as the warning names it. */
  file: string;
  /** File stem — the id the queue would carry. */
  stem: string;
  /** PRD mtime age in ms at scan time (never negative). */
  ageMs: number;
  /** Queue task ids covering the PRD; empty for an `unqueued` finding. */
  taskIds: string[];
  /** One-line human warning, prefixed by the caller. */
  warning: string;
}

export interface CuratorAuditReport {
  repoPath: string;
  /** Absolute path of the scanned PRD directory (may not exist). */
  prdsDir: string;
  /** `docs/prds/*.md` files considered. */
  scanned: number;
  /** Queue tasks read for coverage. */
  tasks: number;
  staleAfterMs: number;
  findings: PrdAuditFinding[];
  /** Convenience projection of `findings[].warning`, same order. */
  warnings: string[];
  /**
   * Always 0. The audit is advisory-only by contract (Q15); the field exists
   * so a machine caller can assert the invariant instead of trusting the docs.
   */
  enqueued: number;
}

export function formatPrdAge(ageMs: number): string {
  const days = Math.floor(ageMs / (24 * 60 * 60 * 1000));
  if (days >= 1) return `${days}d`;
  return `${Math.max(0, Math.floor(ageMs / (60 * 60 * 1000)))}h`;
}

/**
 * Scan `docs/prds/*.md` against queue coverage and return the advisory
 * findings. Read-only: no queue write, no PRD write, no throw on a missing or
 * unreadable directory (it simply scans nothing).
 */
export function auditPrdCoverage(
  repoPath: string,
  opts: CuratorAuditOptions = {},
): CuratorAuditReport {
  const staleAfterMs = opts.staleAfterMs ?? DEFAULT_STALE_AFTER_MS;
  const now = (opts.now ?? (() => Date.now()))();
  const prdsDir = join(repoPath, CURATOR_PRD_DIR);

  const tasks = listTasks(repoPath);
  // Join key per task: its id, plus the PRD file stem when the queue points at
  // a differently-named file (the queue keeps its own copy under
  // .devagent/prds/, so the stem is the only thing shared with docs/prds/).
  const cover = new Map<string, QueuedTask[]>();
  for (const t of tasks) {
    const keys = [t.id];
    const prdKey = t.prdPath ? basename(t.prdPath).replace(/\.md$/, '') : undefined;
    if (prdKey && prdKey !== t.id) keys.push(prdKey);
    for (const k of keys) cover.set(k, [...(cover.get(k) ?? []), t]);
  }

  let files: string[] = [];
  try {
    if (existsSync(prdsDir)) files = readdirSync(prdsDir).filter((f) => f.endsWith('.md')).sort();
  } catch {
    files = [];
  }

  const findings: PrdAuditFinding[] = [];
  for (const f of files) {
    const stem = f.replace(/\.md$/, '');
    const file = join(CURATOR_PRD_DIR, f);
    let ageMs = 0;
    try {
      ageMs = Math.max(0, now - statSync(join(prdsDir, f)).mtimeMs);
    } catch {
      continue; // vanished mid-scan — never fail an advisory pass
    }
    const covering = cover.get(stem) ?? [];
    if (covering.length === 0) {
      findings.push({
        kind: 'unqueued',
        file,
        stem,
        ageMs,
        taskIds: [],
        warning:
          `${file} (${formatPrdAge(ageMs)} old) has no queue task covering it — ` +
          `the next scout cycle should enqueue it or the curator should retire the file`,
      });
      continue;
    }
    const open = covering.filter((t) => t.status === 'pending' || t.status === 'claimed');
    if (open.length > 0 && ageMs >= staleAfterMs) {
      findings.push({
        kind: 'stale',
        file,
        stem,
        ageMs,
        taskIds: covering.map((t) => t.id),
        warning:
          `${file} is queued as ${open.map((t) => `${t.id} (${t.status})`).join(', ')} but has sat ` +
          `unchanged for ${formatPrdAge(ageMs)} >= ${formatPrdAge(staleAfterMs)} threshold — ` +
          `check why the board has not picked it up`,
      });
    }
  }

  return {
    repoPath,
    prdsDir,
    scanned: files.length,
    tasks: tasks.length,
    staleAfterMs,
    findings,
    warnings: findings.map((f) => f.warning),
    enqueued: 0,
  };
}

/** Render the report as `[prd-audit]`-prefixed warning lines (empty when clean). */
export function formatAuditReport(report: CuratorAuditReport): string {
  const lines = [
    `[prd-audit] scanned ${report.scanned} PRD(s) in ${report.prdsDir} against ${report.tasks} queue task(s) — ${report.findings.length} warning(s), advisory only (Q15: no enqueue)`,
  ];
  for (const f of report.findings) lines.push(`[prd-audit] warn ${f.kind} ${f.warning}`);
  return lines.join('\n');
}
