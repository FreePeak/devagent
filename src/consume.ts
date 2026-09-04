import { existsSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
import { claimNextPending, readTask, setTaskStatus, updateTask, type QueuedTask } from './queue.js';
import { RunLogger } from './logger.js';
import { syntheticTicketFromPrompt } from './task.js';
import type { PipelineDeps } from './pipeline.js';
import { loadConfig, loadOrchestrateConfig } from './config.js';
import { detectTestCommand, type TestCommand } from './validation/test-gate.js';
import { runCli } from './workers/spawn-utils.js';
import { runPipeline } from './pipeline.js';
import { isTransientProviderError } from './resilience/classify.js';

export interface ConsumeOptions {
  repoPath: string;
  autoPr: boolean;
  autoMerge: boolean;
  maxLoops: number;
  timeoutMs: number;
  /** Claim identity for queue file */
  workerId?: string;
}

export interface ConsumeResult {
  ok: boolean;
  taskId?: string;
  detail: string;
  prUrl?: string;
  merged?: boolean;
}

function isConsumeTransient(detail: string): boolean {
  // Also treat watchdog / timeout wording as transient
  if (/timed.?out|watchdog|no-progress/i.test(detail)) return true;
  return isTransientProviderError(detail);
}

export interface RegressionOracleResult {
  /** true when the suite is green, skipped, or disabled; false on a red suite. */
  passed: boolean;
  /** true when no runnable command was found or the gate is disabled. */
  skipped: boolean;
  /** Why the gate skipped (or null when it ran). */
  reason?: 'no-test-command' | 'disabled' | 'worktree-failed' | 'install-failed';
  /** Tail of the failing suite output, for the regression-failed detail. */
  excerpt?: string;
}

/**
 * Run `npm ci --ignore-scripts` in a lockfile'd npm worktree so the suite
 * sees node_modules (a fresh worktree ships without it). Returns a skip
 * result when the install fails: the suite is not run, because a red run
 * caused by missing modules would false-block every merge. Non-npm suites
 * and repos without a lockfile need no install and return null.
 */
async function installSuiteDeps(
  staging: string,
  testCommand: TestCommand,
  timeoutMs: number,
  lg: (level: 'info' | 'warn', message: string, extra: Record<string, unknown>) => void,
): Promise<RegressionOracleResult | null> {
  if (testCommand.cmd !== 'npm') return null;
  const lockfiles = ['package-lock.json', 'npm-shrinkwrap.json'];
  if (!lockfiles.some((f) => existsSync(join(staging, f)))) return null;
  const install = await runCli('npm', ['ci', '--ignore-scripts'], { cwd: staging, timeoutMs });
  if (install.exitCode === 0) return null;
  lg('warn', 'regression oracle skipped: dependency install failed', {
    gate: 'regression',
    stderr: install.stderr.slice(0, 200),
  });
  return { passed: true, skipped: true, reason: 'install-failed' };
}

/**
 * Regression oracle (PRD §17 Phase 4, gate G6): before an auto-merge, check
 * out the PR branch in a throwaway worktree and run the repo's full test
 * suite there. A red suite blocks the merge; a skipped gate (no runnable
 * test command / knob off / failed dependency install) lets auto-merge
 * proceed. The worktree is always removed, even when the suite fails.
 */
export async function runRegressionOracle(
  repoPath: string,
  branch: string,
  opts: { timeoutMs: number; enabled?: boolean; log?: RunLogger },
): Promise<RegressionOracleResult> {
  const lg = (level: 'info' | 'warn', message: string, extra: Record<string, unknown>) => {
    if (opts.log) opts.log[level]('validate', message, extra);
  };
  const enabled = opts.enabled ?? loadOrchestrateConfig(repoPath).regressionOracle;
  if (enabled === false) return { passed: true, skipped: true, reason: 'disabled' };
  const worktreesRoot = join(repoPath, '.devagent-worktrees');
  mkdirSync(worktreesRoot, { recursive: true });
  const staging = join(worktreesRoot, `regression-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`);
  const add = await runCli('git', ['worktree', 'add', '--detach', staging, branch], {
    cwd: repoPath,
    timeoutMs: 60_000,
  });
  if (add.exitCode !== 0) {
    lg('warn', 'regression oracle worktree add failed; skipping gate', {
      gate: 'regression',
      branch,
      stderr: add.stderr.slice(0, 200),
    });
    return { passed: true, skipped: true, reason: 'worktree-failed' };
  }
  try {
    const testCommand = detectTestCommand(staging);
    if (!testCommand) return { passed: true, skipped: true, reason: 'no-test-command' };
    // Fresh worktrees lack node_modules; npm suites with a lockfile get an
    // install first, and a failed install skips the gate (fail-open).
    const install = await installSuiteDeps(staging, testCommand, opts.timeoutMs, lg);
    if (install) return install;
    const run = await runCli(testCommand.cmd, testCommand.args, { cwd: staging, timeoutMs: opts.timeoutMs });
    if (run.exitCode === 0) {
      lg('info', 'regression oracle passed', { gate: 'regression', branch });
      return { passed: true, skipped: false };
    }
    lg('warn', 'regression oracle blocked merge: suite failed', {
      gate: 'regression',
      branch,
      exitCode: run.exitCode,
    });
    const lines = `${run.stdout}${run.stderr}`.trimEnd().split('\n');
    return { passed: false, skipped: false, excerpt: lines.slice(-15).join('\n') };
  } finally {
    const remove = await runCli('git', ['worktree', 'remove', '--force', staging], {
      cwd: repoPath,
      timeoutMs: 60_000,
    });
    if (remove.exitCode !== 0) {
      lg('warn', 'regression oracle worktree remove failed', {
        gate: 'regression',
        path: staging,
        stderr: remove.stderr.slice(0, 200),
      });
    }
  }
}

/** Claim one pending task and run it through the pipeline -> PR -> optional auto-merge. */
export async function consumeOnce(opts: ConsumeOptions): Promise<ConsumeResult> {
  const workerId = opts.workerId ?? `consume-${process.pid}`;
  const task: QueuedTask | null = claimNextPending(opts.repoPath, workerId);
  if (!task) {
    return { ok: true, detail: 'no pending tasks' };
  }

  const log = new RunLogger();
  log.info('consume', `Claimed ${task.id}: ${task.title}`, { workerId });

  try {
    const result = await runQueuedTask(task, opts, log);
    if (result.ok) {
      setTaskStatus(opts.repoPath, task.id, 'done');
      updateTask(opts.repoPath, task.id, { lastError: undefined });
      return { ok: true, taskId: task.id, detail: result.detail, prUrl: result.prUrl, merged: result.merged };
    }
    // Infinite retry for transient infra failures: requeue as pending instead
    // of terminal failed so the next consume loop retries after backoff.
    // Bounded failures (test gate, lint) go to failed as before.
    const config = loadConfig(opts.repoPath);
    const infinite = config.resilience?.apiMaxAttempts === undefined || config.resilience.apiMaxAttempts === Infinity;
    if (infinite && isConsumeTransient(result.detail)) {
      // Reap only stale workers scoped to this repo's worktrees — never
      // interactive sessions or workers for other projects.
      try {
        const { join } = await import('node:path');
        const { findStaleWorkerPids, killStaleProcessTree } = await import('./resilience/reaper.js');
        const stale = findStaleWorkerPids(config.resilience?.noProgressTimeoutMs ?? 10 * 60_000, {
          cwdPrefix: join(opts.repoPath, '.devagent-worktrees'),
        });
        for (const s of stale) if (s.command.includes(task.id)) killStaleProcessTree(s.pid);
      } catch {}
      const { backoffDelay } = await import('./sessionguard/backoff.js');
      // Backoff before requeueing so we don't hot-loop a flapping endpoint.
      await new Promise((r) => setTimeout(r, backoffDelay((task.attempts ?? 0) + 1)));
      updateTask(opts.repoPath, task.id, { status: 'pending', lastError: result.detail.slice(0, 2000) } as never);
      // Best-effort: set pending directly if updateTask path coalesces
      setTaskStatus(opts.repoPath, task.id, 'pending' as never);
      updateTask(opts.repoPath, task.id, { lastError: result.detail.slice(0, 2000) });
      log.warn('consume', `Transient infra failure for ${task.id}, requeued as pending`, { detail: result.detail.slice(0, 120) });
      return { ok: false, taskId: task.id, detail: `${result.detail} (transient — requeued)`, prUrl: result.prUrl, merged: result.merged };
    }
    setTaskStatus(opts.repoPath, task.id, 'failed', result.detail);
    return { ok: false, taskId: task.id, detail: result.detail, prUrl: result.prUrl, merged: result.merged };
  } catch (err) {
    const msg = (err as Error).message;
    const config = loadConfig(opts.repoPath);
    const infinite = config.resilience?.apiMaxAttempts === undefined || config.resilience.apiMaxAttempts === Infinity;
    if (infinite && isConsumeTransient(msg)) {
      // No hard 60_000 threshold — respect configured noProgress timeout
      const staleMs = config.resilience?.noProgressTimeoutMs ?? 10 * 60_000;
      try {
        const { join } = await import('node:path');
        const { findStaleWorkerPids, killStaleProcessTree } = await import('./resilience/reaper.js');
        const stale = findStaleWorkerPids(staleMs, {
          cwdPrefix: join(opts.repoPath, '.devagent-worktrees'),
        });
        for (const s of stale) if (s.command.includes(task.id)) killStaleProcessTree(s.pid);
      } catch {}
      const { backoffDelay } = await import('./sessionguard/backoff.js');
      await new Promise((r) => setTimeout(r, backoffDelay(1)));
      setTaskStatus(opts.repoPath, task.id, 'pending' as never);
      updateTask(opts.repoPath, task.id, { lastError: msg.slice(0, 2000) });
      return { ok: false, taskId: task.id, detail: `crashed: ${msg} (transient — requeued)` };
    }
    setTaskStatus(opts.repoPath, task.id, 'failed', msg);
    log.error('consume', `Task ${task.id} crashed: ${msg}`);
    return { ok: false, taskId: task.id, detail: `crashed: ${msg}` };
  }
}

async function runQueuedTask(
  task: QueuedTask,
  opts: ConsumeOptions,
  log: RunLogger,
): Promise<{ ok: boolean; detail: string; prUrl?: string; merged?: boolean }> {
  const ticket = {
    id: task.id,
    title: task.title,
    description: task.goal + (task.description ? `\n\n${task.description}` : ''),
    labels: [] as string[],
    acceptanceCriteria: task.acceptanceCriteria,
    url: '',
    trackerInternalId: task.id,
  };

  // Build PipelineDeps that use the queued goal as synthetic ticket (no Linear fetch).
  // We reuse the task prompt path: implementStage + gates come from deps-style.
  const cfg = {
    ticketId: task.id,
    repoPath: opts.repoPath,
    worker: loadConfig(opts.repoPath).worker,
    autoPr: opts.autoPr,
    interactive: false,
    maxLoops: opts.maxLoops,
    timeoutMs: opts.timeoutMs,
    dryRun: false,
    cleanup: (loadConfig(opts.repoPath).cleanup ?? 'auto') as 'auto' | 'keep' | 'always',
    dropOrcaWorkspace: Boolean(loadConfig(opts.repoPath).dropOrcaWorkspace),
  };

  // Use buildDeps-equivalent but with fetchTicket stubbed to the queued ticket
  const creds = (await import('./config.js')).loadCredentials();
  const { buildDeps } = await import('./deps.js');
  const deps: PipelineDeps = {
    ...buildDeps(creds as never, cfg, log),
    fetchTicket: async () => ticket,
  };

  const outcomes = await runPipeline(cfg, deps, log);
  const failed = outcomes.find((o) => o.stage === 'failed') as { stage: 'failed'; reason: string } | undefined;
  if (failed) {
    return { ok: false, detail: failed.reason };
  }

  const publish = outcomes.find((o) => o.stage === 'publish') as { stage: 'publish'; prUrl?: string; note: string } | undefined;
  const prUrl = publish?.prUrl;

  if (opts.autoMerge && prUrl) {
    // G5 STRIDE gate (PRD section 11): static review of the branch diff
    // before auto-merge. HIGH/CRITICAL findings block the merge; MEDIUM/LOW
    // are advisory. A missing/failed diff is treated as an empty diff (pass).
    let diff = '';
    const branch =
      (outcomes.find((o) => o.stage === 'implement') as { stage: 'implement'; branch?: string } | undefined)
        ?.branch ?? parsePrBranch(prUrl);
    if (branch) {
      const { spawnCli } = await import('./workers/index.js');
      for (const base of ['main', 'origin/main']) {
        try {
          const res = await spawnCli('git', ['diff', `${base}...${branch}`], {
            cwd: opts.repoPath,
            timeoutMs: 60_000,
          });
          if (res.exitCode === 0 && res.stdout.trim()) {
            diff = res.stdout;
            break;
          }
        } catch {
          // fall through to the next base / empty diff
        }
      }
    }

    const { evaluateStride, parseStrideAllowlist, STRIDE_ALLOWLIST_PATH } = await import(
      './gates/stride.js'
    );

    // Per-path allowlist (PRD Q25): the PR may commit
    // .devagent/stride-allowlist.json so findings in fixture/test files do
    // not stall autoMerge. The file is read from the PR branch itself (so it
    // is reviewable in the diff), never from the base; an absent, unreadable,
    // or malformed allowlist fails closed (no suppression).
    let allowlistPaths: string[] | undefined;
    if (branch) {
      const { spawnCli } = await import('./workers/index.js');
      try {
        const res = await spawnCli('git', ['show', `${branch}:${STRIDE_ALLOWLIST_PATH}`], {
          cwd: opts.repoPath,
          timeoutMs: 15_000,
        });
        if (res.exitCode === 0) {
          const parsed = parseStrideAllowlist(res.stdout);
          if (parsed) allowlistPaths = parsed;
          else log.warn('validate', `G5 STRIDE allowlist ignored (malformed): ${STRIDE_ALLOWLIST_PATH}`);
        }
      } catch {
        // no allowlist on the branch: fail closed, no suppression
      }
    }

    const evaluation = await evaluateStride({ diff, allowlistPaths });
    log.info(
      'validate',
      `G5 STRIDE gate ${evaluation.severityMax === 'HIGH' || evaluation.severityMax === 'CRITICAL' ? 'blocked' : 'passed'}`,
      {
        gate: 'stride',
        severityMax: evaluation.severityMax,
        allowlist: allowlistPaths ?? [],
        findings: evaluation.findings.map((f) => ({ category: f.category, severity: f.severity, file: f.file, line: f.line })),
      },
    );

    if (evaluation.findings.some((f) => f.severity === 'HIGH' || f.severity === 'CRITICAL')) {
      return {
        ok: true,
        detail: `done: ${task.id} -> ${prUrl} (stride gate blocked merge: ${evaluation.severityMax} findings: ${evaluation.findings.length})`,
        prUrl,
        merged: false,
      };
    }

    if (branch) {
      // Regression oracle (PRD §17 Phase 4): full test suite on the PR branch
      // in a throwaway worktree. A red suite blocks the merge; skipped gates
      // (no test command / knob off) proceed to auto-merge.
      const regression = await runRegressionOracle(opts.repoPath, branch, {
        timeoutMs: opts.timeoutMs,
        log,
      });
      if (!regression.passed) {
        const excerpt = regression.excerpt ? `: ${regression.excerpt}` : '';
        return {
          ok: true,
          detail: `done: ${task.id} -> ${prUrl} (regression-failed${excerpt})`,
          prUrl,
          merged: false,
        };
      }
    }

    try {
      const { autoMergePr } = await import('./integrations/github.js');
      await autoMergePr(opts.repoPath, prUrl);
      return { ok: true, detail: `done: ${task.id} -> ${prUrl} (auto-merged)`, prUrl, merged: true };
    } catch (err) {
      return { ok: true, detail: `done: ${task.id} -> ${prUrl} (auto-merge failed: ${(err as Error).message})`, prUrl, merged: false };
    }
  }

  // Self-update hook: after green merge, optionally pull+build
  if (prUrl) {
    const config = loadConfig(opts.repoPath);
    if (config.selfUpdate) {
      try {
        const { runSelfUpdate } = await import('./self-update.js');
        await runSelfUpdate(opts.repoPath, log);
      } catch (err) {
        log.warn('consume', `self-update skipped: ${(err as Error).message}`);
      }
    }
  }

  if (prUrl) return { ok: true, detail: `done: ${task.id} -> ${prUrl}`, prUrl };
  const implement = outcomes.find((o) => o.stage === 'implement') as { ok: boolean } | undefined;
  if (implement && !implement.ok) return { ok: false, detail: `implementation failed for ${task.id}` };
  return { ok: true, detail: `done: ${task.id} (no PR: missing GITHUB_TOKEN or branch)`, prUrl: undefined };
}

/** Derive the PR head branch from a PR URL (/pull/<n> form), or null when unknown. */
function parsePrBranch(prUrl: string): string | null {
  const m = /\/pull\/(\d+)/.exec(prUrl);
  return m ? `devagent/pull-${m[1]}` : null;
}
