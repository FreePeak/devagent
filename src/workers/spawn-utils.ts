import { execFile, execFileSync, type ExecFileOptionsWithStringEncoding, spawn } from 'node:child_process';

export interface SpawnCliOptions {
  cwd: string;
  timeoutMs: number;
  /** Extra env vars merged over the base environment. Never logged or included in results. */
  env?: Record<string, string>;
  /**
   * When true, `env` replaces process.env as the base (still minus
   * NESTED_ENV_BLOCKLIST) instead of being merged over it. Used by worker
   * sandboxing: a scrubbed env cannot unset inherited secrets by merging.
   */
  replaceEnv?: boolean;
  /**
   * Watchdog: kill the child when no output (stdout or stderr) arrives for
   * this long. 0 disables. When killed by the watchdog, `timedOut` is true
   * so callers treat it as a transient provider failure and retry forever.
   * Default 0 for git/gh callers; workers pass 10m when infinite retry is on.
   */
  noProgressTimeoutMs?: number;
}

export interface SpawnCliResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  timedOut: boolean;
}

/**
 * Env vars injected by agent harnesses / CI that corrupt nested CLI
 * dispatches (live-smoke lesson: a parent's ANTHROPIC_MODEL makes child
 * `claude -p` fail model resolution and emit nothing on stdout).
 */
const NESTED_ENV_BLOCKLIST = ['ANTHROPIC_MODEL', 'ANTHROPIC_SMALL_FAST_MODEL', 'CLAUDE_CODE_ENTRYPOINT', 'CLAUDECODE'];

/**
 * Fallback PATH segments for children spawned from minimal-env contexts
 * (launchd plists without EnvironmentVariables, scrubbed worker sandboxes).
 * Without them `git`/`gh`/homebrew tools ENOENT and publish stages die with
 * "spawn git ENOENT" (live-smoke lesson 2026-08-25).
 */
export const FALLBACK_PATH_SEGMENTS = [
  '/opt/homebrew/bin',
  '/usr/local/bin',
  '/usr/bin',
  '/bin',
  '/usr/sbin',
  '/sbin',
];

function ensureUsablePath(env: NodeJS.ProcessEnv): void {
  const p = env.PATH ?? '';
  if (p.includes('/usr/bin') && p.includes('/bin')) return;
  const missing = FALLBACK_PATH_SEGMENTS.filter((seg) => !p.split(':').includes(seg));
  env.PATH = missing.length > 0 ? `${p ? `${p}:` : ''}${missing.join(':')}` : p;
}

export function buildEnv(opts: SpawnCliOptions): NodeJS.ProcessEnv {
  const baseEnv: NodeJS.ProcessEnv = opts.replaceEnv ? { ...opts.env } : { ...process.env };
  for (const k of NESTED_ENV_BLOCKLIST) delete baseEnv[k];
  if (opts.env && !opts.replaceEnv) Object.assign(baseEnv, opts.env);
  ensureUsablePath(baseEnv);
  // Keep PWD consistent with the spawned cwd: CLIs that trust $PWD over
  // getcwd() (live-smoke lesson 2026-08-26: opencode resolved its project
  // root from the inherited PWD and workers operated on the wrong tree)
  // otherwise silently run against the parent's directory while the harness
  // gates inspect the intended one.
  if (opts.cwd) {
    baseEnv.PWD = opts.cwd;
    delete baseEnv.OLDPWD;
  }
  return baseEnv;
}

/**
 * Run a CLI to completion with a hard timeout plus optional no-progress watchdog.
 * On timeout (wall or idle) the child is killed with SIGKILL and timedOut=true
 * is returned (exitCode -1) instead of throwing, so callers can map it to WorkerResult.
 * Idle detection: any stdout/stderr line resets the watchdog clock.
 * Stdin is ignored: headless prompts come via argv; leaving stdin open makes
 * some CLIs block waiting for piped input.
 */
