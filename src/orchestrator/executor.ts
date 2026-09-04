import { createHash } from 'node:crypto';
import { appendFileSync, existsSync, mkdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { OrchestratorTask, ProjectBoard } from './types.js';
import type { RunLogger } from '../logger.js';
import type { ExecutorFailureClass, WorkerName } from '../types.js';
import { createWorktree } from '../git/worktree.js';
import { buildImplementationPrompt, buildRepairPrompt, loadLessons } from '../prompt.js';
import { sanitizeTicketId } from '../git/worktree.js';

/**
 * Executor role: implements one small precise task in its own worktree
 * (branch devagent/<task-id>), verifies with the repo's test suite (G1),
 * and reports done/failed with evidence. Fresh worktree per retry.
 *
 * Failure surface (PRD:775): each failed attempt appends a normalized
 * failure signature to a per-task trail.jsonl under the repo (survives
 * worktree cleanup — the trail outlives any single attempt). When the task
 * exhausts attempts with N+ identical trailing signatures, the executor
 * marks taskInterrupt, aborts the worker, and returns the compact
 * post-mortem (failure class, last gate excerpt, attempts, trail hash) so
 * the scheduler can thread it into the ledger on board archive (closing the
 * loop-57/58 diagnostic gap: loops died on the same goal with only
 * `attempts: 3`-style evidence).
 */

/** Trail root under the repo; same dir the ledger uses so it survives resets. */
const TRAIL_ROOT = '.devagent/runs/orchestration';

/** One failure signature row in a task's trail.jsonl. */
export interface TrailSignature {
  ts: string;
  attempt: number;
  /** Normalized hash of the failure excerpt — identical failures collide. */
  signature: string;
  failureClass: ExecutorFailureClass;
  /** Bounded failure excerpt (gate detail / worker error). */
  excerpt: string;
}

/** Per-task trail path (repo-scoped so it persists across fresh worktrees). */
export function taskTrailPath(repoPath: string, taskId: string): string {
  return join(repoPath, TRAIL_ROOT, `trail-${sanitizeTicketId(taskId)}.jsonl`);
}

/**
 * Normalize a failure excerpt into a stable signature: trim + collapse
 * whitespace + lowercase so trivial churn (quoting, line wrapping) does not
 * fragment otherwise-identical failures into distinct signatures. ISO
 * timestamps are stripped so worker errors that embed the clock still collide
 * across attempts (identical failure, different minute).
 */
export function failureSignature(excerpt: string): string {
  const normalized = excerpt
    .replace(/\d{4}-\d{2}-\d{2}[T ][\d:.Z+-]+/g, ' <ts> ')
    .replace(/\s+/g, ' ')
    .trim()
    .toLowerCase();
  return createHash('sha256').update(normalized).digest('hex').slice(0, 16);
}

/** Append one failure signature to the task trail. Best-effort by design. */
export function appendTrailSignature(
  repoPath: string,
  taskId: string,
  record: Omit<TrailSignature, 'ts'>,
): void {
  try {
    const file = taskTrailPath(repoPath, taskId);
    mkdirSync(join(repoPath, TRAIL_ROOT), { recursive: true });
    appendFileSync(file, `${JSON.stringify({ ...record, ts: new Date().toISOString() })}\n`);
  } catch {
    // best-effort observability only
  }
}

/** Read a task trail, oldest first; [] when absent/corrupt. */
export function readTrailSignatures(repoPath: string, taskId: string): TrailSignature[] {
  const file = taskTrailPath(repoPath, taskId);
  if (!existsSync(file)) return [];
  const out: TrailSignature[] = [];
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    if (!line.trim()) continue;
    try {
      out.push(JSON.parse(line) as TrailSignature);
    } catch {
      // skip corrupt lines; a trail is data, not truth
    }
  }
  return out;
}

/**
 * The trailing run of identical signatures, if it is at least `n` long.
 * Returns the trailing slice on a hit, null otherwise (PRD:775 — "N+
 * identical trailing trail.jsonl failure signatures").
 */
