package loopdriver

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// taskWallShim installs a devagent-fake whose body runs verbatim; the shim
// receives $PIDFILE to record backgrounded-children pids for tree-liveness
// assertions and cleanup.
func taskWallShim(t *testing.T, body string) {
	t.Helper()
	dir := fakeBinDir(t, map[string]string{"devagent-fake": body})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("PIDFILE", filepath.Join(t.TempDir(), "grandchild.pid"))
}

// killPidfileAtCleanup kills the pid recorded in $PIDFILE when the test
// ends, so a grandchild that outlived the shim cannot linger on the box.
func killPidfileAtCleanup(t *testing.T) {
	t.Helper()
	pidPath := os.Getenv("PIDFILE")
	t.Cleanup(func() {
		b, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			return
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
}

// taskWallDriver builds a driver with a 20s task wall over the fixture
// repo. The wall is generous: under a saturated `go test ./...` (the #271
// class) a short wall fires before the shim even spawns its children — it
// must cut a live dispatch, not race fork latency. The wall-vs-drain rc
// contract is pinned deterministically by TestDispatchRcMapping.
func taskWallDriver(t *testing.T, repo string) *driver {
	t.Helper()
	d := gateDriver(t, repo, "")
	d.cfg.DevagentBin = "devagent-fake" // gateDriver leaves the real `devagent` default
	d.cfg.TaskTimeout = 20
	return d
}

// TestDispatchRcMapping pins the ProcessState-driven rc contract with real
// child states — no wall-clock timing: a child that finished 0 keeps 0 even
// when the wall fired around its drain (the shipped-task-never-relabeled
// invariant runTaskPhase consumes); only a child the wall actually killed
// reports 124.
func TestDispatchRcMapping(t *testing.T) {
	state := func(exit int) *os.ProcessState {
		t.Helper()
		cmd := exec.Command("sh", "-c", "exit "+itoa(exit))
		if err := cmd.Run(); err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatalf("sh -c exit %d: %v", exit, err)
			}
		}
		return cmd.ProcessState
	}
	if rc := dispatchRc(nil, false); rc != 1 {
		t.Fatalf("nil state rc = %d, want 1 (Start failed)", rc)
	}
	if rc := dispatchRc(state(0), false); rc != 0 {
		t.Fatalf("clean exit rc = %d, want 0", rc)
	}
	if rc := dispatchRc(state(0), true); rc != 0 {
		t.Fatalf("clean exit with wall fired rc = %d, want 0 — a finished "+
			"dispatch must not be relabeled failed by a drain crossing the wall", rc)
	}
	if rc := dispatchRc(state(137), true); rc != 124 {
		t.Fatalf("wall-killed child rc = %d, want 124", rc)
	}
	if rc := dispatchRc(state(7), false); rc != 7 {
		t.Fatalf("own exit code rc = %d, want 7", rc)
	}
}

