// Package loopdriver is the Go port of scripts/selfbuild-loop.sh (FR-GO-13,
// issue #202): the self-build loop driver as a library — one RunLoop entry
// preserving the live factory iteration semantics (starvation gate with
// degraded-row exemptions, already-shipped guard, sync-docs rc
// classification, PRD currency gate, issue-first pick, research/PO pane-run
// with abort guards, task dispatch, cleanup schedule, byte-compatible
// ledger rows). The bash driver stays production until the FR-GO-15 soak
// gate; see DECISION.md.
package loopdriver

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/orchestrator"
)

// run.go ports the main iteration loop of scripts/selfbuild-loop.sh
// (lines 230-601). Every iteration opens .selfbuild/logs/loop-N.log in
// append mode; phase messages land there (the bash `{...} >> "$LOG" 2>&1`
// block), and a fall-through iteration closes with the end marker + a
// `tail -5` to stdout.

// ledgerLines reads the ledger via the shared orchestrator reader.
func ledgerLines(repo string) []string {
	return orchestrator.ReadLedgerLines(filepath.Join(repo, ".selfbuild", "ledger.jsonl"))
}

// starved ports starved(): EvaluateStarvation over the ledger (degraded
// statuses exempt, productive break on ok|pr-open|merged|pushed).
func (d *driver) starved() bool {
	v := orchestrator.EvaluateStarvation(ledgerLines(d.cfg.Repo), d.cfg.StarvationLimit)
	return v.Starved
}

// breakerTripped prints the breaker line and reports whether the
// consecutive-failure budget is exhausted.
func (d *driver) breakerTripped(logF io.Writer) bool {
	if *d.fails >= d.cfg.MaxConsecutiveFailures {
		_, _ = fmt.Fprintf(logF, "circuit breaker: %d consecutive failures\n", *d.fails)
		return true
	}
	return false
}

// outcome classifies how an iteration body ended, mirroring the bash
// control flow (continue / fall-through to the tail / terminal exit).
type outcome int

const (
	outcomeNext        outcome = iota // proceed to the next phase in this iteration
	outcomeSkip                       // continue: next iteration, no tail, no fails reset
	outcomeNextReset                  // continue with explicit fails=0 (Q27 skip)
	outcomeFallThrough                // end marker + tail -5 + fails=0
	outcomeExit1                      // circuit breaker or push failure
)

// haltExit arms the exit watchdog for a terminal RunLoop verdict and
// returns the code the driver exits with. The graceful path (log close,
// deferred release, CLI unwind) is expected to finish long before the
// timer; the watchdog only fires if something still blocks. The hard exit
// skips the deferred lock release, which self-heals: the next start clears
// a dead-holder lock as stale (acquireLock).
func (d *driver) haltExit(code int) int {
	delay, exit, stderr := d.cfg.ExitWatchdogDelay, d.cfg.Exit, d.cfg.Stderr
	time.AfterFunc(delay, func() {
		_, _ = fmt.Fprintf(stderr, "[loop] exit watchdog: still alive %s after the halt verdict — forcing exit %d (issue #286)\n", delay, code)
		exit(code)
	})
	return code
}

