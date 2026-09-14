package commands

// updown.go is `devagent up` / `devagent down` (issue #371): one command to
// bring the automated workflow driver up, one to take it down.
//
// Before this, starting the factory meant assembling tribal knowledge: the
// SELFBUILD_* exports are read from the environment only
// (internal/loopdriver/config.go:245), DevagentBin defaults to a bare
// `devagent` a Go-only checkout never installed on PATH, `make loop-start`
// needs `./devagent-go` built first and knows nothing about the work lane,
// and an empty lane makes the driver ship loop plumbing instead of product
// (issue #355). `up` collapses all of it — check, seed, start, report — and
// `down` stops exactly what was recorded, by pid, never a blind pattern
// kill.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"

	"github.com/FreePeak/devagent/internal/prdintake"
	"github.com/FreePeak/devagent/internal/queue"
)

// DefaultDaemonAddr is where the FR-CTRL control-plane daemon listens, the
// TUI's default endpoint too (internal/tui DefaultDaemonURL).
const DefaultDaemonAddr = "127.0.0.1:7788"

// defaultHealthWindow is how long `up` watches the driver it just started
// before calling the start good (issue #371). Fifteen seconds clears the
// driver's own startup sequence (state pull, lock acquisition, first
// heartbeat write) on a warm machine without turning `up` into a supervisor.
const defaultHealthWindow = 15 * time.Second

// driverHeartbeatPath is the loop's own liveness record (internal/loopdriver
// writeHeartbeat).
func driverHeartbeatPath(repo string) string {
	return filepath.Join(repo, ".selfbuild", "heartbeat.json")
}

// UpOptions configures one `up` run. Every function field is a
// hermetic-test seam; nil runs the real thing.
type UpOptions struct {
	// RepoPath is the checkout to drive; "" = cwd.
	RepoPath string
	// Daemon also brings up the control-plane daemon (the TUI's data
	// source). Unset means yes; only --no-daemon clears it.
	Daemon *bool
	// Foreground runs the driver in this terminal (stdio inherited, `up`
	// waits for it) instead of detaching it.
	Foreground bool
	// Scout also runs the 24/7 research lane (`devagent scout --interval N`,
	// FR-SCOUT-01) as a second detached child, which is the other half of
	// keeping the lane filled: PRD intake reads what the operator wrote, the
	// scout reads what the repo still needs. Off by default because each
	// cycle spends worker tokens; `config.scout.enabled` is what the
	// LaunchAgent route uses, so `up --scout` is the foreground-of-the-factory
	// equivalent.
	Scout bool
	// ScoutIntervalMinutes overrides `config.scout.intervalMinutes` for the
	// child `up` starts (0 = config, else 30).
	ScoutIntervalMinutes int
	// SkipChecks starts the driver even when a required prerequisite failed.
	SkipChecks bool
	// DryRun reports the plan and changes nothing: no config write, no
	// intake, no spawn.
	DryRun bool
	// HealthWindow is how long `up` waits for proof that the driver it
	// started is actually running (issue #371: a green `up` must mean a
	// running factory, not a spawn receipt — a driver that dies holding the
	// lock for two seconds is otherwise reported healthy). 0 = the default
	// 15s; negative disables the wait.
	HealthWindow time.Duration
	// Sleep is the poll seam (nil = time.Sleep).
	Sleep          func(time.Duration)
	MaxIntakeItems int

	// Checks runs the prerequisite gate (nil = RunInit, whose devagent.json
	// merge is idempotent — existing choices always win).
	Checks func(InitOptions) (InitResult, error)
	// Intake ingests docs/PRD.md into the queue (nil = prdintake.Ingest).
	Intake func(prdintake.Options) (prdintake.Report, error)
	// Spawn launches one factory child.
	Spawn func(Child) (ChildHandle, error)
	// Alive probes pid liveness (nil = the signal-0 probe).
	Alive func(pid int) bool
	// PortOpen reports whether an address already accepts connections
	// (nil = a bounded TCP dial).
	PortOpen func(addr string) bool
	// Terminate stops one recorded pid (nil = the process-group SIGTERM).
	// Tests must inject it: `down` otherwise signals a live system process.
	Terminate func(pid int) error
	// SelfExe is the executable children launch from (nil = os.Executable),
	// so `./devagent-go up` supervises `./devagent-go loop`.
	SelfExe func() string
	// CountIssues reads the tracker lane's depth (nil = `gh issue list
	// --label selfbuild`; skipped entirely on --dry-run, which must not hit
	// the network).
	CountIssues func(dir string) (int, error)
	// Dirty reports whether a path has uncommitted changes in the checkout
	// (nil = `git diff --quiet`). A dirty state doc is the one condition
	// where a healthy driver deliberately does nothing.
	Dirty  func(dir, path string) (bool, error)
	Stdout io.Writer
	Stderr io.Writer
}

