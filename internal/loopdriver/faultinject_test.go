package loopdriver

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Seeded, deterministic fault-injection simulation of the loop-driver run
// cycle (FR-VAL-04 chaos soak, issue #292). A math/rand/v2 PCG stream turns
// a seed into a fault script that the fake devagent binary serves one task
// dispatch at a time; the REAL RunLoop consumes it over the hermetic
// golden-soak fixture. The assertion is the driver's documented recovery
// contract, derived independently in Go: dispatch rc != 0 (worker error, or
// the timeout wall's rc 124) is a `failed` row + breaker bump, a rc-0 run
// without PR evidence is a `no-pr` row that preserves the failure streak, a
// ship is an `ok` row that resets it, and breakerAt consecutive failures end
// the run with exit 1. Same seed → same script → same ledger, byte for byte.

type simFault string

const (
	simOK          simFault = "ok"           // rc 0 + "PR opened:" → ship
	simWorkerError simFault = "worker-error" // rc 3 → failed row, breaker bump
	simTimeout     simFault = "timeout"      // fake sleeps past the wall → rc 124
	simNoPR        simFault = "no-pr"        // rc 0 without evidence → no-pr row
)

// simScript draws n faults from the seeded PCG stream. Probabilities are
// consulted in a fixed order, so the stream position of every fault is a
// function of the seed alone.
func simScript(seed int64, n int, pWorkerError, pTimeout, pNoPR float64) []simFault {
	rng := rand.New(rand.NewPCG(uint64(seed), 0))
	script := make([]simFault, 0, n)
	for i := 0; i < n; i++ {
		switch r := rng.Float64(); {
		case r < pWorkerError:
			script = append(script, simWorkerError)
		case r < pWorkerError+pTimeout:
			script = append(script, simTimeout)
		case r < pWorkerError+pTimeout+pNoPR:
			script = append(script, simNoPR)
		default:
			script = append(script, simOK)
		}
	}
	return script
}

// predictRun applies the driver's recovery contracts to a script and returns
// the predicted ledger status sequence and exit code. This is the
// simulation's oracle: ~20 lines of documented semantics (runTaskPhase +
// breakerTripped), not a re-implementation of the iteration body — a driver
// regression that changes classification or breaker arithmetic breaks the
// match.
func predictRun(script []simFault, breakerAt int) (statuses []string, exitCode int) {
	fails := 0
	for _, f := range script {
		switch f {
		case simOK:
			statuses = append(statuses, "ok")
			fails = 0
		case simNoPR:
			// bash `continue` without a reset: the streak survives.
			statuses = append(statuses, "no-pr")
		default: // simWorkerError, simTimeout — both dispatch rc != 0
			statuses = append(statuses, "failed")
			fails++
			if breakerAt > 0 && fails >= breakerAt {
				return statuses, 1
			}
		}
	}
	return statuses, 0
}

