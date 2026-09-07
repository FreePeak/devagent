// Go port of the spawn-cli streaming watchdog semantics (Q30/Q31/Q34,
// test/spawn-utils.test.ts + PRD Q33 fixtures): fake binaries on PATH, no
// real worker CLIs.

package workers

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	// its first macOS execution (Gatekeeper scan) before any output.
	bin := fakeBin(t, "fake-worker", progressThenExitBody)
	res := spawnCliStreaming(bin, nil, SpawnCliOptions{TimeoutMs: 10_000, NoProgressTimeoutMs: intPtr(1500), ColdStartTimeoutMs: 1500})
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
