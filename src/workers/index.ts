import type { WorkerAdapter, WorkerName } from '../types.js';
import { ClaudeCodeAdapter } from './claude-code.js';
import { OpenCodeAdapter } from './opencode.js';

export { ClaudeCodeAdapter } from './claude-code.js';
export { OpenCodeAdapter } from './opencode.js';
export { spawnCli } from './spawn-utils.js';
export type { SpawnCliOptions, SpawnCliResult } from './spawn-utils.js';

/** Map of every registered worker adapter. */
export const workers: Record<WorkerName, WorkerAdapter> = {
  'claude-code': new ClaudeCodeAdapter(),
  opencode: new OpenCodeAdapter(),
};

/** Factory: resolve a worker adapter by name. Throws on unknown names. */
export function getWorker(name: WorkerName): WorkerAdapter {
  const worker = workers[name];
  if (!worker) {
    throw new Error(`Unknown worker: ${name}`);
  }
  return worker;
}
