package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/config"
)

// fakeCli serves herdr CLI replies from fixture JSON files, mirroring the
// vitest stub CLIs (test/herdr-sweep.test.ts): `pane list` from a pane-list
// fixture, `pane process-info` per pane, `agent list` from a roster fixture,
// and `workspace close` logged instead of executed.
//
// It also enforces the session-scoping contract: herdr resolves every
// subcommand inside one session, so a command that ships without `--session`
// answers for whatever session the binary defaults to and the caller reads a
// false negative as an answer. `pane process-info` is the case that mattered
// (2026-09-13: unscoped, so every live worker looked idle and the orphan class
// was unreachable); the fake fails loudly instead of serving it.
type fakeCli struct {
	t *testing.T
	// paneList: testdata path (or inline JSON) for `pane list`.
	paneList string
	// agentList: testdata path (or inline JSON) for `agent list`.
	agentList string
	// procInfo: pane id -> testdata path (or inline JSON) for `pane
	// process-info`.
	procInfo map[string]string
	// failList makes pane list/agent list exit 1 (server_not_running).
	failList bool
	// closed records workspace close invocations.
	closed []string
	// calls records every argv as invoked (joined with spaces, --session
	// included), in order.
	calls []string
	// dir is this package's directory at construction, so fixture paths stay
	// valid when a test chdirs to control the config the sweep resolves.
	dir string
}

func newFakeCli(t *testing.T) *fakeCli {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return &fakeCli{t: t, procInfo: map[string]string{}, dir: dir}
}

// called reports whether any recorded argv contains want.
func (f *fakeCli) called(want string) bool {
	for _, c := range f.calls {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// fixture reads a testdata file from this package's directory, captured when
// the fake was built: a sweep test that chdirs (to control which devagent.json
// the resolver reads) must still find its fixtures.
func (f *fakeCli) fixture(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "testdata", rel))
	if err != nil {
		f.t.Fatalf("read fixture %s: %v", rel, err)
	}
	return string(data)
}

func (f *fakeCli) HerdrCli(args []string, timeoutMs int) CliResult {
	raw := strings.Join(args, " ")
	session := ""
	cmd := args
	if len(args) >= 2 && args[0] == "--session" {
		session, cmd = args[1], args[2:]
	}
	f.calls = append(f.calls, raw)
	body := func(s string) string {
		if strings.HasPrefix(s, "testdata/") {
			return f.fixture(strings.TrimPrefix(s, "testdata/"))
		}
		return s
	}
	switch {
	case len(cmd) >= 2 && cmd[0] == "pane" && cmd[1] == "list":
		if f.failList {
			return CliResult{Code: 1, Stderr: "server_not_running"}
		}
		return CliResult{Code: 0, Stdout: body(f.paneList)}
	case len(cmd) >= 2 && cmd[0] == "agent" && cmd[1] == "list":
		if f.failList {
			return CliResult{Code: 1, Stderr: "server_not_running"}
		}
		return CliResult{Code: 0, Stdout: body(f.agentList)}
	case len(cmd) >= 2 && cmd[0] == "pane" && cmd[1] == "process-info":
		if session == "" {
			f.t.Errorf("pane process-info invoked without --session (argv %q): the probe would answer for herdr's default session", raw)
			return CliResult{Code: 1, Stderr: "fakeCli: unscoped process-info"}
		}
		pane := ""
		for i, a := range cmd {
			if a == "--pane" && i+1 < len(cmd) {
				pane = cmd[i+1]
			}
		}
		return CliResult{Code: 0, Stdout: body(f.procInfo[pane])}
	case len(cmd) >= 2 && cmd[0] == "workspace" && cmd[1] == "close":
		f.closed = append(f.closed, cmd[2])
		return CliResult{Code: 0, Stdout: `{"id":"x","result":{}}`}
	case len(cmd) >= 2 && cmd[0] == "workspace" && cmd[1] == "list":
		// The pane-list fixtures double as workspace_list answers so
		// EnsureHerdrServer short-circuits.
		return CliResult{Code: 0, Stdout: `{"id":"x","result":{"type":"workspace_list","workspaces":[]}}`}
	default:
		return CliResult{Code: 2, Stderr: "fakeCli: unsupported " + raw}
	}
}

// sweepEnabled is the default HerdrSweepSettings (TS { enabled: true,
// denySessions: [] }).
func sweepEnabled() *config.HerdrSweepSettings {
	s := config.HerdrSweepSettings{Enabled: true}
	return &s
}

func boolPtr(b bool) *bool { return &b }

