// Package file mirrors test/autopr.test.ts (FR-GO-07, issue #194): the
// decision fixtures drive the pure gates and the scripted gh seam — no real
// gh, no network. The localCiFixer end-to-end cases use real git against a
// temp bare 'origin' (same as the vitest originals) with a fake gh seam and
// a fake worker dispatcher.
package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/ledger"
)

func prStatusFixture(overrides func(*PrStatus)) PrStatus {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
	s := PrStatus{
		Number: 9, Title: "Loop 48", HeadRefName: "devagent/x", BaseRefName: "main",
		State: "OPEN", Mergeable: "MERGEABLE", ReviewDecision: "", HeadRefOid: "abc123",
		UpdatedAt: now,
		Checks:    []PrCheck{{Name: "test", Status: "COMPLETED", Conclusion: strPtr("SUCCESS")}},
	}
	if overrides != nil {
		overrides(&s)
	}
	return s
}

// ghResp scripts one seam response; err != nil = throw.
type ghResp struct {
	out string
	err error
}

// scriptedGh mirrors the TS scriptedGh helper: maps "subcommand" to a queued
// response and records every call.
func autoScriptedGh(responses map[string]ghResp) (RunGh, *[][]string) {
	var calls [][]string
	run := func(args []string, _ string) (*GhResult, error) {
		key := args[0]
		if len(args) > 0 && args[0] == "pr" {
			key = args[1]
		}
		calls = append(calls, args)
		r, ok := responses[key]
		if !ok {
			return &GhResult{}, nil
		}
		if r.err != nil {
			return nil, r.err
		}
		return &GhResult{Stdout: r.out}, nil
	}
	return run, &calls
}

// sequencedGh mirrors the TS sequencedGh: each "pr view" consumes the next
// queued JSON payload (re-poll sequences).
func autoSequencedGh(views []string, rest map[string]ghResp) (RunGh, *[][]string) {
	var calls [][]string
	qi := 0
	run := func(args []string, _ string) (*GhResult, error) {
		calls = append(calls, args)
		key := args[0]
		if args[0] == "pr" {
			key = args[1]
		}
		if key == "view" {
			if qi >= len(views) {
				return nil, fmt.Errorf("unexpected extra pr view (queue exhausted)")
			}
			payload := views[qi]
			qi++
			return &GhResult{Stdout: payload}, nil
		}
		r, ok := rest[key]
		if !ok {
			return &GhResult{}, nil
		}
		if r.err != nil {
			return nil, r.err
		}
		return &GhResult{Stdout: r.out}, nil
	}
	return run, &calls
}

func autoCallCount(calls *[][]string, sub string) int {
	n := 0
	for _, c := range *calls {
		if len(c) > 1 && c[0] == "pr" && c[1] == sub {
			n++
		}
	}
	return n
}

func autoCalledWith(calls *[][]string, needle string) bool {
	for _, c := range *calls {
		for _, a := range c {
			if strings.Contains(a, needle) {
				return true
			}
		}
	}
	return false
}

// --- evaluateChecks (green rollup) ---

func TestEvaluateChecksPassesWhenAllCompletedSucceeded(t *testing.T) {
	// passes when all completed checks succeeded
	v := EvaluateChecksOf(prStatusFixture(nil))
	if v.Pending {
		t.Fatal("must not be pending")
	}
	if !v.Passed {
		t.Fatal("must pass")
	}
	if len(v.FailedChecks) != 0 {
		t.Fatalf("failedChecks must be empty, got %v", v.FailedChecks)
	}
}

func TestEvaluateChecksSkippedPassesFailureBlocks(t *testing.T) {
	// treats SKIPPED as passing but FAILURE as blocking
	v := EvaluateChecksOf(prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{
			{Name: "test", Status: "COMPLETED", Conclusion: strPtr("SUCCESS")},
			{Name: "gitStream.cm", Status: "COMPLETED", Conclusion: strPtr("SKIPPED")},
			{Name: "lint", Status: "COMPLETED", Conclusion: strPtr("FAILURE")},
		}
	}))
	if v.Passed {
		t.Fatal("FAILURE must block")
	}
	if len(v.FailedChecks) != 1 || v.FailedChecks[0] != "lint=FAILURE" {
		t.Fatalf("expected ['lint=FAILURE'], got %v", v.FailedChecks)
	}
}

func TestEvaluateChecksPendingWhenAnyRunning(t *testing.T) {
	// marks pending when any check is still running
	v := EvaluateChecksOf(prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{{Name: "test", Status: "IN_PROGRESS"}}
	}))
	if !v.Pending {
		t.Fatal("running check must be pending")
	}
}

// --- scanAddedLinesForHazards ---

func TestScanAddedLinesReportsDA101OnAddedLinesOnly(t *testing.T) {
	// scans only added lines of new files and reports DA101
	diff := strings.Join([]string{
		"diff --git a/src/new.ts b/src/new.ts",
		"--- /dev/null",
		"+++ b/src/new.ts",
		"@@ -0,0 +1,3 @@",
		"+const x = 1;",
		"+fetch(url).then((r) => r.json());",
		"+context line that is not added",
		"-removed line with .then(",
	}, "\n")
	findings := ScanAddedLinesForHazards(diff)
	found := false
	for _, f := range findings {
		if f.RuleID == "DA101" && f.File == "src/new.ts" {
			found = true
		}
		if f.Severity == HazardSeverityMedium || f.Severity == HazardSeverityHigh {
			if f.RuleID == "DA102" || f.RuleID == "DA103" || f.RuleID == "DA104" {
				t.Fatalf("unexpected rule %s", f.RuleID)
			}
		}
	}
	if !found {
		t.Fatal("expected DA101 on the added fetch().then line")
	}
}

func TestScanAddedLinesCleanDiff(t *testing.T) {
	// reports nothing for a clean diff
	if findings := ScanAddedLinesForHazards("+++ b/src/a.ts\n+const ok = 1;\n"); len(findings) != 0 {
		t.Fatalf("clean diff must have no findings, got %v", findings)
	}
}

// --- evaluateAutoReview ---

func TestEvaluateAutoReviewApprovesOnGreen(t *testing.T) {
	// approves on green CI, mergeable, no hazards
	r := EvaluateAutoReview(prStatusFixture(nil), ReviewEvidence{MergeMethod: "squash"})
	if r.Event != ReviewEventApprove {
		t.Fatalf("expected APPROVE, got %s", r.Event)
	}
	if !strings.Contains(r.Body, "approved") {
		t.Fatalf("body must say approved: %q", r.Body)
	}
}

