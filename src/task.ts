import { existsSync } from 'node:fs';
import { basename } from 'node:path';
import type { RunLogger } from './logger.js';
import { runPipeline } from './pipeline.js';
import type { PipelineDeps } from './pipeline.js';
import type { TicketSpec } from './types.js';

/**
 * Orchestrator-facing one-shot task mode: any external harness (Orca,
 * CI, another DevAgent) can drive DevAgent with a raw prompt and a cwd,
 * the same contract it uses for claude-code/opencode workers:
 *   devagent task --prompt "add rate limiting to /api" [--auto-pr]
 * The prompt becomes a synthetic ticket; fetch/post stages are local.
 */

export interface TaskOptions {
  prompt: string;
  repoPath: string;
  autoPr: boolean;
  autoMerge?: boolean;
  maxLoops: number;
  timeoutMs: number;
  /** Post-run worktree disposal policy; default 'auto'. */
  cleanup?: 'auto' | 'keep' | 'always';
  /** Drop the enclosing Orca workspace after done when repoPath is Orca-managed. */
  dropOrcaWorkspace?: boolean;
  log: RunLogger;
  /**
   * Task identity: names the synthetic ticket, worktree (.devagent-worktrees/<id>)
   * and branch (devagent/<id>). Concurrent dispatches (pooled remote hosts,
   * parallel Orca runs) must not share one id — git refuses to check a branch
   * out twice across worktrees of the same repo.
   */
  taskId?: string;
}

/**
 * Collision-free default task id (loop 66): the previous constant `TASK` made
 * every concurrent run fight over `.devagent-worktrees/TASK` and the branch
 * `devagent/TASK` ("already used by worktree at ..."). Epoch36 + random suffix
 * keeps ids unique per invocation while staying sanitize-safe.
 */
export function defaultTaskId(now: () => number = Date.now): string {
  const epoch36 = now().toString(36);
  const rand = Math.random().toString(36).slice(2, 6);
  return `TASK-${epoch36}-${rand}`;
}

export function syntheticTicketFromPrompt(prompt: string, taskId?: string): TicketSpec {
  // First line becomes the title; whole prompt stays as description
  const [firstLine, ...rest] = prompt.trim().split('\n');
  const id = taskId ?? defaultTaskId();
  return {
    id,
    title: firstLine!.slice(0, 80),
    description: rest.join('\n').trim() || firstLine!,
    labels: ['orchestrated'],
    acceptanceCriteria: [],
    url: '',
    trackerInternalId: id,
  };
}

export interface TaskDeps {
  runPipelineDeps: PipelineDeps;
  /** Worker dispatch identical to deps.ts implementStage; injected by caller. */
  implementStage(cfg: TaskOptions, ticket: TicketSpec, log: RunLogger): Promise<{ ok: boolean; worker: string; attempts: number; worktreePath?: string }>;
  publishStage?(cfg: TaskOptions, ticket: TicketSpec, impl: { ok: boolean; worktreePath?: string }): Promise<string | undefined>;
}

/** Remote boundary of task publishing, injected so the logic stays testable over real git fixtures. */
export interface TaskPublishDeps {
  commitAllChanges(worktreePath: string, message: string): Promise<boolean>;
  currentBranch(worktreePath: string): Promise<string>;
  listChangedFiles(worktreePath: string, baseBranch: string, ref?: string): Promise<string[]>;
  pushBranch(repoPath: string, branch: string): Promise<void>;
  createPr(o: { repoPath: string; branch: string; title: string; body: string }): Promise<string>;
}

export interface TaskPublishOptions {
  repoPath: string;
  prompt: string;
  /** Base used for the empty-diff guard and PR evidence. */
  baseBranch: string;
  log: RunLogger;
}

/**
 * Publish a finished task worktree as a PR (dogfood loops 7-9 lesson):
 * - commits whatever the worker left uncommitted — agents routinely edit
 *   without committing, and an uncommitted change silently ships as an empty PR;
 * - pushes the branch the worktree ACTUALLY has checked out. The previous
 *   implementation invented `devagent/task-<runId>`, a ref nobody ever
 *   created, so every push died with "src refspec does not match any";
 * - refuses to open a PR when the diff vs base is empty.
 * - when cleanup=auto already removed the worktree, publishes from the
 *   surviving run branch in the main repo (snapshot already landed there).
 */
