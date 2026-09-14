package scout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/queue"
)

// Ported behavior tests for the live cycle (run.go, FR-SCOUT-01 revival).
// Every test injects Dispatch — the real worker CLI is NEVER spawned here
// (the repo's hermetic rule); defaultDispatch's contract is checked at its
// seam, not against a live omp.

var scoutTestNow = time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC)

// cannedScoutPayload is one valid ---TASK---/---PRD--- block in the exact
// shape ParseScoutOutput demands (goal with the "Goal:" prefix, id/title
// present).
const cannedScoutPayload = `---TASK---
id: SCOUT-TEST-IDEA
title: Add scout dry-run summary line
goal: Goal: Print a one-line summary of what a dry-run cycle would enqueue.
criteria: summary printed to stdout; no files written
---PRD---
# Add scout dry-run summary line
## Goal
One cycle, one line.
## Acceptance criteria
- summary printed to stdout
`

func fixedDispatch(raw string) DispatchFn {
	return func(worker, model, prompt string, timeout time.Duration) (string, error) {
		return raw, nil
	}
}

func testOpts(repo string, dispatch DispatchFn) RunOptions {
	return RunOptions{
		RepoPath: repo,
		Worker:   "opencode",
		Dispatch: dispatch,
		Now:      func() time.Time { return scoutTestNow },
	}
}

func mustRun(t *testing.T, opts RunOptions) RunResult {
	t.Helper()
	res, err := RunOnce(opts)
	if err != nil {
		t.Fatalf("RunOnce: %v (res=%+v)", err, res)
	}
	return res
}

func countQueue(t *testing.T, repo string) int {
	t.Helper()
	return len(queue.ListTasks(repo, ""))
}

