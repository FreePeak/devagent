// chaos_test.go — FR-VAL-04 (issue #292), worker-level scenario: a fake
// provider that emits nothing must be killed by the no-progress watchdog
// within its budget (not by the wall clock), and the adapter's retry loop
// must increment to attempt 2 and recover. The watchdog-health ledger rows
// carry the evidence. See internal/loopdriver/chaos_test.go for the
// scenario → package split.
package workers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
)

// Scenario 3 (issue #292): fake provider hang → the no-progress watchdog
// kills within budget and the attempt increments. Mutation check: removing
// the watchdog kill leaves the wall clock to fire at 30s — the ledger row's
// WallClockMs and the coalesced WatchdogFired flag then expose the
// regression.
func TestChaosSilentProviderWatchdogKillIncrementsAttempt(t *testing.T) {
	repo := t.TempDir()
	cwd := t.TempDir()
	binDir := t.TempDir()
	calls := filepath.Join(binDir, "chaos-omp-calls")
	// Attempt 1: emit nothing, hang until killed (exec so the kill lands on
	// the script process itself, closing the pipes immediately). Attempt 2:
	// a healthy result envelope.
	script := fmt.Sprintf(`#!/bin/sh
c=$(cat %q 2>/dev/null || echo 0)
c=$((c + 1))
echo "$c" > %q
if [ "$c" = "1" ]; then
  exec sleep 120
fi
echo '{"type":"result","result":"chaos-recovered","session_id":"s-2","is_error":false}'
exit 0
`, calls, calls)
	if err := os.WriteFile(filepath.Join(binDir, "omp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	a := OmpAdapter{adapterBase{Sleep: func(int) {}}} // zero inter-attempt backoff
	start := time.Now()
	res := a.Spawn(WorkerSpawnOptions{
		Prompt:    "Goal: chaos hang",
		Cwd:       cwd,
		TimeoutMs: 30_000, // wall backstop: the watchdog, not this, must kill
		// 3s budget: comfortably above the fork+exec+sh startup a
		// saturated `go test ./...` can impose on the fake (the #271
		// class), still far under the 30s wall the mutation must hit.
		NoProgressTimeoutMs: intPtr(3_000),
		WatchdogLedger:      &WatchdogLedgerContext{RepoPath: repo, TaskId: "CHAOS-HANG", Attempt: 1, Worker: "omp"},
	})
	elapsed := time.Since(start)

	if res.ExitCode != 0 || res.TimedOut || res.ResultText != "chaos-recovered" {
		t.Fatalf("attempt 2 must recover: %+v", res)
	}
	// The coalesced stream evidence must remember the attempt-1 watchdog
	// kill — a wall-clock kill would leave this false (pins the omp.go
	// coalescing arm; a deleted sawWatchdogFired propagation fails here).
	if !res.WatchdogFired {
		t.Fatalf("attempt-1 kill must be a no-progress kill, not the wall: %+v", res)
	}
	if elapsed > 25*time.Second {
		t.Fatalf("hang recovery took %v — the 3s watchdog did not fire in budget", elapsed)
	}
	// Attempt incremented: the fake was launched a second time (the file
	// holds the current invocation count).
	callsData, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.TrimSpace(string(callsData)); n != "2" {
		t.Fatalf("worker launches = %q, want 2 (attempt 1 killed, attempt 2 recovers)", n)
	}

	// The kill is on the ledger: an attempt-1 watchdog-health row with
	// watchdogFired and a wall clock inside the burn budget, plus the clean
	// attempt-2 teardown row.
	raw, err := os.ReadFile(filepath.Join(repo, ledger.LedgerDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("watchdog rows must reach the ledger: %v", err)
	}
	var firedRow, cleanRow map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad watchdog row %q: %v", line, err)
		}
		if row["event"] != "watchdog-health" {
			continue
		}
		if row["watchdogFired"] == true && firedRow == nil {
			firedRow = row
		}
		if row["watchdogFired"] == false && cleanRow == nil {
			cleanRow = row
		}
	}
	if firedRow == nil {
		t.Fatalf("no watchdog-health row records the firing: %s", raw)
	}
	if wall := firedRow["wallClockMs"].(float64); wall < 500 || wall >= 10000 {
		t.Fatalf("firing row wallClockMs = %v, want the ~3s watchdog kill, not the 30s wall", firedRow["wallClockMs"])
	}
	if cleanRow == nil {
		t.Fatalf("attempt-2 teardown row missing: %s", raw)
	}
}
