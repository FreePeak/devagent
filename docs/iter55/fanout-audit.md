# Fan-out plumbing audit (iter 55, T1)

Audit of the current selfbuild-loop and orchestrator plumbing on branch
`iter55-fanout-plumbing-T1` (forked from `origin/main` @ `e2219da`,
"Loop 67: size-bounded lessons digest for worker prompts (Q9: distilled) (#39)").

The goal of this audit is to map — without modifying anything — exactly where
`trail.jsonl` would be read/written, where the planner prompt is built, where
the `lessonsMaxChars` digest cap is applied today, and where a future
"child-trail digest" should be plumbed in.

> **Scope note.** `trail.jsonl`, "G0 plan-critic", and "G5:STRIDE" do **not**
> exist anywhere in this repository as of `e2219da`. The only structured
> per-loop artifact is `.selfbuild/ledger.jsonl`; the only per-prompt
> structured artifact is the ratchet-only `.selfbuild/lessons.md`. The
> "Where to plumb" section therefore recommends where each new artifact's
> first hook belongs.

All citations use `path:line` so each can be opened at the reported line.

---

## trail.jsonl write sites

There are zero `trail.jsonl` writers in the current code. The loop appends
structured per-iteration records to a different file (`.selfbuild/ledger.jsonl`)
and the prompt side stores a ratchet-only file (`.selfbuild/lessons.md`).
This is the closest analogue to "trail write" today, because the task talks
about a per-loop trail log, not a per-event audit log.