// orphanSeams installs the DEVAGENT_SWEEP_* test seams. The collector stub is
// keyed by the pattern the sweep asks for (what production passes to
// `pgrep -f`), so a probe retargeted away from paneRunOwnerPattern misses and
// the test fails instead of being handed the old answer.
func orphanSeams(t *testing.T, ownerPids string, ancestry map[string][]string) {
	t.Helper()
	assertSeamPids(t, "owner pids", ownerPids)
	stub, err := json.Marshal(map[string]string{paneRunOwnerPattern: ownerPids})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVAGENT_SWEEP_OWNER_PIDS_JSON", string(stub))
	if ancestry != nil {
		for pid := range ancestry {
			assertSeamPids(t, "ancestry pid", pid)
		}
		data, err := json.Marshal(ancestry)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DEVAGENT_SWEEP_ANCESTRY_JSON", string(data))
	}
}

// assertSeamPids fails a seam that would resolve pids <= 1 the same way the
// real probe drops them (parsePidList): a stub handing out "0" or "bogus" is
// an answer, not a matched owner.
func assertSeamPids(t *testing.T, what, raw string) {
	t.Helper()
	for _, field := range strings.Fields(raw) {
		n, err := strconv.Atoi(field)
		if err != nil || n <= 1 {
			t.Fatalf("%s = %q: seam pids must be real pids (>1), matching parsePidList", what, raw)
		}
	}
}

// captureSeams stubs the per-pane fd table behind paneDispatchEvidence:
// pane id -> the paths lsof would report for the pane's fd 1/2. Keyed by pane
// like the process-info fixtures, so it can never depend on a fixture pid
// disagreeing with the process-info reply for the same pane.
func captureSeams(t *testing.T, fds map[string]string) {
	t.Helper()
	for paneID := range fds {
		if !strings.Contains(paneID, ":") {
			t.Fatalf("capture seam key %q is not a pane id", paneID)
		}
	}
	data, err := json.Marshal(fds)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVAGENT_SWEEP_PANE_CAPTURE_JSON", string(data))
}

// ---------- Sweep safety (FR-VIS-07) ----------

func TestSweepNeverClosesLiveWorkerEvenWhenIdle(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SWEEP", "")
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty", got)
	}
}

func TestSweepClosesIdleWorktreePaneWithIdleShell(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].PaneID != "wX:p1" || got[0].Reason != "agent-idle" {
		t.Fatalf("stale = %+v, want [wX:p1 agent-idle]", got)
	}
	if got[0].WorkspaceID != "wX" || got[0].Label != "TASK-abc-a1" || got[0].AgentStatus != "idle" {
		t.Fatalf("row fields = %+v", got[0])
	}
}

func TestSweepNeverListsOperatorScratchPanes(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-scratch.json"
	cli.procInfo["wV:p1"] = "testdata/sweep/process-info-zsh.json"
	cli.procInfo["wV:p2"] = "testdata/sweep/process-info-zsh.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty", got)
	}
}

func TestSweepClosesAgentlessLeftoverInWorktree(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-agentless.json"
	cli.procInfo["wT:p1"] = "testdata/sweep/process-info-zsh.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "no-agent" {
		t.Fatalf("stale = %+v, want [no-agent]", got)
	}
}

// ---------- Orphaned-driver sweep (2026-09-07 orphan-pane class) ----------

func TestOrphanDefaultSweepLeavesLiveWorkerAlone(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		"4242": {"timeout 7200 devagent-go task"},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty (no --orphans)", got)
	}
}

func TestOrphanClosesLiveWorkerDetachedFromDriver(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		// Owner CLI alive but its ancestry has no loop driver (driver died).
		"4242": {"timeout 7200 devagent-go task", "launchd"},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" || got[0].PaneID != "wX:p1" {
		t.Fatalf("stale = %+v, want [wX:p1 orphaned-driver]", got)
	}
}

func TestOrphanSparesLiveWorkerWithLoopDriverAncestor(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		"4242": {
			"bash /repo/scripts/selfbuild-loop.sh",
			"timeout 7200 devagent-go task",
		},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty (live driver owns it)", got)
	}
}

func TestOrphanSparesLiveWorkerWithGoDriverAncestor(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		// The production driver after #205 is `devagent-go loop` (make
		// loop-start); a pane owned by it is live, not orphaned.
		"4242": {
			"./devagent-go loop",
			"timeout 7200 devagent-go task",
		},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty (live Go driver owns it)", got)
	}
}

func TestOrphanClosesLiveWorkerWithNoOwnerCLI(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil)
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" {
		t.Fatalf("stale = %+v, want [orphaned-driver]", got)
	}
}

