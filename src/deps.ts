import type { Credentials } from './config.js';
import type { PipelineDeps, ImplementResult } from './pipeline.js';
import type { RunConfig, TicketClass } from './types.js';
import type { RunLogger } from './logger.js';
import type { ImplementationPlan } from './planner.js';
import { fetchTicket, postTicketComment } from './integrations/linear.js';
import { runMigrationStaticGate } from './validation/runner.js';
import { createWorktree } from './git/worktree.js';
import { getWorker } from './workers/index.js';
import { buildImplementationPrompt, buildRepairPrompt } from './prompt.js';

export interface StageConfig {
  repoPath: string;
  maxLoops: number;
  timeoutMs: number;
  worker: RunConfig['worker'];
  autoPr: boolean;
}

/** Shared PipelineDeps construction for both `run` and `serve` entry points. */
export function buildDeps(creds: Credentials, cfg: StageConfig, log: RunLogger): PipelineDeps {
  return {
    fetchTicket: (id) => fetchTicket(id, creds.linearApiKey!),
    postTicketComment: (internalId, comment) => postTicketComment(internalId, comment, creds.linearApiKey!),
    runGateG1: (worktreePath, timeoutMs) =>
      import('./validation/test-gate.js').then((m) => m.runTestGate(worktreePath, timeoutMs)),
    runGateG2: (worktreePath, timeoutMs) =>
      import('./validation/migration-apply-gate.js').then((m) => m.runMigrationApplyGate(worktreePath, timeoutMs)),
    runGateG4: async (worktreePath) => {
      const { collectChangedSourceFiles, analyzeAsyncHazards } = await import('./validation/async-review.js');
      const { spawnCli } = await import('./workers/spawn-utils.js');
      const base = (await import('./config.js')).loadConfig(worktreePath).githubBaseBranch ?? 'main';
      const runGit = async (args: string[]) => {
        const r = await spawnCli('git', args, { cwd: worktreePath, timeoutMs: 30_000 });
        return { exitCode: r.exitCode, stdout: r.stdout };
      };
      const files = await collectChangedSourceFiles(worktreePath, base, runGit);
      const findings = analyzeAsyncHazards(files);
      const blocking = findings.some((f) => f.severity === 'high');
      return {
        passed: !blocking,
        detail: `${files.length} changed file(s), ${findings.length} finding(s)${blocking ? ' (blocking high-severity)' : ''}`,
      };
    },
    runGateG3: (repoPath, classification: TicketClass) => {
      const r = runMigrationStaticGate({ repoPath, classification });
      return { passed: r.passed, findings: r.findings, detail: r.detail };
    },
    implementStage: (c, plan, lg) => implementStage(cfg, plan as ImplementationPlan, lg),
    publishStage: async (_c, plan, impl) => {
      if (!impl.worktreePath || !creds.githubToken) return undefined;
      const branch = `devagent/${plan.ticket.id}`;
      const { createPr } = await import('./integrations/github.js');
      return createPr({
        repoPath: cfg.repoPath,
        branch,
        title: `[${plan.ticket.id}] ${plan.ticket.title}`,
        body: buildPrBody(plan),
      });
    },
  };
}

/** Dispatch a worker inside an isolated worktree with the retry loop (FR-IMPL-01..04). */
async function implementStage(
  cfg: StageConfig & Pick<RunConfig, 'worker' | 'maxLoops'>,
  plan: ImplementationPlan,
  log: RunLogger,
): Promise<ImplementResult> {
  if (cfg.worker === 'both') {
    const { runFanout } = await import('./workers/fanout.js');
    const { runTestGate } = await import('./validation/test-gate.js');
    log.info('implement', 'Fan-out mode: dispatching both workers', {});
    const winner = await runFanout(plan, ['claude-code', 'opencode'], log, {
      repoPath: cfg.repoPath,
      timeoutMs: cfg.timeoutMs,
      scoreLeg: (wt, ms) => runTestGate(wt, ms).then((r) => r.passed),
    });
    if (!winner) {
      return { ok: false, worker: 'claude-code', attempts: 1 };
    }
    log.info('implement', `Fan-out winner: ${winner.worker} (tests ${winner.testsPassed})`, {});
    return { ok: true, worker: winner.worker, worktreePath: winner.worktreePath, attempts: 1 };
  }

  const workerName = cfg.worker;
  const worker = getWorker(workerName);
  const prompt = buildImplementationPrompt(plan);
  let repairPrompt = prompt;
  const maxAttempts = Math.max(1, cfg.maxLoops);

  // Isolated worktree + branch per run (FR-IMPL-01); falls back to repoPath on failure
  let cwd = cfg.repoPath;
  let worktreePath: string | undefined;
  try {
    const wt = await createWorktree(cfg.repoPath, plan.ticket.id);
    cwd = wt.worktreePath;
    worktreePath = wt.worktreePath;
    log.info('implement', `Worktree ready: ${wt.worktreePath} (branch ${wt.branch})`, {});
  } catch (err) {
    log.warn('implement', `Worktree creation failed, running in repo root: ${(err as Error).message}`);
  }

  try {
    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      const attemptPrompt = attempt === 1 ? prompt : repairPrompt;
      log.info('implement', `Worker ${workerName} attempt ${attempt}/${maxAttempts}`, {});
      const result = await worker.spawn({ prompt: attemptPrompt, cwd, timeoutMs: cfg.timeoutMs });
      log.info('implement', `Attempt ${attempt} finished`, {
        exitCode: result.exitCode,
        timedOut: result.timedOut,
        durationMs: result.durationMs,
        events: result.events.length,
      });
      if (result.timedOut || result.exitCode !== 0) {
        repairPrompt = buildRepairPrompt(plan, attempt, result.resultText ?? `worker exited ${result.exitCode}`);
        continue;
      }
      // Worker reports success: verify with the repo's own test suite before accepting
      const { runTestGate } = await import('./validation/test-gate.js');
      const g1 = await runTestGate(cwd, cfg.timeoutMs);
      log.info('implement', `Attempt ${attempt} test gate: ${g1.passed ? 'passed' : 'failed'}`, {
        detail: g1.detail?.split('\n')[0],
      });
      if (g1.passed) {
        return { ok: true, worker: workerName, attempts: attempt, worktreePath };
      }
      repairPrompt = buildRepairPrompt(plan, attempt, g1.detail ?? 'test suite failed');
    }
    return { ok: false, worker: workerName, attempts: maxAttempts, worktreePath };
  } finally {
    if (worktreePath) log.info('implement', `Worktree preserved for inspection: ${worktreePath}`, {});
  }
}

/** PR body with plan, ticket link, and evidence placeholders (FR-DELIVER-01). */
function buildPrBody(plan: ImplementationPlan): string {
  const t = plan.ticket;
  return [
    `Closes ${t.id}${t.url ? ` (${t.url})` : ''}.`,
    '',
    '## Summary',
    `Automated implementation classified as **${plan.classification}** by DevAgent.`,
    '',
    '## Plan',
    ...plan.tasks.map((task, i) => `${i + 1}. ${task}`),
    '',
    '## Validation',
    '- G3 static migration analysis: passed',
    '- Test suite: see CI run on this branch',
    '',
    '## Acceptance criteria',
    ...(t.acceptanceCriteria.length ? t.acceptanceCriteria.map((c) => `- [ ] ${c}`) : ['- (see ticket)']),
  ].join('\n');
}