func TestEvaluateAutoReviewBlocksOnRedCI(t *testing.T) {
	// blocks on red CI
	r := EvaluateAutoReview(prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{{Name: "test", Status: "COMPLETED", Conclusion: strPtr("FAILURE")}}
	}), ReviewEvidence{MergeMethod: "squash"})
	if r.Event != ReviewEventRequestChanges {
		t.Fatalf("expected REQUEST_CHANGES, got %s", r.Event)
	}
	if !strings.Contains(r.Reason, "CI failed") {
		t.Fatalf("reason must name CI: %q", r.Reason)
	}
}

func TestEvaluateAutoReviewBlocksOnConflictsKeepsHazardsAdvisory(t *testing.T) {
	// blocks on conflicts but keeps hazards advisory
	conflict := EvaluateAutoReview(prStatusFixture(func(s *PrStatus) { s.Mergeable = "CONFLICTING" }), ReviewEvidence{MergeMethod: "squash"})
	if !strings.Contains(conflict.Reason, "conflicts") || conflict.Event != ReviewEventRequestChanges {
		t.Fatalf("conflict must block: %+v", conflict)
	}
	hazard := EvaluateAutoReview(prStatusFixture(nil), ReviewEvidence{
		Hazards:     []gates.Finding{{RuleID: "DA101", Severity: "high", Message: "m", File: "a.ts", Line: intPtr(1)}},
		MergeMethod: "squash",
	})
	if hazard.Event != ReviewEventApprove {
		t.Fatalf("advisory hazards must not block: %+v", hazard)
	}
	if !strings.Contains(hazard.Body, "DA101") {
		t.Fatalf("hazard must appear in body: %q", hazard.Body)
	}
}

// --- evaluateMergeQueueGate + ageHours ---

func TestMergeQueueGateSkipsRedAcrossGrace(t *testing.T) {
	// skips a PR red across the grace window
	red := prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{{Name: "test", Status: "COMPLETED", Conclusion: strPtr("FAILURE")}}
		s.UpdatedAt = time.Now().Add(-30 * time.Hour).UTC().Format(time.RFC3339Nano)
	})
	v := EvaluateMergeQueueGate(red, MergeQueueGateOptions{GraceHours: strPtrF(24), Now: autoInt64P2(timeNowMs())})
	if !v.Skip || v.Reason != SkipReasonRedAcrossGrace {
		t.Fatalf("expected red-across-grace skip: %+v", v)
	}
	if !strings.Contains(v.Detail, "24h grace") {
		t.Fatalf("detail must name the grace window: %q", v.Detail)
	}
}

func TestMergeQueueGatePassesRedWithinGrace(t *testing.T) {
	// passes a PR red within the grace window
	fresh := prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{{Name: "test", Status: "COMPLETED", Conclusion: strPtr("FAILURE")}}
		s.UpdatedAt = time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	})
	v := EvaluateMergeQueueGate(fresh, MergeQueueGateOptions{GraceHours: strPtrF(24), Now: autoInt64P2(timeNowMs())})
	if v.Skip {
		t.Fatalf("red within grace must pass: %+v", v)
	}
}

func TestMergeQueueGateUnparseableUpdatedAtIsOverdue(t *testing.T) {
	// treats an unparseable updatedAt as overdue (consistent with the sweeps)
	v := EvaluateMergeQueueGate(prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{{Name: "test", Status: "COMPLETED", Conclusion: strPtr("FAILURE")}}
		s.UpdatedAt = "not-a-date"
	}), MergeQueueGateOptions{GraceHours: strPtrF(24), Now: autoInt64P2(timeNowMs())})
	if !v.Skip || v.Reason != SkipReasonRedAcrossGrace {
		t.Fatalf("unknown age must count as overdue: %+v", v)
	}
}

func TestMergeQueueGateNeverSkipsPendingOrGreen(t *testing.T) {
	// never skips on grace when checks are pending or green
	pending := prStatusFixture(func(s *PrStatus) {
		s.Checks = []PrCheck{{Name: "test", Status: "IN_PROGRESS"}}
	})
	if EvaluateMergeQueueGate(pending, MergeQueueGateOptions{GraceHours: strPtrF(0), Now: autoInt64P2(timeNowMs())}).Skip {
		t.Fatal("pending must not skip")
	}
	if EvaluateMergeQueueGate(prStatusFixture(nil), MergeQueueGateOptions{GraceHours: strPtrF(0), Now: autoInt64P2(timeNowMs())}).Skip {
		t.Fatal("green must not skip")
	}
}

func TestMergeQueueGateSkipsSupersededNonCandidate(t *testing.T) {
	// skips a non-candidate head superseded by a same-base candidate
	conflicting := prStatusFixture(func(s *PrStatus) { s.Number = 7; s.Mergeable = "CONFLICTING" })
	v := EvaluateMergeQueueGate(conflicting, MergeQueueGateOptions{SupersedingCandidate: intPtr(9)})
	if !v.Skip || v.Reason != SkipReasonSuperseded {
		t.Fatalf("expected superseded skip: %+v", v)
	}
	if !strings.Contains(v.Detail, "#9") {
		t.Fatalf("detail must name the candidate: %q", v.Detail)
	}
}

func TestMergeQueueGateDoesNotSkipCandidateItself(t *testing.T) {
	// does not skip when this PR is the candidate or no candidate is given
	if EvaluateMergeQueueGate(prStatusFixture(nil), MergeQueueGateOptions{}).Skip {
		t.Fatal("no candidate given: must not skip")
	}
	if EvaluateMergeQueueGate(prStatusFixture(nil), MergeQueueGateOptions{SupersedingCandidate: intPtr(9)}).Skip {
		t.Fatal("this PR is the candidate: must not skip")
	}
}

func TestMergeQueueGateSupersessionRequiresCompletedChecks(t *testing.T) {
	// supersession requires completed checks with no failures (evidence rule)
	pending := prStatusFixture(func(s *PrStatus) {
		s.Number = 7
		s.Mergeable = "CONFLICTING"
		s.Checks = []PrCheck{{Name: "test", Status: "IN_PROGRESS"}}
	})
	failing := prStatusFixture(func(s *PrStatus) {
		s.Number = 7
		s.Mergeable = "CONFLICTING"
		s.Checks = []PrCheck{{Name: "test", Status: "COMPLETED", Conclusion: strPtr("FAILURE")}}
	})
	if EvaluateMergeQueueGate(pending, MergeQueueGateOptions{SupersedingCandidate: intPtr(9)}).Skip {
		t.Fatal("pending checks carry no evidence: must not skip")
	}
	if EvaluateMergeQueueGate(failing, MergeQueueGateOptions{SupersedingCandidate: intPtr(9)}).Skip {
		t.Fatal("failing checks are red, not superseded: must not skip")
	}
}