// RunLoop executes the self-build loop until max iterations, the circuit
// breaker, or the starvation gate ends it. The returned int mirrors the
// bash driver's exit code: 0 for intentional stops (starvation, cap, lock
// contention), 1 for the circuit breaker / push-mode failure. Every
// terminal return passes through haltExit: the process must die once the
// driver has decided to stop (issue #286).
func RunLoop(cfg LoopConfig) int {
	cfg = cfg.WithDefaults()
	d := &driver{cfg: cfg, stateDir: filepath.Join(cfg.Repo, ".selfbuild")}

	_ = os.MkdirAll(d.stateDir+"/research", 0o755)
	_ = os.MkdirAll(d.stateDir+"/goals", 0o755)
	_ = os.MkdirAll(d.stateDir+"/logs", 0o755)
	_ = os.MkdirAll(d.stateDir+"/curation", 0o755)

	// GRADIENT adjacent-category scan + failure-cluster capture (once per
	// driver invocation, before the iteration loop; failed captures degrade
	// to empty, not a dead driver).
	gradient := d.gradientScan()
	clusters := d.failureClusters()
	// Durable state restore before numbering.
	if err := newStateSync(cfg.Repo, cfg.Stdout, cfg.Now).Pull(); err != nil {
		_, _ = fmt.Fprintln(cfg.Stdout, "[state] pull failed, starting from local state")
	}

	// Tracker repo fallback: SELFBUILD_GH_REPO unset derives from
	// `git remote get-url origin` (pick_issue's sed chain).
	if cfg.GHRepo == "" {
		if out, ok := d.gitQuiet("remote", "get-url", "origin"); ok {
			cfg.GHRepo = ghRepoFromRemote(strings.TrimSpace(out))
			d.cfg = cfg
		}
	}
	release, ok := acquireLock(cfg, d)
	if !ok {
		return d.haltExit(0)
	}
	defer release()

	fails := 0
	d.fails = &fails
	for {
		n := nextLoopNumber(d.stateDir + "/ledger.jsonl")

		// Iteration cap checked at loop head: the tail check was unreachable
		// for skip/continue paths — a capped run could skip-cycle forever
		// (2026-09-04 smoke evidence).
		if cfg.MaxIterations > 0 && n >= cfg.MaxIterations {
			_, _ = fmt.Fprintln(cfg.Stdout, "max iterations reached")
			return d.haltExit(0)
		}

		logPath := filepath.Join(d.stateDir, "logs", fmt.Sprintf("loop-%d.log", n))
		logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			_, _ = fmt.Fprintf(cfg.Stderr, "[loop] cannot open %s: %v\n", logPath, err)
			return d.haltExit(1)
		}
		_, _ = fmt.Fprintf(logF, "=== self-build loop %d start %s ===\n", n, rowTimestamp(cfg.Now))

		// Starvation gate: halt a loop that stopped shipping (checked before
		// spending tokens). Exit 0 — an intentional stop; exit 1 + hub
		// restart=on-failure would resurrect the halt every backoff interval
		// (2026-09-06: 58 hollow loop-106 starts).
		if d.starved() {
			_, _ = fmt.Fprintf(logF, "[starvation] %d consecutive non-productive iterations — halting loop\n", cfg.StarvationLimit)
			_ = logF.Close()
			return d.haltExit(0)
		}

		// Sweep auto-pr leftovers whose grace period has elapsed.
		d.sweepCleanup(logF)

		d.herdrSweep(logF)

		out := d.runIteration(n, logF, gradient, clusters)
		switch out {
		case outcomeSkip:
			// fails unchanged (bash `continue` without reset)
		case outcomeNextReset, outcomeFallThrough:
			fails = 0
			if out == outcomeFallThrough {
				_, _ = fmt.Fprintf(logF, "=== self-build loop %d end %s ===\n", n, rowTimestamp(cfg.Now))
				tailFile(cfg.Stdout, logPath, 5)
			}
		case outcomeExit1:
			_ = logF.Close()
			return d.haltExit(1)
		case outcomeNext:
			// keep fails as-is
		}
		_ = logF.Close()
	}
}

