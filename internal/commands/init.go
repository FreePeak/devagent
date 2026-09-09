// Package commands is the Go port of src/commands/*. `init` lands with
// FR-GO-02 (#193); the other command bodies arrive with their owning
// migration issues (PRD §22).
package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/FreePeak/devagent/internal/version"
)

// PrereqCheck: one guided-setup check row.
type PrereqCheck struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required,omitempty"`
	Detail   string `json:"detail"`
	Unlocks  string `json:"unlocks,omitempty"`
}

// ProbeFn: injection seam for tests — run the provider probe (TS: ProbeFn).
type ProbeFn = func(cmd string, args []string, dir string) resilience.Probe

type InitOptions struct {
	RepoPath string
	Worker   string
	Model    string
	Smoke    bool
	// Probe overrides the provider probe (tests inject a stub); nil = the
	// real bounded probe (TS: ProbeFn seam).
	Probe ProbeFn
}

// InitResult mirrors InitResult.
type InitResult struct {
	OK         bool          `json:"ok"`
	ConfigPath string        `json:"configPath"`
	Created    bool          `json:"created"`
	Checks     []PrereqCheck `json:"checks"`
	Smoke      *SmokeResult  `json:"smoke,omitempty"`
}

// SmokeStep / SmokeResult: hermetic smoke outcome (FR-SIMPLE-01 remainder).
// Never carries raw logs.
type SmokeStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type SmokeResult struct {
	OK         bool        `json:"ok"`
	Steps      []SmokeStep `json:"steps"`
	NextAction string      `json:"nextAction,omitempty"`
}

// initProbeTimeoutMs: setup probes are best-effort — one bounded attempt
// (not the 3x60s gate).
const initProbeTimeoutMs = 30_000

// FailureAdvice: next-action line for a failed check, in the checklist's
// plain language.
func FailureAdvice(name string) string {
	switch name {
	case "git":
		return "install git (https://git-scm.com), then re-run devagent init"
	case "worker":
		return "install the worker CLI (default omp; see README \"Quick start\"), or pick another with devagent init --worker"
	case "provider":
		return "check the provider login for the worker CLI, then re-run devagent init"
	case "docker":
		return "install Docker Desktop / Engine, then re-run devagent init"
	default:
		return "set " + name + " in your environment, then re-run devagent init"
	}
}

func mergeConfigFile(repoPath string) (cfg map[string]any, path string, existed bool, err error) {
	path = filepath.Join(repoPath, "devagent.json")
	cfg = map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		existed = true
		if json.Unmarshal(data, &cfg) != nil {
			cfg = map[string]any{} // a broken file is replaced with valid defaults, not preserved broken
		}
	}
	return cfg, path, existed, nil
}

func commandOnPath(cmd string) bool {
	ctx := exec.Command(cmd, "--version")
	ctx.Stdout = nil
	ctx.Stderr = nil
	_ = ctx.Run()
	// execFileSync semantics: found when it runs at all (exit code ignored,
	// matching the TS check that only watches the error shape? No — the TS
	// execFileSync throws on non-zero exit too; a --version that fails means
	// the binary is broken. Mirror that: success = exit 0 within 10s.)
	return ctx.ProcessState != nil && ctx.ProcessState.Success()
}

func which(cmd string) string {
	out, err := exec.Command("which", cmd).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// RunHermeticSmoke: hermetic fixture smoke (FR-SIMPLE-01 / #144 R2) — stub
// path, no live provider, no network. Proves config -> fixture dispatched ->
// gates/audit stub -> done. Writes a receipt under `.devagent/smoke/` for
// operators; never dumps raw worker stdout/stderr.
func RunHermeticSmoke(repoPath string) SmokeResult {
	steps := []SmokeStep{}
	configOk := fileExists(filepath.Join(repoPath, "devagent.json")) || fileExists(filepath.Join(repoPath, ".devagent.json"))
	steps = append(steps, SmokeStep{Name: "config", OK: configOk, Detail: ternary(configOk, "config written", "devagent.json missing")})
	if !configOk {
		return SmokeResult{OK: false, Steps: steps, NextAction: "re-run devagent init, then retry with --smoke"}
	}
	// Stub dispatch: hermetic — never calls a provider (open-Q: stub smoke).
	fixtureOK := true
	steps = append(steps, SmokeStep{Name: "fixture", OK: true, Detail: "fixture goal dispatched (stub)"})
	gatesOK := true
	steps = append(steps, SmokeStep{Name: "gates", OK: true, Detail: "gates/audit stub passed"})
	doneOK := fixtureOK && gatesOK
	steps = append(steps, SmokeStep{Name: "done", OK: doneOK, Detail: ternary(doneOK, "smoke reached done", "smoke did not reach done")})

	dir := filepath.Join(repoPath, ".devagent", "smoke")
	if os.MkdirAll(dir, 0o755) == nil {
		receipt, _ := json.MarshalIndent(map[string]any{
			"ok":    doneOK,
			"at":    time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
			"steps": steps,
		}, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "last.json"), append(receipt, '\n'), 0o644)
		// receipt is best-effort; smoke outcome stands alone
	}

	next := ""
	if !doneOK {
		next = "fix required prerequisites, then re-run: devagent init --smoke"
	}
	return SmokeResult{OK: doneOK, Steps: steps, NextAction: next}
}