// Repairing the orphan class also made its probes load-bearing: a pgrep or ps
// that cannot run (absent binary, the 5s cap, a rejected pattern) used to read
// as "no collector" and would close every live worker in the session on a host
// that cannot inspect its own process table. An unanswered probe spares; only an
// answer reaps.
func TestOrphanClassGoesInertWhenProbesCannotAnswer(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"

	// No seam entry for the pattern the sweep asks for = no answer at all,
	// which must not be confused with the probe answering "no collector".
	t.Setenv("DEVAGENT_SWEEP_OWNER_PIDS_JSON", `{"some-other-probe":"4242\n"}`)
	if got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("stale = %+v, want empty (unanswered owner probe spares)", got)
	}

	// Collector found, ancestry walk never completed: still no evidence.
	orphanSeams(t, "4242\n", map[string][]string{})
	if got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("stale = %+v, want empty (unanswered ancestry spares)", got)
	}

	// The same walk completed with no driver above it IS an answer, and it reaps.
	orphanSeams(t, "4242\n", map[string][]string{"4242": {}})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" {
		t.Fatalf("stale = %+v, want [orphaned-driver] on a completed driverless walk", got)
	}
}

func TestSweepOrphansConfigArmsClassWithoutCallerFlag(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil) // no pane-run owner at all
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("default = %v, want empty", got)
	}
	sweep := config.HerdrSweepSettings{Enabled: true, Orphans: boolPtr(true)}
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" {
		t.Fatalf("config-armed = %+v, want [orphaned-driver]", got)
	}
}

// ---------- Orphaned dispatches outside the worktrees (2026-09-13) ----------
//
// The loop's research/PO phases dispatch panes in the driver's own checkout
// (FR-VIS-06), where cwd proves nothing about who started the pane — an
// operator's own omp sits in the same directory. Those panes are sweepable only
// with dispatch evidence: the capture contract held on the worker's own
// stdout/stderr, plus a run nobody collects any more.

// mainCheckoutCli: one devagent-dispatched pane (wM:p1, worker pid 42 with the
// capture contract) beside one hand-run operator pane (wS:p1, worker pid 43
// without it), both live, both in the main checkout.
func mainCheckoutCli(t *testing.T) *fakeCli {
	t.Helper()
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-main-checkout.json"
	cli.procInfo["wM:p1"] = "testdata/sweep/process-info-omp.json"
	cli.procInfo["wS:p1"] = "testdata/sweep/process-info-omp-handrun.json"
	return cli
}

const mainCheckoutCapture = "/private/tmp/devagent-herdr-4242/out /private/tmp/devagent-herdr-4242/err"

func TestOrphanReapsMainCheckoutDispatchAndSparesHandRun(t *testing.T) {
	cli := mainCheckoutCli(t)
	orphanSeams(t, "", nil) // no dispatcher alive -> nobody collects the run
	captureSeams(t, map[string]string{"wM:p1": mainCheckoutCapture})
	opts := SweepOptions{Orphans: true, Sweep: sweepEnabled()}
	got := FindStalePanes(cli, "devagent", opts)
	if len(got) != 1 || got[0].PaneID != "wM:p1" || got[0].Reason != "orphaned-driver" {
		t.Fatalf("stale = %+v, want [wM:p1 orphaned-driver]", got)
	}
	SweepStalePanes(cli, "devagent", opts, false)
	if len(cli.closed) != 1 || cli.closed[0] != "wM" {
		t.Fatalf("closed = %v, want [wM] (the hand-run pane stays open)", cli.closed)
	}
}

func TestOrphanMainCheckoutSparedWhileDispatcherRuns(t *testing.T) {
	cli := mainCheckoutCli(t)
	// A dispatcher still hanging off the live driver is collecting the run —
	// the sweep may not assert orphanhood, however stale the pane looks.
	orphanSeams(t, "4242\n", map[string][]string{"4242": {"./devagent-go loop --max-iterations 0"}})
	captureSeams(t, map[string]string{"wM:p1": mainCheckoutCapture})
	if got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("stale = %+v, want empty (live dispatcher collects it)", got)
	}
}

func TestOrphanMainCheckoutWithoutDispatchEvidenceIsSpared(t *testing.T) {
	cli := mainCheckoutCli(t)
	orphanSeams(t, "", nil)
	captureSeams(t, map[string]string{}) // neither worker holds the contract
	if got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("stale = %+v, want empty (no pane proves a devagent dispatch)", got)
	}
}

func TestDefaultSweepNeverListsMainCheckoutPanes(t *testing.T) {
	cli := mainCheckoutCli(t)
	orphanSeams(t, "", nil)
	captureSeams(t, map[string]string{"wM:p1": mainCheckoutCapture})
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("stale = %+v, want empty (orphan class unarmed)", got)
	}
}

