import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { WorkerName } from './types.js';
import { validateModelId } from './workers/model-id.js';

/**
 * Effective configuration: defaults <- devagent.json (repo or cwd) <- env credentials.
 * Credentials come exclusively from the environment (FR-OPS-02).
 */
/**
 * Post-run worktree disposal policy (auto-cleanup stage).
 * - 'auto'   (default): success -> snapshot+remove the run worktree;
 *            failure -> preserve it for debugging.
 * - 'keep':  never remove (pre-2.0 preserve-for-inspection behavior).
 * - 'always': snapshot+remove even when the run failed.
 */
export type CleanupMode = 'auto' | 'keep' | 'always';

export interface ScoutConfig {
  enabled?: boolean;
  worker?: WorkerName;
  intervalMinutes?: number;
  maxQueued?: number;
  /** Model override forwarded to the scout worker CLI (provider/model). */
  model?: string;
  /**
   * Fetch + fast-forward origin before each live cycle so docs/PRD.md reads
   * see the operator's latest edits (default true; false skips the sync).
   */
  syncDocs?: boolean;
}

export interface DevAgentConfig {
  worker: WorkerName | 'both';
  maxLoops: number;
  timeoutMinutes: number;
  pinnedVersions?: Partial<Record<WorkerName, string>>;
  linearTeamId?: string;
  githubBaseBranch?: string;
  /** Opt out of manual review: DevAgent auto-reviews and auto-merges its own PRs. */
  autoMerge?: boolean;
  /** Model override forwarded to the worker CLI (provider/model, e.g. opencode-go/ox-alpha-free). */
  model?: string;
  /** Variant override forwarded to the worker CLI (e.g. max, high). */
  variant?: string;
  testCommand?: string;
  /** Repo-local lessons file injected into worker prompts (defaults to .devagent/lessons.md). */
  lessonsFile?: string;
  cleanup?: CleanupMode;
  /** When repoPath is an Orca-managed workspace, drop card+dir via orca-cli after done. Opt-in. */
  dropOrcaWorkspace?: boolean;
  /** 24/7 scout: research → PRD → queue (FR-SCOUT-01). Absent = disabled. */
  scout?: ScoutConfig;
  /** Queue storage locations; defaults .devagent/queue + .devagent/prds */
  queue?: { dir?: string; prdsDir?: string };
  /** Self-update devagent after successful merge (FR-SELF-01). Opt-in. */
  selfUpdate?: boolean;
  /**
   * Resilience: worker retry budget is Infinity by default; this caps
   * apiMaxAttempts when set. Null/false disables the watchdog; a number
   * enables the no-progress watchdog (default 10m when resilience block present,
   * else 0 for back-compat). Env overrides: DEVAGENT_API_MAX_ATTEMPTS, DEVAGENT_NO_PROGRESS_TIMEOUT_MS.
   */
  resilience?: { apiMaxAttempts?: number; noProgressTimeoutMs?: number };
  /**
   * Herdr runtime: run worker CLIs inside herdr (https://github.com/herdrdev/herdr)
   * panes in a dedicated persistent session so runs are visible, reattachable,
   * and survive client disconnects. Opt-in. Env overrides:
   * DEVAGENT_HERDR=1|0, DEVAGENT_HERDR_SESSION=<name>.
   */
  herdr?: { enabled?: boolean; session?: string };
  /**
   * Spawn visibility (FR-VIS-01/04): whether worker CLIs launch in herdr panes
   * the operator can jump into ("visible") or as plain child processes
   * ("headless"). Resolved by spawnVisibility(): DEVAGENT_VISIBILITY env >
   * config > "visible". Independent of herdr.enabled, which stays the
   * opt-in gate for runtime availability.
   */
  spawn?: { visibility?: 'visible' | 'headless' };
  /** Character budget for injected lessons; oldest entries are dropped whole (default 4000). */
  lessonsMaxChars?: number;
  /**
   * Trigram-Jaccard similarity threshold for the lessons dedupe guard (PRD
   * Phase 4 "Lessons eval guard"): a candidate lesson at or above this
   * similarity to an existing entry is rejected before append (default 0.8,
   * see src/lessons/guard.ts). Must be within [0, 1].
   */
  lessonsDedupeSimilarity?: number;
  /**
   * Zombie-PR hygiene sweep (`devagent autosweep --stale-prs`): open PRs that
   * are not mergeable candidates get skipped with a comment, and PRs red
   * across `graceDays` get auto-closed. dryRun (the default) only reports.
   */
  zombiePrs?: { graceDays?: number; dryRun?: boolean };
  /**
   * Zombie-PR hygiene for `devagent/TASK-*` PRs (PRD §17, `devagent
   * pr-hygiene`): auto-close PRs whose head base branch was merged or
   * deleted (base-superseded), flag PRs red across `graceHours` (default 24)
   * and skip autoMerge until a green check arrives. dryRun (the default)
   * only reports.
   */
  prHygiene?: { graceHours?: number; dryRun?: boolean };
}