export function spawnCli(cmd: string, args: string[], opts: SpawnCliOptions): Promise<SpawnCliResult> {
  const noProgressMs = opts.noProgressTimeoutMs ?? 0;
  if (noProgressMs > 0) return spawnCliStreaming(cmd, args, opts);

  return new Promise((resolve) => {
    const controller = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, opts.timeoutMs);

    const baseEnv = buildEnv(opts);
    execFile(
      cmd,
      args,
      {
        cwd: opts.cwd,
        timeout: opts.timeoutMs,
        killSignal: 'SIGKILL',
        signal: controller.signal,
        encoding: 'utf8',
        maxBuffer: 64 * 1024 * 1024,
        env: baseEnv,
        // NOTE: stdin must stay a pipe (default). stdio:'ignore' makes
        // `claude -p` emit empty stdout (live-smoke lesson).
      } as ExecFileOptionsWithStringEncoding,
      (error, stdout, stderr) => {
        clearTimeout(timer);
        if (timedOut) {
          resolve({ exitCode: -1, stdout: String(stdout ?? ''), stderr: String(stderr ?? ''), timedOut: true });
          return;
        }
        // Non-timeout failure (non-zero exit): error.code holds the numeric exit code.
        // Spawn failures like ENOENT carry a string code; normalize those to -1 so we
        // never leak errno details through the result shape (and never read as success).
        // A null error is genuine success -> 0.
        const rawCode = (error as { code?: unknown } | null)?.code;
        const exitCode = error === null ? 0 : typeof rawCode === 'number' ? rawCode : -1;
        resolve({
          exitCode: timedOut ? -1 : exitCode,
          stdout: String(stdout ?? ''),
          stderr: String(stderr ?? ''),
          timedOut,
        });
      },
    );
  });
}

/**
 * Streaming variant with no-progress watchdog. Any stdout/stderr output
 * resets the watchdog clock. When the watchdog fires, the child (and its
 * process group when possible) is SIGKILLed. Used by worker adapters for
 * infinite transient retry; git/gh callers keep the execFile path.
 */
export function spawnCliStreaming(
  cmd: string,
  args: string[],
  opts: SpawnCliOptions,
): Promise<SpawnCliResult> {
  const noProgressMs = opts.noProgressTimeoutMs ?? 0;
  return new Promise((resolve) => {
    const env = buildEnv(opts);
    const child = spawn(cmd, args, {
      cwd: opts.cwd,
      env,
      stdio: ['pipe', 'pipe', 'pipe'],
      detached: false,
    });

    const stdoutChunks: string[] = [];
    const stderrChunks: string[] = [];
    let timedOut = false;
    let exitCode: number | null = null;
    let done = false;
    let lastProgressAt = Date.now();

    const touch = () => {
      lastProgressAt = Date.now();
    };

    const finish = () => {
      if (done) return;
      done = true;
      if (wallTimer) clearTimeout(wallTimer);
      if (watchdog) clearInterval(watchdog);
      resolve({
        exitCode: timedOut ? -1 : (exitCode ?? -1),
        stdout: stdoutChunks.join(''),
        stderr: stderrChunks.join(''),
        timedOut,
      });
    };

    child.stdout?.on('data', (c: Buffer) => {
      stdoutChunks.push(c.toString('utf8'));
      touch();
    });
    child.stderr?.on('data', (c: Buffer) => {
      stderrChunks.push(c.toString('utf8'));
      touch();
    });

    // Drain readline to ensure line-based progress is observed even for
    // non-newline chunked parsers; the raw data handler already touches.

    child.on('error', (err: NodeJS.ErrnoException) => {
      if (done) return;
      const code = (err as unknown as { code?: unknown }).code;
      exitCode = typeof code === 'number' ? code : -1;
      // ENOENT etc: no stdout/stderr, not a timeout
      stdoutChunks.push('');
      stderrChunks.push((err as Error).message);
      finish();
    });

    child.on('close', (code) => {
      exitCode = code;
      finish();
    });

    const wallTimer = setTimeout(() => {
      if (done) return;
      timedOut = true;
      try {
        child.kill('SIGKILL');
      } catch {}
      // Fallback: if still alive after 1s, force again then finish
      setTimeout(() => {
        try {
          child.kill('SIGKILL');
        } catch {}
        finish();
      }, 1000).unref?.();
    }, opts.timeoutMs);
    wallTimer.unref?.();

    let watchdog: NodeJS.Timeout | null = null;
    if (noProgressMs > 0) {
      watchdog = setInterval(() => {
        if (done || timedOut) return;
        if (Date.now() - lastProgressAt >= noProgressMs) {
          timedOut = true;
          try {
            child.kill('SIGKILL');
          } catch {}
          // Give close handler a chance; watchdog keeps polling until wallTimer caps
        }
      }, Math.min(1000, Math.max(200, Math.floor(noProgressMs / 4))));
      watchdog.unref?.();
    }

    // Ensure stdin doesn't block CLIs expecting EOF
    try {
      child.stdin?.end();
    } catch {}
  });
}

