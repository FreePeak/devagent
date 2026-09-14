package scout

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/workers"
)

// This file is the Go port of runScoutOnce/runScoutLoop (FR-SCOUT-01): the
// live scout cycle the `com.devagent.scout` LaunchAgent has been invoking
// since the factory bootstrap. Until this port landed the shipped binary
// exited 3 for every non-replay mode while the LaunchAgent kept launching
// `devagent scout --repo … --interval …` — a silently dead queue writer
// (2026-09-14 audit, the FR-SCOUT-01 revival). The queue half is
// internal/queue (FR-GO-04 #194) and the worker runtime half is
// internal/workers, so the cycle is now fully expressible here.

// DispatchFn runs one worker invocation and returns its raw output stream
// (the text `ExtractScoutPayload` consumes). It is THE test seam: every
// hermetic test injects a canned payload instead of spawning a real CLI.
type DispatchFn func(worker, model, prompt string, timeout time.Duration) (string, error)

// RunOptions carries one live cycle's inputs. Zero values defer to
// config.Load(repo): flags/env the CLI resolved win here, config fills the
// rest, and the constants below are the last-resort defaults.
type RunOptions struct {
	RepoPath string
	Worker   string
	Model    string
	// Timeout is the worker dispatch budget (--timeout, minutes at the CLI).
	Timeout time.Duration
	DryRun  bool
	// Dispatch overrides the real worker spawn. nil + DryRun => NO dispatch
	// at all (see RunOnce: a dry run must never spend a worker turn);
	// nil otherwise => the defaultDispatch path (workers.GetWorker + Spawn).
	Dispatch DispatchFn
	Now      func() time.Time
}

// RunResult is the observable outcome of one cycle — what `scout --once`
// prints, what the heartbeat records, and what the exit code derives from.
type RunResult struct {
	OK      bool
	Queued  bool
	Skipped bool
	TaskID  string
	Title   string
	Status  string
	Detail  string
}

// Cycle statuses: the heartbeat `lastStatus` vocabulary. scout-status
// renders them and operators triage on them, so the strings are part of the
// contract.
const (
	StatusQueued    = "queued"
	StatusDeduped   = "deduped"
	StatusLocked    = "locked"
	StatusQueueFull = "queue-full"
	StatusDryRun    = "dry-run"
)

// defaultMaxQueued caps the scout backlog when the operator never set
// scout.maxQueued. Five is a deliberate ceiling, not a measurement: the
// consume loop works FIFO through one task per PR cycle, and the 2026-09-01
// incident (12 same-goal duplicates in one day) proved an uncapped scout
// manufactures duplicates the builder cannot drain.
const defaultMaxQueued = 5

// defaultScoutTimeoutMinutes is the worker budget when neither --timeout
// nor config.timeoutMinutes speaks. 30 mirrors
// scripts/install-scout-launchagent.sh's TIMEOUT_MIN default so the
// LaunchAgent and a bare run behave identically.
const defaultScoutTimeoutMinutes = 30

// defaultScoutIntervalMinutes only guards RunLoop against interval<=0 (the
// CLI resolves --interval/config before calling). Same 30-minute default as
// the LaunchAgent's `--interval 30`.
const defaultScoutIntervalMinutes = 30