// The kill switch has to cover the widened class: DEVAGENT_HERDR_SWEEP_ORPHANS=0
// reins in a driver that always passes --orphans, with no code or config edit.
func TestEnvOrphansKillSwitchDisarmsMainCheckoutClass(t *testing.T) {
	cli := mainCheckoutCli(t) // before the chdir: fixtures resolve from the package dir
	// No devagent.json in this dir: the resolver falls back to defaults with
	// the env override on top, which is exactly what the driver sees.
	chdir(t, t.TempDir())
	t.Setenv("DEVAGENT_HERDR_SWEEP", "")
	t.Setenv("DEVAGENT_HERDR_SWEEP_ORPHANS", "0")
	orphanSeams(t, "", nil)
	captureSeams(t, map[string]string{"wM:p1": mainCheckoutCapture})
	if got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true}); len(got) != 0 {
		t.Fatalf("stale = %+v, want empty (orphans killed by env)", got)
	}
}

// The dispatch discriminator is a file-descriptor shape, so prove it against a
// real process and the real lsof probe rather than only against
// DEVAGENT_SWEEP_PANE_CAPTURE_JSON: a child with stdout+stderr on
// `<tmp>/devagent-herdr-<n>/out` is dispatch evidence, and the same child
// writing anywhere else is not.
func TestProcFdsCaptureSeesRealCaptureContract(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof unavailable")
	}
	captureDir, err := os.MkdirTemp("", "devagent-herdr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(captureDir) })
	captureOut, err := os.Create(filepath.Join(captureDir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = captureOut.Close() }()
	child := startWriter(t, captureOut)

	if got := procFdsCapture(child.Process.Pid); !captureFileRe.MatchString(got) {
		t.Fatalf("fd probe = %q, want the capture path %s", got, captureDir)
	}

	plainDir, err := os.MkdirTemp("", "herdr-scratch-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(plainDir) })
	plainOut, err := os.Create(filepath.Join(plainDir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plainOut.Close() }()
	plain := startWriter(t, plainOut)
	if got := procFdsCapture(plain.Process.Pid); captureFileRe.MatchString(got) {
		t.Fatalf("fd probe = %q, want no capture contract", got)
	}
}

// startWriter runs a live child with both stdout and stderr on w (the fd pair
// the sweep probes).
func startWriter(t *testing.T, w *os.File) *exec.Cmd {
	t.Helper()
	child := exec.Command("/bin/sh", "-c", "sleep 10")
	child.Stdout, child.Stderr = w, w
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	return child
}

// The owner probe is a command-line shape, and the 2026-09-13 failure was a
// shape no real process ever carries: `herdr pane run <pane>` types the script
// into the pane's shell and exits, so the pattern it searched for matched
// nothing and every pane read as orphaned-by-absence. Pin the pattern against
// the argv the dispatchers actually build (internal/loopdriver/dispatch.go:
// `task --prompt …`, `pane-run --cwd … -- <bin> …`, both behind the bash
// driver's `timeout N` wrapper), and against the shapes that must NOT match —
// the transient pane-run client and a worker whose quoted prompt names devagent
// commands (the argv0 anchor is what stops it impersonating its own collector).
func TestPaneRunOwnerPatternMatchesRealDispatchShapes(t *testing.T) {
	re := regexp.MustCompile(paneRunOwnerPattern)
	mustMatch := []string{
		"/usr/local/bin/devagent-go task --prompt Goal: land #1 --repo /repo --worker omp",
		"devagent task --prompt hi",
		"timeout 5400 /usr/local/bin/devagent-go task --prompt hi --auto-pr",
		"/tmp/x/devagent pane-run --cwd /repo --timeout 900 --out /tmp/o --err /tmp/e --done /tmp/d -- omp -p hi",
	}
	for _, argv := range mustMatch {
		if !re.MatchString(argv) {
			t.Errorf("owner pattern missed a real dispatch: %q", argv)
		}
	}
	mustNotMatch := []string{
		// the pre-fix target: it never exists while the pane runs.
		"herdr --session devagent pane run wX:p1 echo hi",
		// a worker quoting devagent commands inside its prompt.
		"omp -p run devagent task --prompt x",
		// the sweep itself and the driver that runs it must not spare panes.
		"/usr/local/bin/devagent-go herdr-sweep --orphans",
		"timeout 5400 /usr/local/bin/devagent-go loop --max-iterations 4",
		"/usr/local/bin/devagent-review task --prompt x",
	}
	for _, argv := range mustNotMatch {
		if re.MatchString(argv) {
			t.Errorf("owner pattern matched a non-collector: %q", argv)
		}
	}
}

// The answered/unanswered split lives in the seam-free branch, so the stubs
// cannot prove it: run the real `pgrep` and pin its own exit codes. "No process
// matches" is evidence a collector is absent (the sweep may reap); a probe that
// could not inspect the process table is not (it must spare).
func TestPsPidsMatchingAnswersOnlyWhenPgrepAnswers(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep unavailable")
	}
	if pids, answered := psPidsMatching(`^zzq-no-such-process-\d{9}$`); !answered || len(pids) != 0 {
		t.Fatalf("no-match probe = %v (answered=%v), want an answered empty set", pids, answered)
	}
	// An unclosed bracket class is a regex the probe cannot run at all: exit 2,
	// which must never be reported as "answered, nothing found".
	if pids, answered := psPidsMatching(`^[a-`); answered || len(pids) != 0 {
		t.Fatalf("failed probe = %v (answered=%v), want no answer", pids, answered)
	}
}

// ---------- Deny toggle (FR-VIS-10 / Q23) ----------

func TestEnvSweepToggleStopsSweepBeforeListing(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	t.Setenv("DEVAGENT_HERDR_SWEEP", "0")
	got := FindStalePanes(cli, "devagent", SweepOptions{})
	if len(got) != 0 {
		t.Fatalf("find = %v, want empty", got)
	}
	if got := SweepStalePanes(cli, "devagent", SweepOptions{}, false); len(got) != 0 {
		t.Fatalf("sweep = %v, want empty", got)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("sweep listed panes despite toggle: %v", cli.calls)
	}
}

func TestConfigSweepEnabledFalseIsSameSwitch(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	sweep := config.HerdrSweepSettings{Enabled: false}
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep}); len(got) != 0 {
		t.Fatalf("stale = %v, want empty", got)
	}
}