export async function publishTaskBranch(
  opts: TaskPublishOptions,
  impl: { ok: boolean; worktreePath?: string },
  io: TaskPublishDeps,
): Promise<string | undefined> {
  if (!impl.worktreePath) return undefined;

  const title = opts.prompt.split('\n')[0]!.slice(0, 80);
  // cleanup=auto removes a successful run's worktree after snapshotting its
  // uncommitted output onto the run branch. Publishing must then happen from
  // the main repo against that branch: git add in a removed cwd exits -1 with
  // empty stderr (loop 57-58: "git add -A exited -1" right after a green test
  // gate, tripping the selfbuild circuit breaker).
  const wtAlive = existsSync(impl.worktreePath);
  const branch = wtAlive
    ? await io.currentBranch(impl.worktreePath)
    : `devagent/${basename(impl.worktreePath)}`;
  if (wtAlive) {
    await io.commitAllChanges(impl.worktreePath, `devagent(task): ${title}`);
  }
  const changed = await io.listChangedFiles(
    wtAlive ? impl.worktreePath : opts.repoPath,
    opts.baseBranch,
    wtAlive ? undefined : branch,
  );
  if (changed.length === 0) {
    opts.log.warn('task', 'nothing changed vs base; skipping PR', { branch, baseBranch: opts.baseBranch });
    return undefined;
  }

  await io.pushBranch(opts.repoPath, branch);
  return io.createPr({
    repoPath: opts.repoPath,
    branch,
    title,
    body: `Automated task via \`devagent task\`.\n\n## Prompt\n${opts.prompt}`,
  });
}

/** Minimal pipeline execution for prompt-driven tasks (no tracker round-trip). */
export async function runTask(opts: TaskOptions, deps: TaskDeps): Promise<{ ok: boolean; prUrl?: string; note: string }> {
  const ticket = syntheticTicketFromPrompt(opts.prompt, opts.taskId);
  opts.log.info('task', `Task starting`, { title: ticket.title, taskId: ticket.id });

  const impl = await deps.implementStage(opts, ticket, opts.log);
  if (!impl.ok) {
    return { ok: false, note: 'implementation failed validation' };
  }

  if (!opts.autoPr) {
    return { ok: true, note: `worktree ready for review: ${impl.worktreePath ?? '(repo root)'}` };
  }
  const prUrl = await deps.publishStage?.(opts, ticket, impl);
  return { ok: true, prUrl, note: prUrl ? `PR opened: ${prUrl}` : 'no remote credentials; branch preserved locally' };
}
// ============================================================================
// PRD backlog pick reconciliation (curation run 24 decision)
// Extends the curator title-match rule from the 2026-08-31 queue sweep:
// before dispatching a backlog item, validate against merged PR titles and
// PRD completion notes; a shipped item is rejected and struck from the Phase 4
// backlog section in the same run.

export interface BacklogItem {
  /** Backlog item id, e.g. "Q40" */
  id: string;
  /** Bold title text from the line, e.g. "Operator-role provider preflight" */
  title: string;
  /** Full markdown line, e.g. "- **Title** — description (Q40)." */
  line: string;
  /** Whether the line is already struck (wrapped in ~~) */
  struck: boolean;
}

export interface BacklogPickCheck {
  ok: boolean;
  shipped: boolean;
  message: string;
  /** Line text suitable as dispatch prompt, available when ok=true and shipped=false */
  prompt?: string;
  /** IDs of items struck in the current run (for updating docs/PRD.md) */
  struckIds: string[];
}

/**
 * Extract a backlog item id from a bullet line: the LAST standalone Q-token
 * (e.g. "(Q40)" or "(2026-09-01 human deep-dive; Q38)"). Items conventionally
 * carry their id in a trailing parenthetical, but the real PRD has placed it
 * mid-parenthetical (GRADIENT, Q38), so the token may appear anywhere.
 */
function extractItemId(line: string): string | null {
  const m = line.match(/\b([A-Z]+\d+)\b/g);
  return m ? m[m.length - 1]! : null;
}

export function parseBacklogItems(prd: string): BacklogItem[] {
  const lines = prd.split('\n');
  const items: BacklogItem[] = [];
  let inSection = false;

  for (const raw of lines) {
    const trimmed = raw.trimStart();

    // Track heading boundaries
    if (/^#{1,6}\s/.test(trimmed)) {
      if (inSection) break; // next heading → end of section
      if (/current backlog/i.test(trimmed) && /phase 4/i.test(trimmed)) {
        inSection = true;
      }
      continue;
    }
    if (!inSection) continue;

    // Only parse bullet lines (including struck ones wrapped in ~~)
    if (!/^[-*]\s/.test(trimmed) && !/^~~[-*]\s/.test(trimmed)) continue;
    const struck = trimmed.startsWith('~~-') || trimmed.startsWith('~~*');
    const body = struck ? trimmed.replace(/^~~/, '').replace(/~~$/, '').trimStart() : trimmed;

    // Extract id: the last standalone Q-token anywhere in the bullet
    const id = extractItemId(body);
    if (!id) continue;

    const titleM = body.match(/\*\*(.+?)\*\*/);
    items.push({
      id,
      title: (titleM?.[1] ?? '').trim(),
      line: raw,
      struck,
    });
  }

  return items;
}

/**
 * Extract completion notes from the PRD: blockquote paragraphs starting with
 * `> **Completed` — these list what shipped in each curation run. Multi-line
 * blockquotes are joined so the full note text is matchable.
 */
export function extractCompletionNotes(prd: string): string[] {
  const notes: string[] = [];
  const lines = prd.split('\n');
  let cur: string | null = null;
  for (const raw of lines) {
    const t = raw.trim();
    if (!t.startsWith('>')) {
      if (cur) notes.push(cur);
      cur = null;
      continue;
    }
    const text = t.replace(/^>\s*/, '');
    if (text.startsWith('**Completed')) {
      if (cur) notes.push(cur);
      cur = text;
    } else if (cur) {
      cur += ' ' + text;
    }
  }
  if (cur) notes.push(cur);
  return notes;
}

