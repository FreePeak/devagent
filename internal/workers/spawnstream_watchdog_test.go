// Go port of the spawn-cli streaming watchdog semantics (Q30/Q31/Q34,
// test/spawn-utils.test.ts + PRD Q33 fixtures): fake binaries on PATH, no
// real worker CLIs.

package workers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
)

// fakeBin writes an executable shell script onto a fresh PATH dir and
// returns its absolute path.
func fakeBin(t *testing.T, name string, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// thinkingLoopBody: a glm-style deliberation stream — thinking_delta
// forever, zero tool calls (2026-08-31 evidence: 60k+ deltas, 8-11MB,
// full hour).
const thinkingLoopBody = `while true; do
  echo '{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" x"}}'
  sleep 0.05
done`

// silentBody: startup chatter then silence (the omp plugin/MCP init wedge
// class — startup lines are not adapter-classified progress).
const silentBody = `echo '{"type":"session","id":"s-1"}'
sleep 60`

// progressThenExitBody: meaningful output then a clean exit.
const progressThenExitBody = `echo '{"type":"tool_execution_start","toolName":"read"}'
echo '{"type":"result","result":"done"}'
exit 0`

func TestSpawnCliStreaming_ColdStartDeadlineKillsThinkingOnlyStream(t *testing.T) {
	// Q31: until the classifier has seen one progress line, the cold-start
	// budget is binding — thinking_delta chatter must not keep the launch
	// alive.
	bin := fakeBin(t, "fake-worker", thinkingLoopBody)
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 30_000, ColdStartTimeoutMs: 300})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if !res.ColdStart {
		t.Fatalf("expected ColdStart=true, got %+v", res)
	}
	if res.ExitCode != -1 {
		t.Fatalf("expected exit code -1 on watchdog kill, got %d", res.ExitCode)
	}
}

func TestSpawnCliStreaming_NoProgressWatchdogFiresOnSilence(t *testing.T) {
	// The silence clock: no adapter-classified progress for the budget.
	bin := fakeBin(t, "fake-worker", silentBody)
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 30_000, NoProgressTimeoutMs: intPtr(300)})
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	if res.ColdStart {
		t.Fatalf("expected ColdStart=false for a post-start silence kill, got %+v", res)
	}
}

func TestSpawnCliStreaming_MeaningfulOutputResetsClockAndCompletes(t *testing.T) {
	// A run that produces adapter-classified progress and exits cleanly
	// must NOT be killed by the watchdog (the thinking-only regression:
	// counting raw bytes as progress meant the clock never fired for
	// deliberation-only runs; here we pin the inverse — real work passes).
	// Deadlines stay >= 1s: a freshly written binary can cost ~600ms on
	// its first macOS execution (Gatekeeper scan) before any output — and
	// `go test ./...` parallel-package load has pushed the first exec past
	// the old 1500ms budget (observed ColdStart kill at 1.5s), so the cold
	// start rides a 5s budget; the kill-on-cold-start contract is pinned by
	// TestSpawnCliStreaming_ColdStartDeadlineKillsThinkingOnlyStream.
	bin := fakeBin(t, "fake-worker", progressThenExitBody)
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 10_000, NoProgressTimeoutMs: intPtr(1500), ColdStartTimeoutMs: 5000})
	if res.TimedOut {
		t.Fatalf("expected clean completion, got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d (stderr %q)", res.ExitCode, res.Stderr)
	}
	if res.ColdStart {
		t.Fatalf("unexpected ColdStart, got %+v", res)
	}
}