// writeFaultScript materializes the script as one fault word per line, the
// format the fake's sed -n "${c}p" serves by dispatch index.
func writeFaultScript(t *testing.T, script []simFault) string {
	t.Helper()
	words := make([]string, 0, len(script))
	for _, f := range script {
		words = append(words, string(f))
	}
	path := filepath.Join(t.TempDir(), "fault-script")
	if err := os.WriteFile(path, []byte(strings.Join(words, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// runSeededSoak drives the real RunLoop over the fixture with the seeded
// fault script installed and returns the observed ledger statuses + rc.
// Each iteration picks a distinct issue (the rotating fake serves issue
// #(200+pick) with a modulus larger than the script), so no already-shipped
// re-pick interleaves with the faults; the fake tracker reports every
// opened PR MERGED, so ok rows are ships.
func runSeededSoak(t *testing.T, repo string, script []simFault, breakerAt int) ([]string, int) {
	t.Helper()
	t.Setenv("GH_ROTATE_STATE", filepath.Join(t.TempDir(), "pick-count"))
	t.Setenv("GH_ROTATE_MOD", fmt.Sprint(len(script)+4)) // every pick distinct
	t.Setenv("GH_PR_STATE", "MERGED")
	t.Setenv("DEVAGENT_FAULT_SCRIPT", writeFaultScript(t, script))
	t.Setenv("DEVAGENT_FAULT_STATE", filepath.Join(t.TempDir(), "dispatch-count"))
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxIterations = len(script) + 1 // an extra iteration would over-serve ok and fail the prediction
		c.TaskTimeout = 1                 // the timeout fault sleeps 2 — the wall must fire first
		c.MaxConsecutiveFailures = breakerAt
		c.StarvationLimit = len(script) + 1 // the breaker scenario owns the halting contract
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	rows := readLedger(t, repo)
	statuses := make([]string, 0, len(rows))
	for _, row := range rows {
		s, _ := row["status"].(string)
		statuses = append(statuses, s)
	}
	return statuses, rc
}

// assertStatuses fails the test unless the observed ledger statuses match
// the prediction exactly; the iteration log rides along on failure.
func assertStatuses(t *testing.T, repo string, got, want []string) {
	t.Helper()
	for i, w := range want {
		if i >= len(got) {
			t.Fatalf("ledger rows = %d, want %d (statuses %v)", len(got), len(want), want)
		}
		if got[i] != w {
			logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
			t.Fatalf("row %d status = %q, want %q\ngot: %v\nlog:\n%s", i+1, got[i], w, got, logData)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("ledger rows = %d (%v), want %d (%v)", len(got), got, len(want), want)
	}
}

// TestSimScriptDeterministic pins the seed contract: the same seed draws the
// same script, a different seed (overwhelmingly) draws a different one.
func TestSimScriptDeterministic(t *testing.T) {
	a1 := simScript(7, 16, 0.3, 0.1, 0.1)
	a2 := simScript(7, 16, 0.3, 0.1, 0.1)
	b := simScript(8, 16, 0.3, 0.1, 0.1)
	if fmt.Sprint(a1) != fmt.Sprint(a2) {
		t.Fatalf("seed 7 script not reproducible:\n%v\n%v", a1, a2)
	}
	if fmt.Sprint(a1) == fmt.Sprint(b) {
		t.Fatalf("seeds 7 and 8 drew identical scripts: %v", a1)
	}
}

// TestSeededFaultInjectionSoak: the seeded sweep. Five seeds × 8 iterations
// of mixed ok/worker-error/timeout/no-pr faults (breaker disabled), the
// ledger must match the prediction exactly and every ship must close its
// issue while every fault leaves it open.
func TestSeededFaultInjectionSoak(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 7, 42} {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			script := simScript(seed, 8, 0.3, 0.12, 0.15)
			want, wantRC := predictRun(script, 1000)

			repo := initFixtureRepo(t)
			installFakes(t, repo)
			got, rc := runSeededSoak(t, repo, script, 1000)

			if rc != wantRC {
				t.Fatalf("rc = %d, want %d (predicted %v, got %v)", rc, wantRC, want, got)
			}
			assertStatuses(t, repo, got, want)

			calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
			if err != nil {
				t.Fatal(err)
			}
			okShips, closes := 0, strings.Count(string(calls), "gh issue close")
			for _, s := range want {
				if s == "ok" {
					okShips++
				}
			}
			if closes != okShips {
				t.Fatalf("issue closes = %d, want %d (one per ok ship, none per fault)\n%s", closes, okShips, calls)
			}
		})
	}
}

// TestSeededTimeoutWallRecovery: the wall fault class end to end. The fake
// sleeps 2s against TaskTimeout=1, so the dispatch wall must fire (rc 124) —
// recorded as a failed row — and the next ok iteration must ship and reset
// the streak. If the wall stopped firing, the fake would exit 0 with a PR
// line and the prediction would not match.
func TestSeededTimeoutWallRecovery(t *testing.T) {
	script := []simFault{simTimeout, simOK, simOK}
	want, _ := predictRun(script, 1000)

	repo := initFixtureRepo(t)
	installFakes(t, repo)
	got, rc := runSeededSoak(t, repo, script, 1000)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	assertStatuses(t, repo, got, want)
}

// TestSeededBreakerTrip: consecutive dispatch failures trip the circuit
// breaker at MaxConsecutiveFailures — exit 1, the run stops at exactly
// breakerAt failed rows, and no issue was closed by a faulted iteration.
func TestSeededBreakerTrip(t *testing.T) {
	script := simScript(11, 4, 1.0, 0, 0) // every iteration fails
	want, _ := predictRun(script, 3)

	repo := initFixtureRepo(t)
	installFakes(t, repo)
	got, rc := runSeededSoak(t, repo, script, 3)

	if rc != 1 {
		t.Fatalf("rc = %d, want 1 (breaker exit)", rc)
	}
	assertStatuses(t, repo, got, want)
	if len(got) != 3 {
		t.Fatalf("rows = %d (%v), want 3 — the breaker must stop the run at the limit", len(got), got)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "gh issue close") {
		t.Fatalf("faulted iterations must leave their issues open\n%s", calls)
	}
}