// devagentFiles counts everything under .devagent: the "nothing written"
// assertions go on real files, not on our own struct echo.
func devagentFiles(t *testing.T, repo string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(repo, ".devagent")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRunOnceEnqueuesOnceWithSidecarAndHeartbeat(t *testing.T) {
	repo := tmpRepo(t)
	var gotWorker, gotModel string
	var gotPrompt string
	var gotTimeout time.Duration
	opts := testOpts(repo, func(worker, model, prompt string, timeout time.Duration) (string, error) {
		gotWorker, gotModel, gotPrompt, gotTimeout = worker, model, prompt, timeout
		return cannedScoutPayload, nil
	})
	res := mustRun(t, opts)

	if !res.OK || !res.Queued || res.Status != StatusQueued {
		t.Fatalf("res = %+v", res)
	}
	if res.TaskID != "SCOUT-TEST-IDEA" || res.Title != "Add scout dry-run summary line" {
		t.Errorf("task = %q / %q", res.TaskID, res.Title)
	}
	tasks := queue.ListTasks(repo, queue.StatusPending)
	if len(tasks) != 1 {
		t.Fatalf("pending tasks = %d, want 1", len(tasks))
	}
	task := tasks[0]
	if task.Source == nil || *task.Source != "scout" {
		t.Errorf("source = %v, want scout", task.Source)
	}
	if len(task.AcceptanceCriteria) != 2 {
		t.Errorf("criteria = %v", task.AcceptanceCriteria)
	}
	if task.PrdPath == nil {
		t.Fatal("PRD sidecar path missing on the queue row")
	}
	sidecar := filepath.Join(repo, ".devagent", "prds", "SCOUT-TEST-IDEA.md")
	if raw, err := os.ReadFile(sidecar); err != nil || !strings.Contains(string(raw), "## Goal") {
		t.Fatalf("sidecar %s: %v", sidecar, err)
	}
	// The seam received the resolved worker + the built prompt + the
	// default 30-minute budget (config timeoutMinutes).
	if gotWorker != "opencode" || gotModel != "" || !strings.Contains(gotPrompt, "You are the DevAgent SCOUT") {
		t.Errorf("dispatch args: worker=%q model=%q prompt has scout header=%v", gotWorker, gotModel, strings.Contains(gotPrompt, "SCOUT"))
	}
	if gotTimeout != 30*time.Minute {
		t.Errorf("timeout = %v, want 30m", gotTimeout)
	}
	hb := ReadHeartbeat(repo)
	if hb == nil || hb.LastStatus == nil || *hb.LastStatus != StatusQueued ||
		hb.LastTaskID == nil || *hb.LastTaskID != "SCOUT-TEST-IDEA" ||
		hb.Worker == nil || *hb.Worker != "opencode" {
		t.Fatalf("heartbeat = %+v", hb)
	}
	// Cycle 6 of the contract: the lock is released for the next cycle.
	if _, err := os.Stat(scoutLockPath(repo)); !os.IsNotExist(err) {
		t.Errorf("scout lock still present after cycle: %v", err)
	}
}

func TestRunOnceSecondCycleIsDedupedNotFailed(t *testing.T) {
	repo := tmpRepo(t)
	// Same payload twice: the second enqueue must surface ErrAlreadyQueued
	// as a DEDUP HIT (OK, Skipped) — the deterministic per-day fallback id
	// depends on this being a non-failure (2026-09-01 incident).
	first := mustRun(t, testOpts(repo, fixedDispatch(cannedScoutPayload)))
	if !first.Queued {
		t.Fatalf("first cycle = %+v", first)
	}
	second := mustRun(t, testOpts(repo, fixedDispatch(cannedScoutPayload)))
	if !second.OK || !second.Skipped || second.Queued || second.Status != StatusDeduped {
		t.Fatalf("second cycle = %+v", second)
	}
	if second.TaskID != "SCOUT-TEST-IDEA" {
		t.Errorf("dedup must still name the task: %+v", second)
	}
	if n := countQueue(t, repo); n != 1 {
		t.Errorf("queue rows = %d, want 1", n)
	}
}

func TestRunOnceLiveForeignLockWritesNothing(t *testing.T) {
	repo := tmpRepo(t)
	// A LIVE holder (this test pid — AcquireScoutLock checks liveness, not
	// ownership) must yield Status locked with ZERO writes: clobbering the
	// holder's heartbeat would misattribute the run in scout-status.
	lock := scoutLockPath(repo)
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(`{"pid":%d,"at":%d}`, os.Getpid(), scoutTestNow.UnixMilli())
	if err := os.WriteFile(lock, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := mustRun(t, testOpts(repo, fixedDispatch(cannedScoutPayload)))
	if !res.OK || !res.Skipped || res.Status != StatusLocked {
		t.Fatalf("res = %+v", res)
	}
	if n := countQueue(t, repo); n != 0 {
		t.Errorf("locked cycle enqueued %d tasks", n)
	}
	if hb := ReadHeartbeat(repo); hb != nil {
		t.Errorf("locked cycle wrote a heartbeat: %+v", hb)
	}
	// The cycle must not steal or delete the holder's lock either.
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("foreign lock must survive: %v", err)
	}
}

func TestRunOnceQueueFullNeverDispatches(t *testing.T) {
	repo := tmpRepo(t)
	// Dispatch is the expensive step (minutes + tokens); a queue-full
	// cycle must decide BEFORE paying for it.
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"),
		[]byte(`{"scout":{"maxQueued":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "OLD-1", Title: "old", Goal: "Goal: old"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	res := mustRun(t, testOpts(repo, func(worker, model, prompt string, timeout time.Duration) (string, error) {
		calls++
		return cannedScoutPayload, nil
	}))
	if !res.OK || !res.Skipped || res.Status != StatusQueueFull {
		t.Fatalf("res = %+v", res)
	}
	if calls != 0 {
		t.Errorf("queue-full cycle dispatched %d times", calls)
	}
	if n := countQueue(t, repo); n != 1 {
		t.Errorf("queue rows = %d, want 1", n)
	}
	// queue-full still heartbeats: scout-status's staleness gate must see
	// the alive-check, not read a skip as a death.
	hb := ReadHeartbeat(repo)
	if hb == nil || hb.LastStatus == nil || *hb.LastStatus != StatusQueueFull {
		t.Fatalf("heartbeat = %+v", hb)
	}
}

func TestRunOnceUnparseablePayloadEnqueuesDailyFallback(t *testing.T) {
	repo := tmpRepo(t)
	res := mustRun(t, testOpts(repo, fixedDispatch("I feel inspired today, no markers here.")))
	wantID := "SCOUT-20260914-fallback"
	if !res.OK || !res.Queued || res.Status != StatusQueued || res.TaskID != wantID {
		t.Fatalf("res = %+v, want queued %s", res, wantID)
	}
	if !strings.Contains(res.Detail, "unparseable") {
		t.Errorf("detail must record WHY: %q", res.Detail)
	}
	if n := countQueue(t, repo); n != 1 {
		t.Errorf("queue rows = %d, want 1", n)
	}
}

func TestRunOnceDispatchErrorFallsBackNotStarves(t *testing.T) {
	repo := tmpRepo(t)
	// docs/SCOUT.md failure policy: a dead worker must not starve the
	// queue; the heartbeat detail keeps the cause diagnosable.
	res := mustRun(t, testOpts(repo, func(worker, model, prompt string, timeout time.Duration) (string, error) {
		return "", errors.New("Unknown worker: nope")
	}))
	if !res.OK || !res.Queued || res.TaskID != "SCOUT-20260914-fallback" {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Detail, "dispatch failed") {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestRunOnceDryRunWritesNothing(t *testing.T) {
	repo := tmpRepo(t)
	opts := testOpts(repo, fixedDispatch(cannedScoutPayload))
	opts.DryRun = true
	res := mustRun(t, opts)
	if !res.OK || res.Queued || res.Status != StatusDryRun {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Detail, "prompt ") || !strings.Contains(res.Detail, "SCOUT-TEST-IDEA") {
		t.Errorf("dry-run must preview prompt + would-enqueue: %q", res.Detail)
	}
	if files := devagentFiles(t, repo); len(files) != 0 {
		t.Errorf("dry-run left files: %v", files)
	}
}

func TestRunOnceDryRunWithoutSeamSkipsDispatch(t *testing.T) {
	repo := tmpRepo(t)
	// `scout --once --dry-run` is documented (docs/SCOUT.md) as the
	// deterministic-fallback preview with NO AI call: with no injected
	// seam RunOnce must not resolve the real defaultDispatch either.
	opts := RunOptions{RepoPath: repo, Worker: "opencode", DryRun: true,
		Now: func() time.Time { return scoutTestNow }}
	res := mustRun(t, opts)
	if res.Status != StatusDryRun || !strings.Contains(res.Detail, "no dispatch") {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(res.Detail, "SCOUT-20260914-fallback") {
		t.Errorf("no-dispatch dry-run previews the fallback: %q", res.Detail)
	}
	if files := devagentFiles(t, repo); len(files) != 0 {
		t.Errorf("dry-run left files: %v", files)
	}
}

func TestRunLoopTicksUntilStop(t *testing.T) {
	repo := tmpRepo(t)
	// RunLoop is synchronous over RunOnce, so the counter needs no lock:
	// dispatch #2 closes stop and the next select returns. Same payload
	// twice also proves the loop keeps ticking past a deduped cycle.
	stop := make(chan struct{})
	n := 0
	dispatch := func(worker, model, prompt string, timeout time.Duration) (string, error) {
		n++
		if n == 2 {
			close(stop)
		}
		return cannedScoutPayload, nil
	}
	RunLoop(repo, 5*time.Millisecond, stop, testOpts("", dispatch))
	if n != 2 {
		t.Errorf("cycles = %d, want 2", n)
	}
	if count := len(queue.ListTasks(repo, queue.StatusPending)); count != 1 {
		t.Errorf("queue rows = %d, want 1", count)
	}
}
