/**
 * Operator doc-sync surface (PRD §17 defect + Q41 degradation surface):
 * `devagent sync-docs` refreshes the work-selection docs (docs/PRD.md) from
 * origin before any doc-driven selection, replacing the inline fetch/merge
 * in scripts/selfbuild-loop.sh. Machine consumers read --json
 * {ok, upToDate, diverged, dirty, detail}; the exit code carries the same
 * classification so callers map outcomes without grepping output text:
 *   0 ok (synced or already up to date)
 *   1 generic failure (fetch/network/unknown — provider-side)
 *   2 dirty refusal (tracked docs locally modified, linear history)
 *   3 diverged (rebase conflict, or diverged + dirty tracked docs)
 */
import { syncWorkSelectionDocs } from '../git/doc-sync.js';

/** Class-specific exit codes, mirrored in the sync-docs command description. */
export const SYNC_DOCS_EXIT = { ok: 0, failed: 1, dirty: 2, diverged: 3 } as const;

/** Run the doc sync and render the result (human text or --json payload). */
export async function runSyncDocs(
  opts: { json?: boolean; repoPath?: string; branch?: string } = {},
): Promise<void> {
  const result = await syncWorkSelectionDocs(opts.repoPath ?? process.cwd(), { branch: opts.branch });
  const payload = {
    ok: result.ok,
    upToDate: result.alreadyUpToDate === true,
    diverged: result.diverged === true,
    dirty: result.dirty === true,
    detail: result.detail,
  };
  if (opts.json) {
    console.log(JSON.stringify(payload, null, 2));
  } else if (payload.ok) {
    console.log(`[sync-docs] ${result.detail}`);
  } else {
    console.error(`[sync-docs] ${result.detail}`);
  }
  if (!payload.ok) {
    process.exitCode = payload.diverged
      ? SYNC_DOCS_EXIT.diverged
      : payload.dirty
        ? SYNC_DOCS_EXIT.dirty
        : SYNC_DOCS_EXIT.failed;
  }
}