// TestSpawnCliStreaming_NoProgressClockDefersToColdStart pins the Q31
// window semantics: while clockResets == 0 the no-progress clock never
// fires, even when noProgressMs < coldStartMs and the child stays silent
// well past noProgressMs. Pre-fix, a loaded box's slow fork+exec breached
// the tighter no-progress budget first and killed a healthy startup
// (WatchdogFired at ~noProgressMs with MeaningfulBytes 0, issue #271
// class). Deterministic: the child sleeps past noProgressMs, emits one
// meaningful line (the deference contract covers this transition), and
// the run must complete cleanly.
func TestSpawnCliStreaming_NoProgressClockDefersToColdStart(t *testing.T) {
	bin := fakeBin(t, "fake-worker", `sleep 1.2
echo '{"type":"tool_execution_start","toolName":"read"}'
exit 0`)
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 15_000, NoProgressTimeoutMs: intPtr(500), ColdStartTimeoutMs: 5000})
	if res.TimedOut || res.WatchdogFired || res.ColdStart {
		t.Fatalf("cold-start window must defer the no-progress clock, got %+v", res)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit 0, got %d (stderr %q)", res.ExitCode, res.Stderr)
	}
}

func TestSpawnCliStreaming_WatchdogHealthRow(t *testing.T) {
	// Q34: an armed clock + ledger context emits a watchdog-health row with
	// the TS field names and the firing evidence.
	bin := fakeBin(t, "fake-worker", silentBody)
	var rows []WatchdogHealthRecord
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{
		TimeoutMs:           30_000,
		NoProgressTimeoutMs: intPtr(300),
		WatchdogLedger:      &WatchdogLedgerContext{RepoPath: t.TempDir(), TaskId: "T1", Attempt: 2, Worker: "omp"},
		WatchdogSink:        func(r WatchdogHealthRecord) { rows = append(rows, r) },
	})
	if !res.TimedOut || res.ColdStart {
		t.Fatalf("expected no-progress kill, got %+v", res)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one watchdog-health row, got %d", len(rows))
	}
	r := rows[0]
	if r.Event != "watchdog-health" || r.Kind != "event" {
		t.Fatalf("row event/kind = %q/%q, want watchdog-health/event", r.Event, r.Kind)
	}
	if r.TaskId != "T1" || r.Attempt != 2 || r.Worker != "omp" || r.Site != "spawn-cli" {
		t.Fatalf("row identity mismatch: %+v", r)
	}
	if r.Runtime != "direct" || r.Visible {
		t.Fatalf("direct-exec row must say runtime=direct visible=false: %+v", r)
	}
	if !r.WatchdogFired || r.ColdStartFired {
		t.Fatalf("row firing flags mismatch: %+v", r)
	}
	if r.NoProgressTimeoutMs != 300 {
		t.Fatalf("row budget = %d, want 300", r.NoProgressTimeoutMs)
	}
	if r.WallClockMs <= 0 || r.IdleMs < 0 {
		t.Fatalf("row clocks = wall %d idle %d", r.WallClockMs, r.IdleMs)
	}
}

func TestSpawnCliStreaming_WallClockTimeout(t *testing.T) {
	// No watchdog armed: the hard wall clock still kills with timedOut.
	bin := fakeBin(t, "fake-worker", "sleep 30")
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 250})
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("expected wall-clock kill, got %+v", res)
	}
}

func TestSpawnCliStreaming_ExitCodePropagates(t *testing.T) {
	bin := fakeBin(t, "fake-worker", "echo out; echo err >&2; exit 3")
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 5_000})
	if res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("expected exit 3, got %+v", res)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" {
		t.Fatalf("stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestSpawnCliStreaming_MissingBinary(t *testing.T) {
	// ENOENT: exit -1, stderr carries the error, not a timeout.
	res := spawnCliStreaming("/nonexistent/devagent-fake-bin-xyz", nil, SpawnCliOptions{TimeoutMs: 5_000})
	if res.ExitCode != -1 || res.TimedOut {
		t.Fatalf("expected spawn-failure shape, got %+v", res)
	}
	if res.Stderr == "" {
		t.Fatalf("expected stderr to carry the spawn error")
	}
}