func TestAgeHoursMatchesSweepImplementation(t *testing.T) {
	// ageHours matches the sweep implementation
	age := AgeHours(time.Now().Add(-48*time.Hour).UTC().Format(time.RFC3339Nano), timeNowMs())
	if age == nil || *age < 47.9 {
		t.Fatalf("expected >=47.9h, got %v", age)
	}
	if AgeHours("not-a-date", timeNowMs()) != nil {
		t.Fatal("unparseable timestamp must return nil")
	}
}

// --- autoReviewAndMergeOne (scripted gh seam) ---

const basePrView = `{"number":9,"title":"T","headRefName":"devagent/x","baseRefName":"main","state":"OPEN","mergeable":"MERGEABLE","reviewDecision":"","author":{"login":"someone-else"},"statusCheckRollup":[{"name":"test","status":"COMPLETED","conclusion":"SUCCESS"}],"updatedAt":"2026-09-08T00:00:00.000Z"}`

func redPrView() string {
	v := map[string]any{}
	_ = json.Unmarshal([]byte(basePrView), &v)
	v["statusCheckRollup"] = []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "FAILURE"}}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestAutoMergeApprovesThenMergesOnGreen(t *testing.T) {
	// approves then merges on green evidence
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: basePrView}, "diff": {}, "review": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, run)
	if o.Action != ActionMerged {
		t.Fatalf("expected merged, got %+v", o)
	}
	if autoCallCount(calls, "review") == 0 || !autoCalledWith(calls, "--approve") {
		t.Fatal("review call must carry --approve")
	}
	if autoCallCount(calls, "merge") == 0 || !autoCalledWith(calls, "--squash") || !autoCalledWith(calls, "--delete-branch") {
		t.Fatal("merge call must carry --squash --delete-branch")
	}
}

func TestAutoMergeMergesWithAdvisoryHazardNote(t *testing.T) {
	// merges with an advisory hazard note in the approval body
	run, calls := autoScriptedGh(map[string]ghResp{
		"view":   {out: basePrView},
		"diff":   {out: "+++ b/src/a.ts\n+fetch(url).then((r) => r.json());"},
		"review": {},
		"merge":  {},
	})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, run)
	if o.Action != ActionMerged {
		t.Fatalf("expected merged, got %+v", o)
	}
	if !autoCalledWith(calls, "DA101") {
		t.Fatal("review body must carry the DA101 finding")
	}
}

func TestAutoMergeCommentsInsteadOfApprovingSelfAuthored(t *testing.T) {
	// comments instead of approving a self-authored PR, then merges
	selfView := strings.Replace(basePrView, `"someone-else"`, `"linh.doan"`, 1)
	run, calls := autoScriptedGh(map[string]ghResp{
		"view":    {out: selfView},
		"api":     {out: "linh.doan"},
		"diff":    {},
		"review":  {err: fmt.Errorf("self-approval is forbidden")},
		"comment": {},
		"merge":   {},
	})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, run)
	if o.Action != ActionMerged {
		t.Fatalf("expected merged, got %+v", o)
	}
	if autoCallCount(calls, "comment") != 1 {
		t.Fatal("verdict must be posted as a comment")
	}
	if autoCallCount(calls, "review") != 0 {
		t.Fatal("no review may be posted for a self-authored PR")
	}
}

func TestAutoMergeRedCIFixerCannotRunNoRepo(t *testing.T) {
	// posts request-changes and never merges when CI is red and the local
	// fixer cannot run (no repo)
	t.Setenv("DEVAGENT_REMOTE_TARGET", "")
	run, calls := autoScriptedGh(map[string]ghResp{
		"view":   {out: redPrView()},
		"diff":   {},
		"review": {},
		"merge":  {err: fmt.Errorf("should not be called")},
	})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, run)
	// '/repo' does not exist so the fixer fails fast and the outcome is the
	// structured ci-fix-failed instead of a bare request-changes dead end
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	if !strings.Contains(o.Detail, "ci-fix local dispatch") {
		t.Fatalf("detail must trace to the local dispatch: %q", o.Detail)
	}
	if autoCallCount(calls, "merge") != 0 || autoCallCount(calls, "review") != 0 {
		t.Fatal("no merge/review may happen when the fixer cannot run")
	}
}

func TestAutoMergeSkipsBaseFilterMismatch(t *testing.T) {
	// skips PRs whose base does not match the filter
	otherBase := strings.Replace(basePrView, `"baseRefName":"main"`, `"baseRefName":"pre-dogfood-r1"`, 1)
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: otherBase}, "review": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 1, AutoReviewAndMergeOneOpts{AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{BaseBranch: "main"}}, run)
	if o.Action != ActionSkipped {
		t.Fatalf("expected skipped, got %+v", o)
	}
	if !strings.Contains(o.Detail, "base is pre-dogfood-r1") {
		t.Fatalf("detail must name the base: %q", o.Detail)
	}
	if autoCallCount(calls, "review") != 0 {
		t.Fatal("no review may be posted for a base mismatch")
	}
}

func TestAutoMergeDryRunEvaluatesWithoutPosting(t *testing.T) {
	// dry-run evaluates without posting reviews or merging
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: basePrView}, "diff": {}, "review": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{DryRun: true}}, run)
	if !strings.HasPrefix(o.Detail, "[dry-run]") {
		t.Fatalf("detail must be marked [dry-run]: %q", o.Detail)
	}
	if autoCallCount(calls, "review") != 0 || autoCallCount(calls, "merge") != 0 {
		t.Fatal("dry-run must not post or merge")
	}
}

func TestAutoMergeFallsBackToAutoMergeUnderProtection(t *testing.T) {
	// falls back to --auto merge when direct merge is refused by protection
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: basePrView}, "diff": {}, "review": {}})
	merges := 0
	wrapped := func(args []string, cwd string) (*GhResult, error) {
		if len(args) > 1 && args[0] == "pr" && args[1] == "merge" {
			merges++
			if !autoContainsStr(args, "--auto") {
				return nil, fmt.Errorf("gh: pull request 9 is not mergeable: required reviews")
			}
		}
		return run(args, cwd)
	}
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, wrapped)
	if o.Action != ActionMerged {
		t.Fatalf("expected merged via --auto, got %+v", o)
	}
	if merges != 2 {
		t.Fatalf("expected 2 merge attempts, got %d", merges)
	}
	_ = calls
}

func autoContainsStr(items []string, needle string) bool {
	for _, s := range items {
		if s == needle {
			return true
		}
	}
	return false
}

// --- CI-fixer state machine (failed-then-green / still-red / no-fixer) ---

func fixerOK(note string) CiFixer {
	return func(CiFixRequest) CiFixResult { return CiFixResult{OK: true, Note: note} }
}

