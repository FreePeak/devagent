import { runCommandInHerdrPane } from '../integrations/herdr.js';
import { spawnCli, type SpawnCliOptions, type SpawnCliResult } from './spawn-utils.js';

export interface RunWorkerCliOptions extends SpawnCliOptions {
  /** Route this launch through the herdr pane runtime (see integrations/herdr.ts). */
  herdr?: boolean;
  /** Pane/workspace label shown in the herdr session. */
  label?: string;
}

let fallbackWarned = false;

/**
 * Execute a worker CLI launch either inside a herdr pane (opts.herdr, or
 * DEVAGENT_HERDR=1 as an env-wide override) or as a direct child process.
 * When herdr is unavailable or misbehaves, falls back to direct execution —
 * workers must keep running; the runtime is a visibility enhancement, never a
 * hard dependency. The fallback is warned about once per process.
 */
export async function runWorkerCli(cmd: string, args: string[], opts: RunWorkerCliOptions): Promise<SpawnCliResult> {
  if (shouldUseHerdr(opts.herdr)) {
    try {
      const viaHerdr = await runCommandInHerdrPane(cmd, args, opts);
      if (viaHerdr) return viaHerdr;
      warnFallback(`herdr session not reachable; running ${cmd} directly`);
    } catch (err) {
      warnFallback(`herdr pane run failed (${(err as Error).message}); running ${cmd} directly`);
    }
  }
  return spawnCli(cmd, args, opts);
}

/** Explicit per-spawn flag wins; otherwise DEVAGENT_HERDR=1|0 decides (default off). */
export function shouldUseHerdr(explicit?: boolean): boolean {
  if (explicit !== undefined) return explicit;
  const env = process.env.DEVAGENT_HERDR;
  if (env !== undefined && env !== '') return env !== '0' && !/^false$/i.test(env);
  return false;
}

function warnFallback(message: string): void {
  if (fallbackWarned) return;
  fallbackWarned = true;
  console.warn(`[herdr] ${message}`);
}