func TestDeniedSessionNeverListedOthersStillSweep(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	sweep := config.HerdrSweepSettings{Enabled: true, DenySessions: []string{"devagent"}}
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep}); len(got) != 0 {
		t.Fatalf("devagent = %v, want empty", got)
	}
	if got := SweepStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep}, false); len(got) != 0 {
		t.Fatalf("sweep devagent = %v, want empty", got)
	}
	if got := FindStalePanes(cli, "devagent-ci", SweepOptions{Sweep: &sweep}); len(got) != 1 || got[0].PaneID != "wX:p1" {
		t.Fatalf("devagent-ci = %+v, want [wX:p1]", got)
	}
}

func TestSweepDenyReasonNamesBound(t *testing.T) {
	cases := []struct {
		session string
		sweep   config.HerdrSweepSettings
		want    string
	}{
		{"devagent", config.HerdrSweepSettings{Enabled: true}, ""},
		{"devagent", config.HerdrSweepSettings{Enabled: false, DenySessions: []string{"devagent"}}, "disabled"},
		{"devagent", config.HerdrSweepSettings{Enabled: true, DenySessions: []string{"devagent", "other"}}, "session-denied"},
		{"other", config.HerdrSweepSettings{Enabled: true, DenySessions: []string{"devagent"}}, ""},
	}
	for _, tc := range cases {
		if got := SweepDenyReason(tc.session, tc.sweep); got != tc.want {
			t.Errorf("SweepDenyReason(%q) = %q, want %q", tc.session, got, tc.want)
		}
	}
}

// ---------- Operator-attach exemption (FR-VIS-10) ----------

func TestRosterRunningPaneSparedAndReported(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	found := FindStalePanes(cli, "devagent", SweepOptions{})
	if len(found) != 1 || found[0].Reason != SweepReasonOperatorAttached {
		t.Fatalf("found = %+v, want [operator-attached]", found)
	}
	swept := SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(swept) != 1 || swept[0].Reason != SweepReasonOperatorAttached {
		t.Fatalf("swept = %+v, want [operator-attached]", swept)
	}
	if len(cli.closed) != 0 {
		t.Fatalf("closed = %v, want none", cli.closed)
	}
}

func TestNonLiveRosterDoesNotExemptPane(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-idle.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	if got := FindStalePanes(cli, "devagent", SweepOptions{}); len(got) != 1 || got[0].Reason != "agent-idle" {
		t.Fatalf("found = %+v, want [agent-idle]", got)
	}
	SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(cli.closed) != 1 || cli.closed[0] != "wX" {
		t.Fatalf("closed = %v, want [wX]", cli.closed)
	}
}

