// chaos_test.go — FR-VAL-04 (issue #292), task-level scenario: SIGKILL the
// worker CLI mid-run → the FR-IMPL retry loop must move to attempt 2/3 with
// a repair prompt, and the first attempt must be classified on the ledger
// (exitCode -1, timedOut false, its watchdog-health row present) — not
// silently lost. See internal/loopdriver/chaos_test.go for the scenario →
// package split.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/workers"
)

// Scenario 2 (issue #292): the fake omp streams nothing, records its pid,
// and hangs until the test SIGKILLs it mid-run; attempt 2 recovers with a
// healthy result envelope. Mutation check: dropping the worker-error retry
// (`logicAttempts++; continue`) ends the loop after one attempt and fails
// the Attempts==2 assertion.
func TestChaosWorkerKillMidRunRetriesAttemptTwoOfThree(t *testing.T) {
	repo := t.TempDir()
	cwd := t.TempDir()
	binDir := t.TempDir()
	marker := filepath.Join(cwd, "chaos-worker-marker")
	pidFile := filepath.Join(cwd, "chaos-worker-pid")
	invocations := filepath.Join(binDir, "chaos-omp-invocations.log")
	script := fmt.Sprintf(`#!/bin/sh
c=$(cat %q 2>/dev/null || echo 0)
c=$((c + 1))
echo "$c" > %q
if [ "$c" = "1" ]; then
  echo $$ > %q
  : > %q
  exec sleep 300
fi
echo '{"type":"result","result":"recovered-on-retry","session_id":"s-2","is_error":false}'
exit 0
`, invocations, invocations, pidFile, marker)
	if err := os.WriteFile(filepath.Join(binDir, "omp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Killer: once the worker is provably mid-run (marker + pid on disk),
	// SIGKILL it — the chaos injection itself.
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				if data, err := os.ReadFile(pidFile); err == nil {
					if pid, cerr := strconv.Atoi(strings.TrimSpace(string(data))); cerr == nil {
						if p, perr := os.FindProcess(pid); perr == nil {
							_ = p.Kill() // SIGKILL on unix; type-checks on windows
						}
					}
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	log := &impCaptureLog{}
	var prompts []string
	adapter := &workers.OmpAdapter{}
	env := impBaseRetryEnv(repo)
	env.workerName = "omp"
	env.spawn = func(opts workers.WorkerSpawnOptions) workers.WorkerResult {
		prompts = append(prompts, opts.Prompt)
		return adapter.Spawn(opts)
	}
	env.coldStartMs = 0 // keep the silence clock authoritative
	env.backoff = func(int) int { return 0 }
	apiMax := 3.0
	env.apiMaxAttempts = &apiMax
	env.testGate = func(string, int) (gates.GateResult, error) {
		return gates.GateResult{Passed: true}, nil
	}

	// The 30s spawn wall bounds the mutated (kill-never-happens) case.
	res, succeeded, err := impRunRetryLoop(env, impTestPlan(), StageConfig{TimeoutMs: 30_000}, log)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !succeeded || !res.OK || res.Attempts != 2 {
		t.Fatalf("killed attempt must be retried to recovery: %+v succeeded=%v", res, succeeded)
	}
	if !log.has("info", "Worker omp attempt 2/3") {
		t.Fatalf("missing attempt-2 log: %+v", log.Entries)
	}
	// First attempt classified, not silently lost: exitCode -1 (signal
	// kill) and timedOut false (a wall kill would be misclassification).
	if v, ok := log.kvOf("info", "Attempt 1 finished", "exitCode"); !ok || v != -1 {
		t.Fatalf("attempt-1 exitCode = %v (ok=%v), want -1", v, ok)
	}
	if v, ok := log.kvOf("info", "Attempt 1 finished", "timedOut"); !ok || v != false {
		t.Fatalf("attempt-1 timedOut = %v (ok=%v), want false", v, ok)
	}
	// The retry is a repair dispatch that carries the kill as evidence.
	if len(prompts) != 2 {
		t.Fatalf("prompts = %d, want 2", len(prompts))
	}
	if !strings.Contains(prompts[1], "did NOT pass validation") || !strings.Contains(prompts[1], "worker exited -1") {
		t.Fatalf("attempt-2 prompt must be the repair prompt carrying the kill: %.200s", prompts[1])
	}
	// Exactly two worker launches (the counter file holds the current
	// invocation count).
	invData, err := os.ReadFile(invocations)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.TrimSpace(string(invData)); n != "2" {
		t.Fatalf("worker launches = %q, want 2", n)
	}
	// The first attempt also left its watchdog-health row on the ledger.
	raw, err := os.ReadFile(filepath.Join(repo, ledger.LedgerDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("attempt-1 watchdog-health row must exist: %v", err)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad ledger row %q: %v", line, err)
		}
		if row["event"] == "watchdog-health" && row["attempt"] == float64(1) && row["taskId"] == "T-IMP" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no attempt-1 watchdog-health row: %s", raw)
	}
}