// RunOnce executes exactly one scout cycle. The ordering is load-bearing,
// mirrored from the TS original, and each guard names its incident:
//
//  1. lock        — two overlapping scouts double-enqueue (LaunchAgent +
//     manual run); a LIVE foreign holder means someone else is mid-cycle, so
//     we return Status "locked", Skipped, OK — and write NOTHING, because
//     clobbering the holder's heartbeat would lie to scout-status about who
//     ran.
//  2. config      — Scout.{Worker,Model,MaxQueued,IntervalMinutes}
//     defaults; flags/env passed in opts win.
//  3. queue depth — checked BEFORE dispatch on purpose: dispatch is the
//     expensive step (a real worker run costs minutes and tokens), and a
//     queue-full cycle that dispatched first paid full price to throw the
//     answer away.
//  4. prompt → dispatch → extract → parse — a dispatch ERROR or an
//     unparseable payload falls back to FallbackTask (docs/SCOUT.md: "so
//     the queue never starves"); only the queue write can fail the cycle.
//  5. enqueue     — ErrAlreadyQueued is a DEDUP HIT, not a failure: the
//     deterministic per-UTC-day fallback id relies on it (a random id per
//     cycle accumulated 12 same-goal duplicates on 2026-09-01).
//  6. heartbeat   — every non-locked outcome records itself, including
//     queue-full and enqueue failures: scout-status's staleness gate must
//     see that the scout ALIVE-checked the queue, not guess death from
//     silence.
//
// DryRun performs every step except EnqueueTask/WriteHeartbeat and reports
// Status "dry-run"; with no injected Dispatch it also skips the worker call
// entirely — `scout --once --dry-run` is documented (docs/SCOUT.md) as the
// deterministic-fallback preview with NO AI call, and a preview must never
// spend a real dispatch when no seam provides a fake one.
func RunOnce(opts RunOptions) (RunResult, error) {
	failed := RunResult{Status: "failed"}
	repo := strings.TrimSpace(opts.RepoPath)
	if repo == "" {
		return failed, errors.New("scout: RepoPath is required")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	if !AcquireScoutLock(repo, now()) {
		// Deliberately no ReleaseScoutLock (we do not own it) and no
		// heartbeat (see guard 1): the holder owns both files this window.
		return RunResult{OK: true, Skipped: true, Status: StatusLocked,
			Detail: fmt.Sprintf("live scout lock held by pid %d", ReadScoutLockPid(repo))}, nil
	}
	defer ReleaseScoutLock(repo)

	cfg, err := config.Load(repo)
	if err != nil {
		return failed, err
	}
	var sc *config.ScoutConfig
	if cfg.Scout != nil {
		sc = cfg.Scout
	}
	worker := opts.Worker
	if worker == "" && sc != nil {
		worker = sc.Worker
	}
	if worker == "" {
		worker = cfg.Worker // repo-wide default ("omp"); never empty in practice
	}
	model := opts.Model
	if model == "" && sc != nil {
		model = sc.Model
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		minutes := cfg.TimeoutMinutes
		if minutes <= 0 {
			minutes = defaultScoutTimeoutMinutes
		}
		timeout = time.Duration(minutes) * time.Minute
	}
	maxQueued := defaultMaxQueued
	if sc != nil && sc.MaxQueued != nil && *sc.MaxQueued >= 1 {
		maxQueued = int(*sc.MaxQueued)
	}

	// done/failed rows are excluded on purpose: the cap protects the
	// consume backlog, and terminal rows accumulate forever otherwise.
	depth := len(queue.ListTasks(repo, queue.StatusPending)) + len(queue.ListTasks(repo, queue.StatusClaimed))
	if depth >= maxQueued {
		res := RunResult{OK: true, Skipped: true, Status: StatusQueueFull,
			Detail: fmt.Sprintf("queue depth %d >= maxQueued %d (pending+claimed)", depth, maxQueued)}
		writeCycleHeartbeat(repo, res, worker, sc)
		return res, nil
	}

	prompt := BuildScoutPrompt(repo, cfg, nil)

	// Dispatch resolution (see RunOptions.Dispatch): the live path binds a
	// repo-aware closure over the worker runtime; dry-run without a seam
	// dispatches nothing.
	dispatch := opts.Dispatch
	if dispatch == nil && !opts.DryRun {
		w, t := worker, timeout
		dispatch = func(_, model, prompt string, _ time.Duration) (string, error) {
			return defaultDispatch(repo, w, model, prompt, t)
		}
	}
	var whys []string
	var task *ScoutTask
	if dispatch != nil {
		raw, derr := dispatch(worker, model, prompt, timeout)
		if derr != nil {
			// Fallback policy (docs/SCOUT.md): a missing binary / dead
			// worker must not starve the queue; record WHY so the heartbeat
			// stays diagnosable.
			whys = append(whys, fmt.Sprintf("dispatch failed: %v", derr))
		} else if payload := ExtractScoutPayload(raw, worker); payload != nil {
			task = ParseScoutOutput(*payload)
		}
		if task == nil && derr == nil {
			whys = append(whys, "unparseable scout output")
		}
	} else {
		whys = append(whys, "no dispatch (dry-run)")
	}
	if task == nil {
		fb := FallbackTask(prompt, now())
		task = &fb
	}

	res := RunResult{OK: true, TaskID: task.ID, Title: task.Title}
	if opts.DryRun {
		res.Status = StatusDryRun
		res.Detail = strings.Join(whys, "; ")
		appendDetail(&res, fmt.Sprintf("prompt %d chars, would enqueue %s %q (no queue/heartbeat writes)",
			len(prompt), task.ID, task.Title))
		return res, nil
	}

	_, qerr := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID:                 task.ID,
		Title:              task.Title,
		Goal:               task.Goal,
		AcceptanceCriteria: task.Criteria,
		PrdMarkdown:        task.PRDMarkdown,
		Source:             "scout",
	})
	var already queue.ErrAlreadyQueued
	switch {
	case qerr == nil:
		res.Queued = true
		res.Status = StatusQueued
		res.Detail = fmt.Sprintf("enqueued %s %q", task.ID, task.Title)
	case errors.As(qerr, &already):
		// Dedup hit: the per-day fallback id (or a manual re-run of the
		// same id) already owns the slot. Success, not failure — the queue
		// holds the task; the NEXT day's fallback id gets a fresh slot.
		res.Skipped = true
		res.Status = StatusDeduped
		res.Detail = fmt.Sprintf("task %s already queued", task.ID)
	default:
		res.OK = false
		res.Status = "enqueue-failed"
		res.Detail = qerr.Error()
		writeCycleHeartbeat(repo, res, worker, sc)
		return res, fmt.Errorf("scout enqueue %s: %w", task.ID, qerr)
	}
	for _, w := range whys {
		res.Detail += "; " + w
	}
	writeCycleHeartbeat(repo, res, worker, sc)
	return res, nil
}