function normalizeTitle(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9\s]/g, ' ').replace(/\s+/g, ' ').trim();
}

/**
 * Check a backlog pick against merged PR titles and PRD completion notes.
 * Implements the curator title-match rule from the 2026-08-31 queue sweep
 * (docs/PRD.md:773): a pick is already shipped when its id or bold title text
 * appears in a merged PR title, or its bold title text appears in a completion
 * note. The whole current backlog is reconciled in the same pass: every
 * confirmed-shipped (non-struck) item is returned in `struckIds` so the caller
 * can strike them from the Phase 4 backlog section in the same run — the pick
 * being among them rejects it.
 *
 * Completion notes are title-matched only (never id-matched): notes reference
 * open items by id too (e.g. the run-21 note "Q27 ... deeper failure-class
 * carryover stays on the backlog"), so an id hit there would strike an item
 * that is still current.
 *
 * @param pickId  backlog item id, e.g. "Q40"
 * @param prd     full text of docs/PRD.md
 * @param mergedTitles  list of merged PR commit subjects (from git log origin/main)
 * @returns       check result with shipped status, message, prompt (accepted
 *                picks), and ids to strike in the same run
 */
export function checkBacklogPick(pickId: string, prd: string, mergedTitles: string[]): BacklogPickCheck {
  const items = parseBacklogItems(prd);
  const pick = items.find((i) => i.id.toLowerCase() === pickId.toLowerCase());

  if (!pick) {
    return { ok: false, shipped: false, message: `backlog item ${pickId} not found in the Phase 4 current backlog`, struckIds: [] };
  }
  if (pick.struck) {
    return { ok: false, shipped: true, message: `${pickId} already shipped (struck in docs/PRD.md)`, struckIds: [] };
  }

  const notes = extractCompletionNotes(prd);
  const evidenceFor = (item: BacklogItem): string | null => {
    const idRe = new RegExp(`\\b${item.id.toLowerCase()}\\b`);
    const titleNorm = normalizeTitle(item.title);
    for (const raw of mergedTitles) {
      const h = normalizeTitle(raw);
      if (idRe.test(h)) return raw.trim();
      if (titleNorm && h.includes(titleNorm)) return raw.trim();
    }
    for (const raw of notes) {
      const h = normalizeTitle(raw);
      // Notes are title-matched only: an id hit can strike an item that a
      // note merely references while it stays open (see the Q27 example above).
      if (titleNorm && h.includes(titleNorm)) return raw.trim();
    }
    return null;
  };

  // Reconcile the whole backlog; the picked item being confirmed-shipped rejects it.
  const struckIds: string[] = [];
  for (const item of items) {
    if (item.struck) continue;
    const evidence = evidenceFor(item);
    if (!evidence) continue;
    if (item.id.toLowerCase() === pickId.toLowerCase()) {
      return { ok: false, shipped: true, message: `${pickId} already shipped: ${evidence}`, struckIds: [...struckIds, item.id] };
    }
    struckIds.push(item.id);
  }

  return {
    ok: true,
    shipped: false,
    message: `${pickId} is current backlog — dispatch ok`,
    prompt: pick.line,
    struckIds,
  };
}

/**
 * Strike confirmed-shipped backlog items by wrapping their lines in ~~...~~.
 * Returns the updated PRD text. Only strikes items not already struck.
 *
 * @param prd   full text of docs/PRD.md
 * @param ids   item ids to strike (e.g. ["Q40", "Q39"])
 * @returns     updated PRD text
 */
export function strikeBacklogItems(prd: string, ids: string[]): string {
  const idSet = new Set(ids.map((id) => id.toLowerCase()));
  return prd.split('\n').map((line) => {
    const trimmed = line.trimStart();
    if (!/^[-*]\s/.test(trimmed)) return line;
    if (trimmed.startsWith('~~-') || trimmed.startsWith('~~*')) return line; // already struck
    const body = trimmed.replace(/^~~/, '').replace(/~~$/, '').trimStart();
    const id = extractItemId(body);
    if (id && idSet.has(id.toLowerCase())) {
      return line.replace(/^(\s*)/, '$1~~').replace(/\s*$/, (sp) => '~~' + sp);
    }
    return line;
  }).join('\n');
}

/**
 * Fetch merged PR titles from the repo's git history.
 * Reads the last 30 subjects of origin/main commits (squash-merge PR titles).
 * Returns [] on failure (no origin, offline, etc.).
 */
export async function listMergedPrTitles(repoPath: string): Promise<string[]> {
  try {
    const { runCli } = await import('./workers/spawn-utils.js');
    const r = await runCli('git', ['log', 'origin/main', '--oneline', '-30'], { cwd: repoPath, timeoutMs: 30_000 });
    if (r.exitCode !== 0) return [];
    return r.stdout.split('\n').map((l) => l.replace(/^[0-9a-f]+\s+/, '').trim()).filter(Boolean);
  } catch {
    return [];
  }
}