func TestCiFixFailedThenGreenDispatchesOnceAndMerges(t *testing.T) {
	// failed-then-green: dispatches the fixer once, re-polls, and merges
	run, calls := autoSequencedGh([]string{redPrView(), basePrView}, map[string]ghResp{"diff": {}, "review": {}, "merge": {}})
	var fixCalls []CiFixRequest
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			Fixer: func(req CiFixRequest) CiFixResult {
				fixCalls = append(fixCalls, req)
				return CiFixResult{OK: true, Note: "fix pushed"}
			},
		},
	}, run)
	if len(fixCalls) != 1 {
		t.Fatalf("expected 1 fixer dispatch, got %d", len(fixCalls))
	}
	if fixCalls[0].TaskID != "TASK-fix-9" {
		t.Fatalf("expected TASK-fix-9, got %q", fixCalls[0].TaskID)
	}
	if len(fixCalls[0].FailedChecks) != 1 || fixCalls[0].FailedChecks[0] != "test=FAILURE" {
		t.Fatalf("expected ['test=FAILURE'], got %v", fixCalls[0].FailedChecks)
	}
	if !strings.Contains(fixCalls[0].Prompt, "PR #9") || !strings.Contains(fixCalls[0].Prompt, "test=FAILURE") {
		t.Fatalf("prompt must carry PR and failed checks: %q", fixCalls[0].Prompt)
	}
	if o.Action != ActionMerged {
		t.Fatalf("expected merged, got %+v", o)
	}
	if autoCallCount(calls, "view") != 2 { // initial + re-poll after fix
		t.Fatalf("expected 2 views, got %d", autoCallCount(calls, "view"))
	}
}

func TestCiFixStillRedRecordsStructuredOutcome(t *testing.T) {
	// still-red: records a structured ci-fix-failed outcome and never merges
	run, calls := autoSequencedGh([]string{redPrView(), redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{Fixer: fixerOK("fix pushed")},
	}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	if len(o.FailedChecks) != 1 || o.FailedChecks[0] != "test=FAILURE" {
		t.Fatalf("expected ['test=FAILURE'], got %v", o.FailedChecks)
	}
	if o.Attempts == nil || *o.Attempts != 1 {
		t.Fatal("expected attempts 1")
	}
	if o.Summary == nil || !strings.Contains(*o.Summary, "failed: test") {
		t.Fatalf("summary must carry the rollup: %v", o.Summary)
	}
	if autoCallCount(calls, "merge") != 0 || autoCallCount(calls, "review") != 0 {
		t.Fatal("still-red must never merge or review")
	}
}

func TestCiFixNoFixerPropagatesWithoutRePolling(t *testing.T) {
	// no-fixer outcome propagates as ci-fix-failed without re-polling
	run, calls := autoSequencedGh([]string{redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			Fixer: func(CiFixRequest) CiFixResult { return CiFixResult{OK: false, Note: "remote preflight failed"} },
		},
	}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	if o.Attempts == nil || *o.Attempts != 1 {
		t.Fatal("expected attempts 1")
	}
	if o.Summary == nil || !strings.Contains(*o.Summary, "failed: test") {
		t.Fatalf("summary must carry the rollup: %v", o.Summary)
	}
	if !strings.Contains(o.Detail, "remote preflight failed") {
		t.Fatalf("detail must carry the fixer note: %q", o.Detail)
	}
	if autoCallCount(calls, "view") != 1 {
		t.Fatalf("no re-poll after a failed dispatch, got %d views", autoCallCount(calls, "view"))
	}
}

func TestCiFixDispatchThrowCaught(t *testing.T) {
	// dispatch throw is caught and reported as ci-fix-failed
	run, _ := autoSequencedGh([]string{redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			Fixer: func(CiFixRequest) CiFixResult { panic("ssh exploded") },
		},
	}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	if !strings.Contains(o.Detail, "ssh exploded") {
		t.Fatalf("detail must carry the panic: %q", o.Detail)
	}
}

func TestCiFixDefaultsToLocalFixerWithoutRemoteTarget(t *testing.T) {
	// defaults to the built-in dispatcher, which falls back to the local
	// fixer without DEVAGENT_REMOTE_TARGET
	t.Setenv("DEVAGENT_REMOTE_TARGET", "")
	run, _ := autoSequencedGh([]string{redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	if !strings.Contains(o.Detail, "ci-fix local dispatch") {
		t.Fatalf("detail must trace to the local dispatch: %q", o.Detail)
	}
}

// --- CI-fixer ledger rows ---

func autoReadLedgerRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	file := filepath.Join(repo, ledger.LedgerDir, "events.jsonl")
	data, err := os.ReadFile(file)
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err == nil {
			out = append(out, row)
		}
	}
	return out
}

func TestCiFixLedgerRowsFailedThenGreen(t *testing.T) {
	// writes a dispatch row and a failed-then-green outcome row
	repo := t.TempDir()
	run, _ := autoSequencedGh([]string{redPrView(), basePrView}, map[string]ghResp{"diff": {}, "review": {}, "merge": {}})
	o := AutoReviewAndMergeOne(repo, 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{Fixer: fixerOK("fix pushed")},
	}, run)
	if o.Action != ActionMerged {
		t.Fatalf("expected merged, got %+v", o)
	}
	rows := autoReadLedgerRows(t, repo)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d: %v", len(rows), rows)
	}
	if rows[0]["event"] != "ci-fix-dispatched" || rows[0]["taskId"] != "TASK-fix-9" || rows[0]["pr"] != float64(9) {
		t.Fatalf("dispatch row mismatch: %+v", rows[0])
	}
	if checks, _ := rows[0]["failedChecks"].([]any); len(checks) != 1 || checks[0] != "test=FAILURE" {
		t.Fatalf("dispatch row failedChecks mismatch: %+v", rows[0]["failedChecks"])
	}
	if rows[1]["event"] != "ci-fix-outcome" || rows[1]["outcome"] != "failed-then-green" {
		t.Fatalf("outcome row mismatch: %+v", rows[1])
	}
}

