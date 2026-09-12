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

## DECISION: landing evidence on the fallback route (2026-09-12, TASK-mtxu01sd-1ted)

The artifact gate above reads `pick.mergePR`, which only the tracker path ever
sets — so a goal that arrived through the queue or the PO/LLM fallback (`pick`
stays zero there) had no evidence subject at all. Loops 270/271/274/280 each
logged `falling back to LLM selection` (0 `claimed #`), each dispatched a goal
naming the pull request to land (`Goal: Land PR #335/#336/#339/#344`), each of
those pull requests merged inside its own iteration window, and each recorded
the non-productive `no-pr` row: four false negatives in eleven passes against a
starvation limit of five, with Q27 equally blind. Two changes close the route:

1. **Goal-derived evidence subject.** Before the task dispatch, when
   `pick.mergePR == 0`, the merge target is parsed out of the goal text by
   `goalMergePR` — the same `mergePickRe` researchPick parses — and accepted
   only while `prState == "OPEN"`, the pick-time guard's semantics: a stale or
   incidental PR mention self-disqualifies, so nothing here can certify a
   do-nothing iteration. The derived PR then rides the existing wiring
   (`prMerged` → `evidencePR` → `landedArtifactsVerified`): a worker-performed
   merge records `ok` with its artifacts checked, never `no-pr`, and the Q27
   guard sees the ship on the next pass. Derived and still open at record time,
   it is the driver-side verify-and-merge rescue's subject, as on the tracker
   path. The dispatch decision gate above is untouched — the goal file itself
   is never rewritten off the pick path.
2. **The `no-pr` early return retires its queue claim**
   (`markQueueTaskDone(failed, noPRDetail)`), the shape gate's lease-recycling
   precedent: left live the claim re-claimed after its 2h lease and re-burned
   the same no-pr row every cycle, walking the loop into the starvation halt
   instead of ever reaching a verdict. Unreachable for tracker-path iterations
   (`queued == nil`), so the leave-open-for-re-pick semantics there are
   untouched.

Ceiling (named, not fixed): the derivation reads the whole goal text, so a
present-tense merge mention that is *incidental* to the directive — a quoted
example ("…goals like `Goal: Land PR #344`…") or a negated "do not merge PR
#298" — still parses; that is `mergePickRe`'s documented verb-proximity
ceiling, shared with the tracker path's research pick. The OPEN guard is the
cap: an incidental mention can only certify if that pull request happens to
merge inside the same iteration window, and the artifact gate still requires
its touched implementation files on main. Restricting the scan to the goal's
leading directive was considered and rejected: it narrows legitimate coverage
(a directive in a later clause, "Goal: fix X; then land PR #N") and it does
not close the cited class either — that goal is a single line, so its quoted
mention is a leading-clause match, not a later one. A tightening would have to
key on the directive's *position* in the text (the leading clause, before the
first sentence break), not on line boundaries. The upgrade path, if incidental
mentions ever bite, is structural rather than prose-based: the research
prompt's `PICK: action=merge-pr pr=#N` key=value shape (mergePickRe's upgrade
note), with the position-keyed scan as the cheaper intermediate step.

