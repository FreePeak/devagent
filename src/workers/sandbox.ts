import { existsSync, mkdtempSync, writeFileSync } from 'node:fs';
import { homedir, platform, tmpdir } from 'node:os';
import { join } from 'node:path';
import type { SpawnCliOptions } from './spawn-utils.js';

/**
 * Worker sandbox isolation (PRD section 17 Phase 4: deeper sandbox isolation).
 *
 * Workers execute inside untrusted target repos where prompt injection can run
 * arbitrary shell, so agent-CLI spawns must not inherit the orchestrator's full
 * environment (GITHUB_TOKEN, cloud credentials, ...). Two layers:
 *
 * 1. Env scrubbing (default on): secret-shaped env vars are stripped from
 *    claude-code / opencode spawns. Git, docker, gh and test-runner spawns keep
 *    the untouched parent env — they legitimately need credentials.
 * 2. Seatbelt confinement (opt-in via DEVAGENT_SANDBOX=seatbelt, darwin only):
 *    worker commands run under sandbox-exec with a generated profile that
 *    denies writes outside the worktree cwd and temp dirs. Network stays
 *    default-allow this loop (workers must reach LLM APIs) but is a named
 *    policy knob so a future loop can tighten it.
 */

/** Env vars the agent CLIs need to authenticate and run; never stripped. */
const DEFAULT_ENV_ALLOWLIST = [
  // process basics
  'PATH', 'HOME', 'SHELL', 'USER', 'LOGNAME', 'TMPDIR', 'TEMP', 'TMP',
  'TERM', 'TERM_PROGRAM', 'LANG', 'LC_ALL', 'TZ',
  // LLM providers used by the worker CLIs themselves
  'ANTHROPIC_API_KEY', 'ANTHROPIC_BASE_URL', 'ANTHROPIC_AUTH_TOKEN',
  'OPENAI_API_KEY', 'OPENAI_BASE_URL',
  // opencode provider config
  'OPENCODE_API_KEY',
];

/**
 * Secret-shaped variable names to strip from worker environments.
 * Order does not matter; a var is stripped if any pattern matches and it is
 * not on the allowlist.
 */
const CREDENTIAL_PATTERNS = [
  /TOKEN/i,
  /SECRET/i,
  /PASS(WD|WORD)/i,
  /_API_KEY$/,
  /^AWS_/,
  /^AZURE_/,
  /^GOOGLE_|^GCP_/,
  /^DOCKER|^NPM_|^PYPI_/,
  /^SSH_AUTH_SOCK$/, // agent forwarding leaks host credentials into children
  /^GIT_.*_CREDENTIALS?$/,
];

export interface SanitizeWorkerEnvResult {
  /** Scrubbed copy of the input environment. */
  env: Record<string, string>;
  /** Names of stripped variables, for audit logging. */
  stripped: string[];
}

function allowlist(extraAllowlist?: string[]): Set<string> {
  const allowed = new Set(DEFAULT_ENV_ALLOWLIST);
  const raw = process.env.DEVAGENT_WORKER_ENV_ALLOWLIST;
  if (raw) {
    for (const name of raw.split(',')) {
      const trimmed = name.trim();
      if (trimmed) allowed.add(trimmed);
    }
  }
  if (extraAllowlist) {
    for (const name of extraAllowlist) allowed.add(name);
  }
  return allowed;
}

/**
 * Strip credential-shaped env vars from a worker environment.
 * Default-on for all agent-CLI worker spawns; extend with
 * DEVAGENT_WORKER_ENV_ALLOWLIST (comma-separated exact names).
 */
export function sanitizeWorkerEnv(
  baseEnv: NodeJS.ProcessEnv,
  extraAllowlist?: string[],
): SanitizeWorkerEnvResult {
  const allowed = allowlist(extraAllowlist);
  const env: Record<string, string> = {};
  const stripped: string[] = [];
  for (const [name, value] of Object.entries(baseEnv)) {
    if (value === undefined) continue;
    if (!allowed.has(name) && CREDENTIAL_PATTERNS.some((p) => p.test(name))) {
      stripped.push(name);
      continue;
    }
    env[name] = value;
  }
  return { env, stripped };
}

export interface SandboxPolicy {
  /** Paths workers may write to (the worktree cwd goes here). */
  writablePaths: string[];
  /**
   * Network policy. Only 'allow' is implemented this loop — workers must reach
   * LLM APIs — but it is a named field so egress tightening is additive.
   */
  network: 'allow';
}

/**
 * Generate an SBPL profile: everything allowed by default except file writes
 * outside the policy's paths, system temp dirs, and the agent config home.
 */
export function buildSeatbeltProfile(policy: SandboxPolicy): string {
  const home = homedir();
  const writePaths = [
    ...policy.writablePaths,
    '/private/tmp', '/tmp',
    '/private/var/folders', // macOS per-user temp/cache roots
    join(home, '.claude'),
    join(home, '.local', 'share'), // opencode data dir lives here
    join(home, '.cache'),
    join(home, '.npm'),
  ];
  const clauses = writePaths.map((p) => `    (subpath "${p}")`).join('\n');
  return [
    '(version 1)',
    '(allow default)',
    `(deny file-write*)`,
    '(allow file-write*',
    clauses,
    ')',
    // named knob: network stays open for LLM API traffic this loop
    ...(policy.network === 'allow' ? ['(allow network)'] : []),
    '',
  ].join('\n');
}

export interface PreparedWorkerSpawn {
  cmd: string;
  args: string[];
  opts: SpawnCliOptions & { replaceEnv?: boolean };
  /** Names of env vars stripped by scrubbing (audit trail). */
  strippedEnv: string[];
}

/**
 * Shared preparation for both worker adapters so dispatch paths cannot
 * diverge: scrubs the child env (default on) and, when DEVAGENT_SANDBOX=
 * seatbelt on darwin, wraps the command with sandbox-exec using a generated
 * profile scoped to opts.cwd. Fails loudly when confinement is requested but
 * unavailable — silent fail-open would be worse than a crashed spawn.
 */
export async function prepareWorkerSpawn(
  cmd: string,
  args: string[],
  opts: SpawnCliOptions,
): Promise<PreparedWorkerSpawn> {
  const { env: scrubbed, stripped } = sanitizeWorkerEnv(process.env);
  // Caller-provided extras (e.g. model config) still win over the scrubbed base.
  const mergedEnv = opts.env ? { ...scrubbed, ...opts.env } : scrubbed;
  let finalCmd = cmd;
  let finalArgs = [...args];

  if (process.env.DEVAGENT_SANDBOX === 'seatbelt') {
    if (platform() !== 'darwin') {
      throw new Error('DEVAGENT_SANDBOX=seatbelt requires darwin (sandbox-exec); got ' + platform());
    }
    const seatbeltBin = '/usr/bin/sandbox-exec';
    if (!existsSync(seatbeltBin)) {
      throw new Error('DEVAGENT_SANDBOX=seatbelt requested but ' + seatbeltBin + ' is missing');
    }
    const profile = buildSeatbeltProfile({
      writablePaths: [opts.cwd],
      network: 'allow',
    });
    const dir = mkdtempSync(join(tmpdir(), 'devagent-sb-'));
    const profilePath = join(dir, 'worker.sb');
    writeFileSync(profilePath, profile, { mode: 0o600 });
    finalCmd = seatbeltBin;
    finalArgs = ['-f', profilePath, cmd, ...finalArgs];
  }

  return {
    cmd: finalCmd,
    args: finalArgs,
    opts: { ...opts, env: mergedEnv, replaceEnv: true },
    strippedEnv: stripped,
  };
}
