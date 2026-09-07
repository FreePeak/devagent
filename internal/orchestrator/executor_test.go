// Package file mirrors test/executor.test.ts (FR-GO-07, issue #194).
//
// Executor failure surface (PRD:775): after the task exhausts attempts with
// N+ identical trailing trail.jsonl failure signatures, the executor marks
// taskInterrupt and aborts the worker instead of burning another attempt on
// the same wall. These cases exercise the trail-signature machinery directly
// (no worker subprocess) — the same helpers ExecuteTask calls on every
// failed attempt — plus the two dispatch-preflight refusals (model id, Q18
// prompt-size guard) that must fire before any worktree or worker spend.
package orchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/ledger"
)

func tsRow(ts string, attempt int, signature, failureClass, excerpt string) TrailSignature {
	return TrailSignature{TS: ts, Attempt: attempt, Signature: signature, FailureClass: failureClass, Excerpt: excerpt}
}

// --- failureSignature (trail.jsonl failure identity) ---

func TestFailureSignatureCollidesForIdenticalExcerpts(t *testing.T) {
	// collides for identical excerpts (same failure = same signature)
	a := FailureSignature("npm test: 3 failed\n  suite A: broken")
	b := FailureSignature("npm test: 3 failed\n  suite A: broken")
	if a != b {
		t.Fatalf("identical excerpts must collide: %q != %q", a, b)
	}
}

func TestFailureSignatureInsensitiveToWhitespaceCaseAndTimestamps(t *testing.T) {
	// is insensitive to whitespace/case churn and embedded ISO timestamps
	// Same failure, different minute / quoting / line wrapping: still one signature.
	a := FailureSignature("2026-09-01T08:00:12Z  \"TEST FAILED: 3 failed\"")
	b := FailureSignature("2026-09-01T08:00:13Z \"test   failed: 3  failed\"")
	if a != b {
		t.Fatalf("normalized excerpts must collide: %q != %q", a, b)
	}
}

func TestFailureSignatureSeparatesGenuinelyDifferentFailures(t *testing.T) {
	// separates genuinely different failures
	if FailureSignature("npm test: 3 failed") == FailureSignature("npm test: 0 failed") {
		t.Fatal("different failures must not collide")
	}
	if FailureSignature("worker exited 1") == FailureSignature("worker exited 2") {
		t.Fatal("different exit codes must not collide")
	}
}

// --- duplicateTrailingSignatures (N+ identical trailing signatures) ---

func TestDuplicateTrailingSignaturesReturnsTrailingSlice(t *testing.T) {
	// returns the trailing slice when the last N signatures are identical
	trail := []TrailSignature{
		tsRow("t1", 1, "a", "test-gate", "x"),
		tsRow("t2", 2, "b", "test-gate", "y"),
		tsRow("t3", 3, "b", "test-gate", "y"),
		tsRow("t4", 4, "b", "test-gate", "y"),
	}
	hit := DuplicateTrailingSignatures(trail, 3)
	if hit == nil {
		t.Fatal("expected a hit")
	}
	if len(hit) != 3 || hit[0].Signature != "b" || hit[1].Signature != "b" || hit[2].Signature != "b" {
		t.Fatalf("expected ['b','b','b'], got %+v", hit)
	}
	if hit[0].FailureClass != "test-gate" {
		t.Fatalf("expected test-gate class, got %q", hit[0].FailureClass)
	}
}

func TestDuplicateTrailingSignaturesNullWhenFewerThanN(t *testing.T) {
	// returns null when fewer than N trailing signatures exist
	trail := []TrailSignature{
		tsRow("t1", 1, "a", "worker-error", "x"),
		tsRow("t2", 2, "a", "worker-error", "x"),
	}
	if DuplicateTrailingSignatures(trail, 3) != nil {
		t.Fatal("expected nil below threshold")
	}
}

func TestDuplicateTrailingSignaturesNullWhenNotAllIdentical(t *testing.T) {
	// returns null when the trailing signatures are not all identical
	trail := []TrailSignature{
		tsRow("t1", 1, "a", "test-gate", "x"),
		tsRow("t2", 2, "b", "test-gate", "y"),
		tsRow("t3", 3, "c", "test-gate", "z"),
	}
	if DuplicateTrailingSignatures(trail, 3) != nil {
		t.Fatal("expected nil for non-identical tail")
	}
}

func TestDuplicateTrailingSignaturesHonorsCustomThreshold(t *testing.T) {
	// honors a custom threshold
	trail := []TrailSignature{
		tsRow("t1", 1, "a", "test-gate", "x"),
		tsRow("t2", 2, "a", "test-gate", "x"),
	}
	if DuplicateTrailingSignatures(trail, 2) == nil {
		t.Fatal("expected hit at threshold 2")
	}
	if DuplicateTrailingSignatures(trail, 3) != nil {
		t.Fatal("expected nil at threshold 3")
	}
}

// --- evaluateTrailInterrupt (taskInterrupt decision) ---

