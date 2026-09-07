// Go port of the streaming half of src/workers/spawn-utils.ts (FR-GO-05):
// spawnCli's watchdog routing, the streaming variant with the progress
// clocks (no-progress + Q31 cold-start), and the Q30 budget resolver.
//
// The env blocklist/PATH fallback/PWD sync themselves live in
// internal/spawn (BuildEnv) and are reused here — single source of truth.
package workers

import (
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/spawn"
)

// SpawnCliOptions mirrors TS SpawnCliOptions. NoProgressTimeoutMs models
// the TS optional field (nil = unset); ColdStartTimeoutMs is 0-disabled.
type SpawnCliOptions struct {
	Dir       string
	TimeoutMs int
	// Env is merged over the base environment (or replaces it with
	// ReplaceEnv). Never logged or included in results.
	Env        map[string]string
	ReplaceEnv bool
	// NoProgressTimeoutMs: kill the child when no output (stdout or
	// stderr) arrives for this long. Nil/0 disables.
	NoProgressTimeoutMs *int
	// ColdStartTimeoutMs: Q31 cold-start budget — kill the child when no
	// adapter-classified progress line arrives within this long of launch
	// start. 0 disables.
	ColdStartTimeoutMs int
	// WatchdogLedger: Q34 structured watchdog-health ledger context (rows
	// require an armed clock). Nil = no row.
	WatchdogLedger *WatchdogLedgerContext
	// WatchdogSink receives the watchdog-health row when a ledger context
	// is present and a clock is armed.
	// TODO(FR-GO-05 #190): replace with the orchestrator ledger port
	// (appendWatchdogHealthRecord) once the ledger package lands.
	WatchdogSink func(WatchdogHealthRecord)
}

// SpawnCliResult mirrors TS SpawnCliResult.
type SpawnCliResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	TimedOut bool
	// ColdStart: true only when the cold-start (first-progress) deadline
	// killed the launch.
	ColdStart bool
}

// WatchdogHealthRecord mirrors the TS watchdog-health ledger row (Q34).
// JSON tags match the TS row field names byte-for-byte.
type WatchdogHealthRecord struct {
	Ts                  string `json:"ts"`
	Kind                string `json:"kind"`
	Event               string `json:"event"`
	TaskId              string `json:"taskId"`
	Attempt             int    `json:"attempt"`
	Worker              string `json:"worker"`
	Site                string `json:"site"`
	Runtime             string `json:"runtime"`
	Visible             bool   `json:"visible"`
	Visibility          string `json:"visibility"`
	NoProgressTimeoutMs int    `json:"noProgressTimeoutMs"`
	WatchdogFired       bool   `json:"watchdogFired"`
	ColdStartFired      bool   `json:"coldStartFired"`
	WallClockMs         int64  `json:"wallClockMs"`
	ClockResets         int    `json:"clockResets"`
	// MeaningfulBytes: TS counts UTF-16 code units (String.length); the Go
	// port counts UTF-8 bytes. Ledger-only diagnostic — cross-runtime
	// divergence documented per the byte-parity contract.
	MeaningfulBytes int   `json:"meaningfulBytes"`
	IdleMs          int64 `json:"idleMs"`
}

// ResolveNoProgressTimeoutMs resolves the no-progress watchdog budget for
// one launch (PRD Q30) — the single place the spawn path decides, reading
// the adapter's declared capability instead of per-adapter copies.
//
// Precedence, preserving every adapter's existing default exactly:
//  1. An explicit positive caller value always wins.
//  2. An explicit 0 disables the watchdog only for an adapter that declares
//     a 0 default. An adapter declaring a nonzero budget treats 0 as unset —
//     that declaration is a floor, because those CLIs need an armed clock
//     (silent-provider retries; pi additionally routes on it to close
//     stdin, 2026-09-01 smoke).
//  3. DEVAGENT_NO_PROGRESS_TIMEOUT_MS (operator-wide default) when positive.
//  4. The adapter's declared default; 0 when it declares no capabilities.
//
// Explicit is nil when the caller passed nothing (TS undefined).
func ResolveNoProgressTimeoutMs(explicit *int, capabilities *WorkerCapabilities) int {
	declared := 0
	if capabilities != nil {
		declared = capabilities.DefaultNoProgressTimeoutMs
	}
	if explicit != nil && *explicit > 0 {
		return *explicit
	}
	if explicit != nil && *explicit == 0 && declared == 0 {
		return 0
	}
	if env := os.Getenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS"); env != "" {
		// TS: Number(env) + Number.isFinite + > 0. 'Infinity' parses but is
		// not finite; 'not-a-number' does not parse; both fall through.
		if n, err := strconv.ParseFloat(env, 64); err == nil && !math.IsInf(n, 0) && !math.IsNaN(n) && n > 0 {
			return int(n)
		}
	}
	return declared
}

