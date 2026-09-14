package commands

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/prdintake"
	"github.com/FreePeak/devagent/internal/queue"
)

// upProbe records the side effects `up` would have had, so a test asserts the
// plan rather than the processes it started.
type upProbe struct {
	launched []Child
	pids     []int
}

func upFixture(t *testing.T) (string, UpOptions, *upProbe) {
	t.Helper()
	repo := t.TempDir()
	probe := &upProbe{}
	return repo, UpOptions{
		RepoPath: repo,
		Checks: func(InitOptions) (InitResult, error) {
			return InitResult{
				OK:         true,
				ConfigPath: filepath.Join(repo, "devagent.json"),
				Checks:     []PrereqCheck{{Name: "git", OK: true, Required: true}},
			}, nil
		},
		Intake: func(o prdintake.Options) (prdintake.Report, error) {
			return prdintake.Report{
				PRDPath:    prdintake.DefaultPRDPath(o.RepoPath),
				Open:       1,
				Queued:     []prdintake.Item{{ID: "PRD-deadbeef", Title: "ship the thing", Line: 9}},
				QueueDepth: 1,
			}, nil
		},
		Spawn: func(c Child) (ChildHandle, error) {
			pid := 4242 + len(probe.launched) + 1
			if c.Name == "loop" {
				// A real driver proves itself by taking the loop lock and
				// writing its heartbeat; the fake has to, or `up`'s health
				// receipt (verifyDriver) correctly refuses the start.
				proveDriverAlive(t, repo, pid)
			}
			probe.launched = append(probe.launched, c)
			probe.pids = append(probe.pids, pid)
			// The real spawn records a pid file; `down` reads those, so the
			// fake has to as well or the two halves never line up.
			if c.PidFile != "" {
				writePidFile(c.PidFile, pid)
			}
			return ChildHandle{Pid: pid}, nil
		},
		Alive: func(pid int) bool {
			for _, p := range probe.pids {
				if p == pid {
					return true
				}
			}
			return false
		},
		PortOpen:     func(string) bool { return false },
		SelfExe:      func() string { return "/tmp/devagent-go" },
		HealthWindow: 2 * time.Second,
		Sleep:        func(time.Duration) {},
	}, probe
}

func (p *upProbe) child(t *testing.T, name string) Child {
	t.Helper()
	for _, c := range p.launched {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q child launched (launched %v)", name, names(p.launched))
	return Child{}
}

// TestUpStartsDriverSeededAndDaemonized: the point of `up` is that the
// operator types one word. Checks pass, the PRD lane is seeded, and the loop
// child launches from THIS executable with its own SELFBUILD_DEVAGENT_BIN,
// detached, logging where the report says.
func TestUpStartsDriverSeededAndDaemonized(t *testing.T) {
	repo, opts, probe := upFixture(t)

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("up failed: %+v", res.Steps)
	}
	if got := names(probe.launched); strings.Join(got, ",") != "daemon,loop" {
		t.Errorf("launched %v, want the daemon then the driver", got)
	}
	loop := probe.child(t, "loop")
	if loop.Bin != "/tmp/devagent-go" || strings.Join(loop.Args, " ") != "loop" {
		t.Errorf("loop child = %s %v, want the running executable running loop", loop.Bin, loop.Args)
	}
	if !loop.Detach {
		t.Errorf("the loop child must detach so `up` returns")
	}
	if loop.Dir != repo {
		t.Errorf("loop child dir = %q, want the repo", loop.Dir)
	}
	// The historical failure this replaces: the driver shells out to a bare
	// `devagent` that a Go-only checkout never installed on PATH.
	if len(loop.Env) != 1 || loop.Env[0] != "SELFBUILD_DEVAGENT_BIN=/tmp/devagent-go" {
		t.Errorf("loop env = %v, want the driver pinned to this executable", loop.Env)
	}
	if loop.LogPath != filepath.Join(repo, ".selfbuild", "logs", "driver.log") {
		t.Errorf("loop log = %q", loop.LogPath)
	}
	if loop.PidFile == "" {
		t.Errorf("the driver must record a pid so `down` can stop it")
	}
	if res.Intake == nil || len(res.Intake.Queued) != 1 {
		t.Errorf("intake report missing: %+v", res.Intake)
	}
	if res.NextAction == "" {
		t.Errorf("a successful up must name the next action")
	}
}