func TestEvaluateTrailInterruptNullUntilAccumulated(t *testing.T) {
	// returns null until N+ identical trailing signatures accumulate
	repo := t.TempDir()
	// First failure: trail has 1 signature -> keep retrying.
	if d := EvaluateTrailInterrupt(repo, "T1", 1, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold); d != nil {
		t.Fatalf("first failure must not interrupt: %+v", d)
	}
	// Second identical failure: trail has 2 -> still retrying (threshold 3).
	if d := EvaluateTrailInterrupt(repo, "T1", 2, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold); d != nil {
		t.Fatalf("second identical failure must not interrupt: %+v", d)
	}
	trail := ReadTrailSignatures(repo, "T1")
	if len(trail) != 2 {
		t.Fatalf("expected 2 trail rows, got %d", len(trail))
	}
	if trail[0].Signature != trail[1].Signature {
		t.Fatal("identical failures must produce identical signatures")
	}
}

func TestEvaluateTrailInterruptMarksOnThird(t *testing.T) {
	// marks taskInterrupt on the 3rd identical trailing signature and aborts
	repo := t.TempDir()
	EvaluateTrailInterrupt(repo, "T1", 1, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold)
	EvaluateTrailInterrupt(repo, "T1", 2, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold)
	decision := EvaluateTrailInterrupt(repo, "T1", 3, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold)
	if decision == nil {
		t.Fatal("expected a taskInterrupt decision")
	}
	if !decision.Interrupted {
		t.Fatal("decision.interrupted must be true")
	}
	if decision.FailureClass != "test-gate" {
		t.Fatalf("expected test-gate class, got %q", decision.FailureClass)
	}
	if decision.Attempts != 3 {
		t.Fatalf("expected attempts 3, got %d", decision.Attempts)
	}
	// Last gate excerpt carried through for the ledger post-mortem
	if !strings.Contains(decision.LastGateExcerpt, "suite A broken") {
		t.Fatalf("excerpt must carry through: %q", decision.LastGateExcerpt)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(decision.TrailHash) {
		t.Fatalf("trailHash must be 16 hex chars: %q", decision.TrailHash)
	}
	if !strings.Contains(decision.Detail, "identical") {
		t.Fatalf("detail must mention identical count: %q", decision.Detail)
	}
	// Full trail was persisted (survives worktree cleanup for the post-mortem)
	if trail := ReadTrailSignatures(repo, "T1"); len(trail) != 3 {
		t.Fatalf("expected 3 persisted rows, got %d", len(trail))
	}
}

func TestEvaluateTrailInterruptResetsStreakOnDifferentFailure(t *testing.T) {
	// resets the streak when a different failure interrupts the identical run
	repo := t.TempDir()
	EvaluateTrailInterrupt(repo, "T1", 1, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold)
	EvaluateTrailInterrupt(repo, "T1", 2, "test gate failed: suite A broken", "test-gate", DefaultInterruptThreshold)
	// A NEW failure (different signature) breaks the run: not terminal yet.
	if d := EvaluateTrailInterrupt(repo, "T1", 3, "worker crashed: out of memory", "worker-error", DefaultInterruptThreshold); d != nil {
		t.Fatalf("streak reset must clear the run: %+v", d)
	}
	// Two more of the NEW signature -> interrupt on the worker-error class.
	if d := EvaluateTrailInterrupt(repo, "T1", 4, "worker crashed: out of memory", "worker-error", DefaultInterruptThreshold); d != nil {
		t.Fatalf("two of the new signature must stay under threshold: %+v", d)
	}
	d2 := EvaluateTrailInterrupt(repo, "T1", 5, "worker crashed: out of memory", "worker-error", DefaultInterruptThreshold)
	if d2 == nil {
		t.Fatal("expected interrupt on the third identical worker-error")
	}
	if d2.FailureClass != "worker-error" {
		t.Fatalf("expected worker-error class, got %q", d2.FailureClass)
	}
}

func TestEvaluateTrailInterruptPersistsTrailJSONL(t *testing.T) {
	// persists the trail.jsonl under the repo and is readable back
	repo := t.TempDir()
	AppendTrailSignature(repo, "T9", TrailSignatureInput{
		Attempt:      1,
		Signature:    FailureSignature("boom"),
		FailureClass: "test-gate",
		Excerpt:      "boom",
	})
	file := TaskTrailPath(repo, "T9")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("trail file must exist: %v", err)
	}
	raw := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(raw) != 1 {
		t.Fatalf("expected 1 raw row, got %d", len(raw))
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(raw[0]), &row); err != nil {
		t.Fatalf("row must be JSON: %v", err)
	}
	if row["attempt"] != float64(1) || row["signature"] != FailureSignature("boom") || row["failureClass"] != "test-gate" {
		t.Fatalf("row mismatch: %+v", row)
	}
	if _, ok := row["ts"]; !ok {
		t.Fatal("row must carry ts")
	}
	// Key order mirrors the TS writer: attempt, signature, failureClass, excerpt, ts.
	if idx := strings.Index(raw[0], `"attempt"`); idx != 1 { // index 1: after the leading {
		t.Fatalf("attempt must be the first key, got %q", raw[0])
	}
	if !strings.HasSuffix(raw[0], `"ts":"`) && !strings.Contains(raw[0], `,"ts":"`) {
		t.Fatalf("ts must be the last key: %q", raw[0])
	}
	if got := ReadTrailSignatures(repo, "T9"); len(got) != 1 {
		t.Fatalf("expected 1 readable row, got %d", len(got))
	}
	// Missing task -> empty trail
	if got := ReadTrailSignatures(repo, "TX"); len(got) != 0 {
		t.Fatalf("expected empty trail for missing task, got %d", len(got))
	}
}