// runIteration executes one iteration body (bash lines 241-596).
func (d *driver) runIteration(n int, logF io.Writer, gradient, clusters string) outcome {
	cfg := d.cfg

	// Operator preflight (Q40): probe the provider before spending the
	// iteration on research + PO + task.
	if !cfg.DryRun {
		d.phase(n, "preflight", "role=selfbuild")
		if !d.preflight("selfbuild") {
			_, _ = fmt.Fprintf(logF, "[preflight] provider degraded - skipping iteration %d (ledger row written)\n", n)
			d.record(logF, n, "provider-degraded", "preflight: provider probe failed")
			*d.fails++
			if d.breakerTripped(logF) {
				return outcomeExit1
			}
			return outcomeNext
		}
	}

	// Doc freshness gate.
	if !cfg.NoSyncDocs {
		if out := d.runSyncDocs(n, logF); out != outcomeNext {
			return out
		}
	}
	// (sync-docs failure paths return outcomeSkip — bash `continue`)

	// PRD currency gate (2026-09-07 operator policy): a locally modified
	// PRD is the operator's mid-edit pause, not a factory fault — no
	// breaker increment, no starvation count (operator-degraded semantics).
	if !cfg.DryRun {
		if _, clean := d.gitQuiet("diff", "--quiet", "--", "docs/PRD.md"); !clean {
			_, _ = fmt.Fprintln(logF, "[prd-fresh] docs/PRD.md locally modified — operator mid-edit, skipping iteration")
			d.record(logF, n, "operator-degraded", "PRD dirty: operator mid-edit, state doc must land clean")
			cfg.Sleep(secsToDuration(cfg.SyncRetrySecs))
			return outcomeSkip // bash: continue
		}
	}

	// Tracker snapshot (issue-first): one gh call per iteration; a failed
	// listing degrades to empty and the LLM selection path runs instead.
	issuePick := ""
	if !cfg.DryRun {
		issuePick = d.pickIssue()
	}

	// Phase 1: Research.
	prevTail := prevLedgerTail(d.stateDir+"/ledger.jsonl", 3)
	lessonsCtx := d.lessonsCtx()
	if cfg.DryRun {
		_, _ = fmt.Fprintln(logF, "[dry-run] phase 1 research skipped")
		_ = os.WriteFile(filepath.Join(d.stateDir, "research", fmt.Sprintf("loop-%d.md", n)), []byte("# dry-run stub\n"), 0o644)
	} else {
		d.runResearchPhase(n, logF, prevTail, lessonsCtx, gradient, clusters)
	}

	// Phase 2a: queue-first selection (runs even in DRY_RUN).
	queued := claimQueueTask(cfg.Repo)
	if queued != nil {
		_ = os.WriteFile(filepath.Join(d.stateDir, "goals", fmt.Sprintf("loop-%d.md", n)), []byte(queued.Goal+"\n"), 0o644)
	}

	// Phase 2a-issue: the tracker outranks LLM selection. The phase-1 pick
	// text rides along (issue #301): research had concluded "#290 — merge
	// PR #298, not a rewrite" and the goal dispatched anyway said
	// "Implement … in full", burning an hour re-doing green work.
	issueNum, issueTitle := 0, ""
	var pick pickAction
	if queued == nil && issuePick != "" {
		issueNum, issueTitle = parseIssuePick(issuePick)
		pick = d.researchPick(n, issueNum)
		// A pick naming a pull request that is no longer open is reporting
		// history, not work — phase 1 is explicitly asked "does a merged PR
		// already cover it?" — so only an OPEN pull request may route this
		// iteration to merge (issue #301).
		if pick.mergePR != 0 {
			if state := d.prState(pick.mergePR); state != "OPEN" {
				_, _ = fmt.Fprintf(logF, "[issue] research pick names PR #%d but it is %s, not open — implementing #%d\n", pick.mergePR, state, issueNum)
				pick.mergePR = 0
			}
		}
		// merged = shipped, detection half: a PR cross-referenced on the
		// issue's timeline that is OPEN means the work exists on a branch —
		// dispatch the verify-and-merge route instead of a rewrite that
		// cannot open a second PR (#323 Case B: three no-pr rows re-running
		// #316 while its PR sat open). Fires only when research did not name
		// a PR; the pick-time OPEN guard above still applies to both.
		detected := false
		if pick.mergePR == 0 {
			if prNum := d.openPROfIssue(issueNum); prNum != 0 {
				pick.mergePR = prNum
				detected = true
			}
		}
		_, _ = fmt.Fprintf(logF, "[issue] claimed #%d from tracker (issue-first outranks LLM selection): %s\n", issueNum, issueTitle)
		goal := issueGoalTemplate(issueNum, issueTitle)
		if pick.mergePR != 0 {
			goal = mergeGoalTemplate(pick.mergePR, issueNum)
			if detected {
				_, _ = fmt.Fprintf(logF, "[issue] open PR #%d found for issue #%d — verify-and-merge dispatch (merged = shipped)\n", pick.mergePR, issueNum)
			} else {
				_, _ = fmt.Fprintf(logF, "[issue] research pick lands existing PR #%d — verify-and-merge dispatch, not a rewrite\n", pick.mergePR)
			}
		}
		goal = withPickRationale(goal, pick.rationale)
		_ = os.WriteFile(filepath.Join(d.stateDir, "goals", fmt.Sprintf("loop-%d.md", n)), []byte(goal+"\n"), 0o644)
		detail := fmt.Sprintf("#%d %s", issueNum, issueTitle)
		if pick.mergePR != 0 {
			detail = fmt.Sprintf("#%d merge PR #%d", issueNum, pick.mergePR)
		}
		d.phase(n, "issue", detail)
	} else if queued == nil {
		_, _ = fmt.Fprintln(logF, "[issue] no open selfbuild issue found — falling back to LLM selection")
	}

	// Phases 2-3: PO/LLM fallback (empty tracker + empty queue only).
	if queued == nil && issueNum == 0 && !cfg.DryRun {
		if out := d.runPOPhase(n, logF, prevTail, lessonsCtx, gradient, clusters); out != outcomeNext {
			return out
		}
	}

	// Goal-shape gate at the dispatch boundary. The bash invalid branch has
	// NO continue: it falls through to the tail (and the tail's fails=0), so
	// an invalid iteration never trips the breaker — mirror that.
	goalFile := filepath.Join(d.stateDir, "goals", fmt.Sprintf("loop-%d.md", n))
	goalData, gerr := os.ReadFile(goalFile)
	goalText := string(goalData)
	if gerr != nil || !validateGoalShape(goalText) {
		_, _ = fmt.Fprintln(logF, "[validate] goal file is not a Goal: statement — marking iteration invalid")
		d.record(logF, n, "invalid", goalText)
		// A queued claim whose goal is off-contract must be retired here or
		// the claim sits until its 2h lease lapses, re-claims, and records
		// another invalid row every cycle — the starvation halt with the row
		// still live (the failed-path precedent below). Bash never needed
		// this: its queue normalization guaranteed the ^Goal: prefix and it
		// had no word cap, so a queued goal could not fail the gate.
		if queued != nil {
			_ = markQueueTaskDone(cfg.Repo, queued.ID, "failed", goalRejectedDetail, queued)
		}
		return outcomeFallThrough
	}
	goal := strings.TrimRight(goalText, "\n")

	// Q27 guard: never re-implement a goal that already shipped (a ledger
	// entry with a productive status carries the same text). One ledger
	// snapshot serves both guards below, so the preflight judges the same
	// rows this guard did.
	ledger := ledgerLines(cfg.Repo)
	if v := orchestrator.AlreadyShipped(goal, ledger); v.Shipped {
		d.record(logF, n, "skipped", goal)
		if queued != nil {
			_ = markQueueTaskDone(cfg.Repo, queued.ID, "done", "already shipped (Q27 guard)", queued)
		}
		if issueNum != 0 {
			d.closeIssue(issueNum, fmt.Sprintf("self-build loop %d: goal already shipped (Q27 no re-burn guard) — closing as done", n))
		}
		_, _ = fmt.Fprintf(logF, "[ok] loop %d skipped (already shipped)\n", n)
		return outcomeNextReset
	}
	// Landing-evidence preflight: a goal whose previous landing recorded
	// landed-without-artifact must not be dispatched again — the merge behind
	// that row already read MERGED, so the re-run would re-read the same
	// stamp and record the same nothing. It sits beside the Q27 guard, ahead
	// of the DryRun branch, because it is the same kind of dispatch decision:
	// a dry run rehearses the verdict (and dispatches nothing either way).
	// The tracker issue stays open — the work is not on main — while the
	// queue claim is retired, or it would re-claim after its lease and
	// re-burn a skipped row every cycle (the shape gate's precedent).
	if ledgerLandingFingerprint(goal, ledger) != "" {
		_, _ = fmt.Fprintln(logF, "[verify] goal fingerprint matches a landed-without-artifact row — skipping dispatch")
		d.record(logF, n, "skipped", goal)
		if queued != nil {
			_ = markQueueTaskDone(cfg.Repo, queued.ID, "failed", preflightRejectedDetail, queued)
		}
		return outcomeSkip // bash: continue
	}

	if cfg.DryRun {
		_, _ = fmt.Fprintln(logF, "[dry-run] phases 4-7 skipped (implement/test/push)")
		d.record(logF, n, "ok", "(dry-run) "+goal)
		_, _ = fmt.Fprintf(logF, "[ok] loop %d complete\n", n)
		return outcomeFallThrough
	}

	// Phases 4-5-6: task dispatch under the outer wall-clock cap.
	// Driver-side verify-and-merge: when the run ends without shipping
	// evidence (nonzero rc, or rc 0 with no "PR opened:" line) but the
	// picked issue still has an OPEN pull request — research's pick, a
	// cross-referenced one, or one this very run opened — the driver lands
	// it itself instead of stopping one step short (loops 270-272 recorded
	// failed/no-pr while green mergeable PRs sat open).
	var rescuedPR int
	rescue := func() bool {
		rescuedPR = d.verifyAndMergeRescue(n, logF, pick.mergePR, issueNum)
		return rescuedPR != 0
	}
	out, prURL := d.runTaskPhase(n, logF, goal, queued, rescue)
	if out != outcomeNext {
		return out
	}
	// Issue #238: in pr push mode a task rc 0 is NOT shipped until a PR
	// exists. Without one, record a non-productive row and leave the issue
	// open for re-pick (soak-169 closed #230 on rc 0 with zero publish
	// events). push mode "main" commits+pushes below, so its close stays.
	//
	// Issue #301: a verify-and-merge dispatch lands an EXISTING pull request,
	// so it can never print "PR opened:" — that pull request having merged is
	// its publish evidence. The pick-time OPEN guard is what makes the evidence
	// mean anything: without it, a pull request merged last week would "prove"
	// an iteration that did nothing.
	//
	// evidencePR is the pull request whose merged state carries this
	// iteration's ship evidence — the landing-evidence gate's subject below.
	landed := false
	evidencePR := 0
	if pick.mergePR != 0 && d.prMerged(pick.mergePR) {
		landed, evidencePR = true, pick.mergePR
	} else if rescuedPR != 0 && d.prMerged(rescuedPR) {
		landed, evidencePR = true, rescuedPR
	}
	shipped := prURL != "" || landed
	if cfg.PushMode == "pr" && !shipped {
		if pr := d.verifyAndMergeRescue(n, logF, pick.mergePR, issueNum); pr != 0 {
			rescuedPR, evidencePR = pr, pr
			landed = true
		} else {
			_, _ = fmt.Fprintln(logF, "[publish] task succeeded without opening a PR — leaving the issue open for re-pick")
			d.record(logF, n, "no-pr", goal)
			return outcomeSkip // bash: continue
		}
	}

	// A landed merge moved origin/main: the repo-level gate has to judge the
	// merged tree, not the pre-merge checkout the driver is sitting in
	// (issue #301 — otherwise a successful land can only read as failed-tests).
	// The gate still runs when the pull is refused: main being unfast-
	// forwardable is itself worth a non-productive row, not a silent pass.
	if landed {
		if _, ff := d.gitQuiet("pull", "--ff-only"); !ff {
			_, _ = fmt.Fprintln(logF, "[testing] origin/main is not fast-forwardable — the gate judges the pre-merge tree")
		}
	}

	// Post-merge-back repo-level gate: format/lint first (cheap, and it
	// rejects exactly what CI's lint job rejects), then the test command.
	if rc := d.runRepoLintGate(logF); rc != 0 {
		_, _ = fmt.Fprintln(logF, "[testing] format/lint gate failed after merge-back")
		d.record(logF, n, "failed-lint", goal)
		*d.fails++
		if d.breakerTripped(logF) {
			return outcomeExit1
		}
		return outcomeSkip // bash: continue
	}
	if rc := d.runRepoTests(); rc != 0 {
		_, _ = fmt.Fprintln(logF, "[testing] repo tests failed after merge-back")
		d.record(logF, n, "failed-tests", goal)
		*d.fails++
		if d.breakerTripped(logF) {
			return outcomeExit1
		}
		return outcomeSkip // bash: continue
	}

	// Phase 7: push (commit mode only; pr mode already pushed inside task).
	// Failure is fatal: exit 1 immediately (no breaker arithmetic).
	if cfg.PushMode == "main" {
		d.gitQuiet("add", "-A")
		_, committed := d.gitQuiet("commit", "-m", fmt.Sprintf("self-build loop %d: %s", n, firstLineCapped(goal, 90)))
		_, pushed := d.gitQuiet("push")
		if !committed || !pushed {
			_, _ = fmt.Fprintln(logF, "[push] failed")
			d.record(logF, n, "push-failed", goal)
			return outcomeExit1
		}
	}

	// merged = shipped (DECISION.md): the tracker issue closes only when the
	// work is IN main. Three evidence kinds count: a verify-and-merge pick
	// whose PR merged (`landed`), the opened PR having merged since (auto-
	// merge or a human — #323 Case B: the loop used to close the issue at
	// PR-open, stranding six shipped-but-unmergeable branches), and main
	// push mode (commit+push above). evidencePR names the pull request the
	// first two rest on, for the artifact gate below.
	merged := landed || cfg.PushMode == "main"
	if evidencePR == 0 {
		if prNum := prNumberFromURL(prURL); prNum != 0 && d.prMerged(prNum) {
			evidencePR = prNum
			merged = true
		}
	}

	// Landing-evidence gate: a merge verdict must be backed by the artifacts
	// on main, not just gh's status stamp — loop 276 recorded `ok` for a PR
	// merged only as an auto-cleanup snapshot while its implementation files
	// never reached main. A PR whose touched implementation files are absent
	// records the non-productive landed-without-artifact row and leaves the
	// queue claim and tracker issue live, so the work stays pickable instead
	// of reading as shipped.
	if evidencePR != 0 && !d.landedArtifactsVerified(logF, evidencePR) {
		d.record(logF, n, landedWithoutArtifactStatus, goal)
		return outcomeSkip // bash: continue
	}

	// A landed merge records `ok` rather than `merged`: internal/lessons
	// scores the Q39 lesson impact over loop-result rows (guard.go), and the
	// TUI palette keys on `ok` — either a distinct label would read a
	// successful land as a failure. What the loop actually did is recorded
	// where it is read: the iteration log line, the `loop-phase` detail, and
	// this row's goal text.
	//
	// A merely-open PR records `pr-open` — productive for the starvation gate
	// (a PR exists; the loop is not starved) but deliberately NOT an
	// AlreadyShipped match (selfbuild-gate.go ships on ok|merged|pushed, not
	// pr-open), so the next iteration can pick the issue again and drive the
	// merge instead of skipping it and closing the issue as "already shipped".
	status := "ok"
	if !merged {
		status = "pr-open"
	}
	d.record(logF, n, status, goal)
	if queued != nil {
		_ = markQueueTaskDone(cfg.Repo, queued.ID, "done", "", queued)
	}
	if issueNum != 0 {
		if merged {
			d.closeIssue(issueNum, fmt.Sprintf("self-build loop %d: PR merged — shipped: %s", n, goal))
			d.applyPrHygieneTriage(logF)
		} else {
			_, _ = fmt.Fprintf(logF, "[publish] PR opened, not merged — issue #%d stays open for a verify-and-merge pick (merged = shipped)\n", issueNum)
		}
	}
	if cfg.PushMode == "pr" {
		d.scheduleCleanup(n)
	}
	_, _ = fmt.Fprintf(logF, "[ok] loop %d complete\n", n)
	return outcomeFallThrough
}