/**
 * runCli — execFile wrapper that routes through buildEnv so PATH is
 * always usable and NESTED_ENV_BLOCKLIST is honored. Use this anywhere
 * the codebase spawns a git/gh/system CLI directly. Throws on non-zero
 * exit (mirroring child_process.execFile's signature) but normalizes
 * spawn errors to a real Error whose `code` is the underlying errno
 * (e.g. ENOENT) so callers can branch on it.
 *
 * Fixes: src/git/worktree.ts and src/integrations/autopr.ts used to call
 * execFile(cmd, args, { cwd }) with no env, which produced `spawn git
 * ENOENT` whenever the parent process had a minimal PATH (a launchd
 * plist, a worker sandbox, or any future env-scrubbing layer). Delegating
 * to runCli here keeps the entire codebase on a single, env-safe path.
 */
export function runCli(
  cmd: string,
  args: string[],
  opts: SpawnCliOptions,
): Promise<SpawnCliResult> {
  return new Promise((resolve) => {
    const controller = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, opts.timeoutMs);
    const env = buildEnv(opts);
    execFile(
      cmd,
      args,
      {
        cwd: opts.cwd,
        env,
        timeout: opts.timeoutMs,
        killSignal: 'SIGKILL',
        signal: controller.signal,
        encoding: 'utf8',
        maxBuffer: 64 * 1024 * 1024,
      } as ExecFileOptionsWithStringEncoding,
      (error, stdout, stderr) => {
        clearTimeout(timer);
        if (timedOut) {
          resolve({ exitCode: -1, stdout: String(stdout ?? ''), stderr: String(stderr ?? ''), timedOut: true });
          return;
        }
        // Spawn failures (ENOENT) carry a string code; non-zero exits carry
        // a numeric code; null error is genuine success. Normalize so the
        // result shape always has exitCode and never throws. Preserve any
        // stdout/stderr already attached to the Error (the legacy execFile
        // contract attached those fields for callers; tests still rely on
        // them being threaded through).
        const rawCode = (error as { code?: unknown } | null)?.code;
        const exitCode = error === null ? 0 : typeof rawCode === 'number' ? rawCode : -1;
        const errStdout = (error as { stdout?: unknown } | null)?.stdout;
        const errStderr = (error as { stderr?: unknown } | null)?.stderr;
        resolve({
          exitCode: timedOut ? -1 : exitCode,
          stdout: String(errStdout ?? stdout ?? ''),
          stderr: String(errStderr ?? stderr ?? ''),
          timedOut,
        });
      },
    );
  });
}

/**
 * syncCli — execFileSync wrapper that routes through buildEnv. For sites
 * that need synchronous results (reaper.ts ps/lsof probes).
 */
export function syncCli(
  cmd: string,
  args: string[],
  opts: SpawnCliOptions,
): string {
  const env = buildEnv(opts);
  return execFileSync(cmd, args, {
    cwd: opts.cwd,
    env,
    timeout: opts.timeoutMs,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
  }) as string;
}

/**
 * spawnChild — minimal `spawn()` wrapper that routes through buildEnv. For
 * sites that need a long-lived child handle (herdr's daemon, the session
 * guard's claude runner). Returns the ChildProcess so the caller can attach
 * listeners, pipe stdio, or unref for daemon-like behavior.
 */
export function spawnChild(
  cmd: string,
  args: string[],
  opts: SpawnCliOptions & { stdio?: import('node:child_process').StdioOptions },
): import('node:child_process').ChildProcess {
  const env = buildEnv(opts);
  return spawn(cmd, args, {
    cwd: opts.cwd,
    env,
    stdio: opts.stdio ?? 'pipe',
  });
}