// SpawnCli runs a CLI to completion with a hard timeout plus optional
// no-progress watchdog. On timeout (wall or idle) the child is killed with
// SIGKILL and TimedOut=true is returned (exitCode -1) instead of an error,
// so callers can map it to WorkerResult. With a watchdog armed it routes to
// the streaming variant; otherwise it delegates to spawn.RunCli (the
// execFile-equivalent path git/gh callers use).
func SpawnCli(name string, args []string, opts SpawnCliOptions) SpawnCliResult {
	noProgressMs := derefInt(opts.NoProgressTimeoutMs)
	if noProgressMs > 0 || opts.ColdStartTimeoutMs > 0 {
		return spawnCliStreaming(name, args, opts)
	}
	r := spawn.RunCli(name, args, spawn.Options{
		Dir:        opts.Dir,
		TimeoutMs:  opts.TimeoutMs,
		Env:        opts.Env,
		ReplaceEnv: opts.ReplaceEnv,
	})
	return SpawnCliResult{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr, TimedOut: r.TimedOut}
}

// isMeaningfulChunk: a chunk counts when ANY line evidences new work (tool
// call or text); thinking-only chunks never reset the watchdog clock
// (PRD Q33).
func isMeaningfulChunk(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if IsNdjsonProgressLine(line) {
			return true
		}
	}
	return false
}