func TestOperatorAttachedEnvSparesWholeSession(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-two-idle.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	cli.procInfo["wY:p1"] = "testdata/sweep/process-info-zsh.json"
	t.Setenv(PaneEnvOpAttach, "1")
	swept := SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(swept) != 2 {
		t.Fatalf("swept = %+v, want 2 spared rows", swept)
	}
	for _, s := range swept {
		if s.Reason != SweepReasonOperatorAttached {
			t.Fatalf("row reason = %q, want operator-attached", s.Reason)
		}
	}
	if len(cli.closed) != 0 {
		t.Fatalf("closed = %v, want none", cli.closed)
	}
	// A false-ish flag value is not an exemption.
	t.Setenv(PaneEnvOpAttach, "0")
	if got := FindStalePanes(cli, "devagent", SweepOptions{}); len(got) != 2 || got[0].Reason != "agent-idle" || got[1].Reason != "agent-idle" {
		t.Fatalf("found with =0 = %+v, want [agent-idle agent-idle]", got)
	}
}

// FR-VIS-10's exemption is herdr's own attach detection, surfaced through
// PANE_ENV_OP_ATTACH: while an operator is at the wheel nothing in the session
// is sweepable, orphan evidence included.
func TestEnvOperatorAttachOutranksOrphanEvidence(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil) // owner gone -> orphan candidate
	t.Setenv(PaneEnvOpAttach, "1")
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != SweepReasonOperatorAttached {
		t.Fatalf("stale = %+v, want [operator-attached]", got)
	}
	SweepStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()}, false)
	if len(cli.closed) != 0 {
		t.Fatalf("closed = %v, want none (attached operator never closes)", cli.closed)
	}
}

// Companion: the FR-VIS-02 roster's "running" state does NOT outrank orphan
// evidence. Since #317 it is derived from the same foreground-worker probe, so
// sparing on it first reported every orphan as `operator-attached` and the
// class could never fire (2026-09-13).
func TestOrphanEvidenceOutranksRosterRunningSpare(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil)
	opts := SweepOptions{Orphans: true, Sweep: sweepEnabled()}
	got := FindStalePanes(cli, "devagent", opts)
	if len(got) != 1 || got[0].Reason != "orphaned-driver" || got[0].PaneID != "wX:p1" {
		t.Fatalf("stale = %+v, want [wX:p1 orphaned-driver]", got)
	}
	SweepStalePanes(cli, "devagent", opts, false)
	if len(cli.closed) != 1 || cli.closed[0] != "wX" {
		t.Fatalf("closed = %v, want [wX]", cli.closed)
	}
}

// ---------- SweepStalePanes close behavior + dry-run ----------

func TestSweepStalePanesClosesAndDryRunDoesNot(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	got := SweepStalePanes(cli, "devagent", SweepOptions{}, true)
	if len(got) != 1 || got[0].Reason != "agent-idle" {
		t.Fatalf("dry-run = %+v", got)
	}
	if len(cli.closed) != 0 {
		t.Fatalf("dry-run closed %v", cli.closed)
	}
	SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(cli.closed) != 1 || cli.closed[0] != "wX" {
		t.Fatalf("closed = %v, want [wX]", cli.closed)
	}
}

// ---------- FR-VIS-02 roster / FR-VIS-03 attach ----------

func TestListSessionPanesMapsRosterRows(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	panes := ListSessionPanes(cli, "testsession")
	if len(panes) != 3 {
		t.Fatalf("panes = %d rows, want 3", len(panes))
	}
	var abc, old, shell *SessionPaneInfo
	for i := range panes {
		switch panes[i].PaneID {
		case "pane-abc":
			abc = &panes[i]
		case "pane-old":
			old = &panes[i]
		case "pane-shell":
			shell = &panes[i]
		}
	}
	if abc == nil || old == nil || shell == nil {
		t.Fatalf("missing rows: %+v", panes)
	}
	// Worker = label prefix before the FIRST '-a' ('TASK-abc-a1' -> 'TASK');
	// contract-literal parsing, even when the task id itself is hyphenated.
	if abc.TaskID != "TASK-abc" || abc.Worker != "TASK" || abc.Role != "worker" ||
		abc.State != "running" || abc.AgentStatus != "working" ||
		abc.StartedAt != "2026-09-04T10:00:00Z" || abc.WorkspaceID != "ws-abc" {
		t.Fatalf("abc = %+v", abc)
	}
	if !strings.Contains(abc.Cwd, ".devagent-worktrees") {
		t.Fatalf("abc cwd = %q", abc.Cwd)
	}
	if old.TaskID != "TASK-old" {
		t.Fatalf("old taskId = %q", old.TaskID)
	}
	// Scratch (non-worktree) pane: no task semantics -> empty taskId, worker
	// falls back to 'unknown' (label carries no attempt suffix).
	if shell.TaskID != "" || shell.Worker != "unknown" || shell.State != "idle" {
		t.Fatalf("shell = %+v", shell)
	}
}