// runSyncDocs ports the sync-docs rc classification (0 ok / 1 generic /
// 2 dirty refusal / 3 diverged).
func (d *driver) runSyncDocs(n int, logF io.Writer) outcome {
	rc, out := d.syncDocsClass()
	if rc == 0 {
		// Sync already up to date prints "already at origin"; a pulled or
		// rebased sync prints its own one-liner — show it unless stale-noop.
		if !strings.Contains(out, "already at origin") && strings.TrimSpace(out) != "" {
			_, _ = fmt.Fprintf(logF, "[sync-docs] %s\n", strings.TrimRight(out, "\n"))
		}
		return outcomeNext
	}
	_, _ = fmt.Fprintf(logF, "[sync-docs] PRD refresh failed (rc=%d): %s\n", rc, strings.TrimRight(out, "\n"))
	switch rc {
	case 3:
		d.record(logF, n, "operator-diverged", "doc-sync diverged: operator must reconcile (conflict or diverged+dirty PRD)")
	case 2:
		d.record(logF, n, "operator-degraded", "doc-sync deferred: PRD locally modified")
	default:
		d.record(logF, n, "provider-degraded", "doc-sync failed: "+lastLineCapped(out, 120))
		*d.fails++
	}
	// Q41 paging, doc-sync surface: paging is observability and must never
	// fail — or delay — the cycle it reports; the breaker moved below it so
	// the exit does not preempt the page.
	d.pageDegradeBreach(itoa(rc), lastLineCapped(out, 120))
	if d.breakerTripped(logF) {
		return outcomeExit1
	}
	d.cfg.Sleep(secsToDuration(d.cfg.SyncRetrySecs))
	return outcomeSkip // bash: sleep + continue — skip the rest of the iteration
}