export function duplicateTrailingSignatures(signatures: TrailSignature[], n = 3): TrailSignature[] | null {
  if (signatures.length < n) return null;
  const tail = signatures.slice(-n);
  const first = tail[0]!.signature;
  if (!tail.every((s) => s.signature === first)) return null;
  return tail;
}

/** Decision payload returned by the executor on a taskInterrupt. */
export interface InterruptDecision {
  interrupted: true;
  failureClass: ExecutorFailureClass;
  lastGateExcerpt: string;
  attempts: number;
  trailHash: string;
  /** Human summary (identical count + class + hash). */
  detail: string;
}

/**
 * Append the failure signature to the task trail, then decide whether the
 * trail now ends with N+ identical signatures (default N=3, PRD:775). On a
 * hit, returns the taskInterrupt decision payload (failure class, last gate
 * excerpt, attempts, trail hash); null means keep retrying.
 */
export function evaluateTrailInterrupt(
  repoPath: string,
  taskId: string,
  attempts: number,
  excerpt: string,
  failureClass: ExecutorFailureClass,
  threshold = 3,
): InterruptDecision | null {
  const sig = failureSignature(excerpt);
  appendTrailSignature(repoPath, taskId, {
    attempt: attempts,
    signature: sig,
    failureClass,
    excerpt: excerpt.slice(0, 200),
  });
  const trail = readTrailSignatures(repoPath, taskId);
  const dupes = duplicateTrailingSignatures(trail, threshold);
  if (!dupes) return null;
  const trailHash = createHash('sha256').update(dupes.map((d) => d.signature).join('|')).digest('hex').slice(0, 16);
  return {
    interrupted: true,
    failureClass,
    lastGateExcerpt: dupes[dupes.length - 1]!.excerpt,
    attempts,
    trailHash,
    detail: `${dupes.length} identical ${failureClass} failures (trail hash ${trailHash})`,
  };
}

/**
 * Best-effort abort of the task's worker process tree: kill any devagent
 * headless worker whose cwd is inside this task's worktree (repo-scoped, so
 * the user's interactive sessions are never touched). The spawn already
 * returned by the time the interrupt fires, so this covers lingering orphans
 * that would otherwise keep burning provider spend while the task is already
 * decided terminal.
 */
async function abortWorker(worktreePath: string): Promise<void> {
  try {
    const { findStaleWorkerPids, killStaleProcessTree } = await import('../resilience/reaper.js');
    const stale = findStaleWorkerPids(0, { cwdPrefix: worktreePath });
    for (const s of stale) killStaleProcessTree(s.pid, 'taskInterrupt');
  } catch {
    // best-effort abort only
  }
}