// Child is one process `up` launches.
type Child struct {
	// Name keys the pid file and the report row ("loop", "daemon").
	Name string
	// Bin is the executable path.
	Bin     string
	Args    []string
	Dir     string
	LogPath string
	// Env entries are merged over the parent environment (KEY=VALUE).
	Env     []string
	Detach  bool
	Out     io.Writer // foreground: the child's stdio goes here
	ErrOut  io.Writer
	PidFile string
}

// ChildHandle is a launched factory child: its pid, plus a channel closed
// when the OS reaps it. Exited is nil for a caller that cannot observe the
// child (a test seam, or a driver another route already started), in which
// case `up` falls back to a liveness probe.
//
// The channel exists because a signal-0 probe cannot see the case that
// matters: `up` is the driver's parent, so a driver that halts at its own
// gate stays a readable zombie until someone waits it, and a poll that only
// asks "is the pid alive" reports a dead factory as healthy for the whole
// window (measured on the 2026-09-14 smoke: "max iterations reached" on
// stderr, `up` still watching).
type ChildHandle struct {
	Pid    int
	Exited <-chan struct{}
}

// UpStep is one line of the `up` report.
type UpStep struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail"`
	Hint    string `json:"hint,omitempty"`
}

// UpResult is the `up` report (`--json` emits it verbatim for the TUI).
type UpResult struct {
	OK         bool              `json:"ok"`
	RepoPath   string            `json:"repoPath"`
	Steps      []UpStep          `json:"steps"`
	PIDs       map[string]int    `json:"pids,omitempty"`
	Logs       map[string]string `json:"logs,omitempty"`
	Intake     *prdintake.Report `json:"intake,omitempty"`
	NextAction string            `json:"nextAction,omitempty"`
}

// DownResult is the `down` report.
type DownResult struct {
	OK       bool     `json:"ok"`
	RepoPath string   `json:"repoPath"`
	Stopped  []string `json:"stopped"`
	Notes    []string `json:"notes,omitempty"`
}