// runResearchPhase ports the phase-1 research dispatch + extraction.
func (d *driver) runResearchPhase(n int, logF io.Writer, prevTail, lessonsCtx, gradient, clusters string) {
	p := d.researchPaths(n)
	_ = os.Remove(p.done)
	prompt := researchPrompt(n, d.cfg.Repo, prevTail, lessonsCtx, gradient, clusters)
	if rc := d.paneRunDispatch(d.cfg.ResearchBin, prompt, p.raw, p.err, p.done, d.cfg.ResearchTimeout); rc != 0 {
		_, _ = fmt.Fprintf(logF, "[research] pane-run dispatch failed (rc=%d) — falling back to direct dispatch\n", rc)
	}
	if _, err := os.Stat(p.done); err != nil {
		_ = d.directDispatch(d.cfg.ResearchBin, prompt, p.raw, d.cfg.ResearchTimeout)
	}
	_ = os.Remove(p.done)
	_ = os.Remove(p.err)
	d.extractText(p.raw, p.out, p.abort, "[extract-failed] research produced no parsable output\n")
	_ = os.Remove(p.raw)
}

// runPOPhase ports the phases 2-3 PO dispatch + extraction.
func (d *driver) runPOPhase(n int, logF io.Writer, prevTail, lessonsCtx, gradient, clusters string) outcome {
	// PO preflight (Q40): the goal-selection dispatch dies silently under a
	// degraded provider; gate it so the skip lands as a ledger row instead.
	if !d.preflight("po") {
		_, _ = fmt.Fprintln(logF, "[preflight] provider degraded - skipping PO selection (ledger row written)")
		d.record(logF, n, "provider-degraded", "preflight(po): provider probe failed")
		*d.fails++
		if d.breakerTripped(logF) {
			return outcomeExit1
		}
		return outcomeSkip // bash: continue — skip the rest of the iteration
	}
	d.phase(n, "po", fmt.Sprintf("timeout %ds", d.cfg.ClaudeTimeout))
	rawPath := filepath.Join(d.cfg.Repo, "goal.tmp.raw")
	goalTmp := filepath.Join(d.cfg.Repo, "goal.tmp")
	donePath := filepath.Join(d.stateDir, "goals", fmt.Sprintf(".loop-%d.done", n))
	errPath := filepath.Join(d.cfg.Repo, "goal.tmp.err")
	_ = os.Remove(rawPath)
	_ = os.Remove(donePath)
	prompt := poPrompt(n, d.cfg.Repo, prevTail, lessonsCtx, gradient, clusters)
	if rc := d.paneRunDispatch(d.cfg.POBin, prompt, rawPath, errPath, donePath, d.cfg.ClaudeTimeout); rc != 0 {
		_, _ = fmt.Fprintf(logF, "[po] pane-run dispatch failed (rc=%d) — attempting partial extraction\n", rc)
	}
	if _, err := os.Stat(donePath); err != nil {
		if rc := d.directDispatch(d.cfg.POBin, prompt, rawPath, d.cfg.ClaudeTimeout); rc != 0 {
			_, _ = fmt.Fprintf(logF, "[po] direct dispatch failed (rc=%d) — attempting partial extraction\n", rc)
		}
	}
	_ = os.Remove(donePath)
	_ = os.Remove(errPath)
	d.extractText(rawPath, goalTmp, filepath.Join(d.stateDir, "goals", "last.aborted.ndjson"),
		"[extract-failed] PO produced no parsable output\n")
	// Publish: goal.tmp -> goals/loop-N.md (the mv always succeeds).
	if data, rerr := os.ReadFile(goalTmp); rerr == nil {
		_ = os.WriteFile(filepath.Join(d.stateDir, "goals", fmt.Sprintf("loop-%d.md", n)), data, 0o644)
	}
	_ = os.Remove(rawPath)
	return outcomeNext
}