Pinned by `TestRunLoopGoalNamedOpenPRMergedRecordsShip` (OPEN at dispatch,
worker merges mid-run → `ok` plus the artifact gate reading the derived PR),
`TestRunLoopGoalNamedMergedPRRecordsNoPR` (an already-merged mention derives
nothing, records `no-pr`, retires the claim) and `TestGoalMergePR` (the
parse's present-tense contract).

(Recorded 2026-09-12; TASK-mtxu01sd-1ted.)

## DECISION: the merge window certifies, not the pre-dispatch stamp (2026-09-12, TASK-mtxwr39q-nva6)

Every landing verdict above reads a pull request's *state*, and a state says
nothing about **when** a merge happened — so a subject had to have read `OPEN`
*before* the dispatch (the fallback route's derivation above), and an
iteration whose pull request merged mid-run after a reopen had no evidence
subject at all. Loop 282 is the proof: its goal named `PR #345` mid-prose, gh
read that pull request `CLOSED` when the dispatch started (the worker's job
was to reopen it), the worker merged it at 04:39:14Z — inside the iteration's
own window — and the ledger recorded the non-productive row. `prState`
(issues.go) now decodes `state,mergedAt` through `prView`; all five OPEN gates
read the state half exactly as before, and the merge stamp answers the
question the state could not.

Two mechanisms use it, and both bind a merge to `[iterStart, now]`: the stamp
is captured immediately before the dispatch, so the window is the dispatch
window and not the (much longer) research window.

1. **Record-time certification.** `goalMergedWithin` scans the goal text for
   `PR #N` mentions and returns the first that is `MERGED` with an in-window
   stamp; a hit sets `landed`/`evidencePR` and the iteration ships `ok`
   through the existing artifact gate. It is asked on both paths that can end
   without publish evidence: inside the failed-dispatch rescue (loop 282
   exited nonzero) and as the last step before the `no-pr` row (the rc-0
   class of loops 270/271/274/280). The scan is mention-level on purpose: the
   certification does not need to know *why* the goal names the pull request,
   because the window is what qualifies it — a stamp before `iterStart`
   belongs to an earlier iteration (loop 281's mid-prose `#344`) and
   certifies nothing.
2. **Pre-dispatch refusal.** When the goal IS a `Land PR #N` directive whose
   target had already merged before `iterStart`, no dispatch can land it and
   a worker would only spend a run rediscovering that (loop 280's stale
   `Goal: Land PR #344`). The iteration records the non-productive `skipped`
   row and retires the queue claim `failed` with `mergedBeforeStartDetail` —
   the shape gate's lease-recycling precedent. The refusal binds to the
   directive's *position* (`goalHeadMergePR`: the statement must OPEN with
   the directive, nothing but whitespace before the match), because "a goal
   that mentions a pull request" is not "a goal that asks the loop to land
   it". Two shapes stay dispatching: loop 281's goal quoted `Goal: Land PR
   #344` mid-prose while its own work was the fallback-route fix, and — the
   reason the bound is not merely tidiness — the tracker's implement template
   embeds the issue TITLE in its leading clause, so a title like
   "Landing-evidence gap in PR #345 handling" matches `mergePickRe` inside
   the goal. A refusal there is unrecoverable: the tracker goal is
   regenerated identically every iteration, `queued == nil` leaves nothing to
   retire, and five non-productive rows trip the starvation halt — the whole
   loop stops over an issue title.

Ceiling (named, not fixed): the record-time scan is mention-level, so a goal
that merely quotes a pull request — an example, a "see also" — certifies on
that pull request when it happens to merge inside the window. The window and
the artifact gate are the caps: a merge that predates the iteration cannot
certify, and the pull request's touched implementation files must be on main.
Tightening takes the structural route this file already names for
`mergePickRe`: have the research prompt emit `PICK: action=merge-pr pr=#N`
and key the certification on that key=value instead of on prose. One residue
on the refusal side: a goal that OPENS with the stale directive but carries
unrelated work after it ("Goal: Land PR #344; also fix X") is refused on the
merge target alone, so a goal mixing the two directives belongs after the
first sentence break.

Pinned by `TestRunLoopGoalNamedMergedInWindowRecordsShip` (loop 282's window,
failed-dispatch path → `ok` plus the artifact gate reading #345),
`TestRunLoopGoalNamedMergedAfterNoPRRunRecordsShip` (the rc-0 no-pr path),
`TestRunLoopStaleGoalHeadMergedPRSkipsDispatch` (directive refusal, claim
retired, no worker), `TestRunLoopGoalNamedOutOfWindowPRRecordsNoPR` (an
out-of-window mention keeps the unchanged `no-pr`),
`TestRunLoopTrackerTitleReadingLikeMergeDirectiveStillDispatches` (the issue
title that would have halted the loop) and
`TestRunLoopGoalQuotingMergeDirectiveMidProseStillDispatches` (the mid-prose
quote).

(Recorded 2026-09-12; TASK-mtxwr39q-nva6.)

## DECISION: the rescue reaches CLOSED-but-unmerged pull requests (2026-09-12, TASK-mty0vgkg-oerc)

Every mechanism above reads a pull request's state *before* the dispatch, and
`verifyAndMergeRescue` carried the same OPEN guard — so a pull request closed
unmerged was unreachable for all of them. That is exactly the class the route
exists for: #346 and #347 were auto-closed by the zombie sweep's
`BaseBranchGone(main)` transport-noise bug minutes after they opened (green,
mergeable, `origin/main` already contained), and loop 282's #345 was CLOSED at
dispatch with the worker's job being to reopen it. Each time the goal asked for
the pull request to be landed, the branch was landable, and the iteration
recorded `no-pr`. Three pieces, all read-only until the reopen:

1. **`prView` widens.** One `gh pr view` now decodes
   `state,mergedAt,baseRefName,headRefName`. The state and merge-stamp halves
   are unchanged for every existing reader (all five OPEN gates, the window
   certification); the two ref names are what the reopen gate judges.
2. **The goal-text derivation takes a closed subject.** When `pick.mergePR ==
   0` and the goal names a merge target (`goalMergePR`), a CLOSED reference is
   taken as the evidence subject while `reopenSubject` holds: no merge stamp,
   `baseRefName == main`, and the head ref still resolving on origin
   (`gh api repos/<repo>/branches/<ref>` through `ghAPIDecode`, the driver's own
   repo-scoped read). The OPEN case is untouched and takes precedence, and a
   merged reference still falls through to the pre-dispatch refusal.
3. **The rescue reopens before merging.** `verifyAndMergeRescue` no longer
   bails on a non-OPEN state: it re-checks the gate — the derivation vetted the
   subject before the dispatch, and an iteration is long enough for a close to
   land mid-flight — runs `gh pr reopen <n> --repo <tracker>` (repo-scoped,
   60s wall, `pipeDrainDelay` teardown, the `prView` contract), and re-reads the
   state. The reopen is verified, never believed: `gh pr reopen` exiting 0
   without gh reporting OPEN is a refusal. Only then does the ordinary
   `mergePRBounded` → `AutoReviewAndMergeOne` path run — that pipeline skips any
   non-OPEN pull request itself, which is why the reopen has to happen here
   rather than inside it.

Ceiling (named, not fixed): "reopenable" is judged from the pull request's own
refs, not from the reason it was closed — gh does not return that in the same
read, and inferring it from the sweep's close comment would couple the rescue to
prose. A closed-unmerged pull request on main with a live head that a human
closed on purpose (superseded by newer work, abandoned) is therefore reopened
and merged when the goal names it as its merge target and the run ships nothing.
What bounds it: the goal must carry a present-tense merge directive naming that
pull request (`mergePickRe`, the verb-proximity parse this whole route rests
on), the reopen must actually take, `AutoReviewAndMergeOne` must still pass it
(checks, hazard scan, review, base probe), and the artifact gate must find its
touched implementation files on main. The residual exposure is
`mergePickRe`'s, not this change's: the scan is the whole goal text, so an
*incidental* directive-shaped mention — a goal quoting `Goal: Land PR #N`, or a
tracker issue whose TITLE reads like one ("reopen and merge PR #N") — becomes a
subject too, where the OPEN case could only certify a merge that happened anyway
and this one can reopen. Two upgrade paths, in cost order: bind the closed case
to the directive's position (`goalHeadMergePR`, the pre-dispatch refusal's
bound — rejected here because loop 282's goal carries its directive in the
second sentence), or make the parse structural (`PICK: action=merge-pr pr=#N`
from the research prompt) and key both routes on it. For a deliberate close: the
sweep records its verdict (reason plus evidence) in the ledger and
`reopenSubject` refuses a close the driver itself made.

Pinned by `TestRunLoopClosedUnmergedGoalPRReopenedAndMerged` (derivation →
reopen → merge → `ok`, with `gh pr reopen 344` ordered before `gh pr merge
344`), `TestRunLoopClosedGoalPRLandsNothingWhenNotReopenable` (a base-superseded
close and a head ref gone from origin never become subjects, and a reopen gh
refuses ends the rescue — all three keep the `no-pr` row with no merge) and
`TestVerifyAndMergeRescueRefusesNonReopenablePR` (the rescue's own re-check
refuses an ineligible subject before any mutation). The fake gh harness gained
the reopen/merge markers these need: a `pr reopen` decides whether later
`pr view` reads say OPEN, a `pr merge` decides whether they say MERGED.

Live decode note (the fixtures' counterpart, per this file's precedent): the
widened read and the ref probe were checked against the real repository before
the fakes were trusted. `gh pr view 347 --repo FreePeak/devagent --json
state,mergedAt,baseRefName,headRefName` answers `{"baseRefName":"main",
"headRefName":"devagent/TASK-mtxyflr1-fq6y","mergedAt":null,"state":"CLOSED"}` —
an unmerged close carries a JSON `null` stamp, which decodes to the empty string
`mergedAtTime` already treats as no evidence — and `gh api
repos/FreePeak/devagent/branches/devagent/TASK-mtxyflr1-fq6y` resolves the
slash-containing ref, while a missing one answers exit 1 with "Branch not found
(HTTP 404)": the shape `GH_BRANCHES_GONE` reproduces. #347 is itself that class —
closed unmerged on main, its head ref still on origin.

(Recorded 2026-09-12; TASK-mty0vgkg-oerc.)