export interface Credentials {
  linearApiKey?: string;
  githubToken?: string;
}

export const DEFAULT_CONFIG: DevAgentConfig = {
  worker: 'omp',
  maxLoops: 3,
  timeoutMinutes: 30,
  githubBaseBranch: 'main',
};

const CONFIG_FILENAMES = ['devagent.json', '.devagent.json'];

export function loadConfig(repoPath: string = process.cwd()): DevAgentConfig {
  let fileConfig: Partial<DevAgentConfig> = {};
  for (const name of CONFIG_FILENAMES) {
    const p = join(repoPath, name);
    if (existsSync(p)) {
      try {
        fileConfig = JSON.parse(readFileSync(p, 'utf8')) as Partial<DevAgentConfig>;
      } catch (err) {
        throw new Error(`Invalid JSON in ${p}: ${(err as Error).message}`);
      }
      break;
    }
  }

  const envApiMax = process.env.DEVAGENT_API_MAX_ATTEMPTS;
  const envNoProgress = process.env.DEVAGENT_NO_PROGRESS_TIMEOUT_MS;
  const envResilience: Partial<NonNullable<DevAgentConfig['resilience']>> = {};
  if (envApiMax !== undefined && envApiMax !== '') {
    const n = Number(envApiMax);
    if (Number.isFinite(n) && n > 0) envResilience.apiMaxAttempts = n;
    else if (envApiMax === 'Infinity' || envApiMax.toLowerCase() === 'infinity') envResilience.apiMaxAttempts = Infinity;
  }
  if (envNoProgress !== undefined && envNoProgress !== '') {
    const n = Number(envNoProgress);
    if (Number.isFinite(n) && n >= 0) envResilience.noProgressTimeoutMs = n;
  }

  const config: DevAgentConfig = {
    ...DEFAULT_CONFIG,
    ...fileConfig,
    ...(fileConfig.pinnedVersions ? { pinnedVersions: fileConfig.pinnedVersions } : {}),
    ...((fileConfig.resilience || Object.keys(envResilience).length)
      ? { resilience: { ...fileConfig.resilience, ...envResilience } }
      : {}),
  };

  if (!['claude-code', 'opencode', 'omp', 'pi', 'both'].includes(config.worker)) {
    throw new Error(`Invalid worker "${config.worker}" in config; expected claude-code, opencode, omp, pi, or both`);
  }
  if (config.cleanup !== undefined && !['auto', 'keep', 'always'].includes(config.cleanup)) {
    throw new Error(`Invalid cleanup "${config.cleanup}" in config; expected auto, keep, or always`);
  }
  if (config.scout !== undefined) {
    if (config.scout.worker !== undefined && !['claude-code', 'opencode', 'omp', 'pi'].includes(config.scout.worker)) {
      throw new Error(`Invalid scout.worker "${config.scout.worker}"; expected claude-code, opencode, omp, or pi`);
    }
    if (config.scout.intervalMinutes !== undefined && (!Number.isFinite(config.scout.intervalMinutes) || config.scout.intervalMinutes < 1)) {
      throw new Error(`Invalid scout.intervalMinutes "${config.scout.intervalMinutes}"; expected >= 1`);
    }
    if (config.scout.maxQueued !== undefined && (!Number.isFinite(config.scout.maxQueued) || config.scout.maxQueued < 1)) {
      throw new Error(`Invalid scout.maxQueued "${config.scout.maxQueued}"; expected >= 1`);
    }
  }
  if (config.resilience !== undefined) {
    const r = config.resilience;
    if (r.apiMaxAttempts !== undefined && !(Number.isFinite(r.apiMaxAttempts) && r.apiMaxAttempts > 0 || r.apiMaxAttempts === Infinity)) {
      throw new Error(`Invalid resilience.apiMaxAttempts "${r.apiMaxAttempts}"; expected positive number or Infinity`);
    }
    if (r.noProgressTimeoutMs !== undefined && (!Number.isFinite(r.noProgressTimeoutMs) || r.noProgressTimeoutMs < 0)) {
      throw new Error(`Invalid resilience.noProgressTimeoutMs "${r.noProgressTimeoutMs}"; expected >= 0`);
    }
  }
  if (config.herdr !== undefined) {
    if (config.herdr.session !== undefined && !/^[a-z][a-z0-9_-]{0,31}$/.test(config.herdr.session)) {
      throw new Error(`Invalid herdr.session "${config.herdr.session}"; expected [a-z][a-z0-9_-]{0,31}`);
    }
  }
  if (config.zombiePrs !== undefined) {
    const z = config.zombiePrs;
    if (z.graceDays !== undefined && (!Number.isFinite(z.graceDays) || z.graceDays < 0)) {
      throw new Error(`Invalid zombiePrs.graceDays "${z.graceDays}"; expected >= 0`);
    }
  }
  if (config.prHygiene !== undefined) {
    const h = config.prHygiene;
    if (h.graceHours !== undefined && (!Number.isFinite(h.graceHours) || h.graceHours < 0)) {
      throw new Error(`Invalid prHygiene.graceHours "${h.graceHours}"; expected >= 0`);
    }
  }
  if (config.lessonsDedupeSimilarity !== undefined) {
    const t = config.lessonsDedupeSimilarity;
    if (!Number.isFinite(t) || t < 0 || t > 1) {
      throw new Error(`Invalid lessonsDedupeSimilarity "${t}"; expected a number between 0 and 1`);
    }
  }
  return config;
}

