// Go port of src/workers/herdr-runtime.ts (FR-VIS-01 routing decision +
// loud fallback). The herdr pane runtime itself is FR-GO-10; until that
// lands, HerdrPaneRunner stays nil and every launch takes the direct
// child-process path — loudly, once per spawn site, exactly like the TS
// fallback when herdr is unreachable.

package workers

import (
	"os"
	"regexp"
	"sync"

	"github.com/FreePeak/devagent/internal/config"
)

// RunWorkerCliOptions extends SpawnCliOptions with the herdr routing flag.
type RunWorkerCliOptions struct {
	SpawnCliOptions
	// Herdr routes this launch through the herdr pane runtime when set;
	// nil = decide via env/visibility.
	Herdr *bool
}

// TODO(FR-GO-10 #193): replace with the integrations/herdr port.
var HerdrPaneRunner func(cmd string, args []string, opts SpawnCliOptions) (res SpawnCliResult, ok bool, err error)

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