// appendDetail joins cycle diagnostics with "; " without ever leading with
// a separator.
func appendDetail(res *RunResult, s string) {
	if res.Detail == "" {
		res.Detail = s
		return
	}
	res.Detail += "; " + s
}

// writeCycleHeartbeat records the cycle. Best-effort by design: a heartbeat
// write failure must not flip an otherwise-good cycle to failed — the queue
// row is the deliverable, the heartbeat is telemetry.
func writeCycleHeartbeat(repo string, res RunResult, worker string, sc *config.ScoutConfig) {
	patch := HeartbeatPatch{
		LastTaskID: ptrIfNotEmpty(res.TaskID),
		LastStatus: ptrIfNotEmpty(res.Status),
		LastDetail: ptrIfNotEmpty(res.Detail),
		Worker:     ptrIfNotEmpty(worker),
	}
	if sc != nil {
		patch.IntervalMinutes = sc.IntervalMinutes
	}
	_, _ = WriteHeartbeat(repo, patch)
}

// ptrIfNotEmpty models JSON.stringify's drop-on-undefined for the heartbeat
// pointers (an empty Detail must OMIT the key, not serialize "").
func ptrIfNotEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// defaultDispatch is the real worker path: GetWorker, then Spawn one
// headless run in the repo. An empty ResultText with a nonzero ExitCode is
// an ERROR, never a silent empty payload — swallowing it would route a dead
// worker (stale binary, missing CLI, killed child) through the same
// fallback branch as chatty-but-useless output and hide the exit code from
// the heartbeat detail (the stale-binary class, loops 285-290).
func defaultDispatch(repo, worker, model, prompt string, timeout time.Duration) (string, error) {
	w, err := workers.GetWorker(worker)
	if err != nil {
		return "", err
	}
	res := w.Spawn(workers.WorkerSpawnOptions{
		Prompt:    prompt,
		Cwd:       repo,
		Model:     model,
		TimeoutMs: int(timeout / time.Millisecond),
	})
	if res.ResultText == "" && res.ExitCode != 0 {
		detail := res.ErrorText
		if detail == "" {
			detail = "no output"
		}
		return "", fmt.Errorf("worker %q exited %d (timedOut=%v): %s", worker, res.ExitCode, res.TimedOut, detail)
	}
	return res.ResultText, nil
}

// RunLoop is the `--interval` daemon: one immediate cycle, then one per
// tick until stop closes. The scout's resilience model is "keep ticking": a
// failed cycle logs and waits for the next tick — the lock, the per-day
// dedup id, and maxQueued bound any real damage, while exiting on the first
// error would hand operators a dead LaunchAgent with one log line to show
// for it. A slow cycle does not stack work: time.Ticker drops ticks while
// RunOnce runs, which is exactly right for a fixed-cadence researcher.
func RunLoop(repo string, interval time.Duration, stop <-chan struct{}, opts RunOptions) {
	if interval <= 0 {
		interval = defaultScoutIntervalMinutes * time.Minute
	}
	opts.RepoPath = repo
	cycle := func() {
		res, err := RunOnce(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[scout] %s: %v\n", res.Status, err)
			return
		}
		fmt.Printf("[scout] %s %s\n", res.Status, res.Detail)
	}
	cycle()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			cycle()
		}
	}
}