func TestSpawnCliRouting_DispatchesStreamingOnlyWhenArmed(t *testing.T) {
	// spawnCli routes to the streaming variant only when a clock is armed;
	// otherwise it takes the execFile path (spawn.RunCli).
	bin := fakeBin(t, "fake-worker", "echo hi")
	plain := SpawnCli(bin, nil, SpawnCliOptions{TimeoutMs: 5_000})
	if plain.ExitCode != 0 || plain.TimedOut || plain.ColdStart {
		t.Fatalf("unarmed spawn = %+v", plain)
	}
	if plain.Stdout != "hi\n" {
		t.Fatalf("stdout = %q", plain.Stdout)
	}
}

// Thinking-delta bytes must not reset the no-progress clock even when they
// arrive as stderr — the classifier is chunk-split and stream-agnostic.
func TestSpawnCliStreaming_StderrThinkingDoesNotResetClock(t *testing.T) {
	bin := fakeBin(t, "fake-worker", `while true; do
  echo '{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" deep"}}' >&2
  sleep 0.05
done`)
	start := time.Now()
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 30_000, NoProgressTimeoutMs: intPtr(400)})
	if !res.TimedOut {
		t.Fatalf("expected watchdog kill, got %+v", res)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("watchdog fired too late: %v", time.Since(start))
	}
}

// Issue #248 required change 1 (FR-GO-05 #190 TODO closure): a production
// spawn with an armed clock and NO explicit WatchdogSink still emits the
// Q34 watchdog-health row — the default sink appends it to the ledger
// events file of the WatchdogLedgerContext repo.
func TestSpawnCliStreaming_DefaultSinkAppendsLedgerRow(t *testing.T) {
	repo := t.TempDir()
	bin := fakeBin(t, "fake-worker-ledger", silentBody)
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{
		TimeoutMs:           30_000,
		NoProgressTimeoutMs: intPtr(300),
		WatchdogLedger:      &WatchdogLedgerContext{RepoPath: repo, TaskId: "T-obs", Attempt: 3, Worker: "omp"},
	})
	if !res.TimedOut || !res.WatchdogFired {
		t.Fatalf("expected no-progress kill, got %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ledger.LedgerDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("watchdog-health row must land in the ledger: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("row = %s: %v", raw, err)
	}
	if row["event"] != "watchdog-health" || row["taskId"] != "T-obs" {
		t.Fatalf("row identity = %v", row)
	}
	if row["site"] != "spawn-cli" || row["runtime"] != "direct" {
		t.Fatalf("direct-exec row = %v", row)
	}
	if row["watchdogFired"] != true {
		t.Fatalf("row must record the firing: %v", row)
	}
	// herdr-pane parity: attempt and worker come from the ledger context.
	if row["attempt"] != float64(3) || row["worker"] != "omp" {
		t.Fatalf("row identity fields = %v", row)
	}
}

// Issue #248 required change 1: no ledger context = no row (probe/one-off
// spawns are not orchestrated runs), even with a clock armed.
func TestSpawnCliStreaming_NoContextNoRow(t *testing.T) {
	repo := t.TempDir()
	bin := fakeBin(t, "fake-worker-noctx", silentBody)
	spawnCliStreaming(bin, nil, SpawnCliOptions{
		TimeoutMs:           30_000,
		NoProgressTimeoutMs: intPtr(300),
	})
	if _, err := os.Stat(filepath.Join(repo, ledger.LedgerDir)); !os.IsNotExist(err) {
		t.Fatalf("no-ledger-context spawn must not write any ledger: %v", err)
	}
}

// The drain cap must be comfortably under the grandchild lifetime: the
// legacy unbounded wait would block ~5s here (the grandchild holds the
// pipe until sleep 5 exits), the cap returns at ~500ms with everything
// the drain captured so far.
func TestSpawnCliStreaming_PostKillDrainBound(t *testing.T) {
	body := `echo '{"type":"tool_execution_start","toolName":"read"}'
sleep 5 &
echo parent-done
exit 0`
	bin := fakeBin(t, "fake-worker-drain", body)
	start := time.Now()
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{
		TimeoutMs:           30_000,
		NoProgressTimeoutMs: intPtr(5000),
		PostKillDrainWaitMs: 500,
	})
	elapsed := time.Since(start)
	if res.TimedOut || res.ExitCode != 0 {
		t.Fatalf("expected clean exit, got %+v", res)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("drain wait must be bounded well under the grandchild lifetime, took %v", elapsed)
	}
	if !strings.Contains(res.Stdout, "parent-done") {
		t.Fatalf("drained stdout must keep the captured prefix, got %q", res.Stdout)
	}
}