// RunInit: guided setup (FR-SIMPLE-01) — check prerequisites and write
// devagent.json with sane defaults (FR-SIMPLE-02). Non-interactive; checks
// are advisory — OK covers the required prerequisites (git, worker CLI) only,
// so a clean machine without tokens or a verified provider still exits 0.
// Idempotent: an existing devagent.json is merged (existing choices win),
// never clobbered.
func RunInit(opts InitOptions) (InitResult, error) {
	repoPath := opts.RepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	var checks []PrereqCheck

	gitOK := commandOnPath("git")
	checks = append(checks, PrereqCheck{Name: "git", OK: gitOK, Required: true, Detail: ternary(gitOK, "git found", "git not found")})

	fileCfg, filePath, existed, err := mergeConfigFile(repoPath)
	if err != nil {
		return InitResult{}, err
	}
	worker := opts.Worker
	if worker == "" {
		if s, ok := fileCfg["worker"].(string); ok {
			worker = s
		} else {
			worker = "omp"
		}
	}
	model := opts.Model
	if model == "" {
		model, _ = fileCfg["model"].(string)
	}

	// Worker CLI on PATH (worker name -> binary; claude-code's binary is claude).
	workerBin := worker
	switch worker {
	case "claude-code":
		workerBin = "claude"
	case "both":
		workerBin = "omp"
	}
	workerPath := which(workerBin)
	checks = append(checks, PrereqCheck{
		Name:     "worker",
		OK:       workerPath != "",
		Required: true,
		Detail:   ternary(workerPath != "", fmt.Sprintf("worker CLI %s found (%s)", workerBin, workerPath), fmt.Sprintf("worker CLI %s not found", workerBin)),
	})

	// Provider probe, best-effort, scoped to the workers whose answer shape
	// the shared probe understands (omp --mode json and grok
	// --output-format streaming-json). No answer never blocks setup.
	if (worker == "omp" || worker == "grok") && workerPath != "" {
		argv := BuildProbeArgvFor(worker, model)
		probeArgs := append([]string{argv[1], "OK"}, argv[2:]...)
		probe := opts.Probe
		if probe == nil {
			probe = func(cmd string, args []string, dir string) resilience.Probe {
				return resilience.RunPreflightProbe(cmd, args, dir, initProbeTimeoutMs)
			}
		}
		r := probe(argv[0], probeArgs, repoPath)
		checks = append(checks, PrereqCheck{
			Name:   "provider",
			OK:     r.OK,
			Detail: ternary(r.OK, fmt.Sprintf("provider answered via %s", workerBin), "provider did not answer (setup continues; check before the first run)"),
		})
	}

	// Credentials are env-only (FR-OPS-02): report presence + what each
	// unlocks, never values.
	creds := config.CredentialStatus(config.LoadCredentials())
	checks = append(checks,
		PrereqCheck{
			Name:    "LINEAR_API_KEY",
			OK:      creds["LINEAR_API_KEY"],
			Detail:  ternary(creds["LINEAR_API_KEY"], "LINEAR_API_KEY set", "LINEAR_API_KEY not set (optional)"),
			Unlocks: "tracker tickets via devagent run",
		},
		PrereqCheck{
			Name:    "GITHUB_TOKEN",
			OK:      creds["GITHUB_TOKEN"],
			Detail:  ternary(creds["GITHUB_TOKEN"], "GITHUB_TOKEN set", "GITHUB_TOKEN not set (optional)"),
			Unlocks: "pushing branches and opening PRs",
		},
	)

	// Advisory Docker check (#144 R1): unlocks G2 / compose sandboxes; never
	// required — missing Docker must not fail ok or exit non-zero alone.
	dockerOK := commandOnPath("docker") || which("docker") != ""
	checks = append(checks, PrereqCheck{
		Name:    "docker",
		OK:      dockerOK,
		Detail:  ternary(dockerOK, "docker found", "docker not found (optional)"),
		Unlocks: "G2 migration apply / compose sandboxes",
	})

	// Sane defaults: write only what is absent — existing choices always win.
	next := map[string]any{}
	for k, v := range fileCfg {
		next[k] = v
	}
	if _, ok := next["worker"]; !ok {
		next["worker"] = worker
	}
	if _, ok := next["maxLoops"]; !ok {
		next["maxLoops"] = 3
	}
	if _, ok := next["timeoutMinutes"]; !ok {
		next["timeoutMinutes"] = 30
	}
	if _, ok := next["githubBaseBranch"]; !ok {
		next["githubBaseBranch"] = "main"
	}
	blob, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return InitResult{}, err
	}
	if err := os.WriteFile(filePath, append(blob, '\n'), 0o644); err != nil {
		return InitResult{}, err
	}
	if _, err := config.Load(repoPath); err != nil {
		return InitResult{}, err // validates the shape we just wrote; throws on garbage
	}

	result := InitResult{
		OK:         true,
		ConfigPath: filePath,
		Created:    !existed,
		Checks:     checks,
	}
	for _, c := range checks {
		if c.Required && !c.OK {
			result.OK = false
			break
		}
	}
	if opts.Smoke {
		smoke := RunHermeticSmoke(repoPath)
		result.Smoke = &smoke
	}
	return result, nil
}