// RunUp checks the prerequisites, seeds the work lane from docs/PRD.md,
// starts the driver, and reports what is running plus where to look. It is
// idempotent: a live driver is reported, never doubled.
func RunUp(opts UpOptions) (UpResult, error) {
	repo := opts.RepoPath
	if repo == "" {
		repo, _ = os.Getwd()
	}
	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	errOut := opts.Stderr
	if errOut == nil {
		errOut = os.Stderr
	}
	alive := opts.Alive
	if alive == nil {
		alive = processAlive
	}
	withDaemon := opts.Daemon == nil || *opts.Daemon

	result := UpResult{OK: true, RepoPath: repo, PIDs: map[string]int{}, Logs: map[string]string{}}
	step := func(name string, ok bool, detail, hint string) {
		result.Steps = append(result.Steps, UpStep{Name: name, OK: ok, Detail: detail, Hint: hint})
	}
	skip := func(name, detail string) {
		result.Steps = append(result.Steps, UpStep{Name: name, OK: true, Skipped: true, Detail: detail})
	}
	fail := func(name, detail, hint string) {
		step(name, false, detail, hint)
		result.OK = false
	}

	// 1. Prerequisite gate. RunInit is the same check list `devagent init`
	// prints (git, worker CLI, provider, credentials, docker), and its
	// config merge is idempotent, so `up` needs no separate setup step.
	if opts.DryRun {
		skip("checks", "dry run: prerequisites not probed, devagent.json untouched")
	} else {
		checks := opts.Checks
		if checks == nil {
			checks = RunInit
		}
		r, err := checks(InitOptions{RepoPath: repo})
		if err != nil {
			return UpResult{}, err
		}
		if !r.OK && !opts.SkipChecks {
			for _, c := range r.Checks {
				if c.OK || !c.Required {
					continue
				}
				fail("checks", c.Name+": "+c.Detail, FailureAdvice(c.Name))
			}
			result.NextAction = "fix the required check(s) above, then re-run `devagent up`"
			return result, nil
		}
		if !r.OK {
			step("checks", true, "required prerequisites failed; started anyway on --skip-checks", "")
		} else {
			step("checks", true, fmt.Sprintf("%d prerequisite check(s) passed; config %s", len(r.Checks), r.ConfigPath), "")
		}
	}

	// 2. State dirs. The driver mkdirs its own; doing it here means every
	// path `up` prints exists before anything points at it.
	if opts.DryRun {
		skip("dirs", "dry run: nothing created")
	} else {
		var broken []string
		for _, dir := range []string{".selfbuild/research", ".selfbuild/goals", ".selfbuild/logs", ".selfbuild/curation", ".selfbuild/run"} {
			if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
				broken = append(broken, dir+": "+err.Error())
			}
		}
		queue.EnsureQueueDirs(repo)
		if len(broken) > 0 {
			fail("dirs", strings.Join(broken, "; "), "make the repo directory writable")
			return result, nil
		}
		step("dirs", true, ".selfbuild state and .devagent queue directories ready", "")
	}

	// 2b. A dirty state doc is the one case where a *healthy* driver does
	// nothing on purpose: its currency gate skips every iteration while
	// docs/PRD.md has uncommitted edits, stamping `operator-degraded` rows
	// (internal/loopdriver run.go PRD gate). Checked before intake for the
	// same reason: the worktree text is a draft, and a draft is not intent.
	// Without this line the operator starts the factory, watches it stay busy,
	// and gets no explanation.
	if opts.DryRun {
		skip("prd", "dry run: PRD worktree state not probed")
	} else {
		dirty := opts.Dirty
		if dirty == nil {
			dirty = gitDirty
		}
		if on, derr := dirty(repo, "docs/PRD.md"); derr != nil {
			step("prd", true, "PRD worktree state unknown: "+derr.Error(), "")
		} else if on {
			fail("prd", "docs/PRD.md has uncommitted edits — the driver will skip every iteration until they land",
				"commit it (`git commit -m \"docs(prd): …\" docs/PRD.md`) — intake reads the committed text, never a draft")
			return result, nil
		} else {
			step("prd", true, "docs/PRD.md is clean and committed", "")
		}
	}

	// 3. Seed the lane from the committed PRD. This is the difference between a driver that ships
	// product and one that ships plumbing (issue #355): the operator's open
	// PRD checkboxes become queue rows before the next iteration picks.
	intake := opts.Intake
	if intake == nil {
		intake = prdintake.Ingest
	}
	rep, err := intake(prdintake.Options{RepoPath: repo, MaxItems: opts.MaxIntakeItems, DryRun: opts.DryRun})
	switch {
	case err != nil && os.IsNotExist(err):
		skip("intake", "no docs/PRD.md to ingest")
	case err != nil:
		// An unreadable PRD is a lane we could not fill, not a reason to
		// leave the factory down: report it and keep going.
		step("intake", true, "PRD intake skipped: "+err.Error(), "fix docs/PRD.md, then `devagent prd-intake`")
	default:
		result.Intake = &rep
		switch {
		case opts.DryRun:
			skip("intake", fmt.Sprintf("dry run: %d open PRD item(s), %d would queue", rep.Open, len(rep.Queued)))
		case len(rep.Queued) > 0:
			step("intake", true, fmt.Sprintf("queued %d operator item(s) from docs/PRD.md; queue depth %d", len(rep.Queued), rep.QueueDepth), "")
		default:
			step("intake", true, fmt.Sprintf("%d open PRD item(s) already queued; queue depth %d", rep.Open, rep.QueueDepth), "")
		}
	}

	// 3b. The lane census: the one number that predicts whether the next
	// hours ship product or plumbing (issue #355 — an empty tracker and an
	// empty queue leave the driver free to select itself as its own work,
	// which is exactly what it has been doing). Reported before the start so
	// an operator can abort instead of watching.
	pending := len(queue.ListTasks(repo, queue.StatusPending))
	openItems := 0
	if result.Intake != nil {
		openItems = result.Intake.Open
	}
	census := fmt.Sprintf("queue %d pending (%d open PRD item(s))", pending, openItems)
	countIssues := opts.CountIssues
	if countIssues == nil && !opts.DryRun {
		countIssues = countSelfbuildIssues
	}
	if countIssues != nil {
		if n, ierr := countIssues(repo); ierr != nil {
			census += "; tracker not probed: " + ierr.Error()
		} else {
			census += fmt.Sprintf("; tracker %d open selfbuild issue(s)", n)
		}
	}
	switch {
	case pending == 0 && openItems == 0:
		step("lane", true, census, "the driver has nothing to build and will invent work: write `- [ ] <what you want>` in docs/PRD.md (issue #355)")
	default:
		step("lane", true, census, "")
	}

	// 4. The control-plane daemon (the TUI attaches to it). Its port is the
	// liveness answer — not a process-name guess.
	if withDaemon {
		portOpen := opts.PortOpen
		if portOpen == nil {
			portOpen = tcpOpen
		}
		switch {
		case portOpen(DefaultDaemonAddr):
			skip("daemon", "already listening on "+DefaultDaemonAddr)
		case opts.DryRun:
			skip("daemon", "dry run: daemon not started")
		default:
			h, serr := launch(opts, Child{
				Name:    "daemon",
				Bin:     selfExe(opts),
				Args:    []string{"daemon", "--repo", repo},
				Dir:     repo,
				LogPath: filepath.Join(repo, ".selfbuild", "logs", "daemon.log"),
				Detach:  !opts.Foreground,
				Out:     errOut,
				ErrOut:  errOut,
				PidFile: pidFile(repo, "daemon"),
			})
			if serr != nil {
				fail("daemon", serr.Error(), "or start it by hand: devagent daemon --repo "+repo)
				return result, nil
			}
			result.PIDs["daemon"] = h.Pid
			result.Logs["daemon"] = filepath.Join(repo, ".selfbuild", "logs", "daemon.log")
			step("daemon", true, fmt.Sprintf("listening on %s (pid %d)", DefaultDaemonAddr, h.Pid), "")
		}
	}

	self := selfExe(opts)

	// 4b. The scout lane (--scout): the researcher that proposes work between
	// iterations. Same detach + pid record as the driver, so `down` stops it,
	// and the same interval default the LaunchAgent installer uses.
	if opts.Scout {
		interval := opts.ScoutIntervalMinutes
		if interval <= 0 {
			interval = scoutInterval(repo)
		}
		switch {
		case opts.DryRun:
			skip("scout", fmt.Sprintf("dry run: scout not started (every %dm)", interval))
		default:
			h, serr := launch(opts, Child{
				Name:    "scout",
				Bin:     self,
				Args:    []string{"scout", "--repo", repo, "--interval", strconv.Itoa(interval)},
				Dir:     repo,
				LogPath: filepath.Join(repo, ".selfbuild", "logs", "scout.log"),
				Detach:  true,
				PidFile: pidFile(repo, "scout"),
			})
			if serr != nil {
				fail("scout", serr.Error(), "or run it by hand: devagent scout --interval "+strconv.Itoa(interval))
				return result, nil
			}
			result.PIDs["scout"] = h.Pid
			result.Logs["scout"] = filepath.Join(repo, ".selfbuild", "logs", "scout.log")
			step("scout", true, fmt.Sprintf("researcher started (pid %d), one cycle every %dm", h.Pid, interval), "")
		}
	}

	// 5. The driver. A live holder of the loop lock outranks our own pid
	// file: `make loop-start` and a hand-run `devagent loop` are both
	// legitimate starts, and a second concurrent driver is the ledger-race
	// class that has killed this loop before.
	if lp := loopHolderPid(repo); lp != 0 && alive(lp) {
		result.PIDs["loop"] = lp
		skip("loop", fmt.Sprintf("already running (pid %d) — leave it alone; `devagent down` stops it", lp))
	} else if opts.DryRun {
		skip("loop", "dry run: driver not started")
	} else {
		h, serr := launch(opts, Child{
			Name: "loop",
			Bin:  self,
			Args: []string{"loop"},
			Dir:  repo,
			// The driver shells out for task/preflight/sync-docs/pane-run;
			// pinning that at this exact executable is what makes
			// `./devagent-go up` self-contained (its historical default
			// was the bare name `devagent`, absent on a Go-only checkout).
			Env:     []string{"SELFBUILD_DEVAGENT_BIN=" + self},
			LogPath: filepath.Join(repo, ".selfbuild", "logs", "driver.log"),
			Detach:  !opts.Foreground,
			Out:     out,
			ErrOut:  errOut,
			PidFile: pidFile(repo, "loop"),
		})
		if serr != nil {
			fail("loop", serr.Error(), "or run it in the foreground: devagent loop")
			return result, nil
		}
		result.PIDs["loop"] = h.Pid
		result.Logs["loop"] = filepath.Join(repo, ".selfbuild", "logs", "driver.log")
		switch {
		case opts.Foreground:
			step("loop", true, fmt.Sprintf("foreground driver exited (pid %d)", h.Pid), "")
		default:
			proof, why := verifyDriver(opts, repo, h, alive)
			if why != "" {
				fail("loop", fmt.Sprintf("driver pid %d is not running: %s", h.Pid, why),
					"the reason is in "+result.Logs["loop"]+"; `devagent supervision` shows who would restart it")
				return result, nil
			}
			step("loop", true, fmt.Sprintf("driver running (pid %d) — %s", h.Pid, proof), "")
		}
	}

	switch {
	case opts.DryRun:
		result.NextAction = "nothing was changed — re-run without --dry-run to start the factory"
	case opts.Foreground:
		result.NextAction = "driver finished; `devagent up` starts it detached"
	case result.OK:
		result.NextAction = "watch: `devagent status` or `devagent tui`; logs: .selfbuild/logs/driver.log; stop: `devagent down`"
	}
	return result, nil
}