func TestCiFixLedgerRowsStillRed(t *testing.T) {
	// writes a dispatch row and a still-red outcome row
	repo := t.TempDir()
	run, _ := autoSequencedGh([]string{redPrView(), redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	o := AutoReviewAndMergeOne(repo, 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{Fixer: fixerOK("fix pushed")},
	}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	rows := autoReadLedgerRows(t, repo)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0]["event"] != "ci-fix-dispatched" || rows[0]["pr"] != float64(9) {
		t.Fatalf("dispatch row mismatch: %+v", rows[0])
	}
	if rows[1]["event"] != "ci-fix-outcome" || rows[1]["outcome"] != "still-red" || rows[1]["pr"] != float64(9) {
		t.Fatalf("outcome row mismatch: %+v", rows[1])
	}
}

func TestCiFixLedgerRowsOnlyOutcomeWhenNeverDispatched(t *testing.T) {
	// writes only a ci-fix-failed outcome row when dispatch was never dispatched
	repo := t.TempDir()
	run, _ := autoSequencedGh([]string{redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	AutoReviewAndMergeOne(repo, 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			Fixer: func(CiFixRequest) CiFixResult { return CiFixResult{OK: false, Note: "remote preflight failed"} },
		},
	}, run)
	rows := autoReadLedgerRows(t, repo)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0]["event"] != "ci-fix-outcome" || rows[0]["outcome"] != "ci-fix-failed" || rows[0]["pr"] != float64(9) {
		t.Fatalf("outcome row mismatch: %+v", rows[0])
	}
}

func TestCiFixLedgerRowsOnlyOutcomeWhenDispatchThrows(t *testing.T) {
	// writes only a ci-fix-failed outcome row when dispatch throws
	repo := t.TempDir()
	run, _ := autoSequencedGh([]string{redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	AutoReviewAndMergeOne(repo, 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			Fixer: func(CiFixRequest) CiFixResult { panic("ssh exploded") },
		},
	}, run)
	rows := autoReadLedgerRows(t, repo)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0]["event"] != "ci-fix-outcome" || rows[0]["outcome"] != "ci-fix-failed" || rows[0]["pr"] != float64(9) {
		t.Fatalf("outcome row mismatch: %+v", rows[0])
	}
}

func TestCiFixStillRedSequencesEmitCountableRoundTrips(t *testing.T) {
	// still-red sequences emit countable round-trip rows across multiple fix attempts
	repo := t.TempDir()
	run, _ := autoSequencedGh([]string{redPrView(), redPrView(), redPrView(), redPrView()}, map[string]ghResp{"diff": {}, "merge": {}})
	o := AutoReviewAndMergeOne(repo, 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{Fixer: fixerOK("fix pushed")},
	}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	rows := autoReadLedgerRows(t, repo)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows after run 1, got %d", len(rows))
	}
	o = AutoReviewAndMergeOne(repo, 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{Fixer: fixerOK("fix pushed")},
	}, run)
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	rows = autoReadLedgerRows(t, repo)
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows after run 2, got %d", len(rows))
	}
	dispatched, outcomes := 0, 0
	allStillRed := true
	for _, r := range rows {
		switch r["event"] {
		case "ci-fix-dispatched":
			dispatched++
		case "ci-fix-outcome":
			outcomes++
			if r["outcome"] != "still-red" {
				allStillRed = false
			}
		}
	}
	if dispatched != 2 || outcomes != 2 || !allStillRed {
		t.Fatalf("round-trip rows mismatch: dispatched=%d outcomes=%d allStillRed=%v", dispatched, outcomes, allStillRed)
	}
}

// --- defaultCiFixer ---

func TestDefaultCiFixerShortCircuitsWithoutRemoteTarget(t *testing.T) {
	// short-circuits without DEVAGENT_REMOTE_TARGET and never touches remote transport
	t.Setenv("DEVAGENT_REMOTE_TARGET", "")
	remoteCalled := false
	remoteTaskSeam = func(RemoteTaskRequest, RemoteTaskDeps) RemoteTaskResult {
		remoteCalled = true
		return RemoteTaskResult{}
	}
	defer func() { remoteTaskSeam = defaultRemoteTaskSeam }()
	res := DefaultCiFixer(CiFixRequest{RepoPath: "/repo", PR: 42, TaskID: "TASK-fix-42", FailedChecks: []string{"test=FAILURE"}, Prompt: "Fix the failing CI checks on PR #42."})
	if res.OK {
		t.Fatal("local fixer must fail on /repo")
	}
	if !strings.Contains(res.Note, "ci-fix local dispatch") {
		t.Fatalf("note must trace to local dispatch: %q", res.Note)
	}
	if remoteCalled {
		t.Fatal("remote transport must not be touched without a target")
	}
}

func TestDefaultCiFixerDelegatesToRemoteTransport(t *testing.T) {
	// delegates to runRemoteTask with the target, prompt, and TASK-fix-<pr> id
	t.Setenv("DEVAGENT_REMOTE_TARGET", "deploy@host:/srv/app")
	var got RemoteTaskRequest
	remoteTaskSeam = func(req RemoteTaskRequest, _ RemoteTaskDeps) RemoteTaskResult {
		got = req
		return RemoteTaskResult{OK: true, Note: "remote PR opened"}
	}
	defer func() { remoteTaskSeam = defaultRemoteTaskSeam }()
	res := DefaultCiFixer(CiFixRequest{RepoPath: "/repo", PR: 42, TaskID: "TASK-fix-42", FailedChecks: []string{"test=FAILURE"}, Prompt: "Fix the failing CI checks on PR #42."})
	if !res.OK || res.Note != "remote PR opened" {
		t.Fatalf("remote result must propagate: %+v", res)
	}
	if got.Target != "deploy@host:/srv/app" || got.Prompt != "Fix the failing CI checks on PR #42." || got.TaskID != "TASK-fix-42" {
		t.Fatalf("remote request mismatch: %+v", got)
	}
}

// --- localCiFixer (real git, temp bare origin, fake gh seam + fake worker) ---

// tempRepoWithOrigin mirrors the TS fixture: real temp git repo with one
// commit on main, wired to a bare 'origin'. Returns [repo, bare].
func autoTempRepoWithOrigin(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	bare := repo + "-remote.git"
	gitRun := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	autoMustRun(t, "git", "init", "-q", "--bare", "-b", "main", bare)
	gitRun("init", "-q", "-b", "main", ".")
	gitRun("config", "user.email", "t@t")
	gitRun("config", "user.name", "t")
	gitRun("commit", "--allow-empty", "-m", "init")
	gitRun("remote", "add", "origin", bare)
	gitRun("push", "-q", "-u", "origin", "main")
	return repo, bare
}

func autoMustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = "/"
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v failed: %v: %s", name, args, err, out)
	}
}

func TestLocalCiFixerFailsFastOnUnresolvableHead(t *testing.T) {
	// fails fast when gh cannot resolve the PR head branch
	repo, _ := autoTempRepoWithOrigin(t)
	res := LocalCiFixer(CiFixRequest{RepoPath: repo, PR: 7, TaskID: "TASK-fix-7", FailedChecks: []string{"test"}, Prompt: "p"})
	if res.OK {
		t.Fatal("expected failure")
	}
	if !strings.Contains(res.Note, "cannot resolve PR head branch") {
		t.Fatalf("note must name the cause: %q", res.Note)
	}
}