/**
 * Validate `config.model` against the worker adapter's accepted id shape at
 * dispatch preflight (PRD Phase 4 backlog: "Provider model-id validation at
 * dispatch", Q32).
 *
 * Per-adapter id shapes are declared in the worker registry
 * (src/workers/model-id.ts, following the Q33 `isProgress` precedent); this
 * wrapper keeps the existing call sites and tests stable.
 *
 * Returns null when the model is acceptable; otherwise a one-line reason
 * suitable for failure detail / log output.
 */
export function validateWorkerModel(worker: WorkerName, model: string | undefined): string | null {
  return validateModelId(worker, model);
}

/**
 * Whether worker commands should execute inside herdr panes. Config opt-in
 * (`herdr.enabled`), with DEVAGENT_HERDR=1|0 as an env override.
 */
export function herdrEnabled(cfg: DevAgentConfig = loadConfig()): boolean {
  const env = process.env.DEVAGENT_HERDR;
  if (env !== undefined && env !== '') {
    return env !== '0' && env.toLowerCase() !== 'false';
  }
  return cfg.herdr?.enabled === true;
}

/**
 * Spawn visibility resolution (FR-VIS-01/04): DEVAGENT_VISIBILITY env wins,
 * then config `spawn.visibility`, defaulting to "visible" — worker CLIs land
 * in observable herdr panes unless the operator opts out. Unrecognized env
 * values fall through to the config/default rather than failing dispatch.
 */
export function spawnVisibility(cfg: DevAgentConfig = loadConfig()): 'visible' | 'headless' {
  const env = process.env.DEVAGENT_VISIBILITY;
  if (env === 'visible' || env === 'headless') return env;
  return cfg.spawn?.visibility === 'headless' ? 'headless' : 'visible';
}

/** Target herdr session name (config `herdr.session`, env DEVAGENT_HERDR_SESSION, else "devagent"). */
export function herdrSessionName(cfg: DevAgentConfig = loadConfig()): string {
  return process.env.DEVAGENT_HERDR_SESSION || cfg.herdr?.session || 'devagent';
}

export function loadCredentials(env: NodeJS.ProcessEnv = process.env): Credentials {
  return {
    linearApiKey: env.LINEAR_API_KEY,
    githubToken: env.GITHUB_TOKEN,
  };
}

/** Report which credentials are present without ever printing values (FR-OPS-02). */
export function credentialStatus(creds: Credentials): Record<string, boolean> {
  return {
    LINEAR_API_KEY: Boolean(creds.linearApiKey),
    GITHUB_TOKEN: Boolean(creds.githubToken),
  };
}