// RunDown stops the factory processes `up` recorded (and a driver that took
// the loop lock by any other route). It signals recorded pids only — a
// pattern kill can reach a driver started from another checkout, which is
// the class issue #354 was filed about.
func RunDown(opts UpOptions) (DownResult, error) {
	repo := opts.RepoPath
	if repo == "" {
		repo, _ = os.Getwd()
	}
	alive := opts.Alive
	if alive == nil {
		alive = processAlive
	}
	result := DownResult{OK: true, RepoPath: repo}

	// The lock record wins when present: it is what the driver itself wrote.
	loopPid := loopHolderPid(repo)
	if loopPid == 0 {
		loopPid = readPidFile(pidFile(repo, "loop"))
	}
	targets := []struct {
		name string
		pid  int
	}{{"loop", loopPid}, {"daemon", readPidFile(pidFile(repo, "daemon"))}}
	if p := readPidFile(pidFile(repo, "scout")); p != 0 {
		targets = append(targets, struct {
			name string
			pid  int
		}{"scout", p})
	}
	for _, t := range targets {
		switch {
		case t.pid == 0:
			result.Notes = append(result.Notes, t.name+": not running (no pid recorded)")
		case !alive(t.pid):
			result.Notes = append(result.Notes, fmt.Sprintf("%s: pid %d is already gone", t.name, t.pid))
			_ = os.Remove(pidFile(repo, t.name))
		case opts.DryRun:
			result.Notes = append(result.Notes, fmt.Sprintf("%s: dry run, pid %d left running", t.name, t.pid))
		default:
			if err := stop(opts, t.pid); err != nil {
				result.OK = false
				result.Notes = append(result.Notes, fmt.Sprintf("%s: pid %d could not be stopped: %v", t.name, t.pid, err))
				continue
			}
			result.Stopped = append(result.Stopped, fmt.Sprintf("%s (pid %d)", t.name, t.pid))
			_ = os.Remove(pidFile(repo, t.name))
		}
	}
	return result, nil
}