// spawnCliStreaming is the streaming variant with the progress clocks.
// Only adapter-classified progress lines (progress.go) reset the
// no-progress clock; raw output does not. Q31 adds the cold-start
// deadline: until the first progress line, ColdStartTimeoutMs is the
// binding budget. When either fires, the child is SIGKILLed.
func spawnCliStreaming(name string, args []string, opts SpawnCliOptions) SpawnCliResult {
	noProgressMs := derefInt(opts.NoProgressTimeoutMs)
	coldStartMs := opts.ColdStartTimeoutMs
	start := time.Now()

	env := spawn.BuildEnv(spawn.Options{
		Dir:        opts.Dir,
		TimeoutMs:  opts.TimeoutMs,
		Env:        opts.Env,
		ReplaceEnv: opts.ReplaceEnv,
	})

	var (
		mu              sync.Mutex
		finished        bool
		timedOut        bool
		watchdogFired   bool
		coldStartFired  bool
		exitCode        = -1 // TS null until close/error
		lastProgressAt  = start
		meaningfulBytes int
		clockResets     int
	)
	var stdoutBuf, stderrBuf strings.Builder
	var wallTimer, forceTimer *time.Timer
	watchdogStop := make(chan struct{})
	var watchdogStopped sync.Once

	finishCh := make(chan SpawnCliResult, 1)
	finish := func() {
		mu.Lock()
		if finished {
			mu.Unlock()
			return
		}
		finished = true
		timedOutSnapshot := timedOut
		wdFired, csFired := watchdogFired, coldStartFired
		resets, mbytes := clockResets, meaningfulBytes
		idleMs := time.Since(lastProgressAt)
		wallClockMs := time.Since(start)
		exit := -1
		if !timedOutSnapshot {
			exit = exitCode
		}
		stdoutOut, stderrOut := stdoutBuf.String(), stderrBuf.String()
		// Stop the wall/force timers under the same lock that guards their
		// assignment (the AfterFunc callback in the main goroutine writes
		// forceTimer under mu too).
		if wallTimer != nil {
			wallTimer.Stop()
		}
		if forceTimer != nil {
			forceTimer.Stop()
		}
		mu.Unlock()
		watchdogStopped.Do(func() { close(watchdogStop) })

		// Q34: watchdog-health ledger row; requires an armed clock.
		if opts.WatchdogLedger != nil && (noProgressMs > 0 || coldStartMs > 0) && opts.WatchdogSink != nil {
			opts.WatchdogSink(WatchdogHealthRecord{
				Ts:     time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
				Kind:   "event",
				Event:  "watchdog-health",
				TaskId: opts.WatchdogLedger.TaskId,
				// FR-VIS: direct exec child — never operator-visible. When
				// the operator asked for headless the row says so; otherwise
				// this is a fallback from an attempted pane spawn
				// (RunWorkerCli downgrades).
				Runtime:             "direct",
				Visible:             false,
				Visibility:          visibilityLabel(),
				NoProgressTimeoutMs: noProgressMs,
				WatchdogFired:       wdFired,
				ColdStartFired:      csFired,
				WallClockMs:         wallClockMs.Milliseconds(),
				ClockResets:         resets,
				MeaningfulBytes:     mbytes,
				IdleMs:              idleMs.Milliseconds(),
				Site:                "spawn-cli",
				Attempt:             opts.WatchdogLedger.Attempt,
				Worker:              opts.WatchdogLedger.Worker,
			})
		}

		finishCh <- SpawnCliResult{
			ExitCode:  exit,
			Stdout:    stdoutOut,
			Stderr:    stderrOut,
			TimedOut:  timedOutSnapshot,
			ColdStart: csFired,
		}
	}

	cmd := exec.Command(name, args...)
	cmd.Dir = opts.Dir
	cmd.Env = envSlice(env)

	drainWG := sync.WaitGroup{}
	if stdoutPipe, err := cmd.StdoutPipe(); err == nil {
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			drainProgress(stdoutPipe, func(s string, meaningful bool) {
				mu.Lock()
				stdoutBuf.WriteString(s)
				if meaningful {
					meaningfulBytes += len(s)
					clockResets++
					lastProgressAt = time.Now()
				}
				mu.Unlock()
			})
		}()
	}
	if stderrPipe, err := cmd.StderrPipe(); err == nil {
		drainWG.Add(1)
		go func() {
			defer drainWG.Done()
			drainProgress(stderrPipe, func(s string, meaningful bool) {
				mu.Lock()
				stderrBuf.WriteString(s)
				if meaningful {
					meaningfulBytes += len(s)
					clockResets++
					lastProgressAt = time.Now()
				}
				mu.Unlock()
			})
		}()
	}
	// Close our end of stdin immediately: the prompt comes via argv, and an
	// open stdin makes `omp -p` sit in readPipedInput until the pipe closes
	// (2026-09-03 preflight lesson).
	if stdin, err := cmd.StdinPipe(); err == nil {
		_ = stdin.Close()
	}

	if err := cmd.Start(); err != nil {
		// ENOENT etc: no stdout/stderr, not a timeout. stderr carries the
		// error message like the TS 'error' event handler.
		mu.Lock()
		exitCode = -1
		stderrBuf.WriteString(err.Error())
		mu.Unlock()
		finish()
		return <-finishCh
	}

	go func() {
		err := cmd.Wait()
		mu.Lock()
		if err == nil {
			exitCode = 0
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		// Other wait errors (already killed by us): keep the prior code.
		mu.Unlock()
		// Node's 'close' event fires only after all stdio streams flush;
		// mirror that by waiting for the pipe drains so stdout/stderr are
		// fully captured before the result snapshot.
		drainWG.Wait()
		finish()
	}()

	// Hard wall clock. On expiry: SIGKILL, then a 1s force-finish fallback
	// in case the close path never lands (TS wallTimer semantics).
	if opts.TimeoutMs > 0 {
		mu.Lock()
		wallTimer = time.AfterFunc(time.Duration(opts.TimeoutMs)*time.Millisecond, func() {
			mu.Lock()
			timedOut = true
			_ = cmd.Process.Kill()
			forceTimer = time.AfterFunc(1000*time.Millisecond, finish)
			mu.Unlock()
		})
		mu.Unlock()
	}

	// Arm the watchdog poll on whichever clock is tighter; both branches stay
	// gated by their own budget, so a cold-start-only launch polls at the
	// cold-start cadence and vice versa.
	armedClockMs := 0
	if noProgressMs > 0 && coldStartMs > 0 {
		armedClockMs = minInt(noProgressMs, coldStartMs)
	} else {
		armedClockMs = maxInt(noProgressMs, coldStartMs)
	}
	if armedClockMs > 0 {
		interval := time.Duration(minInt(1000, maxInt(200, armedClockMs/4))) * time.Millisecond
		watchdog := time.NewTicker(interval)
		go func() {
			defer watchdog.Stop()
			for {
				select {
				case <-watchdogStop:
					return
				case <-watchdog.C:
					mu.Lock()
					if finished || timedOut {
						mu.Unlock()
						continue
					}
					now := time.Now()
					// Q31: first-progress deadline. Until the classifier has
					// seen one progress line (clockResets == 0),
					// coldStartMs is the binding budget: startup chatter
					// that never evidences new work must not keep a wedged
					// plugin/MCP init alive to the 10m silence clock.
					if coldStartMs > 0 && clockResets == 0 && now.Sub(start) >= time.Duration(coldStartMs)*time.Millisecond {
						coldStartFired = true
						timedOut = true
						mu.Unlock()
						_ = cmd.Process.Kill()
						continue
					}
					if noProgressMs > 0 && now.Sub(lastProgressAt) >= time.Duration(noProgressMs)*time.Millisecond {
						watchdogFired = true
						timedOut = true
						mu.Unlock()
						_ = cmd.Process.Kill()
						// Give the close path a chance; the watchdog keeps
						// polling until the wall clock caps it.
						continue
					}
					mu.Unlock()
				}
			}
		}()
	}

	return <-finishCh
}

// drainProgress reads one output pipe in chunks, forwarding every chunk to
// onChunk with its meaningful classification (PRD Q33 per-chunk progress
// classification via the shared NDJSON core).
func drainProgress(r io.Reader, onChunk func(s string, meaningful bool)) {
	buf := make([]byte, 64*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			s := string(buf[:n])
			onChunk(s, isMeaningfulChunk(s))
		}
		if err != nil {
			return
		}
	}
}

// SpawnVisibilityConfig is the config used to resolve spawn visibility for
// the watchdog-health row. nil = env-only resolution (DEVAGENT_VISIBILITY),
// which is what config.SpawnVisibility does with a zero Config.
// TODO(FR-GO-07 #188): the dispatcher/orchestrator port sets this from the
// loaded config, mirroring TS spawnVisibility(loadConfig()).
var SpawnVisibilityConfig *config.Config

func visibilityLabel() string {
	var cfg config.Config
	if SpawnVisibilityConfig != nil {
		cfg = *SpawnVisibilityConfig
	}
	if config.SpawnVisibility(cfg) == "headless" {
		return "headless"
	}
	return "fallback"
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
