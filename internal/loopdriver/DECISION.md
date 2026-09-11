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
5. **The `^Goal:` invalid path falls through to the iteration tail** (and
   the tail's `fails=0`), exactly like bash — an invalid iteration never
   trips the breaker. Golden tests pin this.

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
