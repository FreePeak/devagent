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
	"strings"

	"github.com/FreePeak/devagent/internal/orchestrator"
)

// run.go ports the main iteration loop of scripts/selfbuild-loop.sh
// (lines 230-601). Every iteration opens .selfbuild/logs/loop-N.log in
// append mode; phase messages land there (the bash `{...} >> "$LOG" 2>&1`
// block), and a fall-through iteration closes with the end marker + a
// `tail -5` to stdout.

// goalValidationRe is the ^Goal: gate on the goal file content.
var goalValidationRe = regexp.MustCompile(`(?m)^Goal:`)

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

// RunLoop executes the self-build loop until max iterations, the circuit
// breaker, or the starvation gate ends it. The returned int mirrors the
// bash driver's exit code: 0 for intentional stops (starvation, cap, lock
// contention), 1 for the circuit breaker / push-mode failure.
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
		return 0
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
			return 0
		}

		logPath := filepath.Join(d.stateDir, "logs", fmt.Sprintf("loop-%d.log", n))
		logF, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(cfg.Stderr, "[loop] cannot open %s: %v\n", logPath, err)
			return 1
		}

		_, _ = fmt.Fprintf(logF, "=== self-build loop %d start %s ===\n", n, rowTimestamp(cfg.Now))

		// Starvation gate: halt a loop that stopped shipping (checked before
		// spending tokens). Exit 0 — an intentional stop; exit 1 + hub
		// restart=on-failure would resurrect the halt every backoff interval
		// (2026-09-06: 58 hollow loop-106 starts).
		if d.starved() {
			fmt.Fprintf(logF, "[starvation] %d consecutive non-productive iterations — halting loop\n", cfg.StarvationLimit)
			_ = logF.Close()
			return 0
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
				fmt.Fprintf(logF, "=== self-build loop %d end %s ===\n", n, rowTimestamp(cfg.Now))
				tailFile(cfg.Stdout, logPath, 5)
			}
		case outcomeExit1:
			_ = logF.Close()
			return 1
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
			fmt.Fprintf(logF, "[preflight] provider degraded - skipping iteration %d (ledger row written)\n", n)
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

	// Phase 2a-issue: the tracker outranks LLM selection.
	issueNum, issueTitle := 0, ""
	if queued == nil && issuePick != "" {
		issueNum, issueTitle = parseIssuePick(issuePick)
		goal := issueGoalTemplate(issueNum, issueTitle)
		_, _ = fmt.Fprintf(logF, "[issue] claimed #%d from tracker (issue-first outranks LLM selection): %s\n", issueNum, issueTitle)
		_ = os.WriteFile(filepath.Join(d.stateDir, "goals", fmt.Sprintf("loop-%d.md", n)), []byte(goal+"\n"), 0o644)
		d.phase(n, "issue", fmt.Sprintf("#%d %s", issueNum, issueTitle))
	} else if queued == nil {
		_, _ = fmt.Fprintln(logF, "[issue] no open selfbuild issue found — falling back to LLM selection")
	}

	// Phases 2-3: PO/LLM fallback (empty tracker + empty queue only).
	if queued == nil && issueNum == 0 && !cfg.DryRun {
		if out := d.runPOPhase(n, logF, prevTail, lessonsCtx, gradient, clusters); out != outcomeNext {
			return out
		}
	}

	// ^Goal: gate. The bash invalid branch has NO continue: it falls
	// through to the tail (and the tail's fails=0), so an invalid iteration
	// never trips the breaker — mirror that.
	goalFile := filepath.Join(d.stateDir, "goals", fmt.Sprintf("loop-%d.md", n))
	goalData, gerr := os.ReadFile(goalFile)
	goalText := string(goalData)
	if gerr != nil || !goalValidationRe.MatchString(goalText) {
		_, _ = fmt.Fprintln(logF, "[validate] goal file missing Goal: line — marking iteration invalid")
		d.record(logF, n, "invalid", goalText)
		return outcomeFallThrough
	}
	goal := strings.TrimRight(goalText, "\n")

	// Q27 guard: never re-implement a goal that already shipped (a ledger
	// entry with a productive status carries the same text).
	if v := orchestrator.AlreadyShipped(goal, ledgerLines(cfg.Repo)); v.Shipped {
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

	if cfg.DryRun {
		_, _ = fmt.Fprintln(logF, "[dry-run] phases 4-7 skipped (implement/test/push)")
		d.record(logF, n, "ok", "(dry-run) "+goal)
		_, _ = fmt.Fprintf(logF, "[ok] loop %d complete\n", n)
		return outcomeFallThrough
	}

	// Phases 4-5-6: task dispatch under the outer wall-clock cap.
	if out := d.runTaskPhase(n, logF, goal, queued); out != outcomeNext {
		return out
	}

	// Post-merge-back repo-level test gate.
	if rc := d.runNpmTest(); rc != 0 {
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

	// OK path.
	d.record(logF, n, "ok", goal)
	if queued != nil {
		_ = markQueueTaskDone(cfg.Repo, queued.ID, "done", "", queued)
	}
	if issueNum != 0 {
		d.closeIssue(issueNum, fmt.Sprintf("self-build loop %d shipped this issue: %s", n, goal))
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
			fmt.Fprintf(logF, "[sync-docs] %s\n", strings.TrimRight(out, "\n"))
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
			fmt.Fprintf(logF, "[po] direct dispatch failed (rc=%d) — attempting partial extraction\n", rc)
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

// runTaskPhase ports the phase-4-7 task dispatch with the failure path
// (failed row + queue done + breaker consult).
func (d *driver) runTaskPhase(n int, logF io.Writer, goal string, queued *claimedTask) outcome {
	d.phase(n, "task", firstLineCapped(goal, 100))
	if rc := d.taskDispatch(goal); rc != 0 {
		_, _ = fmt.Fprintln(logF, "[implement] task failed")
		d.record(logF, n, "failed", goal)
		if queued != nil {
			_ = markQueueTaskDone(d.cfg.Repo, queued.ID, "failed", fmt.Sprintf("implement failed at loop %d", n), queued)
		}
		*d.fails++
		if d.breakerTripped(logF) {
			return outcomeExit1
		}
		return outcomeSkip // bash: continue
	}
	return outcomeNext
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