// launch runs one child through the injected seam or the real spawn path.
func launch(opts UpOptions, c Child) (ChildHandle, error) {
	if opts.Spawn != nil {
		return opts.Spawn(c)
	}
	return spawnChild(c)
}

// stop signals one recorded pid through the injected seam when there is one.
// `down` must never pattern-match a process name: the class issue #354 is
// about a driver from another checkout sharing this repo's state, and a
// wildcard kill cannot tell the two apart.
func stop(opts UpOptions, pid int) error {
	if opts.Terminate != nil {
		return opts.Terminate(pid)
	}
	return terminateTree(pid)
}

// spawnChild launches one factory child. Detached children get their own
// session (detachAttrs) and append into LogPath; a background reaper waits
// them so `up` learns the moment the driver exits, and `up` itself still
// returns immediately (the goroutine dies with the process, after which the
// driver is reparented and keeps running — the whole point of detaching).
// Foreground children inherit the given writers and `up` waits for them, so
// its own exit code is the driver's verdict.
func spawnChild(c Child) (ChildHandle, error) {
	if c.Bin == "" || len(c.Args) == 0 {
		return ChildHandle{}, fmt.Errorf("child %q needs an executable and argv", c.Name)
	}
	logW, errW := c.Out, c.ErrOut
	if c.Detach {
		f, oerr := os.OpenFile(c.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if oerr != nil {
			return ChildHandle{}, fmt.Errorf("open %s: %w", c.LogPath, oerr)
		}
		logW, errW = f, f
		defer func() { _ = f.Close() }()
	}

	cmd := exec.Command(c.Bin, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdout, cmd.Stderr = logW, errW
	if c.Detach {
		detachAttrs(cmd)
	} else {
		cmd.Stdin = os.Stdin
	}
	if serr := cmd.Start(); serr != nil {
		return ChildHandle{}, fmt.Errorf("start %s: %w", c.Name, serr)
	}
	h := ChildHandle{Pid: cmd.Process.Pid}
	if c.PidFile != "" {
		writePidFile(c.PidFile, h.Pid)
	}
	if !c.Detach {
		if werr := cmd.Wait(); werr != nil {
			return h, fmt.Errorf("%s exited: %w", c.Name, werr)
		}
		return h, nil
	}
	exited := make(chan struct{})
	h.Exited = exited
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	return h, nil
}

// selfExe resolves the executable children are launched from.
func selfExe(opts UpOptions) string {
	if opts.SelfExe != nil {
		return opts.SelfExe()
	}
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "devagent"
}

// pidFile is where `up` records a child pid for `down` to find.
func pidFile(repo, name string) string {
	return filepath.Join(repo, ".selfbuild", "run", name+".pid")
}

func writePidFile(path string, pid int) {
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func readPidFile(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, cerr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if cerr != nil || n <= 0 {
		return 0
	}
	return n
}

// loopHolderPid reads the driver's own mkdir-lock record — the authoritative
// answer to "is a driver already running in this repo", whoever started it.
func loopHolderPid(repo string) int {
	return readPidFile(filepath.Join(repo, ".selfbuild", "loop.lock.d", "pid"))
}

// tcpOpen answers "is something listening there" with a bounded dial.
func tcpOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// RenderUpReport prints the `up` report as the plain-language checklist §21
// asks for: one line per step, ✓/–/✗, one advice line per miss, never raw
// logs.
func RenderUpReport(r UpResult, render func(string)) {
	render("devagent up — " + r.RepoPath)
	for _, s := range r.Steps {
		mark := "✓"
		switch {
		case !s.OK:
			mark = "✗"
		case s.Skipped:
			mark = "–"
		}
		render(fmt.Sprintf("  %s %-7s %s", mark, s.Name, s.Detail))
		if s.Hint != "" {
			render("      → " + s.Hint)
		}
	}
	if r.Intake != nil && len(r.Intake.Queued) > 0 {
		render("  built from docs/PRD.md:")
		for _, it := range r.Intake.Queued {
			render(fmt.Sprintf("      %s  %s", it.ID, it.Title))
		}
	}
	for _, name := range []string{"loop", "daemon"} {
		if p := r.Logs[name]; p != "" {
			render(fmt.Sprintf("  log %-6s %s", name, p))
		}
	}
	if r.NextAction != "" {
		render("  next: " + r.NextAction)
	}
}

// RenderDownReport prints `down` in the same idiom.
func RenderDownReport(r DownResult, render func(string)) {
	render("devagent down — " + r.RepoPath)
	for _, s := range r.Stopped {
		render("  ✓ stopped " + s)
	}
	for _, n := range r.Notes {
		render("  – " + n)
	}
}

// verifyDriver proves the detached driver is actually running before `up`
// calls the start good.
//
// Without this, `up` is a spawn receipt: it reports a pid the OS handed out
// and nothing about whether the factory is alive. A driver that halts at its
// own gate — the starvation halt, an iteration cap already reached, a state
// branch it cannot pull — exits with code 0, so no supervisor would ever
// restart it and nothing else says so either (the 2026-09-14 research pass,
// docs/research/2026-09-14-easy-local-setup-selfbuild-loop.md: "green `up`
// must mean a running factory"). Proof is the two artifacts the driver itself
// writes: it holds the loop lock, and its heartbeat names a phase.
//
// The window is bounded and the verdict is early either way: a live driver is
// reported as soon as its own heartbeat lands (usually well under a second),
// a dead one is reported with the reason, and an inconclusive one says what
// it looked for.
func verifyDriver(opts UpOptions, repo string, h ChildHandle, alive func(pid int) bool) (string, string) {
	pid := h.Pid
	window := opts.HealthWindow
	switch {
	case window < 0:
		return "health proof skipped (--wait 0)", ""
	case window == 0:
		window = defaultHealthWindow
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	deadline := time.Now().Add(window)
	for {
		select {
		case <-h.Exited:
			return "", stoppedReason(repo, pid)
		default:
		}
		if !alive(pid) {
			return "", stoppedReason(repo, pid)
		}
		if hb, ok := readDriverHeartbeat(repo); ok && loopHolderPid(repo) == pid && hb.Pid == pid {
			return fmt.Sprintf("holds the loop lock, iteration %d, phase %s", hb.Iteration, hb.Phase), ""
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Sprintf("pid %d is alive but wrote no loop lock or heartbeat within %s", pid, window)
		}
		sleep(100 * time.Millisecond)
	}
}

// driverHeartbeat is the subset of `.selfbuild/heartbeat.json` that proves
// life (the loop's own writeHeartbeat record).
type driverHeartbeat struct {
	Iteration int    `json:"iteration"`
	Phase     string `json:"phase"`
	Pid       int    `json:"pid"`
	UpdatedAt string `json:"updatedAt"`
}

func readDriverHeartbeat(repo string) (driverHeartbeat, bool) {
	raw, err := os.ReadFile(driverHeartbeatPath(repo))
	if err != nil {
		return driverHeartbeat{}, false
	}
	var hb driverHeartbeat
	if json.Unmarshal(raw, &hb) != nil || hb.Pid <= 0 {
		return driverHeartbeat{}, false
	}
	return hb, true
}

// stoppedReason names why a driver that started has already exited, so the
// operator reads a cause instead of a dead pid. The newest per-iteration log
// carries the driver's own verdict (starvation halt, breaker trip, lock
// refusal); driver.log carries only the startup banners, because the driver
// tails the iteration log into it at the END of an iteration.
func stoppedReason(repo string, pid int) string {
	if tail := lastNonEmpty(logTail(repo)); tail != "" {
		return "exited: " + tail
	}
	// A driver that halted at the loop head never opened an iteration log:
	// the starvation halt, the iteration cap, and a lock refusal are all
	// announced on the driver's own stdout before phase work starts.
	if tail := lastNonEmpty(readDriverLog(repo)); tail != "" {
		return "exited: " + tail
	}
	if row := lastLedgerRow(repo); row != "" {
		return "exited; last ledger row: " + row
	}
	return fmt.Sprintf("exited before writing any state (pid %d)", pid)
}

// readDriverLog returns driver.log (the nohup/hub stdout of the driver).
func readDriverLog(repo string) string {
	raw, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "driver.log"))
	if err != nil {
		return ""
	}
	return string(raw)
}

// logTail returns the trailing lines of the newest .selfbuild/logs/loop-N.log.
func logTail(repo string) string {
	dir := filepath.Join(repo, ".selfbuild", "logs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	newest := ""
	highest := -1
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "loop-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		n, cerr := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "loop-"), ".log"))
		if cerr == nil && n > highest {
			highest, newest = n, filepath.Join(dir, name)
		}
	}
	if newest == "" {
		return ""
	}
	raw, rerr := os.ReadFile(newest)
	if rerr != nil {
		return ""
	}
	return string(raw)
}

func lastNonEmpty(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}

func lastLedgerRow(repo string) string {
	raw, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "ledger.jsonl"))
	if err != nil {
		return ""
	}
	return lastNonEmpty(string(raw))
}