func TestLocalCiFixerEndToEnd(t *testing.T) {
	// end-to-end: checks out the PR branch in a worktree, runs the worker,
	// pushes the fix, removes the worktree
	repo, bare := autoTempRepoWithOrigin(t)
	autoMustGit(t, repo, "push", "-q", "origin", "main:refs/heads/devagent/fix-branch")
	autoMustGit(t, repo, "fetch", "origin", "devagent/fix-branch")
	fakeGh := func(args []string, _ string) (*GhResult, error) {
		return &GhResult{Stdout: "devagent/fix-branch\n"}, nil
	}
	worker := fakeDispatcherFunc(func(req WorkerDispatchRequest) WorkerDispatchResult {
		if err := os.WriteFile(filepath.Join(req.Cwd, "ci-fix.txt"), []byte("fixed by localCiFixer test"), 0o644); err != nil {
			return WorkerDispatchResult{ExitCode: 1, ErrorText: err.Error()}
		}
		return WorkerDispatchResult{ExitCode: 0, ResultText: "fixed"}
	})
	res := localCiFixerSeam(CiFixRequest{RepoPath: repo, PR: 7, TaskID: "TASK-fix-7", FailedChecks: []string{"test"}, Prompt: "fix it"}, fakeGh, worker)
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	if !strings.Contains(res.Note, "fix pushed to devagent/fix-branch") {
		t.Fatalf("note must name the branch: %q", res.Note)
	}
	// Worktree removed
	if _, err := os.Stat(filepath.Join(repo, ".devagent-worktrees", "TASK-fix-7")); !os.IsNotExist(err) {
		t.Fatal("fix worktree must be removed")
	}
	// A new commit was pushed
	out, err := exec.Command("git", "ls-remote", bare, "refs/heads/devagent/fix-branch").CombinedOutput()
	if err != nil {
		t.Fatalf("ls-remote failed: %v", err)
	}
	tip := strings.TrimSpace(strings.Split(string(out), "\t")[0])
	localOut, err := exec.Command("git", "-C", repo, "rev-parse", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse failed: %v", err)
	}
	local := strings.TrimSpace(string(localOut))
	if tip == local {
		t.Fatal("a new commit must have been pushed to the fix branch")
	}
}

func autoMustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v: %s", args, err, out)
	}
}

func TestLocalCiFixerRefusesEmptyFix(t *testing.T) {
	// refuses to push an empty fix (worker made no changes)
	repo, _ := autoTempRepoWithOrigin(t)
	autoMustGit(t, repo, "push", "-q", "origin", "main:refs/heads/devagent/fix-branch")
	autoMustGit(t, repo, "fetch", "origin", "devagent/fix-branch")
	fakeGh := func(args []string, _ string) (*GhResult, error) {
		return &GhResult{Stdout: "devagent/fix-branch\n"}, nil
	}
	worker := fakeDispatcherFunc(func(WorkerDispatchRequest) WorkerDispatchResult {
		return WorkerDispatchResult{ExitCode: 0, ResultText: "no-op"}
	})
	res := localCiFixerSeam(CiFixRequest{RepoPath: repo, PR: 7, TaskID: "TASK-fix-7", FailedChecks: []string{"test"}, Prompt: "fix it"}, fakeGh, worker)
	if res.OK {
		t.Fatal("expected refusal")
	}
	if !strings.Contains(res.Note, "no changes") {
		t.Fatalf("note must name the no-op: %q", res.Note)
	}
}

// --- merge-queue gate inside autoReviewAndMergeOne ---

func TestAutoMergeSkipsRedAcrossGraceWithoutFixerOrReview(t *testing.T) {
	// skips a PR red across the grace window without a fixer dispatch or review
	staleRed := redViewUpdated(-30 * time.Hour)
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: staleRed}, "diff": {}, "review": {}, "merge": {}, "comment": {}})
	fixerCalled := false
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			GraceHours: strPtrF(24),
			Fixer:      func(CiFixRequest) CiFixResult { fixerCalled = true; return CiFixResult{OK: true, Note: "never"} },
		},
	}, run)
	if o.Action != ActionSkipped {
		t.Fatalf("expected skipped, got %+v", o)
	}
	if !strings.Contains(o.Detail, "red-across-grace") || !strings.Contains(o.Detail, "24h grace") {
		t.Fatalf("detail must carry the gate reason: %q", o.Detail)
	}
	if fixerCalled || autoCallCount(calls, "review") != 0 || autoCallCount(calls, "comment") != 0 || autoCallCount(calls, "merge") != 0 {
		t.Fatal("red-across-grace must short-circuit before fixer/review/merge")
	}
}

func TestAutoMergeLetsRedWithinGraceReachFixer(t *testing.T) {
	// lets a red-within-grace PR reach the CI-Fixer path (unchanged behavior)
	freshRed := redViewUpdated(-1 * time.Hour)
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: freshRed}, "diff": {}, "merge": {}})
	fixCalls := 0
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{
			GraceHours: strPtrF(24),
			Fixer:      func(CiFixRequest) CiFixResult { fixCalls++; return CiFixResult{OK: false, Note: "pool busy"} },
		},
	}, run)
	if fixCalls != 1 { // the CI-Fixer still gets its shot
		t.Fatalf("expected 1 fixer call, got %d", fixCalls)
	}
	if o.Action != ActionCiFixFailed {
		t.Fatalf("expected ci-fix-failed, got %+v", o)
	}
	if autoCallCount(calls, "merge") != 0 {
		t.Fatal("must not merge")
	}
}

func TestAutoMergeSkipsBaseSuperseded(t *testing.T) {
	// skips a PR whose head base branch was merged or deleted (base-superseded)
	deadBase := strings.Replace(basePrView, `"baseRefName":"main"`, `"baseRefName":"devagent/TASK-mtioq4ik-T0-a0"`, 1)
	run, calls := autoScriptedGh(map[string]ghResp{
		"view": {out: deadBase}, "diff": {}, "review": {}, "merge": {},
		"api": {err: fmt.Errorf("gh: Not Found (404)")},
	})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{}, run)
	if o.Action != ActionSkipped {
		t.Fatalf("expected skipped, got %+v", o)
	}
	if !strings.Contains(o.Detail, "base-superseded") || !strings.Contains(o.Detail, "merged or deleted") {
		t.Fatalf("detail must carry the gate reason: %q", o.Detail)
	}
	if autoCallCount(calls, "review") != 0 || autoCallCount(calls, "comment") != 0 || autoCallCount(calls, "merge") != 0 {
		t.Fatal("dead base must short-circuit")
	}
}