// Regression (2026-09-11, #317): headless worker invocations (`omp -p --mode
// json` via pane-run) never advance herdr's agent_status past "idle", so the
// TUI rendered LIVE workers as "● idle". A pane whose foreground process is a
// worker binary must map to "running" regardless of agent_status; an idle pane
// at its shell keeps the status-derived state.
func TestListSessionPanesUpgradesLiveHeadlessWorker(t *testing.T) {
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-headless-worker.json"
	cli.procInfo["pane-live"] = "testdata/sweep/process-info-omp.json"
	cli.procInfo["pane-dead"] = "testdata/sweep/process-info-zsh.json"
	panes := ListSessionPanes(cli, "")
	if len(panes) != 2 {
		t.Fatalf("panes = %d rows, want 2", len(panes))
	}
	var live, dead *SessionPaneInfo
	for i := range panes {
		switch panes[i].PaneID {
		case "pane-live":
			live = &panes[i]
		case "pane-dead":
			dead = &panes[i]
		}
	}
	if live == nil || dead == nil {
		t.Fatalf("missing rows: %+v", panes)
	}
	// The exact false-idle shape: agent_status "idle", live omp foreground.
	if live.AgentStatus != "idle" || live.State != "running" {
		t.Fatalf("live = agentStatus %q state %q, want idle/running", live.AgentStatus, live.State)
	}
	// Same idle status, shell foreground: stays status-derived.
	if dead.State != "stale" {
		t.Fatalf("dead = state %q, want stale", dead.State)
	}
	// process-info is only paid for on idle/unknown rows, and the working
	// rows of the two-pane fixture never trigger a probe.
	probed := false
	for _, c := range cli.calls {
		if strings.Contains(c, "process-info") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("no process-info probe recorded: %v", cli.calls)
	}
}

func TestListSessionPanesStripsRecoverySuffix(t *testing.T) {
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-recovery-suffix.json"
	panes := ListSessionPanes(cli, "")
	if len(panes) != 1 || panes[0].TaskID != "TASK-x" {
		t.Fatalf("panes = %+v, want TASK-x", panes)
	}
}

func TestListSessionPanesFailsClosed(t *testing.T) {
	cli := newFakeCli(t)
	cli.failList = true
	if got := ListSessionPanes(cli, ""); got != nil {
		t.Fatalf("failed CLI = %v, want nil", got)
	}
	cli.failList = false
	cli.agentList = "not json at all"
	if got := ListSessionPanes(cli, ""); len(got) != 0 {
		t.Fatalf("junk stdout = %v, want empty", got)
	}
}

func TestAttachCommandForRosteredTask(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	if got := AttachCommandFor(cli, "TASK-abc", ""); got != "herdr --session testsession agent attach pane-abc" {
		t.Fatalf("attach = %q", got)
	}
	if got := AttachCommandFor(cli, "TASK-missing", ""); got != "" {
		t.Fatalf("missing = %q, want empty", got)
	}
}

func TestOperatorAttachTraceLiveOnly(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	if !OperatorAttachTrace(cli, "TASK-abc", "") {
		t.Error("TASK-abc should be live")
	}
	if OperatorAttachTrace(cli, "TASK-old", "") {
		t.Error("TASK-old is stale, not live")
	}
	if OperatorAttachTrace(cli, "TASK-missing", "") {
		t.Error("TASK-missing should be absent")
	}
}

// ---------- Sweep render (herdr-sweep stdout contract) ----------

func TestRunHerdrSweepRenderLines(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", true, false) })
	want := "[stale] wX:p1 (TASK-abc-a1) status=idle reason=agent-idle\n" +
		"1 pane(s) found in session \"devagent\"\n"
	if out != want {
		t.Fatalf("dry-run stdout =\n%q\nwant\n%q", out, want)
	}
	out = captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", false, false) })
	want = "[closed] wX:p1 (TASK-abc-a1) status=idle reason=agent-idle\n" +
		"1 pane(s) closed in session \"devagent\"\n"
	if out != want {
		t.Fatalf("sweep stdout =\n%q\nwant\n%q", out, want)
	}
}

func TestRunHerdrSweepDisabledRendersBound(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SWEEP", "0")
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", false, false) })
	want := "[devagent] sweep disabled (herdr.sweep.enabled / DEVAGENT_HERDR_SWEEP=0)\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunHerdrSweepSparedLines(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", true, false) })
	want := "[spared] wX:p1 (TASK-abc-a1) status=idle reason=operator-attached\n" +
		"0 pane(s) found in session \"devagent\"; 1 spared (operator-attached)\n"
	if out != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", out, want)
	}
}