// countSelfbuildIssues reads the depth of the deterministic tracker lane:
// open issues carrying the loop's label. gh resolves the repository from the
// checkout's own origin, so no owner/repo slug is threaded through here, and
// the read is bounded — `up` must not hang on a dead network to report a lane.
func countSelfbuildIssues(dir string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "list",
		"--state", "open", "--label", "selfbuild", "--limit", "200", "--json", "number")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return 0, fmt.Errorf("gh issue list timed out: %w", ctx.Err())
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, errors.New(msg)
	}
	var rows []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return 0, fmt.Errorf("gh issue list output unreadable: %w", err)
	}
	return len(rows), nil
}

// gitDirty answers "does this path have uncommitted changes" with git's own
// exit code (`git diff --quiet -- <path>`), the same probe the driver's PRD
// currency gate uses — so `up` and the driver can never disagree about it.
func gitDirty(dir, path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--quiet", "--", path)
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// exit 1 is git's "there IS a difference"; anything else is a
			// git failure (not a repo, bad path) and must not read as dirty.
			return ee.ExitCode() == 1, nil
		}
		if ctx.Err() != nil {
			return false, fmt.Errorf("git diff timed out: %w", ctx.Err())
		}
		return false, err
	}
	return false, nil
}

// scoutInterval reads the researcher's cadence from devagent.json
// (`scout.intervalMinutes`), falling back to the 30-minute default
// scripts/install-scout-launchagent.sh uses so both routes tick alike.
func scoutInterval(repo string) int {
	if cfg, err := config.Load(repo); err == nil && cfg.Scout != nil && cfg.Scout.IntervalMinutes != nil {
		if n := int(*cfg.Scout.IntervalMinutes); n > 0 {
			return n
		}
	}
	return 30
}
