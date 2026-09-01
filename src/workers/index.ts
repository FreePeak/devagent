import type { WorkerAdapter, WorkerName } from '../types.js';
import { ClaudeCodeAdapter } from './claude-code.js';
import { OmpAdapter } from './omp.js';
import { OpenCodeAdapter } from './opencode.js';
import { PiAdapter } from './pi.js';

export { ClaudeCodeAdapter } from './claude-code.js';
export { OmpAdapter } from './omp.js';
export { OpenCodeAdapter } from './opencode.js';
export { PiAdapter } from './pi.js';
export { spawnCli } from './spawn-utils.js';
export type { SpawnCliOptions, SpawnCliResult } from './spawn-utils.js';

/** Map of every registered worker adapter. */
export const workers: Record<WorkerName, WorkerAdapter> = {
  'claude-code': new ClaudeCodeAdapter(),
  opencode: new OpenCodeAdapter(),
  omp: new OmpAdapter(),
  pi: new PiAdapter(),
};

/** Factory: resolve a worker adapter by name. Throws on unknown names. */
export function getWorker(name: WorkerName): WorkerAdapter {
  const worker = workers[name];
  if (!worker) {
    throw new Error(`Unknown worker: ${name}`);
  }
  return worker;
}