func TestAutoMergeSkipsSupersededSibling(t *testing.T) {
	// skips a non-candidate head superseded by a same-base candidate
	conflicting := strings.Replace(basePrView, `"number":9`, `"number":7`, 1)
	conflicting = strings.Replace(conflicting, `"mergeable":"MERGEABLE"`, `"mergeable":"CONFLICTING"`, 1)
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: conflicting}, "diff": {}, "review": {}, "merge": {}, "comment": {}, "api": {}})
	o := AutoReviewAndMergeOne("/repo", 7, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{SupersedingCandidate: intPtr(9)},
	}, run)
	if o.Action != ActionSkipped {
		t.Fatalf("expected skipped, got %+v", o)
	}
	if !strings.Contains(o.Detail, "superseded") || !strings.Contains(o.Detail, "#9") {
		t.Fatalf("detail must name the candidate: %q", o.Detail)
	}
	if autoCallCount(calls, "review") != 0 || autoCallCount(calls, "comment") != 0 || autoCallCount(calls, "merge") != 0 {
		t.Fatal("superseded head must short-circuit")
	}
}

func TestAutoMergeNeverSkipsGreenCandidateWithSupersedingNumber(t *testing.T) {
	// never skips a green candidate even with a superseding number passed
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: basePrView}, "diff": {}, "review": {}, "merge": {}, "api": {}})
	o := AutoReviewAndMergeOne("/repo", 9, AutoReviewAndMergeOneOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{SupersedingCandidate: intPtr(9)},
	}, run)
	if o.Action != ActionMerged {
		t.Fatalf("expected merged, got %+v", o)
	}
	if autoCallCount(calls, "merge") != 1 {
		t.Fatal("green candidate must merge")
	}
}

func redViewUpdated(age time.Duration) string {
	v := map[string]any{}
	_ = json.Unmarshal([]byte(basePrView), &v)
	v["statusCheckRollup"] = []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "FAILURE"}}
	v["updatedAt"] = time.Now().Add(age).UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(v)
	return string(b)
}

// --- sweepStalePrs (zombie-PR sweep) ---

func sweepPrJSON(number int, title, headRefName, mergeable, headRefOid, updatedAt string, rollup []map[string]any) map[string]any {
	return map[string]any{
		"number": float64(number), "title": title, "headRefName": headRefName,
		"baseRefName": "main", "state": "OPEN", "mergeable": mergeable,
		"reviewDecision": "", "headRefOid": headRefOid, "updatedAt": updatedAt,
		"author": map[string]any{"login": "devagent[bot]"}, "statusCheckRollup": rollup,
	}
}

var greenRollup = []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "SUCCESS"}}
var redRollup = []map[string]any{{"name": "test", "status": "COMPLETED", "conclusion": "FAILURE"}}

func sweepListJSON(prs []map[string]any) string {
	b, _ := json.Marshal(prs)
	return string(b)
}

func TestSweepCommentsSupersededLeavesCandidate(t *testing.T) {
	// comments the superseded PR and leaves the green candidate untouched
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(7, "old", "devagent/old", "CONFLICTING", "old111", time.Now().UTC().Format(time.RFC3339Nano), greenRollup),
		sweepPrJSON(9, "new", "devagent/new", "MERGEABLE", "new222", time.Now().UTC().Format(time.RFC3339Nano), greenRollup),
	})
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}, "comment": {}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{DryRun: autoBoolPtr(false), GraceDays: strPtrF(7)}, run)
	if len(outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(outcomes))
	}
	o7 := outcomes[0]
	if o7.Action != ZombieActionSuperseded {
		t.Fatalf("#7 must be superseded, got %+v", o7)
	}
	if !strings.Contains(o7.Detail, "#9") || !strings.Contains(o7.Detail, "old111") {
		t.Fatalf("detail must name candidate and head sha: %q", o7.Detail)
	}
	if autoCallCount(calls, "comment") != 1 || autoCallCount(calls, "close") != 0 {
		t.Fatalf("exactly one comment, no closes: %v", *calls)
	}
}

func TestSweepAutoClosesRedAcrossGrace(t *testing.T) {
	// auto-closes a PR red across the grace window
	stale := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(5, "stale red", "devagent/stale", "MERGEABLE", "f00d", stale, redRollup),
	})
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}, "comment": {}, "close": {}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{DryRun: autoBoolPtr(false), GraceDays: strPtrF(7)}, run)
	if outcomes[0].Action != ZombieActionClosed || !strings.Contains(outcomes[0].Detail, "grace") {
		t.Fatalf("expected closed with grace detail: %+v", outcomes[0])
	}
	if autoCallCount(calls, "close") != 1 {
		t.Fatal("expected exactly one close call")
	}
}

func TestSweepAutoClosesUnparseableUpdatedAt(t *testing.T) {
	// auto-closes a red PR whose updatedAt is unparseable (unknown age counts as stale)
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(6, "red no timestamp", "devagent/nots", "MERGEABLE", "bad1", "not-a-date", redRollup),
	})
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{DryRun: autoBoolPtr(false), GraceDays: strPtrF(7)}, run)
	if outcomes[0].Action != ZombieActionClosed {
		t.Fatalf("unknown age must count as stale: %+v", outcomes[0])
	}
	if autoCallCount(calls, "close") != 1 {
		t.Fatal("expected close call")
	}
}

func TestSweepLeavesRedWithinGraceUntouched(t *testing.T) {
	// leaves a PR red within the grace window untouched
	fresh := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(5, "fresh red", "devagent/fresh", "MERGEABLE", "beef", fresh, redRollup),
	})
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{DryRun: autoBoolPtr(false), GraceDays: strPtrF(7)}, run)
	if outcomes[0].Action != ZombieActionUntouched || !strings.Contains(outcomes[0].Detail, "within grace") {
		t.Fatalf("red within grace must be untouched: %+v", outcomes[0])
	}
	if autoCallCount(calls, "close") != 0 {
		t.Fatal("no close within grace")
	}
}

func TestSweepDryRunReportsWithoutSideEffects(t *testing.T) {
	// dry-run reports verdicts without commenting or closing anything
	stale := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(5, "stale red", "devagent/stale", "MERGEABLE", "f00d", stale, redRollup),
		sweepPrJSON(7, "superseded", "devagent/sup", "CONFLICTING", "dead", now, greenRollup),
		sweepPrJSON(9, "candidate", "devagent/new", "MERGEABLE", "new222", now, greenRollup),
	})
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{}, run)
	want := []ZombiePrAction{ZombieActionClosed, ZombieActionSuperseded, ZombieActionUntouched}
	for i, w := range want {
		if outcomes[i].Action != w {
			t.Fatalf("outcome %d: want %s got %s", i, w, outcomes[i].Action)
		}
	}
	if !strings.Contains(outcomes[0].Detail, "[dry-run]") || !strings.Contains(outcomes[1].Detail, "[dry-run]") {
		t.Fatal("dry-run details must be marked")
	}
	if autoCallCount(calls, "close") != 0 || autoCallCount(calls, "comment") != 0 {
		t.Fatal("dry-run must not comment or close")
	}
}

