import { execFile } from 'node:child_process';

export interface SpawnCliOptions {
  cwd: string;
  timeoutMs: number;
  /** Extra env vars merged over process.env. Never logged or included in results. */
  env?: Record<string, string>;
}

export interface SpawnCliResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  timedOut: boolean;
}

/**
 * Run a CLI to completion with a hard timeout.
 * On timeout the child is killed with SIGKILL and timedOut=true is returned
 * (exitCode -1) instead of throwing, so callers can map it to WorkerResult.
 */
export function spawnCli(cmd: string, args: string[], opts: SpawnCliOptions): Promise<SpawnCliResult> {
  return new Promise((resolve) => {
    const controller = new AbortController();
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      controller.abort();
    }, opts.timeoutMs);

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
        ...(opts.env ? { env: { ...process.env, ...opts.env } } : {}),
      },
      (error, stdout, stderr) => {
        clearTimeout(timer);
        if (timedOut) {
          resolve({ exitCode: -1, stdout: String(stdout ?? ''), stderr: String(stderr ?? ''), timedOut: true });
          return;
        }
        // Non-timeout failure (non-zero exit): error.code holds the numeric exit code.
        // Spawn failures like ENOENT carry a string code; normalize those to -1 so we
        // never leak errno details through the result shape.
        const rawCode = (error as { code?: unknown } | null)?.code;
        const exitCode = typeof rawCode === 'number' ? rawCode : 0;
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