// readPidfile polls for the shim's recorded pid.
func readPidfile(t *testing.T, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		b, err := os.ReadFile(os.Getenv("PIDFILE"))
		if err == nil {
			pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
			if perr == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("pidfile %s never appeared", os.Getenv("PIDFILE"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTaskDispatchFastPathReturnsChildRc(t *testing.T) {
	repo := initFixtureRepo(t)
	taskWallShim(t, "exit 0\n")
	d := taskWallDriver(t, repo)
	start := time.Now()
	out, rc := d.taskDispatch("goal", "TASK-loop1")
	if rc != 0 {
		t.Fatalf("rc = %d (out %q), want 0", rc, out)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("fast-path dispatch took %s, want immediate return", elapsed)
	}
}

// TestTaskDispatchThreadsRunID pins issue #316 for the loopdriver: the
// dispatch must hand the iteration's run identity to `devagent task --id`,
// so the CLI's run lock, worktree, branch and deferred cleanup row all name
// this run instead of every run sharing the literal "TASK" key.
func TestTaskDispatchThreadsRunID(t *testing.T) {
	repo := initFixtureRepo(t)
	callsPath := filepath.Join(t.TempDir(), "calls.log")
	taskWallShim(t, "echo \"devagent $*\" >> \""+callsPath+"\"\nexit 0\n")
	d := taskWallDriver(t, repo)
	if _, rc := d.taskDispatch("goal", "TASK-loop3"); rc != 0 {
		t.Fatalf("dispatch rc = %d, want 0", rc)
	}
	calls, err := os.ReadFile(callsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--id TASK-loop3") {
		t.Fatalf("dispatch must carry the run id, calls:\n%s", calls)
	}
}

// TestRunTaskIDIsUniquePerRun pins the identity choice (issue #316): a
// per-iteration id with a random suffix — never the claimed queue task's id,
// which a live daemon-dispatched run may still hold the lock for, and never a
// fixed id, which a crashed iteration's leftover branch/worktree would pin.
func TestRunTaskIDIsUniquePerRun(t *testing.T) {
	re := regexp.MustCompile(`^TASK-loop5-[a-z0-9]+$`)
	first, second := runTaskID(5), runTaskID(5)
	if !re.MatchString(first) || !re.MatchString(second) {
		t.Fatalf("runTaskID = %q / %q, want TASK-loop5-<rand> shape", first, second)
	}
	if first == second {
		t.Fatalf("two dispatches of the same iteration share id %q", first)
	}
}

func TestTaskDispatchBoundedDrainKeepsChildRc(t *testing.T) {
	// A 10s wall with a 3s drain: a child that exits 0 well before the
	// deadline keeps rc 0 — the ProcessState-driven mapping (dispatchRc)
	// never lets the drain relabel it; the wall never fires here.
	// Pinned deterministically by TestDispatchRcMapping below.
	repo := initFixtureRepo(t)
	taskWallShim(t, "sleep 30 &\necho $! > \"$PIDFILE\"\necho done\nexit 0\n")
	killPidfileAtCleanup(t)
	d := taskWallDriver(t, repo)
	start := time.Now()
	out, rc := d.taskDispatch("goal", "TASK-loop1")
	if rc != 0 {
		t.Fatalf("rc = %d (out %q), want the child's own 0", rc, out)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("pipe-drain unbounded: %s", elapsed)
	}
}

// TestTaskDispatchWallKillsWedgedWorkerTree is the issue #273 regression:
// a devagent task that wedges past the outer wall must return (rc 124) and
// its whole process tree — including the worker session standing in as the
// grandchild — must be dead. Pre-fix, Wait blocked forever on the
// grandchild-held pipes and the grandchild outlived every anchor.
func TestTaskDispatchWallKillsWedgedWorkerTree(t *testing.T) {
	repo := initFixtureRepo(t)
	taskWallShim(t, "trap '' TERM\nsleep 600 &\necho $! > \"$PIDFILE\"\nwait\n")
	killPidfileAtCleanup(t)
	d := taskWallDriver(t, repo)

	resultCh := make(chan int, 1)
	go func() {
		_, rc := d.taskDispatch("goal", "TASK-loop1")
		resultCh <- rc
	}()

	grand := readPidfile(t, 30*time.Second)
	// The dispatch returns at wall + pipeDrainDelay at the latest (the
	// deadline kill plus the bounded pipe drain): the select must budget
	// both, plus slack, while still catching the pre-fix infinite block.
	select {
	case rc := <-resultCh:
		if rc != 124 {
			t.Fatalf("rc = %d, want 124 (GNU timeout convention)", rc)
		}
	case <-time.After(time.Duration(d.cfg.TaskTimeout)*time.Second + pipeDrainDelay + 10*time.Second):
		t.Fatal("dispatch never returned after the wall — the loop pin of issue #273")
	}
	// The tree must be gone: the group sweep reaped the orphaned session.
	deadline := time.Now().Add(3 * time.Second)
	for processAlive(grand) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive after the wall — orphaned worker session (issue #273)", grand)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestRunDevagentDrainBoundedOnHeldPipe is the issue #286 regression: a
// pane-run/gate helper exits 0 but a grandchild it forked keeps the stdout
// pipe open — os/exec's io.Copy goroutine never sees EOF and the teardown
// pinned forever (the driver logged its starvation halt and never left).
// The drain must return with the child's own output and its own rc 0: the
// drain cutoff is not a step failure.
func TestRunDevagentDrainBoundedOnHeldPipe(t *testing.T) {
	repo := initFixtureRepo(t)
	taskWallShim(t, "sleep 30 &\necho $! > \"$PIDFILE\"\necho helper-done\nexit 0\n")
	killPidfileAtCleanup(t)
	d := taskWallDriver(t, repo)
	start := time.Now()
	out, rc := d.runDevagent("gate-step")
	if rc != 0 {
		t.Fatalf("rc = %d (out %q), want the child's own 0 — the drain cutoff must not relabel it", rc, out)
	}
	if !strings.Contains(out, "helper-done") {
		t.Fatalf("out = %q, want the child's output captured before the drain cut", out)
	}
	if elapsed := time.Since(start); elapsed > pipeDrainDelay+10*time.Second {
		t.Fatalf("held-pipe teardown took %s — unbounded io.Copy (issue #286)", elapsed)
	}
}

// gateDriver builds a driver over the fixture repo with TestCmd overridden;
// an empty testCmd keeps the WithDefaults `go test ./...` fallback.
func gateDriver(t *testing.T, repo, testCmd string) *driver {
	t.Helper()
	cfg := LoopConfig{Repo: repo, TestCmd: testCmd}.WithDefaults()
	return &driver{cfg: cfg, stateDir: filepath.Join(cfg.Repo, ".selfbuild")}
}

// gateFake puts an executable logging shim on PATH: it appends its argv to
// $GATE_LOG and touches gate-ran-here in its cwd (cwd proof without
// resolving the /var → /private/var tempdir symlink).
func gateFake(t *testing.T, name, logPath string) {
	t.Helper()
	dir := fakeBinDir(t, map[string]string{
		name: `#!/bin/sh
echo "` + name + ` $*" >> "$GATE_LOG"
touch gate-ran-here
exit 0
`,
	})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("GATE_LOG", logPath)
}

func TestRunRepoTestsDefaultIsGoTest(t *testing.T) {
	repo := initFixtureRepo(t)
	logPath := filepath.Join(repo, "gate-calls.log")
	gateFake(t, "go", logPath)
	d := gateDriver(t, repo, "")
	if rc := d.runRepoTests(); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	// Default gate is exactly `go test ./...`, run inside cfg.Repo.
	assertFileContains(t, logPath, "go test ./...\n")
	if _, err := os.Stat(filepath.Join(repo, "gate-ran-here")); err != nil {
		t.Fatalf("gate did not run in cfg.Repo: %v", err)
	}
}

func TestRunRepoTestsWordSplitsTestCmd(t *testing.T) {
	repo := initFixtureRepo(t)
	logPath := filepath.Join(repo, "gate-calls.log")
	gateFake(t, "gate-fake", logPath)
	d := gateDriver(t, repo, "gate-fake alpha beta")
	if rc := d.runRepoTests(); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	assertFileContains(t, logPath, "gate-fake alpha beta\n")
}

func TestRunRepoTestsRcPlumbing(t *testing.T) {
	repo := initFixtureRepo(t)
	if rc := gateDriver(t, repo, "true").runRepoTests(); rc != 0 {
		t.Fatalf("passing gate rc = %d, want 0", rc)
	}
	if rc := gateDriver(t, repo, "false").runRepoTests(); rc != 1 {
		t.Fatalf("failing gate rc = %d, want 1", rc)
	}
	// A missing command gates as a failure (the FR-GO-16 ENOENT case),
	// never a crash.
	if rc := gateDriver(t, repo, "no-such-gate-bin").runRepoTests(); rc != 1 {
		t.Fatalf("missing-command gate rc = %d, want 1", rc)
	}
}