// TestUpIsIdempotentAgainstALiveDriver: a second `up` must not start a second
// driver — two concurrent drivers sharing one ledger and one state branch is
// the race class that has killed this loop. The lock holder wins even with no
// pid file of ours on disk.
func TestUpIsIdempotentAgainstALiveDriver(t *testing.T) {
	repo, opts, probe := upFixture(t)
	lockDir := filepath.Join(repo, ".selfbuild", "loop.lock.d")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte("777\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Alive = func(pid int) bool { return pid == 777 }

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range probe.launched {
		if c.Name == "loop" {
			t.Fatalf("a live driver was double-started: %v", names(probe.launched))
		}
	}
	if res.PIDs["loop"] != 777 {
		t.Errorf("report loop pid = %d, want the live holder 777", res.PIDs["loop"])
	}
	var step *UpStep
	for i, s := range res.Steps {
		if s.Name == "loop" {
			step = &res.Steps[i]
		}
	}
	if step == nil {
		t.Fatalf("no loop step reported: %+v", res.Steps)
	}
	if !step.Skipped || !strings.Contains(step.Detail, "already running") {
		t.Errorf("loop step = %+v, want a skipped `already running`", step)
	}
}

// TestUpRefusesBrokenPrerequisites: starting a driver that cannot dispatch a
// worker only burns tokens. `up` stops and says what to install;
// --skip-checks is the only way past it.
func TestUpRefusesBrokenPrerequisites(t *testing.T) {
	_, opts, probe := upFixture(t)
	opts.Checks = func(InitOptions) (InitResult, error) {
		return InitResult{
			OK: false,
			Checks: []PrereqCheck{
				{Name: "git", OK: true, Required: true},
				{Name: "worker", OK: false, Required: true, Detail: "worker CLI omp not found"},
			},
		}, nil
	}

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK || len(probe.launched) != 0 {
		t.Fatalf("driver started despite a failed required check: %+v", res.Steps)
	}
	var hint string
	for _, s := range res.Steps {
		if s.Name == "checks" {
			hint = s.Hint
		}
	}
	if !strings.Contains(hint, "install the worker CLI") {
		t.Errorf("hint = %q, want the concrete fix line", hint)
	}

	opts.SkipChecks = true
	res, err = RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.launched) == 0 {
		t.Errorf("--skip-checks must still start the driver: %+v", res.Steps)
	}
}

// TestUpWithoutDaemon: --no-daemon leaves the control plane alone; the driver
// is the deliverable.
func TestUpWithoutDaemon(t *testing.T) {
	_, opts, probe := upFixture(t)
	noDaemon := false
	opts.Daemon = &noDaemon

	if _, err := RunUp(opts); err != nil {
		t.Fatal(err)
	}
	if got := names(probe.launched); strings.Join(got, ",") != "loop" {
		t.Errorf("launched %v, want only the driver", got)
	}
}

// TestUpDryRunChangesNothing: --dry-run is the command an operator runs to
// read the plan; it must spawn nothing, mkdir nothing, queue nothing.
func TestUpDryRunChangesNothing(t *testing.T) {
	repo, opts, probe := upFixture(t)
	opts.DryRun = true
	var sawDryRun bool
	opts.Intake = func(o prdintake.Options) (prdintake.Report, error) {
		sawDryRun = o.DryRun
		return prdintake.Report{PRDPath: prdintake.DefaultPRDPath(o.RepoPath)}, nil
	}

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(probe.launched) != 0 {
		t.Errorf("dry run launched %v", names(probe.launched))
	}
	if _, err := os.Stat(filepath.Join(repo, ".selfbuild", "logs")); !os.IsNotExist(err) {
		t.Errorf("dry run created state dirs")
	}
	if !sawDryRun {
		t.Errorf("intake must run in dry-run mode")
	}
	if !strings.Contains(res.NextAction, "without --dry-run") {
		t.Errorf("next action = %q", res.NextAction)
	}
}

// TestUpSurvivesAnUnreadablePRD: a missing docs/PRD.md is an unfilled lane,
// not a reason to leave the factory down — the driver still starts, and the
// report says why the lane is empty.
func TestUpSurvivesAnUnreadablePRD(t *testing.T) {
	_, opts, probe := upFixture(t)
	opts.Intake = func(prdintake.Options) (prdintake.Report, error) {
		return prdintake.Report{}, os.ErrNotExist
	}

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(probe.launched) == 0 {
		t.Fatalf("driver refused to start over a missing PRD: %+v", res.Steps)
	}
	for _, s := range res.Steps {
		if s.Name == "intake" && !s.Skipped {
			t.Errorf("intake step = %+v, want it reported as skipped", s)
		}
	}
}