func TestSweepLeavesPendingOrNoChecksUntouched(t *testing.T) {
	// leaves PRs with pending or no checks untouched (no evidence either way)
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(3, "pending", "devagent/p", "MERGEABLE", "p3", old, []map[string]any{{"name": "test", "status": "IN_PROGRESS"}}),
		sweepPrJSON(4, "no checks", "devagent/n", "MERGEABLE", "n4", old, []map[string]any{}),
	})
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{DryRun: autoBoolPtr(false), GraceDays: strPtrF(0)}, run)
	if outcomes[0].Action != ZombieActionUntouched || outcomes[1].Action != ZombieActionUntouched {
		t.Fatalf("no-evidence PRs must be untouched: %+v", outcomes)
	}
	if autoCallCount(calls, "close") != 0 || autoCallCount(calls, "comment") != 0 {
		t.Fatal("no side effects without evidence")
	}
}

func TestSweepSkipsNonOpen(t *testing.T) {
	// skips PRs that are not open (state snapshot raced a close)
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	list := sweepListJSON([]map[string]any{
		sweepPrJSON(2, "closed mid-sweep", "devagent/x", "MERGEABLE", "c2", old, redRollup),
	})
	list = strings.Replace(list, `"state":"OPEN"`, `"state":"CLOSED"`, 1)
	run, calls := autoScriptedGh(map[string]ghResp{"list": {out: list}})
	outcomes := SweepStalePrs("/repo", ZombiePrOptions{DryRun: autoBoolPtr(false), GraceDays: strPtrF(0)}, run)
	if outcomes[0].Action != ZombieActionSkipped || !strings.Contains(outcomes[0].Detail, "CLOSED") {
		t.Fatalf("non-open must be skipped: %+v", outcomes[0])
	}
	if autoCallCount(calls, "close") != 0 || autoCallCount(calls, "comment") != 0 {
		t.Fatal("no side effects for non-open")
	}
}

func autoBoolPtr(b bool) *bool { return &b }

// --- autoReviewAndMerge batch (supersession view) ---

func TestBatchSkipsSupersededAndRedAcrossGraceMergesCandidate(t *testing.T) {
	// skips superseded siblings and red-across-grace PRs in one batch, merges the candidate
	now := time.Now().UTC().Format(time.RFC3339Nano)
	old := time.Now().Add(-30 * time.Hour).UTC().Format(time.RFC3339Nano)
	views := map[string]string{
		"7": autoMustJSON(sweepPrJSON(7, "conflicting", "devagent/old", "CONFLICTING", "old111", now, greenRollup)),
		"4": autoMustJSON(sweepPrJSON(4, "red across grace", "devagent/red", "MERGEABLE", "red444", old, redRollup)),
		"9": autoMustJSON(sweepPrJSON(9, "candidate", "devagent/new", "MERGEABLE", "new222", now, greenRollup)),
	}
	var calls [][]string
	run := func(args []string, _ string) (*GhResult, error) {
		calls = append(calls, args)
		key := args[0]
		if args[0] == "pr" {
			key = args[1]
		}
		if key == "list" {
			return &GhResult{Stdout: sweepListJSON([]map[string]any{
				autoMustJSONMap(views["7"]), autoMustJSONMap(views["4"]), autoMustJSONMap(views["9"]),
			})}, nil
		}
		if key == "view" && len(args) >= 3 {
			return &GhResult{Stdout: views[args[2]]}, nil
		}
		return &GhResult{}, nil
	}
	outcomes := AutoReviewAndMerge("/repo", AutoReviewAndMergeBatchOpts{
		AutoReviewAndMergeOptions: AutoReviewAndMergeOptions{GraceHours: strPtrF(24)},
	}, run)
	want := []struct {
		pr     int
		action string
	}{{4, ActionSkipped}, {7, ActionSkipped}, {9, ActionMerged}}
	for i, w := range want {
		if outcomes[i].PR != w.pr || outcomes[i].Action != w.action {
			t.Fatalf("outcome %d: want #%d %s, got #%d %s", i, w.pr, w.action, outcomes[i].PR, outcomes[i].Action)
		}
	}
	if !strings.Contains(outcomes[0].Detail, "red-across-grace") || !strings.Contains(outcomes[1].Detail, "superseded") || !strings.Contains(outcomes[1].Detail, "#9") {
		t.Fatalf("details must carry gate reasons: %+v", outcomes)
	}
	// only the candidate was reviewed and merged
	if autoCallCount(&calls, "review") != 1 || autoCallCount(&calls, "merge") != 1 {
		t.Fatalf("expected 1 review + 1 merge, got %v", calls)
	}
}

func TestBatchExplicitPrNumbersBypassSupersessionView(t *testing.T) {
	// an explicit --pr set bypasses the supersession view (hand-picked batches override)
	conflicting := autoMustJSON(map[string]any{
		"number": float64(7), "title": "conflicting", "headRefName": "devagent/old",
		"baseRefName": "main", "state": "OPEN", "mergeable": "CONFLICTING",
		"reviewDecision": "", "headRefOid": "old111",
		"updatedAt":         time.Now().UTC().Format(time.RFC3339Nano),
		"author":            map[string]any{"login": "devagent[bot]"},
		"statusCheckRollup": greenRollup,
	})
	run, calls := autoScriptedGh(map[string]ghResp{"view": {out: conflicting}, "diff": {}, "review": {}, "merge": {}, "api": {}})
	outcomes := AutoReviewAndMerge("/repo", AutoReviewAndMergeBatchOpts{PrNumbers: []int{7}, Log: func(string) {}}, run)
	if len(outcomes) != 1 {
		t.Fatalf("expected 1 outcome, got %d", len(outcomes))
	}
	if strings.Contains(outcomes[0].Detail, "superseded") {
		t.Fatalf("hand-picked batches override the supersession view: %q", outcomes[0].Detail)
	}
	if autoCallCount(calls, "list") != 0 {
		t.Fatal("explicit --pr set must not list open PRs")
	}
}

func autoMustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func autoMustJSONMap(raw string) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &m)
	return m
}

func autoInt64P2(v int64) *int64 { return &v }

// strPtrF is the float-pointer helper for gate options.
func strPtrF(f float64) *float64 { return &f }
