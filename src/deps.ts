import { basename } from 'node:path';
import type { Credentials } from './config.js';
import type { PipelineDeps, ImplementResult } from './pipeline.js';
import type { RunConfig, TicketClass, TicketSpec } from './types.js';
import type { RunLogger } from './logger.js';
import type { ImplementationPlan } from './planner.js';
import type { FanoutLeg } from './workers/fanout.js';
import { fetchTicket, postTicketComment } from './integrations/linear.js';
import { GITHUB_ISSUE_REF } from './integrations/github-issues.js';
import { runMigrationStaticGate } from './validation/runner.js';
import { createWorktree, isGitRepository } from './git/worktree.js';
import { getWorker } from './workers/index.js';
import { buildImplementationPrompt, buildRepairPrompt } from './prompt.js';

export interface StageConfig {
  repoPath: string;
  maxLoops: number;
  timeoutMs: number;
  worker: RunConfig['worker'];
  autoPr: boolean;
  autoMerge?: boolean;
}

/**
 * Offline PipelineDeps for --dry-run (plan-only): synthetic ticket, no
 * credentials, and none of the network-touching stages. The pipeline stops
 * after plan, so only fetchTicket/runGateG3 are ever reachable.
 */
export function buildDryRunDeps(ticketId: string): PipelineDeps {
  const ticket: TicketSpec = {
    id: ticketId,
    title: `[dry-run] ${ticketId}`,
    description: 'Synthetic ticket for plan-only execution; no tracker credentials required.',
    labels: [],
    acceptanceCriteria: [],
  };
  return {
    fetchTicket: async () => ticket,
    runGateG3: () => ({ passed: true, findings: [], detail: 'skipped: dry-run' }),
  };
}

/** Shared PipelineDeps construction for both `run` and `serve` entry points. */
export function buildDeps(creds: Credentials, cfg: StageConfig, log: RunLogger): PipelineDeps {
  return {
    fetchTicket: (id) => {
      // GitHub issue refs (owner/repo#n) route to the GitHub Issues adapter
      if (GITHUB_ISSUE_REF.test(id)) {
        if (!creds.githubToken) {
          throw new Error(`GitHub issue ref ${id} requires GITHUB_TOKEN to be set`);
        }
        return import('./integrations/github-issues.js').then((m) => m.fetchGitHubTicket(id, creds.githubToken!));
      }
      // Jira when JIRA_* env present, else Linear (mirrors cli.ts selection)
      if (process.env.JIRA_DOMAIN && process.env.JIRA_EMAIL && process.env.JIRA_API_TOKEN) {
        return import('./integrations/jira.js').then((m) =>
          m.fetchJiraTicket(id, {
            domain: process.env.JIRA_DOMAIN!,
            email: process.env.JIRA_EMAIL!,
            apiToken: process.env.JIRA_API_TOKEN!,
          }),
        );
      }
      return fetchTicket(id, creds.linearApiKey!);
    },
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
    publishStage: async (_c, planRaw, impl) => {
      const plan = planRaw as ImplementationPlan;
      if (!impl.worktreePath) return undefined;
      const branch = `devagent/${plan.ticket.id}`;
      const { pushBranch } = await import('./integrations/github.js');
      const repoCfg = (await import('./config.js')).loadConfig(impl.worktreePath);
      const baseBranch = repoCfg.githubBaseBranch ?? 'main';

      // GitLab path: GITLAB_* env wins when no GitHub credentials present
      const gitlabToken = process.env.GITLAB_TOKEN;
      const gitlabProject = process.env.GITLAB_PROJECT_ID;
      if (gitlabToken && gitlabProject && !creds.githubToken) {
        await pushBranch(impl.worktreePath, branch);
        const { createMergeRequest } = await import('./integrations/gitlab.js');
        return createMergeRequest(
          {
            baseUrl: process.env.GITLAB_BASE_URL || 'https://gitlab.com',
            projectId: gitlabProject,
            token: gitlabToken,
          },
          {
            sourceBranch: branch,
            targetBranch: baseBranch,
            title: `[${plan.ticket.id}] ${plan.ticket.title}`,
            description: buildPrBody(plan, []),
          },
        );
      }
      if (!creds.githubToken) return undefined;
      const { createPr } = await import('./integrations/github.js');
      const { listChangedFiles } = await import('./git/worktree.js');
      const { spawnCli: gitSpawn } = await import('./workers/spawn-utils.js');

      // Divergence guard (Orca lesson): warn when base moved ahead of our
      // fork point — the PR will be flagged out-of-date upstream.
      try {
        const mb = await gitSpawn('git', ['merge-base', 'HEAD', `origin/${baseBranch}`], { cwd: impl.worktreePath, timeoutMs: 30_000 });
        const ob = await gitSpawn('git', ['rev-parse', `origin/${baseBranch}`], { cwd: impl.worktreePath, timeoutMs: 30_000 });
        if (mb.exitCode === 0 && ob.exitCode === 0 && mb.stdout.trim() !== ob.stdout.trim()) {
          log.warn('publish', `${baseBranch} has advanced since this run started; PR may be out-of-date`);
        }
      } catch {
        // no remote or offline: skip the check silently
      }

      // The branch exists only in the local worktree until pushed
      await pushBranch(impl.worktreePath, branch);
      let changedFiles: string[] = [];
      try {
        changedFiles = await listChangedFiles(impl.worktreePath, baseBranch);
      } catch {
        // evidence is best-effort; never block publishing on it
      }
      const prUrl = await createPr({
        repoPath: cfg.repoPath,
        branch,
        title: `[${plan.ticket.id}] ${plan.ticket.title}`,
        body: buildPrBody(plan, changedFiles),
      });
      if (cfg.autoMerge ?? repoCfg.autoMerge) {
        const n = /pull\/(\d+)/.exec(prUrl)?.[1];
        if (n) {
          // Fire-and-forget: auto-merge waits on CI and must not block the run report.
          void import('./integrations/autopr.js')
            .then((m) => m.autoReviewAndMergeOne(cfg.repoPath, Number(n), { baseBranch }))
            .then((o) => log.info('publish', `auto-merge PR #${n}: ${o.action} (${o.detail.slice(0, 120)})`, {}))
            .catch((err) => log.warn('publish', `auto-merge PR #${n} failed: ${(err as Error).message}`));
        }
      }
      return prUrl;
    },
  };
}