func TestRunHerdrSweepNoStalePanes(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-scratch.json"
	cli.procInfo["wV:p1"] = "testdata/sweep/process-info-zsh.json"
	cli.procInfo["wV:p2"] = "testdata/sweep/process-info-zsh.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", false, false) })
	want := "[devagent] no stale panes\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

// ---------- sessions / attach render (FR-VIS-02) ----------

func TestRunSessionsTableAndJSON(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	out := captureStdout(t, func() { _ = RunSessions(cli, false) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("table = %q", out)
	}
	if !strings.HasPrefix(lines[0], "PANE") || !strings.Contains(lines[0], "ATTACH") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "pane-abc") || !strings.Contains(lines[1], "herdr --session testsession agent attach pane-abc") {
		t.Fatalf("row1 = %q", lines[1])
	}
	if !strings.Contains(lines[3], "pane-shell") || !strings.Contains(lines[3], "-") {
		t.Fatalf("row3 = %q", lines[3])
	}
	jsonOut := captureStdout(t, func() { _ = RunSessions(cli, true) })
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("sessions --json not valid JSON: %v\n%s", err, jsonOut)
	}
	if len(parsed) != 3 || parsed[0]["paneId"] != "pane-abc" || parsed[0]["taskId"] != "TASK-abc" {
		t.Fatalf("json rows = %+v", parsed)
	}
	// Key order is the declared TS contract.
	if !strings.Contains(jsonOut, `"taskId"`) || strings.Index(jsonOut, `"taskId"`) > strings.Index(jsonOut, `"paneId"`) {
		t.Fatalf("json key order wrong: %s", jsonOut)
	}
}

func TestRunSessionsEmpty(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = `{"id":"x","result":{"agents":[]}}`
	out := captureStdout(t, func() { _ = RunSessions(cli, false) })
	want := "no worker panes in session \"testsession\"\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunAttachUnknownTask(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	repo := t.TempDir()
	code := RunAttach(cli, repo, "TASK-missing", false)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestRunAttachRecordsLedgerRow(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	repo := t.TempDir()
	code := RunAttach(cli, repo, "TASK-abc", false)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &row); err != nil {
		t.Fatal(err)
	}
	if row["event"] != "operator-attached" || row["taskId"] != "TASK-abc" || row["paneId"] != "pane-abc" || row["session"] != "testsession" || row["kind"] != "event" || row["attempt"] != float64(1) {
		t.Fatalf("row = %v", row)
	}
}

// ---------- pane-run CLI contract ----------

func TestRunPaneRunUnavailableExits3(t *testing.T) {
	// Fast-fail the detached server spawn: a nonexistent DEVAGENT_HERDR_BIN
	// errors synchronously (the TS 'error'-event path) and EnsureHerdrServer
	// returns false without polling 24x500ms.
	t.Setenv("DEVAGENT_HERDR_BIN", filepath.Join(t.TempDir(), "no-such-herdr"))
	cli := newFakeCli(t)
	// EnsureHerdrServer fails: workspace list reports down.
	failing := &downCli{inner: cli}
	dir := t.TempDir()
	code := RunPaneRun(failing, dir, 5, filepath.Join(dir, "out"), filepath.Join(dir, "err"), filepath.Join(dir, "done"), "", "echo", []string{"hi"})
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "done")); !os.IsNotExist(err) {
		t.Fatalf("done marker should be absent (caller falls back), err = %v", err)
	}
}

// downCli fails every workspace list (server down) but records everything.
type downCli struct{ inner CliRunner }

func (d *downCli) HerdrCli(args []string, timeoutMs int) CliResult {
	if len(args) > 1 && args[0] == "--session" {
		args = args[2:]
	}
	if len(args) >= 2 && args[0] == "workspace" && args[1] == "list" {
		return CliResult{Code: 1, Stderr: "server_not_running"}
	}
	return d.inner.HerdrCli(args, timeoutMs)
}

func TestRunPaneRunWritesCaptureContract(t *testing.T) {
	cli := &scriptRunnerCli{t: t}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out")
	errPath := filepath.Join(dir, "err")
	donePath := filepath.Join(dir, "done")
	code := RunPaneRun(cli, dir, 10, outPath, errPath, donePath, "", "echo", []string{"hello"})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if got := readTrim(t, outPath); got != "hello" {
		t.Fatalf("stdout capture = %q, want hello", got)
	}
	if got := readTrim(t, donePath); got != "0" {
		t.Fatalf("done marker = %q, want 0", got)
	}
	// env.sh must be removed by the pane script (secrets never in scrollback).
	if cli.envFileKept {
		t.Fatal("env.sh survived the pane script")
	}
}

func readTrim(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// ---------- Test utils ----------

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}

var _ = fmt.Sprintf  // silence fmt in builds without tests using it
var _ = strconv.Itoa // silence strconv