// Issue #248 required change 4: the bounded wait must actually bound. A
// WaitGroup whose drains never finish returns after ~waitMs with the cap.
func TestWaitPipeDrain_Bounded(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1) // never Done: models a pipe held open past the wall
	start := time.Now()
	waitPipeDrain(&wg, 100)
	elapsed := time.Since(start)
	if elapsed < 90*time.Millisecond || elapsed > 3*time.Second {
		t.Fatalf("bounded wait = %v, want ~100ms", elapsed)
	}
	// A clean drain returns immediately even with a huge cap.
	var done sync.WaitGroup
	done.Add(1)
	done.Done()
	start = time.Now()
	waitPipeDrain(&done, 60_000)
	if time.Since(start) > time.Second {
		t.Fatalf("clean drain must not wait the cap: %v", time.Since(start))
	}
}

// Negative wait = legacy unbounded opt-in: blocks until every drain
// finishes.
func TestWaitPipeDrain_UnboundedOptIn(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		wg.Done()
		close(released)
	}()
	waitPipeDrain(&wg, -1)
	<-released
}

// 0 = the production default cap (named const, default 3s per the issue).
func TestWaitPipeDrain_DefaultCapSentinel(t *testing.T) {
	if DefaultPostKillDrainWaitMs != 3000 {
		t.Fatalf("production cap = %d, want 3000", DefaultPostKillDrainWaitMs)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	start := time.Now()
	waitPipeDrain(&wg, 0)
	if elapsed := time.Since(start); elapsed > time.Duration(DefaultPostKillDrainWaitMs)*time.Millisecond+3*time.Second {
		t.Fatalf("default cap not applied: %v", elapsed)
	}
}

func TestSpawnCliStreaming_PeriodicWatchdogHealthRows(t *testing.T) {
	// FR-VAL-03 #291c: a wedged worker emits watchdog-health rows while it
	// burns its budget — periodic rows with watchdogFired=false during the
	// run, then the firing teardown row. A hang must be visible in the
	// ledger before the manual kill that used to be the only evidence.
	old := watchdogHealthRowInterval
	watchdogHealthRowInterval = 200 * time.Millisecond
	t.Cleanup(func() { watchdogHealthRowInterval = old })

	bin := fakeBin(t, "fake-worker", silentBody)
	var mu sync.Mutex
	var rows []WatchdogHealthRecord
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{
		TimeoutMs:           30_000,
		NoProgressTimeoutMs: intPtr(1200),
		WatchdogLedger:      &WatchdogLedgerContext{RepoPath: t.TempDir(), TaskId: "T291", Attempt: 1, Worker: "omp"},
		WatchdogSink: func(r WatchdogHealthRecord) {
			mu.Lock()
			defer mu.Unlock()
			rows = append(rows, r)
		},
	})
	if !res.TimedOut || res.ColdStart {
		t.Fatalf("expected no-progress kill, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rows) < 2 {
		t.Fatalf("expected >= 2 rows (periodic + teardown), got %d", len(rows))
	}
	if rows[0].WatchdogFired {
		t.Fatalf("first row must be periodic (watchdogFired=false): %+v", rows[0])
	}
	if rows[0].WallClockMs >= 1200 {
		t.Fatalf("first periodic row wall = %d, want inside the budget", rows[0].WallClockMs)
	}
	last := rows[len(rows)-1]
	if !last.WatchdogFired {
		t.Fatalf("teardown row must record the fire: %+v", last)
	}
}
