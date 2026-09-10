// chaos_test.go — FR-VAL-04 (issue #292): scripted fault-injection scenarios
// proving the driver's documented recovery paths against REAL processes, not
// just narrow unit tests (AWS REL12-BP04 / Azure fault-injection practice).
//
// Scenario → package split: the recovery code lives where the fault lands,
// so the suite spans three files that share the issue #292 acceptance
// ("recovery + correct ledger classification; removing the recovery code
// fails the suite"):
//
//   - here (driver): SIGKILL the driver mid-iteration → stale-lock break,
//     board recovery, uncorrupted ledger; network blackhole during state
//     push → push defers, loop continues, no wedge; repeated failing worker
//     → circuit breaker, exit 1, clean rows.
//   - internal/workers/chaos_test.go: silent provider hang → no-progress
//     watchdog kills within budget and the attempt increments.
//   - internal/pipeline/chaos_test.go: SIGKILL the worker mid-run → the
//     task retries attempt 2/3 and the first attempt is classified, not
//     silently lost.
package loopdriver

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// chaosDevagentFake mirrors the installFakes devagent script with chaos
// knobs: the task case can announce its pid + a liveness marker and then
// hang (CHAOS_TASK_SLEEP), succeed instantly (knobs unset), or fail
// (CHAOS_TASK_RC). The success path execs the sleep so the recorded pid IS
// the hanging process and a kill leaves no orphan holding the pipes.
const chaosDevagentFake = `#!/bin/sh
echo "devagent $*" >> "${DEVAGENT_LOG:?}"
case "$1" in
  scan-text) echo GRADIENT-SCAN-TEXT ;;
  ledger) exit 0 ;;
  herdr-sweep) exit 0 ;;
  preflight) exit ${CHAOS_PREFLIGHT_RC:-0} ;;
  sync-docs) echo "already at origin"; exit 0 ;;
  page-degrade-breach) exit 0 ;;
  extract-text) exit 0 ;;
  pane-run) exit 0 ;;
  task)
    if [ -n "${CHAOS_TASK_PIDFILE:-}" ]; then echo $$ > "$CHAOS_TASK_PIDFILE"; fi
    if [ -n "${CHAOS_TASK_MARKER:-}" ]; then : > "$CHAOS_TASK_MARKER"; fi
    rc="${CHAOS_TASK_RC:-0}"
    if [ "$rc" = "0" ]; then
      echo "PR opened: https://example.fake/pr/1"
      exec sleep "${CHAOS_TASK_SLEEP:-0}"
    fi
    exit "$rc" ;;
esac
exit 0
`

// chaosFakes installs the hermetic fake CLI set used by every scenario and
// returns the PATH dir (so a scenario can reference e.g. the fake ssh).
func chaosFakes(t *testing.T, repo string) string {
	t.Helper()
	dir := fakeBinDir(t, map[string]string{
		"devagent-fake": chaosDevagentFake,
		"gh-fake": `#!/bin/sh
echo "gh $*" >> "${DEVAGENT_LOG:?}"
case "$1 $2" in
  "issue list")
    if [ -n "${GH_ROTATE_STATE:-}" ]; then
      c=$(cat "$GH_ROTATE_STATE" 2>/dev/null || echo 0)
      c=$((c + 1))
      echo "$c" > "$GH_ROTATE_STATE"
      n=$(( (c - 1) % 3 + 1 ))
      printf '[{"number":%d,"title":"Chaos goal %d","labels":[{"name":"priority:P0"}]}]' "$((200 + n))" "$n"
    else
      printf '%s' "$GH_ISSUES_JSON"
    fi ;;
  "issue close") exit 0 ;;
esac
exit 0
`,
		"npm":      "#!/bin/sh\nexit 0\n",
		"go":       "#!/bin/sh\nexit 0\n",
		"omp-fake": "#!/bin/sh\nexit 0\n",
	})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("DEVAGENT_LOG", filepath.Join(repo, "devagent-calls.log"))
	return dir
}

