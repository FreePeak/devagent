import type { RunLogger } from './logger.js';

/**
 * Remote execution transport for prompt-driven tasks (Phase 4 backlog).
 * Instead of running the pipeline in the local workspace, delegate the whole
 * `devagent task` run to a shared host over SSH so worker capacity is pooled
 * across repos instead of per-workspace. The local machine is a thin client:
 * it verifies the host is usable, ships the prompt over an SSH command, and
 * extracts the PR URL from the remote output.
 *
 * Scope note (deliberate): no repo mirroring/sync here — the shared host owns
 * its checkout. Fan-out and orchestrated runs are not remote-dispatched yet.
 */

export interface RemoteTarget {
  user?: string;
  host: string;
  /** Absolute repo path on the remote host. */
  path: string;
  port?: number;
}

/**
 * Parse a remote target. Accepted forms:
 *   user@host:/srv/repos/app
 *   host:/srv/repos/app
 *   ssh://user@host:2222/srv/repos/app
 */
export function parseRemoteTarget(target: string): RemoteTarget {
  const trimmed = target.trim();
  if (!trimmed) throw new Error('empty remote target');

  if (trimmed.startsWith('ssh://')) {
    let rest = trimmed.slice('ssh://'.length);
    let port: number | undefined;
    const slash = rest.indexOf('/');
    if (slash === -1) throw new Error(`remote target "${target}" needs a repo path after the host`);
    let authority = rest.slice(0, slash);
    const colon = authority.lastIndexOf(':');
    if (colon !== -1) {
      const p = Number(authority.slice(colon + 1));
      if (!Number.isInteger(p) || p <= 0 || p > 65535) throw new Error(`invalid ssh port in "${target}"`);
      port = p;
      authority = authority.slice(0, colon);
    }
    let user: string | undefined;
    const at = authority.indexOf('@');
    if (at !== -1) {
      user = authority.slice(0, at);
      authority = authority.slice(at + 1);
    }
    if (!authority) throw new Error(`missing host in "${target}"`);
    return { user, host: authority, path: rest.slice(slash), port };
  }

  const colon = trimmed.indexOf(':');
  if (colon === -1 || colon === 0) throw new Error(`remote target "${target}" expected [user@]host:/path/to/repo`);
  const authority = trimmed.slice(0, colon);
  const path = trimmed.slice(colon + 1);
  if (!path.startsWith('/')) throw new Error(`remote path must be absolute in "${target}"`);
  let user: string | undefined;
  let host = authority;
  const at = authority.indexOf('@');
  if (at !== -1) {
    user = authority.slice(0, at);
    host = authority.slice(at + 1);
  }
  if (!host) throw new Error(`missing host in "${target}"`);
  return { user, host, path };
}

/** Single-quote a fragment for POSIX shells (the safe default for ssh). */
export function shellQuote(fragment: string): string {
  return `'${fragment.replace(/'/g, `'\\''`)}'`;
}

/** Build the argv for an ssh invocation running one shell command remotely. */
export function buildSshArgs(target: RemoteTarget, remoteCmd: string): string[] {
  const args = ['ssh', '-o', 'BatchMode=yes'];
  if (target.port !== undefined) args.push('-p', String(target.port));
  args.push(target.user ? `${target.user}@${target.host}` : target.host, remoteCmd);
  return args;
}

export interface RunRemoteTaskOptions {
  target: string;
  prompt: string;
  worker?: string;
  timeoutMs: number;
  log: RunLogger;
}

export interface RemoteRunResult {
  ok: boolean;
  prUrl?: string;
  note: string;
}

export interface RemoteDeps {
  /** Injected runner so tests never touch a real network. Returns stdout + exit code. */
  run(argv: string[], timeoutMs: number): Promise<{ exitCode: number; stdout: string }>;
}

const PR_URL_RE = /https:\/\/github\.com\/[^\s)"']+\/pull\/\d+/;

function extractPrUrl(stdout: string): string | undefined {
  return PR_URL_RE.exec(stdout)?.[0];
}

/**
 * Delegate a `devagent task` run to a remote host:
 * 1. preflight — devagent installed on PATH and target path is a git repo
 * 2. dispatch — `devagent task --prompt <prompt> --auto-pr` on the host
 * The remote side owns credentials and workers; we only report its outcome.
 */
export async function runRemoteTask(
  opts: RunRemoteTaskOptions,
  deps: RemoteDeps,
): Promise<RemoteRunResult> {
  let target: RemoteTarget;
  try {
    target = parseRemoteTarget(opts.target);
  } catch (err) {
    return { ok: false, note: (err as Error).message };
  }

  // Preflight: fail fast (cheap probe) instead of burning the full timeout on
  // an unreachable host or missing devagent install (loop-59 hang lesson).
  const preflightCmd =
    `command -v devagent >/dev/null && test -d ${shellQuote(target.path)} && git -C ${shellQuote(target.path)} rev-parse --git-dir >/dev/null`;
  const preflight = await deps.run(buildSshArgs(target, preflightCmd), Math.min(opts.timeoutMs, 15_000));
  if (preflight.exitCode !== 0) {
    opts.log.warn('task', `remote preflight failed on ${target.host}`, { exitCode: preflight.exitCode });
    return {
      ok: false,
      note: `remote preflight failed on ${target.host}: need devagent on PATH and a git repo at ${target.path}`,
    };
  }

  const parts = [
    `cd ${shellQuote(target.path)}`,
    `devagent task ${shellQuote(opts.prompt)} --auto-pr`,
  ];
  if (opts.worker) parts.push(`--worker ${shellQuote(opts.worker)}`);
  const dispatch = await deps.run(buildSshArgs(target, parts.join(' && ')), opts.timeoutMs);
  const prUrl = extractPrUrl(dispatch.stdout);
  if (dispatch.exitCode !== 0) {
    return { ok: false, prUrl, note: `remote task failed on ${target.host} (exit ${dispatch.exitCode})${prUrl ? `, PR opened anyway: ${prUrl}` : ''}` };
  }
  return {
    ok: true,
    prUrl,
    note: prUrl ? `remote PR opened: ${prUrl}` : `remote task finished without a PR URL`,
  };
}