// TestUpPropagatesASpawnFailure: an executable that cannot start must fail
// the run with the fallback command named, never print a success card. And a
// daemon that cannot start aborts before the driver is launched — half a
// factory is worse than none, because the TUI then reports a driver with no
// control plane to talk to.
func TestUpPropagatesASpawnFailure(t *testing.T) {
	_, opts, _ := upFixture(t)
	opts.Spawn = func(c Child) (ChildHandle, error) {
		if c.Name == "loop" {
			return ChildHandle{}, errors.New("start loop: permission denied")
		}
		return ChildHandle{Pid: 1234}, nil
	}

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Errorf("a failed spawn must fail the run: %+v", res.Steps)
	}
	var hint string
	for _, s := range res.Steps {
		if s.Name == "loop" {
			hint = s.Hint
		}
	}
	if !strings.Contains(hint, "devagent loop") {
		t.Errorf("loop hint = %q, want the foreground fallback", hint)
	}

	_, opts2, _ := upFixture(t)
	opts2.Spawn = func(Child) (ChildHandle, error) { return ChildHandle{}, errors.New("no such file") }
	res2, err := RunUp(opts2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range res2.Steps {
		if s.Name == "loop" {
			t.Errorf("the driver started after the daemon failed: %+v", res2.Steps)
		}
	}
}

// TestUpQueueSeedReachesDisk pins the one thing the seams cannot prove: the
// real intake path writes a claimable queue row, which is what the loop's
// phase-2a claim reads.
func TestUpQueueSeedReachesDisk(t *testing.T) {
	repo, opts, probe := upFixture(t)
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prdintake.DefaultPRDPath(repo), []byte("## Scope\n\n- [ ] Ship the intake lane\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Intake = nil // the real prdintake.Ingest
	opts.PortOpen = func(string) bool { return true }

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("up failed: %+v", res.Steps)
	}
	rows := queue.ListTasks(repo, queue.StatusPending)
	if len(rows) != 1 || rows[0].Source == nil || *rows[0].Source != "prd" {
		t.Fatalf("queue rows = %+v, want one prd-sourced pending row", rows)
	}
	if got := names(probe.launched); strings.Join(got, ",") != "loop" {
		t.Errorf("launched %v, want only the driver (the daemon port was open)", got)
	}
}