// --- instructionPayloadBytes + preflight guards (Q18 / Q32) ---

func TestInstructionPayloadBytesCountsOwnPayloadOnly(t *testing.T) {
	task := OrchestratorTask{
		ID:                  "T1",
		Prompt:              "abc",
		BoundaryConstraints: []string{"de", "f"},
		EvidenceGaps:        []string{"ghi"},
	}
	// "abc\nde\nf\nghi" = 12 bytes; acceptance criteria and expectedOutput excluded.
	if got := InstructionPayloadBytes(task); got != 12 {
		t.Fatalf("expected 12 payload bytes, got %d", got)
	}
}

func TestExecuteTaskRefusesOversizedPromptBeforeDispatch(t *testing.T) {
	repo := t.TempDir()
	dispatched := false
	deps := ExecutorDeps{Dispatcher: fakeDispatcherFunc(func(WorkerDispatchRequest) WorkerDispatchResult {
		dispatched = true
		return WorkerDispatchResult{ExitCode: 0}
	})}
	task := OrchestratorTask{ID: "T-big", Title: "big", Prompt: strings.Repeat("x", 5000)}
	res, err := ExecuteTask(ExecuteTaskArgs{
		Task: &task, Board: &ProjectBoard{}, RepoPath: repo, TimeoutMs: 1000,
		Executor: "opencode", Log: NoopRunLog{}, Deps: deps,
	})
	if err != nil {
		t.Fatalf("preflight refusal must not throw: %v", err)
	}
	if res.OK {
		t.Fatal("expected refusal")
	}
	if res.FailureClass != "prompt-oversized" {
		t.Fatalf("expected prompt-oversized, got %q", res.FailureClass)
	}
	if !strings.Contains(res.Detail, "plan-split") {
		t.Fatalf("detail must carry the plan-split guidance: %q", res.Detail)
	}
	if dispatched {
		t.Fatal("no worker dispatch may happen past the size guard")
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".devagent-worktrees")); !os.IsNotExist(statErr) {
		t.Fatal("no worktree may be created past the size guard")
	}
}

func TestExecuteTaskReportsWorktreeFailure(t *testing.T) {
	// Not a git repo: CreateWorktree fails -> structured worktree-class
	// failure, no dispatcher call.
	repo := t.TempDir()
	deps := ExecutorDeps{Dispatcher: fakeDispatcherFunc(func(WorkerDispatchRequest) WorkerDispatchResult {
		return WorkerDispatchResult{ExitCode: 0}
	})}
	task := OrchestratorTask{ID: "T1", Title: "T1", Prompt: "p"}
	res, err := ExecuteTask(ExecuteTaskArgs{
		Task: &task, Board: &ProjectBoard{}, RepoPath: repo, TimeoutMs: 1000,
		Executor: "opencode", Log: NoopRunLog{}, Deps: deps,
	})
	if err != nil {
		t.Fatalf("worktree failure is a structured result, not a throw: %v", err)
	}
	if res.OK || res.FailureClass != "worktree" {
		t.Fatalf("expected worktree-class failure, got %+v", res)
	}
	if !strings.Contains(res.Detail, "worktree creation failed") {
		t.Fatalf("detail must name the cause: %q", res.Detail)
	}
}

// fakeDispatcherFunc adapts a func into the WorkerDispatcher seam.
type fakeDispatcherFunc func(WorkerDispatchRequest) WorkerDispatchResult

func (f fakeDispatcherFunc) Dispatch(req WorkerDispatchRequest) WorkerDispatchResult {
	return f(req)
}

// Compile-time: the ledger import stays for the shared TrailRoot path shape
// assertion below.
var _ = ledger.LedgerDir

func TestTaskTrailPathSharesLedgerRoot(t *testing.T) {
	// Trail root equals the ledger dir so trails survive resets alongside
	// the ledger (TS TRAIL_ROOT === LEDGER_DIR).
	if TrailRoot != ledger.LedgerDir {
		t.Fatalf("TrailRoot %q must equal LedgerDir %q", TrailRoot, ledger.LedgerDir)
	}
	got := TaskTrailPath("/repo", "T1")
	if want := filepath.Join("/repo", TrailRoot, "trail-T1.jsonl"); got != want {
		t.Fatalf("TaskTrailPath = %q, want %q", got, want)
	}
}