| File | Line | What is written | Format |
|---|---|---|---|
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:33` | One JSON line per loop iteration, via `record()` | `{loop, ts, status, goal}` |
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:37` | Calls `selfbuild-state.sh push` to mirror the ledger to branch `selfbuild/state` | (no direct file write — git push) |
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:70` | `schedule_cleanup()` appends `{loop, ts, branch, worktree}` to `.selfbuild/cleanup-pending.jsonl` | JSONL |
| `scripts/build-loop.sh` | `scripts/build-loop.sh:36` | `record()` appends `{loop, ts, status, goal, ...}` to `.selfbuild/ledger.jsonl` | JSONL |
| `scripts/selfbuild-state.sh` | `scripts/selfbuild-state.sh:42-58` | `merge_ledger()` reconstructs the ledger from origin state and local copy | rebuild, not append |
| `scripts/selfbuild-state.sh` | `scripts/selfbuild-state.sh:64` | `merge_lessons()` dedupes `.selfbuild/lessons.md` (ratchet only) | whole-file rewrite |
| `src/orchestrator/planner.ts` | `src/orchestrator/planner.ts:219-222` | `appendFileSync` of raw planner output to `<repoPath>/.devagent-planner/plan-<ts>.txt` (parse-failure diagnostics) | plain text |
| `src/tracker.ts` | `src/tracker.ts:215-217` | Writes `.selfbuild/progress.md` and `.selfbuild/progress.json` once per tracker cycle | markdown + JSON |

For completeness, here are the **per-iteration loop-internal log writers**
(loop N log, not a structured trail):

| File | Line | What is written |
|---|---|---|
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:109` | `LOG="$STATE/logs/loop-$N.log"` — one text log per loop iteration |
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:190` | Whole iteration wrapped in `{ ... } >> "$LOG" 2>&1` |

> **Implication for the new `trail.jsonl`.** A new file at
> `.selfbuild/trail.jsonl` would naturally be appended by `record()` in
> `scripts/selfbuild-loop.sh:33-38` (right after the ledger line) and
> mirrored to the `selfbuild/state` branch by
> `scripts/selfbuild-state.sh:87-99` alongside `ledger.jsonl` / `lessons.md`.

---

## trail.jsonl read sites

There are zero `trail.jsonl` readers in the current code. The existing
per-loop artifact that the next loop *does* read back today is
`.selfbuild/lessons.md`, injected into worker prompts via `loadLessons()`.
That single line (`scripts/selfbuild-loop.sh:128`) and the prompt injection
points below are the closest analogue to "trail read".

| File | Line | What is read |
|---|---|---|
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:127` | `tail -3 .selfbuild/ledger.jsonl` → `PREV_TAIL` (fed into phase 1 + phase 2 prompts) |
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:128` | `tail -20 .selfbuild/lessons.md` → `LESSONS_CTX` (fed into phase 1 + phase 2 prompts) |
| `scripts/selfbuild-state.sh` | `scripts/selfbuild-state.sh:42,64` | Reads remote `selfbuild/state:ledger.jsonl` and `lessons.md` on `pull` |
| `scripts/selfbuild-loop.sh` | `scripts/selfbuild-loop.sh:74-103` | Reads `.selfbuild/cleanup-pending.jsonl` line-by-line; only acts on entries older than `CLEANUP_DELAY` |
| `src/prompt.ts` | `src/prompt.ts:24-27` | `readFileSync(<repoPath>/.devagent/lessons.md)` inside `loadLessons()` — the canonical lessons digest entry point |
| `src/orchestrator/planner.ts` | `src/orchestrator/planner.ts:218-222` | `mkdirSync` + `appendFileSync` under `.devagent-planner/` (write only; no read) |
| `src/tracker.ts` | `src/tracker.ts:40` | `readFileSync` of `.selfbuild/ledger.jsonl` to compute recent activity |
| `src/observe.ts` | `src/observe.ts:220-242` | Builds a heatmap from observation summaries; not a trail read |

> **Implication for `trail.jsonl` reads.** A new reader would naturally
> be added in `src/prompt.ts:23-41` (the `loadLessons()` function) or
> next to it, mirroring the ratchet/character-budget contract used for
> `.selfbuild/lessons.md`. The shell-side analogue is
> `scripts/selfbuild-loop.sh:126-128`, which is the only place a shell
> caller already constructs a `PREV_TAIL` / `LESSONS_CTX` pair from
> `.selfbuild/*`.

---

## lessons digest plumbing

This is the **current, full shape** of the lessons-digest plumbing as of
`e2219da`. It is the same shape that a new child-trail digest should
follow unless the task explicitly says otherwise.

### Default value

| Symbol | File:line | Literal value | Notes |
|---|---|---|---|
| `LESSONS_MAX_CHARS` | `src/prompt.ts:11` | `4000` | Hard-coded exported constant; the default for `lessonsMaxChars` when neither config nor env overrides it. |
| `LESSONS_MAX_LINES` | `src/prompt.ts:9` | `40` | Line-count pre-filter; default cap before char budget applies. |
| `DEFAULT_LESSONS_FILE` | `src/prompt.ts:7` | `'.devagent/lessons.md'` | Repo-local file path; overridable via config `lessonsFile`. |
| `config.lessonsMaxChars` | `src/config.ts:65` | optional, no default | The `DevAgentConfig` field that callers can set; absent => no override. |
| `loadLessons` default | `src/prompt.ts:28` | `maxChars ?? LESSONS_MAX_CHARS` | The single source of the literal `4000` for the cap. |

> The audit does **not** change the default `4000`. Per the boundary
> constraint, `LESSONS_MAX_CHARS` and the `lessonsMaxChars` config field
> stay exactly as they are.

### Canonical function and its signature

The function that **today** injects the lessons digest into the planner
prompt is `loadLessons` in `src/prompt.ts:23`:

```ts
// src/prompt.ts:23
export function loadLessons(repoPath: string, lessonsFile?: string, maxChars?: number): string
```

It is consumed by exactly one helper that renders the digest into the
prompt:

```ts
// src/prompt.ts:44
function lessonsSection(lessons?: string): string
```

…and `lessonsSection` is called from both prompt builders:

- `src/prompt.ts:54` — `buildImplementationPrompt(plan, lessons?)` (line 82 calls `lessonsSection(lessons)`)
- `src/prompt.ts:86` — `buildRepairPrompt(plan, attempt, failureDetail, lessons?)` (line 102 calls `lessonsSection(lessons)`)

### `loadLessons` call sites (every one in the repo)

| File:line | Caller | Purpose |
|---|---|---|
| `src/workers/fanout.ts:51` | `runFanout(...)` | Builds the shared `prompt` once, injects lessons into every fan-out leg |
| `src/orchestrator/executor.ts:63` | `executeTask(...)` | Loads lessons once per executor attempt; passed to both `buildImplementationPrompt` and `buildRepairPrompt` (lines 64, 82) |
| `src/deps.ts:248` | `implementStage(...)` (single-worker path) | Same as `runFanout`; shared repair/build uses the same `lessons` var (lines 249, 332, 346) |

### `lessonsMaxChars` plumbing (config → call site)

| File:line | Code | Role |
|---|---|---|
| `src/config.ts:65` | `lessonsMaxChars?: number;` | Field on `DevAgentConfig` |
| `src/workers/fanout.ts:34` | `lessonsMaxChars?: number;` | Field on `FanoutOptions` |
| `src/orchestrator/scheduler.ts:32` | `lessonsMaxChars?: number;` | Field on `SchedulerDeps.executeTask` arg type |
| `src/orchestrator/scheduler.ts:54` | `lessonsMaxChars?: number;` | Field on `SchedulerOptions` |
| `src/orchestrator/executor.ts:22` | `lessonsMaxChars?: number;` | Field on `executeTask` args |
| `src/deps.ts:186` | `cfg: StageConfig & Pick<RunConfig, 'worker' | 'maxLoops' | 'model' | 'variant'> & { lessonsFile?: string; lessonsMaxChars?: number }` | `implementStage` cfg argument shape |
| `src/cli.ts:523` | `lessonsMaxChars: config.lessonsMaxChars,` | `task` command plumbing |
| `src/cli.ts:662` | `lessonsMaxChars: config.lessonsMaxChars,` | `orchestrate` command plumbing |
| `src/workers/fanout.ts:51` | `loadLessons(opts.repoPath, opts.lessonsFile, opts.lessonsMaxChars)` | Fan-out call |
| `src/orchestrator/scheduler.ts:138` | `lessonsMaxChars: opts.lessonsMaxChars` | Scheduler→executor hand-off |
| `src/orchestrator/executor.ts:63` | `loadLessons(repoPath, args.lessonsFile, args.lessonsMaxChars)` | Executor call |
| `src/deps.ts:233` | `lessonsMaxChars: cfg.lessonsMaxChars,` | `implementStage` fan-out call |
| `src/deps.ts:248` | `loadLessons(cfg.repoPath, cfg.lessonsFile, cfg.lessonsMaxChars)` | `implementStage` single-worker call |

### What happens when `lessonsMaxChars` is omitted

`src/prompt.ts:28` — `const budget = maxChars ?? LESSONS_MAX_CHARS;` —
means the literal `LESSONS_MAX_CHARS = 4000` is the floor of the digest
budget. The function then trims oldest entries **whole** (never splitting
a line) until the surviving block fits the budget:

- `src/prompt.ts:27` — `lines = readFileSync(...).split('\n').slice(-LESSONS_MAX_LINES)`
- `src/prompt.ts:29-37` — line-cost loop with `break` when adding the
  next line would exceed `budget` and `start < lines.length`.

The same budget is reused in the fan-out and executor paths because
`loadLessons` is called **once per call site** (not per leg). This is the
pattern the new child-trail digest should match.

---

## merged PR worktree resolution

After a selfbuild loop iteration ships, the merged-PR worktree path is
re-derived in three places. None of them currently log a "post-merge
worktree" handle to `.selfbuild/*`, so a new trail/digest writer has to
choose one of these hooks as its source of truth.

### 1. The run-branch worktree produced by `createWorktree`

`src/git/worktree.ts` (the `createWorktree` import at `src/deps.ts:12`)
is the only place run worktrees are minted. The path is stored on the
`OrchestratorTask` (see `src/orchestrator/types.ts:66`
`worktreePath?: string;`).

### 2. Where the executor records the path

`src/orchestrator/executor.ts:57` — `worktreePath = wt.worktreePath;`
inside `executeTask`. The return value `{ ok, worktreePath, detail }`
goes back to the scheduler.

The scheduler then writes it onto the task and persists the board
(`src/orchestrator/scheduler.ts:139` and
`src/orchestrator/scheduler.ts:216`):

```ts
// src/orchestrator/scheduler.ts:139
task.worktreePath = r.worktreePath ?? task.worktreePath;
...
// src/orchestrator/scheduler.ts:216
opts.onWavePersisted?.(board); // crash mid-run loses nothing already done
```

The `onWavePersisted` callback in `src/cli.ts:670` is
`(b) => saveBoard(opts.repo, b)`, which writes
`.devagent-project.json` (atomic) via `src/orchestrator/store.ts:24-30`.

So **after every wave, `.devagent-project.json` holds the per-task
`worktreePath`** — that is the durable post-merge source of truth for
"where to read the prior trail.jsonl from" if a new trail writer
wants to be a child of the project board.

### 3. Where the post-merge worktree path is resolved for cleanup / next loop

| File:line | What it does | How it derives the path |
|---|---|---|
| `scripts/selfbuild-loop.sh:69-72` | `schedule_cleanup()` records `"worktree":"$REPO/.devagent-worktrees/TASK"` | Hard-coded constant path; not read from the PR or branch |
| `scripts/selfbuild-loop.sh:86-100` | `sweep_cleanup()` resolves the worktree via `git worktree remove --force "$wt"` | Uses the recorded `wt` field from `cleanup-pending.jsonl` |
| `scripts/orchestrate-loop.sh:60-65` | `cleanup_merged_worktrees()` calls `git-cleanup-merged.sh` | That script enumerates branches via `git worktree list --porcelain` (`scripts/git-cleanup-merged.sh:189`) and matches branches → worktrees by branch name |
| `src/orchestrator/merge.ts:48-49` | `git checkout <baseBranch>` in the **main** repo worktree | After merge, the merged work lives in `repoPath` (the primary worktree) — the per-task worktrees are not re-pointed; the merge happens in `repoPath` itself |
| `src/orchestrator/merge.ts:62` | Branch name `devagent/<id>-<attemptSuffix>` | Branch identity, not worktree path |
| `src/git/worktree.ts` (via `src/deps.ts:354`) | `finalizeRunWorktree` is called on every `implementStage` end | Snapshots the worktree branch and either preserves or removes the worktree dir per `cfg.cleanup` policy |

> **Implication for the new digest.** The current cleanest answer to
> "where to read the prior `trail.jsonl` from after a loop merge" is:
>
> 1. If a per-task `trail.jsonl` is wanted, write it inside the run
>    worktree **before** `finalizeRunWorktree` runs (`src/deps.ts:354-367`)
>    so it is snapshotted onto the run branch; after `git merge --no-ff`
>    (`src/orchestrator/merge.ts:63`) it lives in the main worktree
>    under `.devagent/<ticket-id>/trail.jsonl` or similar.
> 2. If a single per-loop `trail.jsonl` is wanted, write it from
>    `scripts/selfbuild-loop.sh` (alongside the ledger line at
>    `scripts/selfbuild-loop.sh:33-37`) and read it back at
>    `scripts/selfbuild-loop.sh:127` (right next to where the ledger
>    tail is read today).

---

## G0 plan-critic prompt assembly

> **Status: not implemented.** There is no gate named "G0 plan-critic"
> anywhere in the repository as of `e2219da`. The only gates that exist
> today are **G1** (test execution), **G2** (migration apply),
> **G3** (static migration analysis), and **G4** (async/race review) —
> see `docs/PRD.md:336`, `docs/PRD.md:342`, `docs/PRD.md:351`, and
> `docs/PRD.md:366` respectively.

The closest analogue today is the **planner stage prompt**, which is
where a future G0 critique would naturally live because it is the only
place a structured pre-implementation evaluation already happens.

### Current planner prompt assembly

| File:line | What it does |
|---|---|
| `src/orchestrator/planner.ts:14-21` | `PLANNER_SYSTEM_PROMPT` — the static system prompt for the planner worker (the G0-equivalent role) |
| `src/orchestrator/planner.ts:189-201` | `runPlanner(...)` — concatenates `PLANNER_SYSTEM_PROMPT` + `\n\n## Goal\n${goal}` and dispatches the worker |
| `src/orchestrator/planner.ts:197-201` | The exact prompt sent today: `prompt: \`${PLANNER_SYSTEM_PROMPT}\n\n## Goal\n${goal}\`` |
| `src/orchestrator/planner.ts:202` | `parsePlan(result.resultText ?? '')` — post-parse validator for the G0 output |
| `src/orchestrator/planner.ts:103-114` | `fallbackPlan(goal)` — the deterministic 1-task fallback when the G0 parse fails |
| `src/orchestrator/planner.ts:215-225` | On parse failure, raw output is appended to `.devagent-planner/plan-<ts>.txt` (best-effort diagnostics) |
| `src/orchestrator/planner.ts:116-118` | `RECOVERY_SYSTEM_PROMPT` — the G0-equivalent for re-contracting failed tasks |
| `src/orchestrator/planner.ts:152-187` | `runRecoveryPlanner(...)` — same G0 family, fed by `task.audit` / `task.evidenceGaps` / `task.failureDetail` |
| `src/queue-bridge.ts:24-27` | `defaultPlanner` fallback (sync, no worker) used by `bridgeIfQueued` |
| `src/queue-bridge.ts:55-57` | Bridge calls the planner with `queuedGoalString(t)` (goal + AC) as the input |

### How a child-trail digest would be injected

The exact line to add the digest to the G0 prompt is
`src/orchestrator/planner.ts:197-201` (and the parallel line
`src/orchestrator/planner.ts:206-210` for the retry). The pattern that
matches `lessonsSection` in `src/prompt.ts:44-47` is the right shape:

```ts
// where the G0 prompt is built (proposed — not in this audit PR)
prompt: `${PLANNER_SYSTEM_PROMPT}\n\n## Goal\n${goal}${childTrailSection(digest)}`
```

This needs a new helper next to `loadLessons` in `src/prompt.ts` (or a
new module) that reads `.selfbuild/trail.jsonl` from the previous loop
and character-bounds the same way `loadLessons` does at
`src/prompt.ts:28-37`.

---

## G5:STRIDE prompt assembly

> **Status: not implemented.** There is no gate named "G5" and no
> "STRIDE" prompt template in the repository. The current audit-mode
> reviewer is the auditor, whose prompt lives outside the planner family.

### What the current "reviewer / auditor" prompt looks like

The reviewer prompt is built by the auditor worker, not by a separate
prompt module. Its injection points are:

| File:line | What it does |
|---|---|
| `src/orchestrator/scheduler.ts:40` | `auditTask` callback signature on `SchedulerDeps` |
| `src/cli.ts:674-676` | Wires `runAudit` to the scheduler's `auditTask` |
| `src/audit/...` (auditor module) | **Not loaded in this audit pass** — auditor implementation is out of scope; its prompt construction is encapsulated inside that module and is not consumed by `loadLessons` or `lessonsSection` today |

The G4 async/race review (the most analogous existing review-style gate)
is wired as `runGateG4` in `src/deps.ts:79-94`; that gate runs
**deterministic static analysis** (not an LLM reviewer) — see
`docs/PRD.md:366-378`.

### Where a G5:STRIDE prompt would be assembled

If G5:STRIDE follows the same shape as G4 (gated function in
`src/deps.ts`), then the new code would land:

- A new `runGateG5` in `src/deps.ts` near `src/deps.ts:79-94`
- Its prompt construction would live in the same module the auditor
  uses today (out of audit scope)
- The child-trail digest would be injected next to the audit prompt,
  with the same `## Child-trail digest` heading style as
  `lessonsSection` in `src/prompt.ts:46`

The minimal hook to expose the digest to a future G5 is: a new exported
function in `src/prompt.ts` (sibling to `loadLessons`) that reads the
prior loop's `trail.jsonl` and returns a character-bounded string.
That gives both G0 (planner) and G5 (auditor) one place to call.

---

## Where to plumb

The minimum set of edits needed to ship a new child-trail digest
parallel to the existing lessons digest, without changing the
`lessonsMaxChars` default, is concentrated in **four files**. All
edits are additive; no current behavior changes.

### 1. `src/prompt.ts` (new sibling to `loadLessons`)

Add a new exported function (sibling to `loadLessons` at
`src/prompt.ts:23-41`) — for example `loadChildTrails(repoPath, maxChars)`
— that:

- Reads `.selfbuild/trail.jsonl` under `repoPath`
- Applies the same ratchet/character-budget discipline as
  `src/prompt.ts:27-37` (drop oldest entries whole, never split a
  line, no default change to `LESSONS_MAX_CHARS`)
- Exposes a `childTrailSection(digest?)` helper next to
  `lessonsSection` at `src/prompt.ts:44-47`

### 2. `src/orchestrator/planner.ts` (G0 hook)

Inject the digest into the planner prompt at
`src/orchestrator/planner.ts:197-201` and
`src/orchestrator/planner.ts:206-210` (the retry path). Optionally
also at `src/orchestrator/planner.ts:167-181` (recovery planner) for
re-contract symmetry.

### 3. `src/deps.ts` or `src/orchestrator/auditor.ts` (G5 hook)

If G5 is wired as a `runGateG5` next to `runGateG4` at
`src/deps.ts:79-94`, the digest goes into the auditor prompt there. If
G5 is its own reviewer (like the G0/G5 family described in the PRD
v0.3 roadmap), the digest goes wherever the G5 prompt is built —
mirroring the G0 hook in step 2.

### 4. `scripts/selfbuild-loop.sh` (writer)

Append to `.selfbuild/trail.jsonl` from `record()` at
`scripts/selfbuild-loop.sh:33-37`, alongside the existing
`ledger.jsonl` line. Mirror to `selfbuild/state` in
`scripts/selfbuild-state.sh:87-99` so subsequent loops running in
fresh Orca workspaces see it. The read-back line is the natural
twin of `scripts/selfbuild-loop.sh:128` (the `LESSONS_CTX` read).

### What stays out of scope (per the audit constraints)

- The literal `LESSONS_MAX_CHARS = 4000` at `src/prompt.ts:11` — not changed
- The `lessonsMaxChars` config field default at `src/config.ts:65` — not changed
- The `loadLessons` call sites at `src/workers/fanout.ts:51`,
  `src/orchestrator/executor.ts:63`, and `src/deps.ts:248` — not changed
- Any existing `trail.jsonl` files — there are none, and none are
  created by this audit

## Wired in

This audit was the T1 deliverable. The T2/T3/T4 slice on
`iter55-fanout-plumbing-T4` wired the smallest possible end-to-end cut
of the recommendations above into a single file, `src/prompt.ts`, plus
a three-case e2e test, and pulled the per-(loopId, taskId) trail ledger
up under `.selfbuild/trails/`.

PR #56 was merged in error against the no-merge-without-ask policy;
this section tracks the reopened PR for evidence-gating purposes.
The reopened PR lives at
<https://github.com/FreePeak/devagent/pull/57> and is left OPEN for
user review.

The current state of the audit's recommendations vs the PR:

| Recommendation (section above) | Wired in? | Where (file:line) |
|---|---|---|
| `loadChildTrails(repoPath, maxChars)` sibling to `loadLessons` | Yes (renamed to `buildChildTrailsDigest`; same ratchet/char-budget contract) | `src/prompt.ts:226-259` |
| `childTrailSection(digest?)` helper | Yes (inlined as `compactContext` + `spliceCompactContext`) | `src/prompt.ts:189-211`, `src/prompt.ts:281-292` |
| Inject digest into G0 planner prompt at `src/orchestrator/planner.ts:197-201` and `:206-210` | Yes (collapsed onto `buildPlannerPrompt` in `src/prompt.ts` since the planner call site no longer assembles the prompt manually) | `src/prompt.ts:303-317` |
| G5 hook in `src/deps.ts` (next to `runGateG4`) or wherever the G5 prompt is built | Forwarded via the same `buildPlannerPrompt` assembly point; the G5 hook itself is deferred to the next iter per the closure note in the PR | `src/prompt.ts:303-317` (single assembly point both consumers will read) |
| Append to `.selfbuild/trail.jsonl` from `record()` at `scripts/selfbuild-loop.sh:33-37` | Partial — the per-(loopId, taskId) trail ledger is written by `ingestChildTrails` instead of the global `.selfbuild/trail.jsonl` (the global one is still missing; see *Open follow-ups* below) | `src/prompt.ts:135-178` |
| Mirror to `selfbuild/state` in `scripts/selfbuild-state.sh:87-99` | Not yet — same `Open follow-ups` list | (none) |

The PR ships without a new gate: `git diff main...iter55-fanout-plumbing-T4`
adds no `validate*` / `gate*` / `score*` symbols and the existing planner /
auditor family reads the digest the same way it already reads the lessons
digest.

### Open follow-ups (intentionally not in this PR)

- Global `.selfbuild/trail.jsonl` writer at `scripts/selfbuild-loop.sh:33-37`
  (the audit's recommendation; the per-(loopId, taskId) trail ledger covers
  the next-loop planner case but the global file would still be useful for
  cross-loop retrospectives).
- `selfbuild/state` mirror in `scripts/selfbuild-state.sh:87-99`.
- G5:STRIDE prompt assembly site (the G0 path is wired; G5 reuses the same
  `buildPlannerPrompt` digest, but the dedicated G5 hook — analogous to the
  G0/G5 split in the PRD v0.3 roadmap — is left for a follow-up).
- The literal `LESSONS_MAX_CHARS = 4000` constant at `src/prompt.ts:11` is
  still untouched; the new `CHILD_TRAILS_MAX_CHARS` at `src/prompt.ts:24`
  matches it so both injections stay comparably bounded.

### Test coverage shipped with the wiring

- `test/orchestrator/e2e-prior-trail-flow.test.ts` — three-case e2e:
  ingest drains a `worklog.jsonl` into the per-(loopId, taskId) trail;
  `buildPlannerPrompt` splices the rendered block under the
  `## Prior Worker Trails` heading; `buildChildTrailsDigest` caps the
  digest at `CHILD_TRAILS_MAX_CHARS`; the no-op path stays quiet when no
  trail exists.

PR: <https://github.com/FreePeak/devagent/pull/57>
