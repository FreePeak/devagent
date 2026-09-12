# Self-Build Infinity Loop

DevAgent builds DevAgent. Each loop iteration executes the full product cycle against
this repository, using DevAgent's own pipeline (`devagent task`) as the implementation
engine and Orca as the execution environment.

```
1.Research -> 2.Ideas -> 3.Validate -> 4.Plan -> 5.Implement -> 6.Testing -> 7.Push --+
    ^                                                                                 |
    +---------------------------------------------------------------------------------+
```

## State

All loop state lives in `.selfbuild/` (gitignored):

| Path | Purpose |
|---|---|
| `.selfbuild/ledger.jsonl` | Append-only log: one JSON line per completed iteration `{"loop":N,"ts":"<RFC3339 UTC>","status":S,"goal":G}` (key order loop/ts/status/goal, second-precision timestamps) |
| `.selfbuild/research/loop-N.md` | Phase 1 output for iteration N |
| `.selfbuild/goals/loop-N.md` | Phase 2-3 output: validated goal statement |
| `.selfbuild/logs/loop-N.log` | Full phase log |
| `.selfbuild/lessons.md` | Ratchet-only lessons file: research appends durable lessons (dated), never deletes or edits existing ones |

The next loop number is `ledger lines + 1`. Crash recovery is implicit: an unfinished
iteration simply reruns under its own number.

