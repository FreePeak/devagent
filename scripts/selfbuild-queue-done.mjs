#!/usr/bin/env node
// Mark a claimed queue task done (or failed) after the selfbuild loop's
// implement stage finishes. Usage:
//   DEVAGENT_QUEUE_LEASE=<generation> node scripts/selfbuild-queue-done.mjs \
//     <repo> <taskId> done|failed [detail]
//
// DEVAGENT_QUEUE_LEASE is the fencing token scripts/selfbuild-queue-claim.mjs
// issued for this claim (FR-VIS-09 queue claims). Carrying it makes the write
// atomic-and-owned: if the loop's task outlived its lease and another consumer
// reclaimed the entry, this write is refused instead of clobbering the new
// owner's record. Omitted (a caller with no token) keeps the unfenced
// administrative path.
import { completeTask, failTask, setTaskStatus } from '../src/queue.ts';
import { resolve } from 'node:path';

const [repo, taskId, status, detail] = process.argv.slice(2);
if (!repo || !taskId || !['done', 'failed'].includes(status)) {
  console.error('usage: DEVAGENT_QUEUE_LEASE=<generation> selfbuild-queue-done.mjs <repo> <taskId> done|failed [detail]');
  process.exit(1);
}
const rawLease = process.env.DEVAGENT_QUEUE_LEASE;
const lease = rawLease === undefined || rawLease === '' ? undefined : Number(rawLease);
const written = lease === undefined || !Number.isFinite(lease)
  ? setTaskStatus(resolve(repo), taskId, status, detail ?? undefined)
  : status === 'done'
    ? completeTask(resolve(repo), taskId, lease)
    : failTask(resolve(repo), taskId, lease, detail ?? undefined);
if (!written) {
  // Stale token (or the task is gone): the lease moved on, so this is loud, not silent.
  console.error(`${taskId} -> ${status} REFUSED (stale lease generation ${rawLease})`);
  process.exit(1);
}
console.log(`${taskId} -> ${status}`);
