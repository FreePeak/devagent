#!/usr/bin/env node
// Mark a claimed queue task done (or failed) after the selfbuild loop's
// implement stage finishes. Usage:
//   node scripts/selfbuild-queue-done.mjs <repo> <taskId> done|failed [detail]
import { setTaskStatus } from '../src/queue.ts';
import { resolve } from 'node:path';

const [repo, taskId, status, detail] = process.argv.slice(2);
if (!repo || !taskId || !['done', 'failed'].includes(status)) {
  console.error('usage: selfbuild-queue-done.mjs <repo> <taskId> done|failed [detail]');
  process.exit(1);
}
setTaskStatus(resolve(repo), taskId, status, detail ?? undefined);
console.log(`${taskId} -> ${status}`);
