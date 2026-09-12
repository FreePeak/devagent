# DECISION: bash driver vs Go loopdriver (FR-GO-13, #202)

## Decision

`internal/loopdriver` is the Go port of `scripts/selfbuild-loop.sh` +
`scripts/selfbuild-state.sh` + the queue claim/done protocol, but the bash
driver **stays the production entrypoint** until the decision gate below
passes. The Go path is wired behind `devagent loop` (parent-wired post-merge,
command stubbed in `notPortedIssue`) and is not enabled for the real
selfbuild loop during the FR-GO-15 cutover window.

## Decision gate (when does Go take over?)

The Go driver takes over production only after **it survives at least one
full live iteration** against the real devagent CLI (scan-text → preflight →
issue/queue pick → research → task → repo test gate (`go test ./...` — the
Node tree retired in PR #240, so an `npm test` default can only fail, issue
#300) → record → state push) during the FR-GO-15 soak, with byte-identical
ledger/event rows verified against the bash driver's output on the same
inputs.

## Byte-compatibility contract (enforced by golden tests)

- Ledger rows: `{"loop":N,"ts":"<RFC3339 seconds, UTC>","status":S,"goal":G}`
  in that exact key order; goal text run through the bash transform
  (newline/tab → space, `"` deleted, first 160 chars).
- Events rows: `loop-result` mirrors the ledger row onto
  `.devagent/runs/orchestration/events.jsonl`; `loop-phase` rows carry
  `phase` and an optional `detail` (capped 120 chars, key omitted when
  empty).
- State sync: ledger merge is one-row-per-loop-number keyed by `"loop":<n>`,
  later `"ts"` wins (ties by the lexicographically greater line — GNU sort's
  last-resort comparison), output sorted by loop number; lessons is a
  ratchet-only union of unique lines (`awk '!seen[$0]++'`); push commits via
  `hash-object -w` + two-level `mktree` + `commit-tree` with parent =
  current remote tip, retried exactly once after a re-fetch/re-merge on a
  push race.
- Queue claim/done: `internal/queue` fenced writes; worker id
  `selfbuild-loop-<pid>`; done refused on a stale lease generation (the mjs
  printed `REFUSED (stale lease generation ...)` and exited 1 — the driver
  discards the output either way).

## Deliberate divergences (and why they are safe)

1. **Gates are in-process calls, not CLI rc contracts.** The bash driver
   called `devagent selfbuild-gate` and counted rc 1 as a verdict only when
   the output contained the verdict word (`starved:` / `already shipped`) to
   avoid confusing a crashed CLI (rc 1) with a real gate verdict. The Go
   driver calls `internal/orchestrator.EvaluateStarvation` /
   `AlreadyShipped` directly — there is no subprocess boundary, so the rc +
   output-word double check is moot. The verdict semantics (productive
   break on ok|pr-open|merged|pushed, degraded exemption, 60-char prefix +
   subject-id matching) live in the shared, already-tested package, and the
   loopdriver tests pin the externally visible behavior (halt on 5
   consecutive non-productive rows, degraded rows exempt).
2. **Row writers are owned locally instead of reusing
   `internal/lessons.RecordLoopResult`.** That helper normalizes unknown
   statuses to `failed` and stamps millisecond timestamps — both diverge
   from the bash driver's rows (no normalization; `date -u +%FT%TZ`
   second precision). The ledger/event rows are the state machine's
   durable record; byte parity wins over code reuse. `internal/ledger`
   readers (gates, clusters, lessons-eval) consume both shapes because the
   row *content* is identical in the fields they read.
3. **`EnsureStateBranch` composition.** The bash push path recreated the
   orphan branch implicitly via fetch-fail → push; the Go port keeps the
   same fetch-first shape and derives nothing from
   `internal/git.EnsureStateBranch` (the state branch is pushed by sha,
   never by branch name, so the helper is not needed; `BoundedGitSSHCommand`
   is reused for the ssh hardening).
4. **Extract-failure diagnostics.** The bash driver piped through
   `selfbuild-extract-text.mjs` and on helper failure wrote
   `[extract-failed] research produced no parsable output` (research) /
   `[extract-failed] PO produced no parsable output` (PO). The Go port
   calls `internal/scout.Extract` in-process (same shape-based semantics:
   `[extract-aborted]` diagnostic when the stream has no text, raw stream
   preserved at `last.aborted.ndjson`) and reproduces the
   extract-failed fallback when the raw file is unreadable.
5. **The goal-shape invalid path falls through to the iteration tail** (and
   the tail's `fails=0`), exactly like bash — an invalid iteration never
   trips the breaker. Golden tests pin this. One deliberate extension beyond
   bash: an invalid goal **retires a queue claim as `failed`** with the
   rejection detail (both the goal-file gate and the dispatch boundary).
   Bash's queued goals could never fail its `^Goal:` gate — queue
   normalization guarantees the prefix and bash had no word cap — so its
   no-retirement invalid branch was unreachable for them. The Go shape gate
   introduced the off-cap class (a human goal from POST /dispatch is a
   producer), and an unretired claim re-claims after its 2h lease and
   re-burns an invalid row every cycle: retirement is the failed-path
   precedent (row retired at `failed` + detail). The cap binds every
   producer, not just PO output — the PO prompt is the one statement
   contract all dispatched goals trace back to, and the task row's
   LastError carries the reason to the operator who enqueued it.

## What remains bash-only

- Nothing functional. The Go driver covers the full iteration cycle. The
  only bash-adjacent leftovers are the LaunchAgent wiring and the
  `npx tsx src/cli.ts` wrapper, both replaced by `DevagentBin`/`DevagentArgs`
  on `LoopConfig`.

## DECISION: merged = shipped (2026-09-11, #323)

**Shipped** means the work is IN main. Three kinds of evidence count:

1. a verify-and-merge pick whose PR is MERGED (`landed`),
2. the PR this iteration opened having merged by record time (auto-merge or
   a human),
3. push mode `main` (commit+push in phase 7).

A merely-open PR records the productive `pr-open` row and leaves the tracker
issue **open** — `pr-open` breaks the starvation streak but is NOT an
`AlreadyShipped` match (`ShippedStatuses` = ok|merged|pushed), so the issue
stays re-pickable and the next iteration drives the merge (the
verify-and-merge route that landed #286/#320 and #315/#324). This replaces
close-at-PR-open, which closed the issue as shipped and stranded the work on
unmergeable branches: 2026-09-11 found six such PRs (#323 Case B), five of
them red on CI's `lint` job for a single unformatted file the loop's
test-only gate never caught (now caught by the format/lint gate above the
test gate). `pr-open` rows score as loop successes in internal/lessons
(isLoopSuccess) and render amber in the TUI (merge pending, never green).

**Row status stays `ok` for landed merges** (byte-compatibility contract
above): internal/lessons and the TUI palette key on `ok`; what the loop
actually did lives in the iteration log, `loop-phase` detail, and goal text.

(Recorded 2026-09-11; shipped as PR #326.)

## DECISION: open-PR detection at pick time + lint-gate analysis tolerance (2026-09-11, #328)

1. **Detection half of merged = shipped.** When research names no PR, the
   pick path checks the claimed issue's timeline for a cross-referenced OPEN
   pull request (`openPROfIssue`: `gh api …/issues/N/timeline`, decoded like
   prState; the pick-time OPEN guard applies to the detected PR too) and
   routes the iteration to the verify-and-merge template. Without it,
   merged = shipped inverts the failure: the oldest-first pick re-selects an
   issue whose PR is open and dispatches a rewrite that cannot open a second
   PR (live: three `no-pr` rows re-running #316 while PR #325 sat open).
   Live-verified: iteration 261 detected PR #327 and landed #316.
2. **Lint-gate analysis tolerance.** Tier 2 fails the gate only for a
   completed `golangci-lint run` that reports findings (exit 1, CI's
   issues-found contract). Timeouts, context-loading errors (`Running
   error:`), other exits, and repos without go.mod mean the linter could not
   analyze — logged and skipped, never a `failed-lint` row the repo did not
   earn. Tier 2 logs its version: install CI's pinned golangci-lint
   (ci-go.yml) for parity — a local v2.1.6 certified green where CI's v2.13.2
   is stricter (and a shared-cache artifact produced phantom findings from
   another branch's WIP; re-running the job cleared it).
3. **`ProductiveGoals` filters on the shipped subset** (ok|merged|pushed):
   CheckBacklogPick strikes PRD backlog entries from these goals, and a
   `pr-open` row must not read as landed in the PRD while the Q27 guard
   correctly leaves the issue re-pickable.

(Recorded 2026-09-11; shipped as PR #328.)

## DECISION: a ship verdict needs artifacts on main (2026-09-12, TASK-mtxqd5xx-23cu)

`merged = shipped` reads gh's status stamp, and three consecutive iterations
(276-278) proved the stamp alone lies: loop 276 recorded `ok` for PR #342,
which had merged as an auto-cleanup snapshot only — the new test file it was
supposed to add never reached main — and loop 277 then burned a full
iteration re-running that already-"landed" goal. The driver now verifies the
artifact half of every ship verdict, on both ends:

1. **Landing-evidence gate (post-merge).** When the iteration's ship evidence
   is a merged pull request (`evidencePR`), `landedArtifactsVerified` lists
   the PR's touched files (`gh api …/pulls/N/files`) and main's tree
   (`gh api …/git/trees/main?recursive=1`) and requires every touched
   implementation file to exist as a blob on main. Docs surfaces (`docs/`,
   `*.md`) carry no implementation claim and are excluded — a
   PRD-regeneration PR legitimately ships alone. A mismatch records the
   non-productive `landed-without-artifact` row and returns before the
   `ok`/`pr-open` write, so the queue claim and the tracker issue stay live:
   the work is not on main, and the next iteration may run it again. A gh
   that cannot answer (down, empty GHRepo, unparseable body) logs and passes
   — the gate is evidence against false merges, not a second availability
   gate.
2. **Pick-time preflight.** Before a dispatch, the goal's fingerprint — the
   Q27 guard's rule-1 key, the first 60 normalized characters — is grepped
   for on `landed-without-artifact` rows; a match records `skipped` and does
   not dispatch. Without it the re-picked goal re-reads the same MERGED
   stamp and re-records the same artifact-less ship. Shipped rows stay the
   Q27 guard's business: its verdict closes the issue, which this one must
   not. The matched **queue claim is retired `failed`** with the preflight
   detail (the shape gate's lease-recycling precedent: a claim left sitting
   re-claims after its lease and re-burns a skipped row every cycle, never
   reaching the halt on its own). The tracker issue stays open — the work is
   not on main, and what to do about that is the operator's call, not the
   loop's.

`landed-without-artifact` is deliberately outside `ProductiveStatuses`
(internal/orchestrator): it is not a ship, it does not break the starvation
streak, and the loop halts for the operator instead of spinning. The row key
order and status vocabulary contract above is untouched — the vocabulary
already carried non-productive members (`no-pr`, `failed-*`, `invalid`), and
consumers read unknown statuses as non-success (internal/lessons
`isLoopSuccess`, the TUI palette's default).

Recovery note: the goal-shape fix PR #343 (validateGoalShape at the dispatch
boundary) was landed by this iteration directly — re-opened, head `50891d5`
verified with `go test ./...` in a throwaway worktree, merged as `38e6513` —
so it was not merged by the driver and did not itself exercise the gate
live. Both branches are pinned by fixture tests instead:
`TestRunLoopMergedWithoutArtifactRecordsEvidenceRow`,
`TestRunLoopMergePickRescuedByDriverSideMerge` (artifact-backed ship),
`TestRunLoopPreflightSkipsGoalLandedWithoutArtifact`,
`TestLedgerLandingFingerprint`.

Live decode note: the two API reads were checked against the real repository
before the fixtures were trusted — `pulls/343/files` returns `filename` plus
the lowercase `status` (`added`/`modified`/`removed`/`renamed`), and
`git/trees/main?recursive=1` returns `truncated` plus `tree[].path`/`type`
(`blob`/`tree`). A decode that silently mismatched would pass every merge as
cannot-say while every test stayed green.

(Recorded 2026-09-12; TASK-mtxqd5xx-23cu.)