// chaosLoopConfig is the hermetic non-dry-run config over the fixture repo
// (loopConfigFor's shape with DryRun forced off).
func chaosLoopConfig(repo string) LoopConfig {
	return LoopConfig{
		Repo:             repo,
		DevagentBin:      "devagent-fake",
		GhBin:            "gh-fake",
		DryRun:           false,
		NoSyncDocs:       true,
		PushMode:         "pr",
		MaxIterations:    2,
		CleanupDelaySecs: 1800,
		ResearchBin:      "omp-fake",
		POBin:            "omp-fake",
	}.WithDefaults()
}

// waitForFile polls until path exists or the timeout elapses.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// TestChaosHelperProcess is the re-exec entry point: the test binary runs
// itself as the driver process a scenario can kill. Env-gated, so it is a
// no-op under the normal suite.
func TestChaosHelperProcess(t *testing.T) {
	if os.Getenv("DEVAGENT_CHAOS_HELPER") != "1" {
		return
	}
	os.Exit(RunLoop(chaosLoopConfig(os.Getenv("DEVAGENT_CHAOS_REPO"))))
}

// Scenario 1 (issue #292): SIGKILL the driver mid-iteration → the next
// start must break the stale lock (.selfbuild/loop.lock.d/pid), recover the
// board, and complete a fresh iteration; the ledger shows no corrupted row.
// Mutation check: deleting the stale-holder clearing in acquireLock makes
// the recovery run exit 0 without any iteration, failing the row assertion.
func TestChaosDriverKillBreaksStaleLockAndRecovers(t *testing.T) {
	repo := initFixtureRepo(t)
	chaosFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":201,"title":"Chaos stale-lock goal","labels":[{"name":"priority:P0"}]}]`)

	marker := filepath.Join(repo, "chaos-task-marker")
	pidFile := filepath.Join(repo, "chaos-task-pid")
	// Reap the orphaned sleeping dispatch child when the test ends (the
	// repo's killPidfileAtCleanup convention — os.Process.Kill is SIGKILL
	// on unix and type-checks on windows).
	t.Setenv("PIDFILE", pidFile)
	killPidfileAtCleanup(t)

	// Run 1: the driver as a real process, hanging inside the phase-4 task
	// dispatch (mid-iteration, lock held).
	cmd := exec.Command(os.Args[0], "-test.run=^TestChaosHelperProcess$")
	cmd.Env = append(os.Environ(),
		"DEVAGENT_CHAOS_HELPER=1",
		"DEVAGENT_CHAOS_REPO="+repo,
		"CHAOS_TASK_MARKER="+marker,
		"CHAOS_TASK_PIDFILE="+pidFile,
		"CHAOS_TASK_SLEEP=120",
	)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, marker, 30*time.Second)
	_ = cmd.Process.Kill() // SIGKILL on unix — the driver dies mid-iteration, lock held
	_ = cmd.Wait()

	// The lock survived the kill with the dead driver's pid on record.
	pidData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "loop.lock.d", "pid"))
	if err != nil {
		t.Fatalf("driver died mid-iteration but left no lock pid file: %v\nhelper output:\n%s", err, out.String())
	}
	if got := strings.TrimSpace(string(pidData)); got != strconv.Itoa(cmd.Process.Pid) {
		t.Fatalf("stale lock pid = %q, want the killed driver's %d", got, cmd.Process.Pid)
	}

	// Run 2: the next start must clear the stale lock and ship a fresh
	// iteration.
	var run2 bytes.Buffer
	cfg := chaosLoopConfig(repo)
	cfg.Stdout = &run2
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("recovery run rc = %d, want 0\nrun output:\n%s", rc, run2.String())
	}
	if !strings.Contains(run2.String(), fmt.Sprintf("[lock] stale holder pid %d is gone — clearing lock and retrying", cmd.Process.Pid)) {
		t.Fatalf("recovery run must break the stale lock, output:\n%s", run2.String())
	}
	// No corrupted row: readLedger unmarshals every line, and the recovery
	// run completed exactly one fresh ok iteration.
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "ok" || rows[0]["loop"] != float64(1) {
		t.Fatalf("recovery ledger = %v, want exactly one ok loop-1 row", rows)
	}
}