// prOpenedRe matches the `devagent task` success line carrying the PR URL
// ("PR opened: <url>", the RunTask note printed by the CLI).
var prOpenedRe = regexp.MustCompile(`PR opened: (https?://\S+)`)

// runTaskPhase ports the phase-4-7 task dispatch with the failure path
// (failed row + queue done + breaker consult) and returns the PR URL the
// dispatch reported ("" = no PR was opened). Before recording a failure it
// consults rescue: a run whose picked issue's PR landed anyway ships instead
// of failing (verify-and-merge completion). A goal rejected at the
// dispatch boundary (taskGoalInvalidRC) is the invalid-goal class, not a
// failure: `invalid` row, fall-through, breaker never consulted.
func (d *driver) runTaskPhase(n int, logF io.Writer, goal string, queued *claimedTask, rescue func() bool) (outcome, string) {
	d.phase(n, "task", firstLineCapped(goal, 100))
	out, rc := d.taskDispatch(goal)
	if rc == taskGoalInvalidRC {
		_, _ = fmt.Fprintln(logF, strings.TrimSpace(out))
		d.record(logF, n, "invalid", goal)
		// Same retirement as the goal-file gate: any invalid from the
		// boundary must not leave its claim live. Unreachable for a queued
		// goal today (the file gate validates the identical text first) —
		// this is the guard that keeps the two-gate structure safe if the
		// gates ever diverge.
		if queued != nil {
			_ = markQueueTaskDone(d.cfg.Repo, queued.ID, "failed", goalRejectedDetail, queued)
		}
		return outcomeFallThrough, ""
	}
	if rc != 0 {
		if rescue != nil && rescue() {
			_, _ = fmt.Fprintln(logF, "[verify] task dispatch failed but the picked issue's PR landed — recording the ship")
			return outcomeNext, ""
		}
		_, _ = fmt.Fprintln(logF, "[implement] task failed")
		d.record(logF, n, "failed", goal)
		if queued != nil {
			_ = markQueueTaskDone(d.cfg.Repo, queued.ID, "failed", fmt.Sprintf("implement failed at loop %d", n), queued)
		}
		*d.fails++
		if d.breakerTripped(logF) {
			return outcomeExit1, ""
		}
		return outcomeSkip, "" // bash: continue
	}
	m := prOpenedRe.FindStringSubmatch(out)
	if m == nil {
		return outcomeNext, ""
	}
	return outcomeNext, m[1]
}