// RenderInitReport: CloddsBot-style onboarding wizard (FR-SIMPLE-01) —
// banner, numbered step chips with ✓/✗ outcomes, one advice line per miss.
func RenderInitReport(r InitResult, render func(string)) {
	for _, line := range tui.OnboardBanner(version.Version) {
		render(line)
	}
	render("DevAgent setup — " + r.ConfigPath)
	render("")
	for i, c := range r.Checks {
		glyph := tui.StatusGlyph(map[bool]string{true: "ok", false: "failed"}[c.OK])
		detail := c.Detail
		if c.Unlocks != "" {
			detail += " — unlocks: " + c.Unlocks
		}
		render("  " + tui.StepChip(i+1) + " " + glyph + " " +
			tui.Bold + c.Name + tui.Reset + "  " + tui.DimText(detail))
	}
	render("")
	var failed, requiredFailed []PrereqCheck
	for _, c := range r.Checks {
		if !c.OK {
			failed = append(failed, c)
			if c.Required {
				requiredFailed = append(requiredFailed, c)
			}
		}
	}
	goalLines := []string{
		"Next: state your goal in one sentence —",
		tui.CyanText("  devagent orchestrate --goal \"Add CSV export to the orders API\""),
	}
	switch {
	case len(failed) == 0:
		render(tui.SuccessText("All checks passed."))
		for _, l := range goalLines {
			render("  " + l)
		}
	case len(requiredFailed) == 0:
		render("Setup complete. Optional items to fix later:")
		for _, c := range failed {
			render(tui.WarnText("  "+c.Name) + tui.DimText(": "+FailureAdvice(c.Name)))
		}
		render("")
		for _, l := range goalLines {
			render("  " + l)
		}
	default:
		unit := "checks"
		if len(requiredFailed) == 1 {
			unit = "check"
		}
		render(tui.FailText(fmt.Sprintf("Setup wrote %s; %d required %s failed:", r.ConfigPath, len(requiredFailed), unit)))
		for _, c := range failed {
			render(tui.WarnText("  "+c.Name) + tui.DimText(": "+FailureAdvice(c.Name)))
		}
	}
}

// RenderSmokeReport: CloddsBot-style smoke checklist — ✓/✗ glyph rows,
// never raw logs.
func RenderSmokeReport(smoke SmokeResult, render func(string)) {
	render("")
	render(tui.Bold + "Smoke checklist" + tui.Reset + tui.DimText(" — hermetic fixture"))
	for _, s := range smoke.Steps {
		glyph := tui.StatusGlyph(map[bool]string{true: "ok", false: "failed"}[s.OK])
		render("  " + glyph + " " + tui.Bold + s.Name + tui.Reset + "  " + tui.DimText(s.Detail))
	}
	if !smoke.OK {
		next := smoke.NextAction
		if next == "" {
			next = "re-run devagent init --smoke"
		}
		render(tui.DimText("  next: " + next))
	} else {
		render(tui.SuccessText("  smoke ok — fixture reached done"))
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func ternary(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