**Durable state across workspaces.** `.selfbuild/` is gitignored and Orca spawns each
run in a fresh workspace, so without help every run would restart at loop 1 (this
actually happened on 2026-08-24: runs improvised loop numbers from ambient context).
The driver mirrors `ledger.jsonl` + `lessons.md` to the orphan branch `selfbuild/state`
on origin (`internal/loopdriver/state.go`): it merges origin state before numbering an
iteration and pushes after every recorded iteration, failures included. Merge policy:
ledger is keyed by loop number, later `ts` wins on collision; lessons are a ratchet-only
union. The driver performs both sides automatically — hand-run iterations must go
through `devagent loop` (with `SELFBUILD_MAX_ITERATIONS` pinned) so the pull/push stays
automatic; the bash-era `scripts/selfbuild-state.sh pull|push` helper was retired with
the Node tree (FR-GO-16, #205).

## Phases

2. **Ideas** — Select ONE work item informed by the newest research and the ledger's recent failures/gaps. Selection is **issue-first (2026-09-07 operator policy)**: the task tracker is the GitHub issue queue (open issues labeled `selfbuild`, ordered `priority:P0` > `priority:P1` > `priority:P2`, oldest first within a tier), never a static PRD backlog section. Only when the tracker is empty does the LLM selection path run, deriving one goal from research + ledger + lessons. On an issue-first pick the phase-1 rationale — including a "merge PR #N, not a rewrite" action — rides along into the goal file (issue #301; see "Tracker + PRD policy"); a queue-claimed goal and a PO-derived goal are written verbatim.
3. **Validate** — Goal must pass three checks: it implements an open tracker
   issue (or, empty-tracker fallback, a research-derived goal that nothing in
   the ledger says shipped); scoped to a single iteration (implementable +
   testable in one pass); no open dependency on an earlier failed loop. Written
   to `.selfbuild/goals/loop-N.md`.
4. **Plan** — `devagent task --prompt "<goal>"` plans via the built-in planner.
5. **Implement** — same `devagent task` invocation drives worker CLIs (omp by
   default; claude-code / opencode / pi / grok selectable) in isolated worktrees
   through its internal plan-implement-test loops.
6. **Testing** — `devagent task` gates internally (test-gate, migration rules,
   async-review); after merge-back the driver additionally runs the repo-level test
   gate (`SELFBUILD_TEST_CMD`; default `go test ./...` since the Node-tree
   retirement, issue #300). Failure marks the iteration failed and feeds diagnostics into
   the next Research phase.
7. **Push** — `--auto-pr` pushes the branch and opens a PR. **Policy (locked 2026-08-24):
   product code always ships as a PR, never direct to origin/main**; direct main is
   reserved for docs and `.selfbuild` protocol chores. `SELFBUILD_PUSH_MODE=main`
   remains available but is not the operating default. **PRD-per-PR policy (2026-09-07):
   every PR lands with its `docs/PRD.md` state update** — the sections the change
   affects plus the *Last updated* footer, applied in the same branch via the
   dispatch-prompt policy rider. The shipped iteration closes its tracker
   issue only once the dispatch actually published: in pr push mode a
   task that exits 0 without a `PR opened:` line records a non-productive
   `no-pr` ledger row and leaves the issue open for re-pick (#238) — except
   for a verify-and-merge dispatch (#301), which lands an existing pull
   request and so proves itself with that pull request's merged state
   (`gh pr view --json state`), after fast-forwarding the repo test gate onto
   the merged tree. Its status row is still `ok` — `internal/lessons` scores
   any non-`ok` loop-result row as a failed loop — and the land is recorded in
   the iteration log, the `loop-phase` detail and the goal text; push mode
   `main` closes on the merge-to-main commit.

   **Landing evidence off the tracker path (2026-09-12).** A queue-delivered
   or PO/LLM goal never came through a tracker pick, so a goal that names the
   pull request to land (`Goal: Land PR #N`) derives its evidence subject from
   the goal text itself — the same merge-verb-near-PR matcher the pick path
   parses, under the same pick-time OPEN guard, so a stale mention
   self-disqualifies — and ships `ok` with that pull request's artifacts
   verified on main instead of recording the false `no-pr` rows of loops
   270/271/274/280. The `no-pr` early return also retires its queue claim
   `failed` with the `no-pr` detail (the shape gate's lease-recycling
   precedent) rather than leaving it to re-claim and re-burn the row every
   lease; tracker-path iterations are unaffected (`queued == nil`).

## Running

Single-process infinite runner (the Go driver, `internal/loopdriver` — production
since FR-GO-16, #205; the bash driver `scripts/selfbuild-loop.sh` was deleted in the
same change):

```sh
devagent loop                     # uses defaults below
```

Environment knobs (all optional):

| Var | Default | Meaning |
|---|---|---|
| `SELFBUILD_MAX_ITERATIONS` | `0` | 0 = run until circuit-breaker trips (checked at loop head) |
| `SELFBUILD_MAX_FAILS` | `3` | Circuit breaker: abort after N failed loops in a row |
| `SELFBUILD_STARVATION_LIMIT` | `5` | Halt after N consecutive non-productive iterations across all runs |
| `SELFBUILD_WORKER` | `omp` | Worker CLI passed to `devagent task` |
| `SELFBUILD_MODEL` | *(unset)* | Model pin forwarded to `devagent task` |
| `SELFBUILD_PUSH_MODE` | `pr` | `pr` (branch + PR via auto-pr) or `main` (direct commit) |
| `SELFBUILD_TEST_CMD` | `go test ./...` (default since the Node-tree retirement, #300) | Post-merge-back repo-level test gate (seam #230) |
| `SELFBUILD_ISSUE_LABEL` | `selfbuild` | Issue label defining the loop's tracker queue |
| `SELFBUILD_ISSUE_MAX` | `50` | Max issues fetched per pick (deterministic sort: priority rank, then issue number) |
| `SELFBUILD_GH_REPO` | derived from `git remote get-url origin` | Target repo for the tracker pick (`gh issue`) |
| `SELFBUILD_DEVAGENT_BIN` | `devagent` | CLI binary the driver shells out to (pane-run, task, preflight, sync-docs, scan-text, ledger, herdr-sweep, page-degrade-breach) |
| `SELFBUILD_RESEARCH_BIN` / `SELFBUILD_PO_BIN` | `omp -p --mode json --no-prewalk --no-lsp --no-extensions --model onegw/free` | Research / PO phase headless dispatch commands (research + PO dispatch through pane-run via herdr with this binary as fallback) |
| `SELFBUILD_TASK_TIMEOUT` | `7200` | Wall-clock cap (seconds) on the task dispatch |
| `SELFBUILD_RESEARCH_TIMEOUT` / `SELFBUILD_CLAUDE_TIMEOUT` | `900` / `600` | Wall-clock caps (seconds) on the research / PO dispatches |
| `SELFBUILD_API_MAX_ATTEMPTS` | `40` | Executor retry budget |
| `SELFBUILD_NO_PROGRESS_TIMEOUT_MS` | `600000` | Executor no-progress hang detection |
| `SELFBUILD_CLEANUP_DELAY` | `1800` | Auto-pr leftover grace period (seconds) |
| `SELFBUILD_SYNC_RETRY_SECS` | `60` | Pause between degraded iterations (seconds) |
| `SELFBUILD_VISIBILITY` | `visible` | Spawn visibility (flag > `DEVAGENT_VISIBILITY` > `SELFBUILD_VISIBILITY` > visible) |
| `SELFBUILD_NO_SYNC_DOCS` | *(unset)* | `1` skips the doc-freshness gate |
| `SELFBUILD_DRY_RUN` | `0` | `1` executes all phases without side effects (stub outputs, no research/task/push) |


## Go soak (FR-GO-15)

At the FR-GO-15 cutover (#204) the Go loop driver (`internal/loopdriver`,
exposed as `devagent loop`, FR-GO-13) was soaked against the live loop before
the bash driver (`scripts/selfbuild-loop.sh`) handed over production. The
bash driver was deleted at FR-GO-16 (#205); this section is retained as the
historical record of that soak.

Procedure:

```sh
# Build the Go binary, then run the soak with it as both driver and CLI:
go build -o bin/devagent ./cmd/devagent
SELFBUILD_DEVAGENT_BIN="$PWD/bin/devagent" \
SELFBUILD_MAX_ITERATIONS=<next> \
    bin/devagent loop
```

- `SELFBUILD_DEVAGENT_BIN` pointed the driver's shelled subcommands (pane-run,
  task, preflight, sync-docs, scan-text, ledger, herdr-sweep, page-degrade-breach)
  at the cutover binary instead of the default `devagent` on PATH — every executed
  surface had to be the Go implementation for the soak to count.
- The iteration cap is checked at loop head (`n >= cap` halts before spending
  tokens), so `<next>` is the first loop number the soak must NOT run: a
  one-iteration soak at loop N sets `SELFBUILD_MAX_ITERATIONS=N+1`.
- `SELFBUILD_TEST_CMD` is the post-merge-back repo-level test gate the driver
  runs inside the repo (word-split; default `go test ./...` since PR #240
  deleted the Node tree — an `npm test` default can only fail with ENOENT,
  which stamped every green iteration `failed-tests`, issue #300). Override it
  only for non-standard gates.

**Byte-parity gate.** The Go driver's `.selfbuild/ledger.jsonl` rows must be
byte-identical to the bash driver's on the same inputs:
`{"loop":N,"ts":"<RFC3339, second precision, UTC>","status":"<S>","goal":"<G>"}`
in exactly that key order (loop, ts, status, goal) — the same shape the bash
driver writes with its `date -u +%FT%TZ` printf. The full contract (events,
state sync, queue protocol) is pinned in `internal/loopdriver/DECISION.md`.

**Driver takeover rule** (`internal/loopdriver/DECISION.md`): the Go driver
took over production only after it survived at least one full live iteration
against the real devagent CLI with byte-identical ledger/event rows verified
against the bash driver's output on the same inputs. That gate passed during
the FR-GO-15 soak; with FR-GO-16 (#205) the Go driver is the production
default and the bash driver is gone.

## Tracker + PRD policy (2026-09-07 operator decision)

- **The task tracker is GitHub issues, not a document.** The driver claims the
  highest-priority open `selfbuild` issue each iteration (deterministic: priority
  rank, then oldest issue number), builds it, and closes it with evidence. The
  LLM selection path exists only as the empty-tracker fallback; anything it picks
  still ships against the repo directly.
- **The phase-1 pick rides along (issue #301).** The tracker decides *which*
  issue, research decides *what to do with it* — so on an issue-first pick the
  driver reads this iteration's `.selfbuild/research/loop-N.md` (the `## Pick`
  section, else the last line) and, when that issue is the pick's own subject,
  carries the rationale into `.selfbuild/goals/loop-N.md` and honours its
  action. A pick directing a present-tense "merge PR #N" / "land via open PR
  #N" dispatches `mergeGoalTemplate` — `gh pr checks`, merge the existing pull
  request, close the issue, and land the PRD status update the pull request
  itself may have missed — never a re-implementation of work already green on
  `origin`. The route is taken only while that pull request is still `OPEN`:
  research is asked to weigh merged PRs too ("does a merged PR already cover
  it?"), and a landed PR is history, not work — ship evidence is its merged
  state, which would already be true and would close an untouched issue.
- **The PRD is a state document.** `docs/PRD.md` records what the repo IS:
  status blockquotes, completion notes, architecture, the *Last updated* footer.
  It must reflect the current repo state at all times — enforced three ways:
  1. the loop skips an iteration (operator-degraded) when `docs/PRD.md` is locally dirty;
  2. every dispatch prompt carries the PRD-per-PR policy rider (update affected
     sections + footer in the same PR);
  3. the curator strikes shipped backlog lines and refreshes state claims daily.
- **Priorities:** `priority:P0` (do next) > `priority:P1` > `priority:P2` (backlog).
  Unlabeled selfbuild issues sort as P2. The curator re-prioritizes and files new
  issues (max 3/pass) from recent delivery history; the Q27 re-burn guard still
  skips any goal that already shipped.
- **Curation** (`scripts/prd-curator.sh`) now reconciles the tracker: closes
  shipped issues with evidence, files + re-prioritizes open ones, strikes shipped
  PRD lines, and publishes the PRD state diff as a docs PR. Tracker mutations are
  live on GitHub immediately; only the PRD diff ships as a PR.

## Guardrails (Kitchen Loop lineage)

Research phase 1 of this loop's first iteration (arXiv 2603.25697 "Kitchen Loop",
Ouroboros) surfaced four patterns now encoded here:

1. **Circuit breaker** — abort after `SELFBUILD_MAX_CONSECUTIVE_FAILURES` failed
   iterations within one run (regression-failure gate, default threshold 3).
2. **Starvation gate** — halt when the ledger shows N consecutive non-productive
   iterations across ALL runs; catches slow thrash that the per-run breaker misses.
3. **Lessons ratchet** — durable findings are appended to `lessons.md`, monotonic and
   version-controlled by convention; later iterations consume them instead of
   re-deriving (spec-anchored improvement, not metric-chasing).
4. **Failure feedback** — each iteration's research prompt includes the prior ledger
   tail so defects compound into fixes rather than repeats.

## Orca integration

Two supported modes:

**A. Watchdog automation (recommended).** Orca re-triggers one iteration per schedule;
each run is an isolated worktree session:

```sh
orca automations create \
  --name devagent-selfbuild \
  --trigger '23 */2 * * *' \
  --provider claude \
  --repo name:devagent \
  --workspace-mode new-per-run \
  --base-branch main \
  --prompt "Read .selfbuild/ledger.jsonl for the next loop number (ledger lines + 1). Then run exactly ONE iteration of the DevAgent self-build loop per docs/SELF-BUILD-LOOP.md through the Go driver with its cap pinned one past that number: SELFBUILD_MAX_ITERATIONS=<next+1> devagent loop. The driver performs state pull/push and picks the highest-priority open GitHub issue labeled selfbuild (priority:P0 > P1 > P2; the tracker is refilled by the prd-curator automation). PUSH CODE AS A PULL REQUEST; never direct to origin/main for product code. Do not start a second iteration." \
  --enabled --json
```

Manage with `orca automations list|show|runs|remove`.

**Tracker curation loop.** The build loop consumes the GitHub issue queue as its
goal backlog, so the tracker must stay current or iterations starve/repeat.
`scripts/prd-curator.sh` runs ONE curation pass: research what shipped (recent merged
PRs, ledger failures, lessons), verify claimed capabilities against the repo, then
reconcile the tracker (close shipped issues with evidence, file + re-prioritize)
and refresh docs/PRD.md as a state document (strike shipped backlog lines, update
section 18). Tracker mutations are live on GitHub immediately; the PRD state diff
ships as a docs PR:

```sh
bash scripts/prd-curator.sh                 # one pass, opens a PR when the PRD changes
SELFBUILD_DRY_RUN=1 bash scripts/prd-curator.sh   # preview only
```

Schedule it alongside the build loop so goals never go stale:

```sh
orca automations create \
  --name devagent-prd-curator \
  --trigger '47 6 * * *' \
  --provider claude \
  --repo name:devagent \
  --prompt "Run bash scripts/prd-curator.sh once. It researches recent delivery history, reconciles the GitHub issue tracker (closes shipped selfbuild issues with evidence, files and re-prioritizes new ones), and updates docs/PRD.md as a state document, then pushes a branch and opens a PR with the PRD diff. Do not merge it yourself unless repo tests are green." \
  --base-branch main \
  --enabled --json
```

The two loops are loosely coupled through the tracker: curator files and
prioritizes issues, build loop consumes them. Neither depends on the other's
schedule.

**Spawned-session auto-cleanup.** Mode A leaves one Orca workspace
(`auto-devagent-selfbuild-run-N-<ts>`) plus a live terminal behind per run, forever.
`scripts/orca-selfbuild-cleanup.sh` reclaims them automatically (macOS):

```sh
scripts/orca-selfbuild-cleanup.sh                        # dry-run: show what would go
scripts/orca-selfbuild-cleanup.sh --apply                # close terminals + delete worktrees
scripts/orca-selfbuild-cleanup.sh --install-launchagent  # schedule hourly via LaunchAgent
```

Deletion is gated on two safety checks per workspace: nothing modified within
`ORCA_MIN_AGE_SECS` (default 3600) and HEAD already merged into origin/main (or pushed
verbatim to origin), so no unpushed work is ever destroyed. Nested
`.devagent-worktrees/TASK` registrations are detached first; `orca worktree rm` refuses
the parent otherwise. The LaunchAgent writes `~/Library/Logs/orca-selfbuild-cleanup.log`.
Other knobs: `ORCA_SELFBUILD_REPO` (repo selector, default `name:devagent`),
`ORCA_MAIN_REPO` (git context for merge checks).

**B. Long-lived terminal.** Run `devagent loop` inside an Orca terminal tab
(`orca terminal create`); the driver loops internally. Pair with cc-guard
(`devagent guard-status --resume`) for API-failure recovery.

**C. Factory (scout + Orca workers).** `devagent create --repo . --scout --workers 3` supersedes the single-process loop with a decoupled factory:

- `devagent scout --interval 30` (24/7 LaunchAgent, `opencode` worker) researches `docs/PRD.md §4+§17` + ledger + lessons, writes PRD + task to `.devagent/prds/` + `.devagent/queue/`, heartbeats to `.devagent/scout.heartbeat.json`.
- Multiple `devagent consume --auto-pr [--auto-merge]` workers (each in its own Orca worktree from `orca worktree create`) claim tasks, implement in isolated `.devagent-worktrees/<id>`, validate G1/G3/G4, push `devagent/<id>` and open PR via `gh`, optionally auto-merge and `self-update`.
- See [docs/SCOUT.md](SCOUT.md) and `docs/SCOUT-CREATE-PRD.md` for the full runbook.

## Guardrails (operational)

- Circuit breaker stops the loop after repeated failures instead of thrashing.
- Every iteration starts from an up-to-date main (`git pull --ff-only`; skipped cleanly
  when offline).
- Research phase explicitly reviews the previous iteration's failure mode so defects
  compound into fixes, not repeats.
- No secrets are read or written; workers inherit repo-local env only.
