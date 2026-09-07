// Worker launch routing (src/workers/herdr-runtime.ts): execute a worker CLI
// launch either inside a herdr pane or as a direct child process. When herdr
// is unavailable or misbehaves, falls back to direct execution — workers must
// keep running; the runtime is a visibility enhancement, never a hard
// dependency. Every fallback is loud, once per spawn site.
package herdr

import (
	"fmt"
	"os"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/spawn"
)

// RunWorkerOptions mirrors RunWorkerCliOptions (SpawnCliOptions + herdr flag).
type RunWorkerOptions struct {
	Dir        string
	TimeoutMs  int
	Env        map[string]string
	ReplaceEnv bool
	// Herdr routes this launch through the herdr pane runtime; nil defers to
	// ShouldUseHerdr (visibility-derived default).
	Herdr *bool
}

// ShouldUseHerdr mirrors shouldUseHerdr(): herdr routing decision.
// Precedence: explicit per-spawn flag wins; then DEVAGENT_HERDR=1|0 (legacy
// env-wide override); then DEVAGENT_VISIBILITY ("headless" forces direct
// spawn, "visible" routes to panes); then the configured spawn.visibility,
// which defaults to visible — FR-VIS-01 flips the historical default so worker
// launches are observable unless the operator opts out. DEVAGENT_HERDR stays
// ahead of visibility so an explicit runtime toggle keeps working during the
// rollout.
//
// The error mirrors the TS throw path: resolving spawn visibility loads
// devagent.json, and an invalid config propagates (TS calls spawnVisibility()
// outside runWorkerCli's try/catch).
func ShouldUseHerdr(explicit *bool) (bool, error) {
	if explicit != nil {
		return *explicit, nil
	}
	if env, present := os.LookupEnv("DEVAGENT_HERDR"); present && env != "" {
		return env != "0" && !strings.EqualFold(env, "false"), nil
	}
	visibility := os.Getenv("DEVAGENT_VISIBILITY")
	if visibility != "visible" && visibility != "headless" {
		cfg, err := loadConfigOrEmpty(cwdOrEmpty())
		if err != nil {
			return false, err
		}
		// config.SpawnVisibility re-checks the env (a no-op here) and applies
		// the config > "visible" precedence.
		visibility = config.SpawnVisibility(cfg)
	}
	return visibility != "headless", nil
}

// fallbackWarnedSites is the per-site (worker CLI name) fallback-warning
// dedupe, so an operator sees one loud line per CLI (omp, claude-code, ...)
// per process — not one per launch and not one per process (FR-VIS-01 "no
// silent fallbacks": a silently headless run is indistinguishable from a
// healthy one in analytics, so the fallback must be observable).
var fallbackWarnedSites = map[string]bool{}

// fallbackSink mirrors setFallbackSink's module state: test seam capturing
// fallback warnings instead of stderr.
var fallbackSink func(site, message string)

// SetFallbackSink mirrors setFallbackSink: install the sink (nil restores
// stderr) and clear dedupe state.
func SetFallbackSink(fn func(site, message string)) {
	fallbackSink = fn
	fallbackWarnedSites = map[string]bool{}
}

// WarnFallbackOnce mirrors warnFallbackOnce(): emit the one-time-per-site
// fallback warning — the injected sink in tests, stderr by default.
func WarnFallbackOnce(site, message string) {
	if fallbackWarnedSites[site] {
		return
	}
	fallbackWarnedSites[site] = true
	if fallbackSink != nil {
		fallbackSink(site, message)
		return
	}
	fmt.Fprintf(os.Stderr, "[herdr:%s] %s\n", site, message)
}

// RunWorkerCli mirrors runWorkerCli(): execute a worker CLI launch either
// inside a herdr pane (opts.Herdr, or the visibility-derived default from
// ShouldUseHerdr) or as a direct child process.
func RunWorkerCli(cli CliRunner, cmd string, args []string, opts RunWorkerOptions) (spawn.Result, error) {
	useHerdr, err := ShouldUseHerdr(opts.Herdr)
	if err != nil {
		return spawn.Result{}, err
	}
	if useHerdr {
		res, paneErr := RunCommandInHerdrPane(cli, cmd, args, PaneRunOptions{
			Dir:        opts.Dir,
			TimeoutMs:  opts.TimeoutMs,
			Env:        opts.Env,
			ReplaceEnv: opts.ReplaceEnv,
		})
		switch {
		case paneErr != nil:
			WarnFallbackOnce(cmd, fmt.Sprintf("herdr pane run failed (%v); running %s directly", paneErr, cmd))
		case res != nil:
			return spawn.Result{
				ExitCode: res.ExitCode,
				Stdout:   res.Stdout,
				Stderr:   res.Stderr,
				TimedOut: res.TimedOut,
			}, nil
		default:
			WarnFallbackOnce(cmd, fmt.Sprintf("herdr session not reachable; running %s directly", cmd))
		}
	}
	return spawn.RunCli(cmd, args, spawn.Options{
		Dir:        opts.Dir,
		TimeoutMs:  opts.TimeoutMs,
		Env:        opts.Env,
		ReplaceEnv: opts.ReplaceEnv,
	}), nil
}