// Scenario 4 (issue #292): network blackhole during state push/pull —
// origin points at an unreachable host and the fake ssh is the black hole
// (swallows the transport, never returns) — the push must defer, the loop
// must continue, and nothing may wedge (the 2026-09-06 hung-state-sync
// class). Each state-sync git op is bounded by the networkTimeout context,
// shortened here so the black hole is cut at the bound instead of the
// production 60s; the RunLoop-level deadline fails the scenario fast if
// any state-sync call wedges past it (mutation check: dropping the
// context.WithTimeout in stateSync.git wedges the driver and fails here).
func TestChaosStatePushBlackholeDefers(t *testing.T) {
	repo := initFixtureRepo(t)
	binDir := chaosFakes(t, repo)
	t.Setenv("GH_ROTATE_STATE", filepath.Join(t.TempDir(), "pick-count"))

	sshLog := filepath.Join(repo, "chaos-ssh.log")
	sshBody := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexec sleep 300\n", sshLog)
	sshPath := filepath.Join(binDir, "ssh-blackhole")
	if err := os.WriteFile(sshPath, []byte(sshBody), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_SSH_COMMAND", sshPath)
	cmd := exec.Command("git", "-C", repo, "remote", "add", "origin", "ssh://chaos-blackhole.invalid/srv/git/devagent-state.git")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v: %s", err, out)
	}

	oldTimeout := networkTimeout
	networkTimeout = 500 * time.Millisecond
	t.Cleanup(func() { networkTimeout = oldTimeout })

	cfg := chaosLoopConfig(repo)
	cfg.MaxIterations = 4 // three rotating goals ship, head cap stops the 4th
	done := make(chan int, 1)
	go func() { done <- RunLoop(cfg) }()
	select {
	case rc := <-done:
		if rc != 0 {
			logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
			t.Fatalf("rc = %d, want 0 (a blackholed state push must not kill the loop)\nlog:\n%s", rc, logData)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("RunLoop wedged — a state-sync git call outlived the networkTimeout bound")
	}

	rows := readLedger(t, repo)
	if len(rows) != 3 {
		log1, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
		t.Fatalf("rows = %d, want 3 shipped iterations past the blackhole\nloop-1.log:\n%s", len(rows), log1)
	}
	for _, row := range rows {
		if row["status"] != "ok" {
			t.Fatalf("row = %v, want ok", row)
		}
	}
	// Every iteration deferred its state push instead of wedging.
	for n := 1; n <= 3; n++ {
		assertFileContains(t, filepath.Join(repo, ".selfbuild", "logs", fmt.Sprintf("loop-%d.log", n)), "[state] push deferred")
	}
	// The black hole actually saw the traffic (1 pull fetch + 4 per Push).
	sshData, err := os.ReadFile(sshLog)
	if err != nil || strings.Count(string(sshData), "\n") < 5 {
		t.Fatalf("blackhole ssh must be exercised: %v\n%s", err, sshData)
	}
}

// Scenario 5 (issue #292): repeated failing worker → the circuit breaker
// trips at MaxConsecutiveFailures, the driver exits 1, and the ledger rows
// stay clean (every line parses, every failure classified).
func TestChaosRepeatedWorkerFailureTripsBreaker(t *testing.T) {
	repo := initFixtureRepo(t)
	chaosFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":205,"title":"Chaos breaker goal","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("CHAOS_TASK_RC", "1")

	cfg := chaosLoopConfig(repo)
	cfg.MaxIterations = 5
	cfg.MaxConsecutiveFailures = 3
	if rc := RunLoop(cfg); rc != 1 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-3.log"))
		t.Fatalf("rc = %d, want 1 (circuit breaker)\nlog:\n%s", rc, logData)
	}

	rows := readLedger(t, repo)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want exactly 3 failed rows before the breaker", len(rows))
	}
	for i, row := range rows {
		if row["status"] != "failed" || row["loop"] != float64(i+1) {
			t.Fatalf("row %d = %v, want failed / loop %d", i, row, i+1)
		}
	}
	assertFileContains(t, filepath.Join(repo, ".selfbuild", "logs", "loop-3.log"), "circuit breaker: 3 consecutive failures")
}
