import { promises as dnsPromises } from 'node:dns';
import { existsSync, mkdtempSync, writeFileSync } from 'node:fs';
import { homedir, platform, tmpdir } from 'node:os';
import { isIP } from 'node:net';
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
 *    denies writes outside the worktree cwd and temp dirs. Network defaults
 *    to allow (workers must reach LLM APIs) and can be tightened with
 *    DEVAGENT_SANDBOX_NETWORK=deny, which emits `(deny network*)` in the
 *    profile for fully offline worker runs, or with
 *    DEVAGENT_SANDBOX_NETWORK=allowlist, which denies all sockets then
 *    re-allows exactly the resolved endpoints from
 *    DEVAGENT_SANDBOX_NETWORK_ALLOWLIST.
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
   * Network policy. 'allow' (default) leaves socket creation open so workers
   * can reach LLM APIs; 'deny' emits `(deny network*)` for fully offline runs;
   * 'allowlist' denies all sockets then re-allows exactly the resolved
   * endpoints in networkAllowlist (SBPL is last-match-wins).
   */
  network: 'allow' | 'deny' | 'allowlist';
  /**
   * Already-resolved `ip:port` endpoint literals for the 'allowlist' mode.
   * Hostnames must be resolved before reaching this layer — SBPL cannot DNS.
   */
  networkAllowlist?: string[];
}

/** Default port when a DEVAGENT_SANDBOX_NETWORK_ALLOWLIST entry omits one. */
const DEFAULT_ALLOWLIST_PORT = 443;

/** Split an `host[:port]` allowlist entry, defaulting the port to 443. */
function splitHostPort(entry: string): { host: string; port: number } {
  if (isIP(entry)) return { host: entry, port: DEFAULT_ALLOWLIST_PORT };
  const bracketed = entry.match(/^\[(.+)\](?::(\d+))?$/);
  if (bracketed) {
    return {
      host: bracketed[1]!,
      port: bracketed[2] ? Number(bracketed[2]) : DEFAULT_ALLOWLIST_PORT,
    };
  }
  const colon = entry.lastIndexOf(':');
  if (colon > -1 && /^\d+$/.test(entry.slice(colon + 1))) {
    return { host: entry.slice(0, colon), port: Number(entry.slice(colon + 1)) };
  }
  return { host: entry, port: DEFAULT_ALLOWLIST_PORT };
}

/**
 * Resolve DEVAGENT_SANDBOX_NETWORK_ALLOWLIST entries to literal `ip:port`
 * endpoints. Hostnames go through node:dns lookup (all addresses); entries
 * that are already IPv4/IPv6 literals pass through untouched.
 */
export async function resolveNetworkAllowlist(raw: string): Promise<string[]> {
  const entries = raw
    .split(',')
    .map((e) => e.trim())
    .filter(Boolean);
  if (!entries.length) {
    throw new Error(
      'DEVAGENT_SANDBOX_NETWORK=allowlist requires a non-empty ' +
        'DEVAGENT_SANDBOX_NETWORK_ALLOWLIST (comma-separated host[:port] entries)',
    );
  }
  const endpoints: string[] = [];
  for (const entry of entries) {
    const { host, port } = splitHostPort(entry);
    if (isIP(host)) {
      endpoints.push(`${host}:${port}`);
      continue;
    }
    let addresses;
    try {
      addresses = await dnsPromises.lookup(host, { all: true });
    } catch {
      throw new Error(
        `DEVAGENT_SANDBOX_NETWORK_ALLOWLIST: unresolvable host "${host}" ` +
          '(from entry "' + entry + '")',
      );
    }
    if (!addresses.length) {
      throw new Error(
        `DEVAGENT_SANDBOX_NETWORK_ALLOWLIST: unresolvable host "${host}" ` +
          '(from entry "' + entry + '")',
      );
    }
    for (const { address } of addresses) endpoints.push(`${address}:${port}`);
  }
  return endpoints;
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
    // named knob: SBPL is last-match-wins, so network clauses must come after
    // (allow default) to take effect; allowlist re-allows must come after the
    // blanket deny.
    ...(policy.network === 'allow'
      ? ['(allow network)']
      : policy.network === 'deny'
        ? ['(deny network*)']
        : [
            '(deny network*)',
            ...(policy.networkAllowlist ?? []).map(
              (endpoint) => `(allow network-outbound (remote ip "${endpoint}"))`,
            ),
          ]),
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
    const network = process.env.DEVAGENT_SANDBOX_NETWORK ?? 'allow';
    const profile = buildSeatbeltProfile({
      writablePaths: [opts.cwd],
      network:
        network === 'deny' || network === 'allowlist'
          ? network
          : 'allow',
      ...(network === 'allowlist'
        ? {
            networkAllowlist: await resolveNetworkAllowlist(
              process.env.DEVAGENT_SANDBOX_NETWORK_ALLOWLIST ?? '',
            ),
          }
        : {}),
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