export async function executeTask(args: {
  task: OrchestratorTask;
  board: ProjectBoard;
  repoPath: string;
  timeoutMs: number;
  /** Repo-local lessons file override (see loadLessons). */
  lessonsFile?: string;
  lessonsMaxChars?: number;
  log: RunLogger;
  executor: WorkerName;
}): Promise<{
  ok: boolean;
  worktreePath?: string;
  detail?: string;
  interrupted?: boolean;
  failureClass?: ExecutorFailureClass;
  lastGateExcerpt?: string;
  attempts?: number;
  trailHash?: string;
}> {
  const { task, repoPath, timeoutMs, log, executor } = args;
  const { getWorker } = await import('../workers/index.js');
  const { runTestGate } = await import('../validation/test-gate.js');

  // Model preflight (PRD Phase 4 "Provider model-id validation at dispatch", Q32):
  // reject an invalid config.model BEFORE worktree creation or any worker spend
  // so a bad id fails at the gate in seconds instead of burning attempts
  // mid-board (loop 58: `--model coding` exit-1-in-12s repeated 3x).
  const { validateWorkerModel, loadConfig: loadCfg } = await import('../config.js');
  const modelProblem = validateWorkerModel(executor, loadCfg(repoPath).model);
  if (modelProblem) {
    log.error('task', `${task.id} dispatch preflight failed: ${modelProblem}`, {});
    return { ok: false, detail: `dispatch preflight: ${modelProblem}`, failureClass: 'config' as ExecutorFailureClass };
  }

  const criteria = [...(task.acceptanceCriteria ?? []), ...(task.expectedOutput ? [task.expectedOutput] : [])];
  const ticket = {
    id: task.id,
    title: task.title,
    description: [
      task.prompt,
      task.boundaryConstraints?.length
        ? `\nBoundary constraints (must respect):\n${task.boundaryConstraints.map((c) => `- ${c}`).join('\n')}`
        : '',
      // Targeted re-contracting: retry closes the audited gap, not blind redo
      task.evidenceGaps?.length
        ? `\nPrevious attempt failed independent audit. Close these specific gaps:\n${task.evidenceGaps.map((g) => `- ${g}`).join('\n')}`
        : '',
    ].join(''),
    labels: ['orchestrated'],
    acceptanceCriteria: criteria,
    url: '',
    trackerInternalId: task.id,
  };
  const plan = { ticket, classification: 'endpoint-only' as const, tasks: [], summary: task.title };

  let worktreePath: string | undefined;
  try {
    // branch devagent/<TASKID>-a<attempt>[r<recovery>] keeps retries isolated
    // (fresh tree); recovery grants extend the suffix to avoid branch reuse
    const { attemptSuffix } = await import('./types.js');
    const wt = await createWorktree(repoPath, `${sanitizeTicketId(task.id)}-${attemptSuffix(task.attempts, task.recoveries)}`);
    worktreePath = wt.worktreePath;
  } catch (err) {
    return { ok: false, detail: `worktree creation failed: ${(err as Error).message}`, failureClass: 'worktree' as ExecutorFailureClass };
  }

  const worker = getWorker(executor);
  const lessons = loadLessons(repoPath, args.lessonsFile, args.lessonsMaxChars);
  const prompt = buildImplementationPrompt(plan, lessons);
  const maxAttempts = 2; // in-worker repair loop; scheduler owns cross-wave retries
  const { loadConfig, herdrEnabled } = await import('../config.js');
  const fullCfg = loadConfig(repoPath);
  const resilienceCfg = fullCfg.resilience;
  const noProgressTimeoutMs = resilienceCfg?.noProgressTimeoutMs ?? 10 * 60_000;
  const apiMaxAttempts = resilienceCfg?.apiMaxAttempts;
  const useHerdr = herdrEnabled(fullCfg);
  const model = fullCfg.model;
  const variant = fullCfg.variant;

  // Transient check uses classify module (sync after first import)
  const { isTransientProviderError: isTransient } = await import('../resilience/classify.js');
  const { isNonRetryableApiError: isNonRetryable } = await import('../sessionguard/events.js');

  let attempt = 1;
  let logicAttempts = 0;
  let infraRetries = 0;
  while (logicAttempts < maxAttempts || infraRetries < 200) {
    const spawnOpts: Record<string, unknown> = {
      prompt: attempt === 1 ? prompt : buildRepairPrompt(plan, attempt - 1, 'previous attempt failed the test gate', lessons),
      cwd: worktreePath,
      timeoutMs,
      ...(apiMaxAttempts !== undefined ? { apiMaxAttempts } : {}),
      ...(noProgressTimeoutMs !== undefined ? { noProgressTimeoutMs } : {}),
      ...(useHerdr ? { herdr: true } : {}),
      ...(model ? { model } : {}),
      ...(variant ? { variant } : {}),
      // Q34: ledger identity from the dispatcher, never derived from the
      // (ephemeral) worktree cwd the worker runs in.
      watchdogLedger: { repoPath, taskId: task.id, attempt, worker: executor },
    };
    const result = await worker.spawn(spawnOpts as never);
    try {
      const { findStaleWorkerPids, killStaleProcessTree } = await import('../resilience/reaper.js');
      // Repo-scoped: only headless workers whose cwd is inside this repo's
      // worktrees may be reaped — never the user's interactive sessions.
      const stale = findStaleWorkerPids(noProgressTimeoutMs || 60_000, {
        cwdPrefix: join(repoPath, '.devagent-worktrees'),
      });
      // cwdPrefix guard already filters — skip isDevagentWorker false above
      // For herdr panes the cwd is remote pane cwd, which lsof may not resolve;
      // skip kill for herdr-detected panes (herdr.ts watchdog handles those).
      for (const s of stale) if (!useHerdr) killStaleProcessTree(s.pid);
    } catch {}
    if (result.timedOut || result.exitCode !== 0 || result.errorText || result.noProgress) {
      // Inspect both resultText and errorText: the proxy surfaces transient
      // provider outages (rate-limited empty streams) as stderr-only with no
      // .result field, so resultText alone misses them and the task used to
      // false-fail after 2 attempts instead of retrying.
      // noProgress: the zero-event/empty-output signature (hung worker) —
      // treated as transient infra so the scheduler can fall back cheaply
      // instead of re-burning full-attempt budgets on a dead endpoint.
      const text = [result.resultText, result.errorText].filter(Boolean).join('\n');
      if (
        result.timedOut ||
        result.noProgress ||
        (text && !isNonRetryable(text) && (isTransient(text) || isTransient(result.errorText ?? null)))
      ) {
        infraRetries++;
        try {
          const { recordTransientClass } = await import('../resilience/proxy-state.js');
          recordTransientClass(repoPath, [text, result.timedOut ? 'no-progress watchdog timeout' : ''].filter(Boolean).join(' '));
        } catch {
          // observability must never break the retry
        }
        const { backoffDelay } = await import('../sessionguard/backoff.js');
        await new Promise((r) => setTimeout(r, backoffDelay(infraRetries)));
        attempt++;
        continue;
      }
      logicAttempts++;
      attempt++;

      // Executor failure surface (PRD:775): record the failure signature and
      // abort the worker once N+ identical trailing trail signatures appear.
      const interrupt = evaluateTrailInterrupt(
        repoPath,
        task.id,
        task.attempts,
        text || `worker exited ${result.exitCode}`,
        'worker-error',
      );
      if (interrupt) {
        await abortWorker(worktreePath);
        return { ok: false, worktreePath, ...interrupt, detail: `taskInterrupt: ${interrupt.detail}` };
      }

      if (logicAttempts >= maxAttempts && infraRetries >= 200) break;
      if (logicAttempts >= maxAttempts) break;
      continue;
    }
    const g1 = await runTestGate(worktreePath, timeoutMs);
    if (g1.passed) {
      // Commit the work: uncommitted changes never reach merge-back
      // (live-smoke lesson: gate passed in-tree but merge integrated nothing).
      const { spawnCli } = await import('../workers/spawn-utils.js');
      await spawnCli('git', ['add', '-A'], { cwd: worktreePath, timeoutMs: 30_000 });
      const commit = await spawnCli(
        'git',
        ['commit', '-m', `task ${task.id}: ${task.title}`, '--no-verify'],
        { cwd: worktreePath, timeoutMs: 30_000 },
      );
      if (commit.exitCode !== 0 && !/nothing to commit/.test(commit.stderr + commit.stdout)) {
        return { ok: false, worktreePath, detail: `commit failed: ${commit.stderr.slice(0, 200)}`, failureClass: 'commit' as ExecutorFailureClass };
      }
      return { ok: true, worktreePath };
    }
    log.warn('task', `${task.id} G1 failed on executor attempt ${attempt}`, {});

    // Executor failure surface (PRD:775): record the failure signature and
    // abort the worker once N+ identical trailing trail signatures appear.
    const gateInterrupt = evaluateTrailInterrupt(
      repoPath,
      task.id,
      task.attempts,
      g1.detail ?? 'test gate failed',
      'test-gate',
    );
    if (gateInterrupt) {
      await abortWorker(worktreePath);
      return { ok: false, worktreePath, ...gateInterrupt, detail: `taskInterrupt: ${gateInterrupt.detail}` };
    }

    logicAttempts++;
    attempt++;
    if (logicAttempts >= maxAttempts) break;
  }
  return { ok: false, worktreePath, detail: 'test gate failed after executor attempts', failureClass: 'test-gate' as ExecutorFailureClass };
}