// mergeAttempts caps the driver-side verify-and-merge retries.
const mergeAttempts = 2

// ghRun wraps DefaultRunGh so the orchestrator automerge/sweep calls resolve
// the tracker repo the way the driver's own gh calls do — via GHRepo —
// instead of gh's cwd inference, which fails on checkouts whose origin is
// not a GitHub remote.
func (d *driver) ghRun() orchestrator.RunGh {
	if d.cfg.GhRun != nil {
		return d.cfg.GhRun
	}
	if d.cfg.GHRepo == "" {
		return orchestrator.DefaultRunGh
	}
	repo := d.cfg.GHRepo
	return func(args []string, cwd string) (*orchestrator.GhResult, error) {
		return orchestrator.DefaultRunGh(append(append([]string{}, args...), "--repo", repo), cwd)
	}
}

// mergePRBounded lands pull request pr through the existing
// AutoReviewAndMergeOne pipeline (status → checks wait → CI-Fixer → merge),
// retrying a bounded number of times; between attempts and at the end it
// re-reads gh's merged state, so an attempt whose merge landed even when its
// verdict row was lost still counts.
func (d *driver) mergePRBounded(n int, logF io.Writer, pr int) bool {
	run := d.ghRun()
	for attempt := 1; attempt <= mergeAttempts; attempt++ {
		o := orchestrator.AutoReviewAndMergeOne(d.cfg.Repo, pr, orchestrator.AutoReviewAndMergeOneOpts{
			Log: func(msg string) { _, _ = fmt.Fprintf(logF, "[automerge] %s\n", msg) },
		}, run)
		_, _ = fmt.Fprintf(logF, "[automerge] attempt %d: PR #%d %s (%s)\n", attempt, pr, o.Action, o.Detail)
		if o.Action == orchestrator.ActionMerged || d.prMerged(pr) {
			return true
		}
	}
	return d.prMerged(pr)
}

// verifyAndMergeRescue closes the verify-and-merge loop for an iteration
// whose run ended without shipping evidence: when the picked issue still has
// an OPEN pull request — research's pick (OPEN-guarded at pick time), a
// cross-referenced one, or one the run itself opened — the driver lands it
// through mergePRBounded and reports its number (0 = nothing landed).
func (d *driver) verifyAndMergeRescue(n int, logF io.Writer, pr, issueNum int) int {
	if pr == 0 && issueNum != 0 {
		pr = d.openPROfIssue(issueNum)
	}
	if pr == 0 || d.prState(pr) != "OPEN" {
		return 0
	}
	_, _ = fmt.Fprintf(logF, "[verify] run ended without shipping evidence — driver-side verify-and-merge of open PR #%d\n", pr)
	if d.mergePRBounded(n, logF, pr) {
		return pr
	}
	return 0
}

// applyPrHygieneTriage runs the shipped pr-hygiene sweep in apply mode after
// a ship: zombie TASK PRs (base-superseded, red-across-grace) and the
// landing-evidence triage of issues cited by open PRs (#336) act for real,
// closing zombie duplicates with the evidence the merge just produced.
// Best-effort observability only — a sweep failure never fails the ship.
func (d *driver) applyPrHygieneTriage(logF io.Writer) {
	dryRun := false
	res := orchestrator.SweepTaskPrHygiene(d.cfg.Repo, orchestrator.PrHygieneOptions{
		DryRun: &dryRun,
		Log:    func(msg string) { _, _ = fmt.Fprintf(logF, "[pr-hygiene] %s\n", msg) },
	}, d.ghRun())
	for _, o := range res.Outcomes {
		_, _ = fmt.Fprintf(logF, "[pr-hygiene] #%d %s (%s): %s\n", o.PR, o.Action, o.Reason, o.Detail)
	}
}

// prevLedgerTail mirrors `tail -k "$STATE/ledger.jsonl"`.
func prevLedgerTail(path string, k int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	if len(lines) > k {
		lines = lines[len(lines)-k:]
	}
	return strings.Join(lines, "\n")
}