/** Dispatch a worker inside an isolated worktree with the retry loop (FR-IMPL-01..04). */
export async function implementStage(
  cfg: StageConfig & Pick<RunConfig, 'worker' | 'maxLoops'>,
  plan: ImplementationPlan,
  log: RunLogger,
): Promise<ImplementResult> {
  if (cfg.worker === 'both') {
    const { runFanout } = await import('./workers/fanout.js');
    const { runTestGate } = await import('./validation/test-gate.js');
    const git = await import('./git/worktree.js');
    log.info('implement', 'Fan-out mode: dispatching both workers', {});

    // Merge-assist for the fan-out winner (PRD section 17 Phase 4): make the
    // winning leg publishable through the normal pushBranch/createPr path.
    // Every step is best-effort; a cleanup failure never fails the run.
    const canonicalBranch = `devagent/${plan.ticket.id}`;
    const mergeAssistWinner = async (legs: FanoutLeg[], winner: FanoutLeg): Promise<void> => {
      if (!winner.worktreePath) return;
      try {
        const committed = await git.commitAllChanges(
          winner.worktreePath,
          `devagent(${plan.ticket.id}): fan-out winner (${winner.worker})`,
        );
        if (!committed) log.info('implement', 'Fan-out winner had nothing to commit', {});
      } catch (err) {
        log.warn('implement', `Fan-out winner commit failed: ${(err as Error).message}`);
      }
      try {
        await git.renameCurrentBranch(winner.worktreePath, canonicalBranch);
        log.info('implement', `Fan-out winner branch renamed to ${canonicalBranch}`, {});
      } catch (err) {
        log.warn('implement', `Fan-out winner branch rename failed: ${(err as Error).message}`);
      }
      for (const leg of legs) {
        if (leg === winner || !leg.worktreePath) continue;
        try {
          await git.removeWorktree(cfg.repoPath, basename(leg.worktreePath));
          if (leg.branch) await git.deleteBranch(cfg.repoPath, leg.branch);
          log.info('implement', `Fan-out loser cleaned up: ${leg.worker}`, {});
        } catch (err) {
          log.warn('implement', `Fan-out loser cleanup failed (${leg.worker}): ${(err as Error).message}`);
        }
      }
    };

    const winner = await runFanout(plan, ['claude-code', 'opencode'], log, {
      repoPath: cfg.repoPath,
      timeoutMs: cfg.timeoutMs,
      scoreLeg: (wt, ms) => runTestGate(wt, ms).then((r) => r.passed),
      onSelected: mergeAssistWinner,
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

  // Isolated worktree + branch per run (FR-IMPL-01); aborts for git repos instead
  // of silently executing in repo root
  let cwd = cfg.repoPath;
  let worktreePath: string | undefined;
  try {
    const wt = await createWorktree(cfg.repoPath, plan.ticket.id);
    cwd = wt.worktreePath;
    worktreePath = wt.worktreePath;
    log.info('implement', `Worktree ready: ${wt.worktreePath} (branch ${wt.branch})`, {});
  } catch (err) {
    const message = (err as Error).message;
    if (await isGitRepository(cfg.repoPath)) {
      log.error('implement', `Aborting run: worktree creation failed: ${message}`);
      throw new Error(`worktree creation failed: ${message}`);
    }
    log.warn('implement', `Not a git repository, running in repo root: ${message}`);
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

/** PR body with plan, changed-file evidence, and acceptance criteria (FR-DELIVER-01). */
function buildPrBody(plan: ImplementationPlan, changedFiles: string[]): string {
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
    ...(changedFiles.length
      ? ['## Files changed', ...changedFiles.map((f) => `- \`${f}\``), '']
      : []),
    '## Validation',
    '- G3 static migration analysis: passed',
    '- Test suite: see CI run on this branch',
    '',
    '## Acceptance criteria',
    ...(t.acceptanceCriteria.length ? t.acceptanceCriteria.map((c) => `- [ ] ${c}`) : ['- (see ticket)']),
  ].join('\n');
}
