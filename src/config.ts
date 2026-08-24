import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { WorkerName } from './types.js';

/**
 * Effective configuration: defaults <- devagent.json (repo or cwd) <- env credentials.
 * Credentials come exclusively from the environment (FR-OPS-02).
 */
export interface DevAgentConfig {
  worker: WorkerName | 'both';
  maxLoops: number;
  timeoutMinutes: number;
  pinnedVersions?: Partial<Record<WorkerName, string>>;
  linearTeamId?: string;
  githubBaseBranch?: string;
  /** Opt out of manual review: DevAgent auto-reviews and auto-merges its own PRs. */
  autoMerge?: boolean;
  testCommand?: string;
  /** Repo-local lessons file injected into worker prompts (defaults to .devagent/lessons.md). */
  lessonsFile?: string;
}

export interface Credentials {
  linearApiKey?: string;
  githubToken?: string;
}

export const DEFAULT_CONFIG: DevAgentConfig = {
  worker: 'claude-code',
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

  const config: DevAgentConfig = {
    ...DEFAULT_CONFIG,
    ...fileConfig,
    ...(fileConfig.pinnedVersions ? { pinnedVersions: fileConfig.pinnedVersions } : {}),
  };

  if (!['claude-code', 'opencode', 'both'].includes(config.worker)) {
    throw new Error(`Invalid worker "${config.worker}" in config; expected claude-code, opencode, or both`);
  }
  return config;
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