func writePid(t *testing.T, dir, name, pid string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(pid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDownStopsRecordedPidsOnly: `down` signals the pids it holds a record of
// and nothing else — no pattern match that could reach a driver started from
// another checkout (issue #354).
func TestDownStopsRecordedPidsOnly(t *testing.T) {
	repo := t.TempDir()
	runDir := filepath.Join(repo, ".selfbuild", "run")
	writePid(t, runDir, "loop.pid", "500")
	writePid(t, runDir, "daemon.pid", "501")

	var killed []string
	res, err := RunDown(UpOptions{
		RepoPath:  repo,
		Alive:     func(pid int) bool { return pid == 500 },
		Terminate: func(pid int) error { killed = append(killed, strconv.Itoa(pid)); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("down reported failure: %+v", res)
	}
	if strings.Join(killed, ",") != "500" {
		t.Errorf("killed %v, want only the live loop pid", killed)
	}
	if len(res.Stopped) != 1 || !strings.Contains(res.Stopped[0], "loop (pid 500)") {
		t.Errorf("stopped = %v, want the live loop only", res.Stopped)
	}
	if _, err := os.Stat(filepath.Join(runDir, "loop.pid")); !os.IsNotExist(err) {
		t.Errorf("a stopped driver's pid file must be removed")
	}
	if !strings.Contains(strings.Join(res.Notes, "|"), "daemon: pid 501 is already gone") {
		t.Errorf("notes = %v, want the dead daemon reported as gone", res.Notes)
	}
}

// TestDownPrefersTheLockHolder: a driver started by `make loop-start` or a
// bare `devagent loop` has no pid file of ours but does hold the lock — that
// record is the authoritative pid.
func TestDownPrefersTheLockHolder(t *testing.T) {
	repo := t.TempDir()
	writePid(t, filepath.Join(repo, ".selfbuild", "loop.lock.d"), "pid", "600")
	writePid(t, filepath.Join(repo, ".selfbuild", "run"), "loop.pid", "601")

	var killed []string
	res, err := RunDown(UpOptions{
		RepoPath:  repo,
		Alive:     func(pid int) bool { return pid == 600 },
		Terminate: func(pid int) error { killed = append(killed, strconv.Itoa(pid)); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(killed, ",") != "600" {
		t.Errorf("killed %v, want the lock holder, not the stale pid file", killed)
	}
	if len(res.Stopped) != 1 || !strings.Contains(res.Stopped[0], "pid 600") {
		t.Errorf("stopped = %v", res.Stopped)
	}
}

// TestDownDryRunSignalsNothing: the operator can ask what would stop.
func TestDownDryRunSignalsNothing(t *testing.T) {
	repo := t.TempDir()
	writePid(t, filepath.Join(repo, ".selfbuild", "run"), "loop.pid", "700")

	var killed []string
	res, err := RunDown(UpOptions{
		RepoPath:  repo,
		DryRun:    true,
		Alive:     func(int) bool { return true },
		Terminate: func(pid int) error { killed = append(killed, strconv.Itoa(pid)); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(killed) != 0 || len(res.Stopped) != 0 {
		t.Errorf("dry run signalled %v", killed)
	}
	if !strings.Contains(strings.Join(res.Notes, "|"), "dry run") {
		t.Errorf("notes = %v, want the dry-run note", res.Notes)
	}
}

// TestDownReportsNothingRunning: a repo with no driver must not look broken.
func TestDownReportsNothingRunning(t *testing.T) {
	res, err := RunDown(UpOptions{RepoPath: t.TempDir(), Alive: func(int) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || len(res.Stopped) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Notes) != 2 {
		t.Errorf("notes = %v, want one per candidate", res.Notes)
	}
}

// TestDownReportsAFailedStop: a pid that refuses to die is an operator
// problem, and `down` must say so with a nonzero verdict rather than pretend.
func TestDownReportsAFailedStop(t *testing.T) {
	repo := t.TempDir()
	runDir := filepath.Join(repo, ".selfbuild", "run")
	writePid(t, runDir, "loop.pid", "800")

	res, err := RunDown(UpOptions{
		RepoPath:  repo,
		Alive:     func(int) bool { return true },
		Terminate: func(int) error { return errors.New("operation not permitted") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Errorf("a refused signal must fail the run: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(runDir, "loop.pid")); err != nil {
		t.Errorf("an unstopped driver keeps its pid record: %v", err)
	}
}

// TestUpNamesAnEmptyLane: #355 is the "long runtime, little value" bug — an
// empty lane is not an emergency, it is the reason the next ten hours ship
// loop plumbing. `up` must say so, with the fix, before starting.
func TestUpNamesAnEmptyLane(t *testing.T) {
	_, opts, _ := upFixture(t)
	opts.Intake = func(o prdintake.Options) (prdintake.Report, error) {
		return prdintake.Report{PRDPath: prdintake.DefaultPRDPath(o.RepoPath)}, nil
	}
	opts.CountIssues = func(string) (int, error) { return 0, nil }

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	var hint, detail string
	for _, s := range res.Steps {
		if s.Name == "lane" {
			detail, hint = s.Detail, s.Hint
		}
	}
	if !strings.Contains(detail, "tracker 0 open selfbuild issue") {
		t.Errorf("lane detail = %q, want the tracker count", detail)
	}
	if !strings.Contains(hint, "docs/PRD.md") || !strings.Contains(hint, "will invent work") {
		t.Errorf("lane hint = %q, want the empty-lane remedy", hint)
	}
}

// TestUpExplainsAHeadOfIterationHalt: the halt paths that matter most (an
// iteration cap already reached, a starvation halt, a lock refusal) happen
// BEFORE any iteration log opens, so the reason only exists in driver.log —
// and `up` must read it there rather than report an unexplained dead pid.
func TestUpExplainsAHeadOfIterationHalt(t *testing.T) {
	repo, opts, probe := upFixture(t)
	opts.Spawn = func(c Child) (ChildHandle, error) {
		probe.launched = append(probe.launched, c)
		// The real spawn path reaps its child; so does the fake, or the
		// health receipt would be watching a pid nobody waited.
		exited := make(chan struct{})
		close(exited)
		return ChildHandle{Pid: 5556, Exited: exited}, nil
	}
	opts.Alive = func(int) bool { return false }
	writeRepoFileForUp(t, repo, "logs/driver.log", "[state] pulled 0 ledger entries\nmax iterations reached\n")

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("a driver that halted at the loop head was reported healthy: %+v", res.Steps)
	}
	var detail string
	for _, s := range res.Steps {
		if s.Name == "loop" {
			detail = s.Detail
		}
	}
	if !strings.Contains(detail, "max iterations reached") {
		t.Errorf("loop detail = %q, want the halt line from driver.log", detail)
	}
}

// TestUpRefusesToStartBehindADirtyStateDoc: the driver's own currency gate
// skips every iteration while docs/PRD.md is uncommitted, so a factory started
// over a draft looks busy and ships nothing — and intake would have queued the
// draft as if it were intent. `up` must say so before it starts anything.
func TestUpRefusesToStartBehindADirtyStateDoc(t *testing.T) {
	_, opts, probe := upFixture(t)
	opts.Dirty = func(string, string) (bool, error) { return true, nil }

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Errorf("up started over a dirty docs/PRD.md: %+v", res.Steps)
	}
	if len(probe.launched) != 0 {
		t.Errorf("started %v behind the dirty PRD", names(probe.launched))
	}
	for _, s := range res.Steps {
		if s.Name == "intake" {
			t.Errorf("intake ran on a draft PRD: %+v", s)
		}
	}
	var hint string
	for _, s := range res.Steps {
		if s.Name == "prd" {
			hint = s.Hint
		}
	}
	if !strings.Contains(hint, "git commit") {
		t.Errorf("prd hint = %q, want the commit line", hint)
	}
}

// TestUpProceedsOnACleanStateDoc pins the other direction, including that an
// unanswerable probe (not a git checkout) must not block the start.
func TestUpProceedsOnACleanStateDoc(t *testing.T) {
	_, opts, _ := upFixture(t)
	opts.Dirty = func(string, string) (bool, error) { return false, nil }
	if res, err := RunUp(opts); err != nil || !res.OK {
		t.Errorf("clean PRD refused the start: %+v (%v)", res.Steps, err)
	}

	_, opts2, probe2 := upFixture(t)
	opts2.Dirty = func(string, string) (bool, error) { return false, errors.New("not a git repository") }
	res2, err := RunUp(opts2)
	if err != nil {
		t.Fatal(err)
	}
	if !res2.OK || len(probe2.launched) == 0 {
		t.Errorf("an unknown PRD state blocked the start: %+v", res2.Steps)
	}
}

// TestUpScoutLaneIsASecondRecordedChild: `--scout` runs the researcher as
// its own detached child with its own pid file and log, so `down` stops both
// lanes and neither hides the other.
func TestUpScoutLaneIsASecondRecordedChild(t *testing.T) {
	repo, opts, probe := upFixture(t)
	opts.Scout = true
	opts.ScoutIntervalMinutes = 45

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("up failed: %+v", res.Steps)
	}
	scout := probe.child(t, "scout")
	if strings.Join(scout.Args, " ") != "scout --repo "+repo+" --interval 45" {
		t.Errorf("scout args = %q", strings.Join(scout.Args, " "))
	}
	if scout.LogPath != filepath.Join(repo, ".selfbuild", "logs", "scout.log") || scout.PidFile == "" {
		t.Errorf("scout child = %+v, want its own log + pid record", scout)
	}
	if res.Logs["scout"] == "" {
		t.Errorf("the report must name the scout log")
	}

	// `down` stops it, and only reports the lanes it has a record for.
	down, err := RunDown(UpOptions{
		RepoPath:  repo,
		Alive:     func(int) bool { return true },
		Terminate: func(int) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var sawScout bool
	for _, s := range down.Stopped {
		if strings.Contains(s, "scout") {
			sawScout = true
		}
	}
	if !sawScout {
		t.Errorf("down stopped %v, want the scout lane included", down.Stopped)
	}
}

func names(children []Child) []string {
	var out []string
	for _, c := range children {
		out = append(out, c.Name)
	}
	return out
}

// proveDriverAlive writes the two artifacts a live driver produces — the loop
// lock record and a heartbeat naming a phase — so a test can assert `up`'s
// health receipt against real evidence rather than a stub return value.
func proveDriverAlive(t *testing.T, repo string, pid int) {
	t.Helper()
	lockDir := filepath.Join(repo, ".selfbuild", "loop.lock.d")
	writePid(t, lockDir, "pid", strconv.Itoa(pid))
	hb := `{"iteration":12,"phase":"research","pid":` + strconv.Itoa(pid) + `,"updatedAt":"2026-09-14T00:00:00Z"}`
	if err := os.WriteFile(driverHeartbeatPath(repo), []byte(hb), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestUpHealthReceiptRejectsADriverThatDied: a green `up` must mean a running
// factory. A driver that takes the lock and then exits — an iteration cap
// already reached, a starvation halt — exits 0, so no supervisor tells anyone;
// `up` has to, and it has to name the cause from the driver's own log.
func TestUpHealthReceiptRejectsADriverThatDied(t *testing.T) {
	repo, opts, probe := upFixture(t)
	opts.Spawn = func(c Child) (ChildHandle, error) {
		probe.launched = append(probe.launched, c)
		return ChildHandle{Pid: 5555}, nil // alive for a moment, then gone
	}
	opts.Alive = func(int) bool { return false }
	// The driver's last verdict, as it lands in its own iteration log.
	writeRepoFileForUp(t, repo, "logs/loop-12.log", "=== self-build loop 12 start ===\n[starvation] 5 consecutive non-productive iterations — halting loop\n")

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatalf("a dead driver was reported healthy: %+v", res.Steps)
	}
	var detail string
	for _, s := range res.Steps {
		if s.Name == "loop" {
			detail = s.Detail
		}
	}
	if !strings.Contains(detail, "halting loop") {
		t.Errorf("loop detail = %q, want the halt reason from the driver log", detail)
	}
}

// TestUpHealthReceiptNeedsProof: a pid that is alive but has taken neither the
// loop lock nor written a heartbeat is a process, not a factory — the wait is
// bounded and the report says what it looked for.
func TestUpHealthReceiptNeedsProof(t *testing.T) {
	repo, opts, probe := upFixture(t)
	opts.Spawn = func(c Child) (ChildHandle, error) {
		probe.launched = append(probe.launched, c)
		return ChildHandle{Pid: 6666}, nil
	}
	opts.Alive = func(int) bool { return true }
	opts.HealthWindow = 20 * time.Millisecond

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Errorf("an unproven driver was reported healthy: %+v", res.Steps)
	}
	var detail string
	for _, s := range res.Steps {
		if s.Name == "loop" {
			detail = s.Detail
		}
	}
	if !strings.Contains(detail, "no loop lock or heartbeat") {
		t.Errorf("loop detail = %q, want what the health check looked for", detail)
	}
	_ = repo
}

// TestUpReportsALiveDriverInsteadOfProvingItsOwn: when a driver already holds
// the lock and has published a heartbeat before `up` runs, `up` must report
// that holder and start nothing — the health receipt is for the driver `up`
// itself launched, and a second driver in one repo is the race #354 is about.
func TestUpReportsALiveDriverInsteadOfProvingItsOwn(t *testing.T) {
	repo, opts, probe := upFixture(t)
	// A driver that is already live, started by some other route.
	proveDriverAlive(t, repo, 4243)
	opts.Alive = func(pid int) bool { return pid == 4243 }

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("up failed: %+v", res.Steps)
	}
	for _, c := range probe.launched {
		if c.Name == "loop" {
			t.Errorf("a live driver was double-started: %v", names(probe.launched))
		}
	}
	if res.PIDs["loop"] != 4243 {
		t.Errorf("report loop pid = %d, want the existing holder 4243", res.PIDs["loop"])
	}
}

// TestUpHealthProofCanBeSkipped: `--wait 0` is the escape hatch for a caller
// that starts the factory from a script and does its own watching.
func TestUpHealthProofCanBeSkipped(t *testing.T) {
	_, opts, probe := upFixture(t)
	opts.Spawn = func(c Child) (ChildHandle, error) {
		probe.launched = append(probe.launched, c)
		return ChildHandle{Pid: 7777}, nil
	}
	opts.Alive = func(int) bool { return true }
	opts.HealthWindow = -1

	res, err := RunUp(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Errorf("--wait 0 must skip the proof instead of failing it: %+v", res.Steps)
	}
}

func writeRepoFileForUp(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, ".selfbuild", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
