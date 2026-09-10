// Go port of src/workers/herdr-runtime.ts (FR-VIS-01 routing decision +
// loud fallback). HerdrPaneRunner is the pane-vs-direct seam: nil until
// WireHerdrPaneRunner installs the herdr runtime (FR-GO-10 / issue #291b),
// and every uninstalled launch takes the direct child-process path —
// loudly, once per spawn site, exactly like the TS fallback when herdr is
// unreachable.

package workers

import (
	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/herdr"
	"os"
	"regexp"
	"sync"
)

// RunWorkerCliOptions extends SpawnCliOptions with the herdr routing flag.
type RunWorkerCliOptions struct {
	SpawnCliOptions
	// Herdr routes this launch through the herdr pane runtime when set;
	// nil = decide via env/visibility.
	Herdr *bool
}

// HerdrPaneRunner routes a launch through the herdr pane runtime. Nil = the
// direct-spawn fallback; WireHerdrPaneRunner installs the production
// implementation and tests may inject their own.
var HerdrPaneRunner func(cmd string, args []string, opts SpawnCliOptions) (res SpawnCliResult, ok bool, err error)

// WireHerdrPaneRunner connects the seam to the production herdr pane
// runtime (FR-GO-10 / FR-VAL-03 issue #291b): before this runs,
// HerdrPaneRunner is nil and every launch takes the direct-spawn fallback
// regardless of visibility. Called from cli.Execute; tests keep the nil
// seam and inject their own runner.
func WireHerdrPaneRunner() {
	HerdrPaneRunner = func(cmd string, args []string, opts SpawnCliOptions) (SpawnCliResult, bool, error) {
		var wd *herdr.WatchdogContext
		if opts.WatchdogLedger != nil {
			wd = &herdr.WatchdogContext{
				RepoPath: opts.WatchdogLedger.RepoPath,
				TaskID:   opts.WatchdogLedger.TaskId,
				Attempt:  opts.WatchdogLedger.Attempt,
				Worker:   opts.WatchdogLedger.Worker,
			}
		}
		res, err := herdr.RunCommandInHerdrPane(herdr.ExecRunner{}, cmd, args, herdr.PaneRunOptions{
			Dir:                 opts.Dir,
			TimeoutMs:           opts.TimeoutMs,
			Env:                 opts.Env,
			ReplaceEnv:          opts.ReplaceEnv,
			NoProgressTimeoutMs: derefInt(opts.NoProgressTimeoutMs),
			ColdStartTimeoutMs:  opts.ColdStartTimeoutMs,
			Watchdog:            wd,
		})
		if err != nil {
			return SpawnCliResult{}, false, err
		}
		if res == nil {
			return SpawnCliResult{}, false, nil
		}
		return SpawnCliResult{
			ExitCode:  res.ExitCode,
			Stdout:    res.Stdout,
			Stderr:    res.Stderr,
			TimedOut:  res.TimedOut,
			ColdStart: res.ColdStart,
		}, true, nil
	}
}

// fallbackWarnedSites dedupes the fallback warning PER SPAWN SITE (the
// worker CLI name) so an operator sees one loud line per CLI per process —
// not one per launch and not one per process (FR-VIS-01 "no silent
// fallbacks": a silently headless run is indistinguishable from a healthy
// one in analytics, so the fallback must be observable).
var (
	fallbackMu          sync.Mutex
	fallbackWarnedSites = map[string]bool{}
	fallbackSink        func(site string, message string)
)

// SetFallbackSink captures fallback warnings instead of stderr and clears
// dedupe state — the TS test seam.
func SetFallbackSink(fn func(site string, message string)) {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	fallbackSink = fn
	fallbackWarnedSites = map[string]bool{}
}

// WarnFallbackOnce emits the one-time-per-site fallback warning: stderr by
// default, the injected sink in tests.
func WarnFallbackOnce(site string, message string) {
	fallbackMu.Lock()
	defer fallbackMu.Unlock()
	if fallbackWarnedSites[site] {
		return
	}
	fallbackWarnedSites[site] = true
	if fallbackSink != nil {
		fallbackSink(site, message)
		return
	}
	_, _ = os.Stderr.WriteString("[herdr:" + site + "] " + message + "\n")
}

// RunWorkerCli executes a worker CLI launch either inside a herdr pane
// (opts.Herdr, or the visibility-derived default from ShouldUseHerdr) or as
// a direct child process. When herdr is unavailable or misbehaves, it falls
// back to direct execution — workers must keep running; the runtime is a
// visibility enhancement, never a hard dependency. Every fallback is loud,
// once per spawn site (WarnFallbackOnce).
func RunWorkerCli(cmd string, args []string, opts RunWorkerCliOptions) SpawnCliResult {
	if ShouldUseHerdr(opts.Herdr) {
		if HerdrPaneRunner == nil {
			WarnFallbackOnce(cmd, "herdr session not reachable; running "+cmd+" directly")
		} else {
			res, ok, err := HerdrPaneRunner(cmd, args, opts.SpawnCliOptions)
			if err != nil {
				WarnFallbackOnce(cmd, "herdr pane run failed ("+err.Error()+"); running "+cmd+" directly")
			} else if ok {
				return res
			} else {
				WarnFallbackOnce(cmd, "herdr session not reachable; running "+cmd+" directly")
			}
		}
	}
	return SpawnCli(cmd, args, opts.SpawnCliOptions)
}

var herdrFalseRe = regexp.MustCompile(`(?i)^false$`)

// ShouldUseHerdr is the herdr routing decision. Precedence: explicit
// per-spawn flag wins; then DEVAGENT_HERDR=1|0 (legacy env-wide override);
// then DEVAGENT_VISIBILITY ("headless" forces direct spawn, "visible"
// routes to panes); then the configured spawn.visibility, which defaults to
// visible — FR-VIS-01 flips the historical default so worker launches are
// observable unless the operator opts out. DEVAGENT_HERDR stays ahead of
// visibility so an explicit runtime toggle keeps working during the
// rollout.
func ShouldUseHerdr(explicit *bool) bool {
	if explicit != nil {
		return *explicit
	}
	if env := os.Getenv("DEVAGENT_HERDR"); env != "" {
		return env != "0" && !herdrFalseRe.MatchString(env)
	}
	return visibility() != "headless"
}

func visibility() string {
	var cfg config.Config
	if SpawnVisibilityConfig != nil {
		cfg = *SpawnVisibilityConfig
	}
	return config.SpawnVisibility(cfg)
}