// lessonsCtx builds the "Accumulated lessons (do not re-derive): ..." block:
// last 40 lines capped at 4000 bytes (head -c), exactly like the bash
// driver. Missing lessons file yields empty context.
func (d *driver) lessonsCtx() string {
	data, err := os.ReadFile(d.stateDir + "/lessons.md")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return ""
	}
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	digest := strings.Join(lines, "\n")
	if len(digest) > 4000 {
		digest = digest[:4000]
	}
	return "Accumulated lessons (do not re-derive): " + digest
}

// lastLineCapped mirrors `tail -1 | cut -c1-N`.
func lastLineCapped(s string, cap int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	last := ""
	if len(lines) > 0 {
		last = lines[len(lines)-1]
	}
	return truncateRunes(last, cap)
}

// parseIssuePick splits "NUM\tTITLE".
func parseIssuePick(pick string) (int, string) {
	i := strings.IndexByte(pick, '\t')
	if i < 0 {
		return 0, ""
	}
	n := 0
	_, _ = fmt.Sscanf(pick[:i], "%d", &n)
	return n, pick[i+1:]
}

// pickAction is what phase-1 research concluded about the issue the tracker
// handed it: land an already-open pull request, or implement from scratch
// (mergePR 0), plus the pick rationale verbatim.
type pickAction struct {
	mergePR   int
	rationale string
}

// researchPickHeadingRe matches the research artifact's trailing "## Pick"
// heading (the phase-1 prompt asks for the ranked top-3 "then THE single
// pick").
var researchPickHeadingRe = regexp.MustCompile(`(?im)^\s*#+\s*pick\b`)

// mergePickRe matches a pick that says do-not-implement, land-the-PR: a
// present-tense merge/land/ship verb within one clause of a pull-request
// reference ("merge PR #298", "land via open PR #298"). Past tense
// ("merged", "landed", "shipped") and bare "via"/"through" are excluded on
// purpose: research is asked "does a merged PR already cover it?"
// (prompts.go), so history is written in exactly those words.
//
// ponytail: verb proximity over prose, so a negated directive ("do not merge
// PR #298") still reads as a merge — and a false hit is the destructive
// direction: the merge route prints no "PR opened:", so shipping rests on
// prMerged, which is true for a PR that merged weeks ago, and the iteration
// would record a productive row AND close an issue nobody worked. runIteration
// caps that: the route requires prState == OPEN at pick time, so every stale or
// incidental PR reference self-disqualifies and a miss degrades to today's
// implement dispatch. Upgrade path if phrasing ever defeats the parse: have the
// research prompt emit `PICK: issue=#N action=merge-pr pr=#N
// rationale=<one line>` and parse key=value instead of prose.
var mergePickRe = regexp.MustCompile(`(?i)\b(?:merge|merges|merging|land|lands|landing|ship|ships|shipping)\b[^#.\n]{0,24}?PR\s*#(\d+)`)

// researchPickText returns the pick rationale from a phase-1 research
// artifact: the "## Pick" section body, or — with no heading — the last
// non-empty line (the prompt asks for the pick last).
func researchPickText(text string) string {
	if i := researchPickHeadingRe.FindStringIndex(text); i != nil {
		return strings.TrimSpace(text[i[1]:])
	}
	lines := tailLines(text, 8)
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// issueRefRe matches a GitHub `#N` reference, capturing the "PR" that makes
// it a pull-request mention rather than an issue one.
var issueRefRe = regexp.MustCompile(`(?i)(PR\s*)?#(\d+)`)

// firstIssueRef returns the issue the pick acts on: its first `#N` reference
// that is not a pull-request mention. Real artifacts lead with it
// ("**#290 — merge PR #298.**") and name other issues only in passing
// ("…fall through to #291"), so first-ref is what ties a pick to an iteration.
func firstIssueRef(text string) int {
	for _, m := range issueRefRe.FindAllStringSubmatch(text, -1) {
		if m[1] != "" {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		return n
	}
	return 0
}

// researchPick reads iteration loopNum's research artifact and reports what
// it said about issueNum. Only the pick whose subject IS issueNum counts: a
// missing artifact, an extract-failure diagnostic, or a pick about another
// issue (including one that merely mentions issueNum as a fallback) yields the
// zero action — the implement template with no rationale, i.e. the pre-#301
// behavior.
func (d *driver) researchPick(loopNum, issueNum int) pickAction {
	if issueNum <= 0 {
		return pickAction{}
	}
	data, err := os.ReadFile(d.researchPaths(loopNum).out)
	if err != nil {
		return pickAction{}
	}
	pick := researchPickText(string(data))
	if firstIssueRef(pick) != issueNum {
		return pickAction{}
	}
	a := pickAction{rationale: rowText(pick, pickRationaleCap)}
	if m := mergePickRe.FindStringSubmatch(pick); m != nil {
		if pr, err := strconv.Atoi(m[1]); err == nil && pr > 0 && pr != issueNum {
			a.mergePR = pr
		}
	}
	return a
}

// tailFile prints the last n lines of path to w (bash `tail -5 "$LOG"`).
func tailFile(w io.Writer, path string, n int) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, line := range lines {
		_, _ = fmt.Fprintln(w, line)
	}
}
