// Create command body: provisioning for the self-build factory (queue dirs,
// devagent.json merge, LaunchAgent plists, Orca worker worktrees). Port of
// src/create.ts; error strings and printed shapes are byte-identical.

package pipeline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/integrations"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/spawn"
)

// CreateOptions mirrors create.ts CreateOptions.
type CreateOptions struct {
	RepoPath string
	Scout    bool
	// Tracker: progress-tracker agent — devagent track --interval (role 2 of
	// the self-build factory).
	Tracker bool
	// Builder agent: scripts/build-loop.sh consuming the queue (role 3).
	Builder bool
	// Orchestrator agent: scripts/orchestrate-loop.sh driving the DAG board
	// (role 4).
	Orchestrator bool
	// Goal text handed to the orchestrator loop for planning when no board
	// exists.
	OrchestratorGoal     string
	Workers              int
	AutoMerge            bool
	SelfUpdate           bool
	DryRun               bool
	IntervalMinutes      int // 0 = unset (TS default 30)
	ScoutWorker          string
	TrackIntervalMinutes *int // nil = unset (TS default 15)
	// Runner is the Orca CLI seam (mirrors the TS CliRunner injection);
	// nil = spawn.RunCli.
	Runner integrations.OrcaRunner
}

// CreateResult mirrors create.ts CreateResult. Optional TS fields are ""
// /nil when unset.
type CreateResult struct {
	OK                bool     `json:"ok"`
	Detail            string   `json:"detail"`
	Dirs              []string `json:"dirs"`
	ConfigPath        string   `json:"configPath,omitempty"`        // "" = undefined
	LaunchAgentPlist  string   `json:"launchAgentPlist,omitempty"`  // "" = undefined
	LaunchAgentPlists []string `json:"launchAgentPlists,omitempty"` // nil = undefined
	OrcaWorktrees     []string `json:"orcaWorktrees,omitempty"`     // nil = undefined
}

// plistSpec mirrors create.ts PlistSpec.
type plistSpec struct {
	label       string
	logName     string
	programArgs []string
	repoPath    string
	// env entries merged into the plist EnvironmentVariables (e.g.
	// ORCHESTRATOR_GOAL), in insertion order.
	envKeys []string
	envVals map[string]string
}

func createResolveConfigPath(repoPath string) string {
	// Prefer devagent.json when both exist; otherwise devagent.json is the
	// write target
	a := filepath.Join(repoPath, "devagent.json")
	b := filepath.Join(repoPath, ".devagent.json")
	if fileExists(b) && !fileExists(a) {
		return b
	}
	return a
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func createRunner(opts CreateOptions) integrations.OrcaRunner {
	if opts.Runner != nil {
		return opts.Runner
	}
	return func(cmd string, args []string, o spawn.Options) spawn.Result {
		return spawn.RunCli(cmd, args, o)
	}
}

// xmlEsc mirrors the TS xmlEsc (attribute/text escaping for the plist).
func xmlEsc(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// BuildLaunchAgentPlist mirrors buildLaunchAgentPlist. The Go build is a
// single self-contained binary, so the interpreter-argv slots the TS template
// needed collapse onto the running executable path.
func BuildLaunchAgentPlist(spec plistSpec) string {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/tmp"
	}
	logPath := filepath.Join(home, "Library/Logs", spec.logName)
	// launchd default PATH is minimal; embed the current PATH so agents can
	// find opencode/claude/git/node installed in user locations.
	installPath := os.Getenv("PATH")
	if installPath == "" {
		installPath = "/usr/bin:/bin"
	}
	homeEnv := os.Getenv("HOME")
	lines := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		fmt.Sprintf("  <key>Label</key><string>%s</string>", spec.label),
		`  <key>EnvironmentVariables</key>`,
		`  <dict>`,
		fmt.Sprintf(`    <key>PATH</key><string>%s</string>`, xmlEsc(installPath)),
		fmt.Sprintf(`    <key>HOME</key><string>%s</string>`, homeEnv),
	}
	for _, k := range spec.envKeys {
		lines = append(lines, fmt.Sprintf(`    <key>%s</key><string>%s</string>`, xmlEsc(k), xmlEsc(spec.envVals[k])))
	}
	lines = append(lines,
		`  </dict>`,
		`  <key>ProgramArguments</key>`,
		`  <array>`,
	)
	for _, a := range spec.programArgs {
		lines = append(lines, fmt.Sprintf(`    <string>%s</string>`, xmlEsc(a)))
	}
	lines = append(lines,
		`  </array>`,
		fmt.Sprintf(`  <key>WorkingDirectory</key><string>%s</string>`, spec.repoPath),
		`  <key>RunAtLoad</key><true/>`,
		`  <key>KeepAlive</key><true/>`,
		`  <key>StandardOutPath</key><string>`+logPath+`</string>`,
		`  <key>StandardErrorPath</key><string>`+logPath+`</string>`,
		`  <key>ThrottleInterval</key><integer>60</integer>`,
		`</dict>`,
		`</plist>`,
		``,
	)
	return strings.Join(lines, "\n")
}

// RolePlistSpecs mirrors rolePlistSpecs: role → plist spec for the self-build
// factory (scout / tracker / builder / orchestrator).
func RolePlistSpecs(opts CreateOptions, intervalMinutes int, scoutWorker string, trackIntervalMinutes int) []plistSpec {
	var specs []plistSpec
	selfExe := createSelfExePath()
	devagentBin := filepath.Join(opts.RepoPath, "dist", "src", "cli.js")
	if opts.Scout {
		specs = append(specs, plistSpec{
			label:    "com.devagent.scout",
			logName:  "devagent-scout.log",
			repoPath: opts.RepoPath,
			programArgs: []string{
				selfExe, devagentBin, "scout", "--repo", opts.RepoPath,
				"--interval", fmt.Sprintf("%d", intervalMinutes),
				"--worker", scoutWorker, "--timeout", "30",
			},
		})
	}
	if opts.Tracker {
		specs = append(specs, plistSpec{
			label:    "com.devagent.tracker",
			logName:  "devagent-tracker.log",
			repoPath: opts.RepoPath,
			programArgs: []string{
				selfExe, devagentBin, "track", "--repo", opts.RepoPath,
				"--interval", fmt.Sprintf("%d", trackIntervalMinutes),
			},
		})
	}
	if opts.Builder {
		specs = append(specs, plistSpec{
			label:       "com.devagent.builder",
			logName:     "devagent-builder.log",
			repoPath:    opts.RepoPath,
			programArgs: []string{"/bin/bash", filepath.Join(opts.RepoPath, "scripts", "build-loop.sh")},
		})
	}
	if opts.Orchestrator {
		spec := plistSpec{
			label:       "com.devagent.orchestrator",
			logName:     "devagent-orchestrator.log",
			repoPath:    opts.RepoPath,
			programArgs: []string{"/bin/bash", filepath.Join(opts.RepoPath, "scripts", "orchestrate-loop.sh")},
			envKeys:     []string{"ORCHESTRATOR_REPO"},
			envVals:     map[string]string{"ORCHESTRATOR_REPO": opts.RepoPath},
		}
		if opts.OrchestratorGoal != "" {
			spec.envKeys = append(spec.envKeys, "ORCHESTRATOR_GOAL")
			spec.envVals["ORCHESTRATOR_GOAL"] = opts.OrchestratorGoal
		}
		specs = append(specs, spec)
	}
	return specs
}

// createSelfExePath mirrors process.execPath (best effort; never fails the
// run — falls back to the plain binary name).
func createSelfExePath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return "devagent"
}

// installPlist mirrors installPlist: write one plist + best-effort launchctl
// bootstrap. Returns the plist path (never "" on darwin; write errors
// surface to the caller).
func installPlist(spec plistSpec, repoPath string) (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		home = "/tmp"
	}
	launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
	logsDir := filepath.Join(home, "Library", "Logs")
	plistPath := filepath.Join(launchAgentsDir, spec.label+".plist")
	if err := os.MkdirAll(launchAgentsDir, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(plistPath, []byte(BuildLaunchAgentPlist(spec)), 0o644); err != nil {
		return "", err
	}
	uid := os.Getuid()
	// best-effort; not loaded yet — fine
	_ = spawn.RunCli("launchctl", []string{"bootout", fmt.Sprintf("gui/%d/%s", uid, spec.label)}, spawn.Options{Dir: repoPath, TimeoutMs: 5000})
	// best-effort; user can bootstrap manually
	_ = spawn.RunCli("launchctl", []string{"bootstrap", fmt.Sprintf("gui/%d", uid), plistPath}, spawn.Options{Dir: repoPath, TimeoutMs: 5000})
	return plistPath, nil
}

// RunCreate mirrors runCreate: provision the self-build factory for repoPath.
func RunCreate(opts CreateOptions) CreateResult {
	// Normalize early: LaunchAgent plists embed repoPath (ProgramArguments +
	// WorkingDirectory); a relative --repo would produce broken agents.
	repoPath, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		repoPath = opts.RepoPath
	}
	if !fileExists(repoPath) {
		return CreateResult{OK: false, Detail: fmt.Sprintf("repoPath does not exist: %s", opts.RepoPath), Dirs: []string{}}
	}

	intervalMinutes := opts.IntervalMinutes
	if intervalMinutes == 0 {
		intervalMinutes = 30
	}
	scoutWorker := opts.ScoutWorker
	if scoutWorker == "" {
		scoutWorker = "omp"
	}
	trackIntervalMinutes := 15
	if opts.TrackIntervalMinutes != nil {
		trackIntervalMinutes = *opts.TrackIntervalMinutes
	}
	dirs := make([]string, 0)

	if opts.DryRun {
		var plan []string
		plan = append(plan, fmt.Sprintf("would ensure %s and %s", queue.QueueDir(repoPath), queue.PrdsDir(repoPath)))
		plan = append(plan, fmt.Sprintf("would merge %s with %s", createResolveConfigPath(repoPath), dryRunConfigJSON(opts, scoutWorker, intervalMinutes)))
		if opts.Scout {
			plan = append(plan, fmt.Sprintf("would install LaunchAgent plist for scout (%s every %dm)", scoutWorker, intervalMinutes))
		}
		if opts.Tracker {
			plan = append(plan, fmt.Sprintf("would install LaunchAgent plist for tracker (every %dm)", trackIntervalMinutes))
		}
		if opts.Builder {
			plan = append(plan, "would install LaunchAgent plist for builder (scripts/build-loop.sh, poll 300s)")
		}
		if opts.Orchestrator {
			goal := "repo default"
			if opts.OrchestratorGoal != "" {
				goal = `"` + truncateRunes(opts.OrchestratorGoal, 80) + `"`
			}
			plan = append(plan, fmt.Sprintf("would install LaunchAgent plist for orchestrator (scripts/orchestrate-loop.sh, goal: %s)", goal))
		}
		if opts.Workers > 0 {
			plan = append(plan, fmt.Sprintf("would provision %d Orca worktree(s) via orca worktree create", opts.Workers))
		}
		return CreateResult{
			OK:     true,
			Detail: strings.Join(plan, "; "),
			Dirs:   []string{queue.QueueDir(repoPath), queue.PrdsDir(repoPath)},
		}
	}

	// 1) ensure queue/prd dirs
	queue.EnsureQueueDirs(repoPath)
	dirs = append(dirs, queue.QueueDir(repoPath), queue.PrdsDir(repoPath))
	if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
		return CreateResult{OK: false, Detail: fmt.Sprintf("config write/validate failed: %v", err), Dirs: dirs}
	}

	// 2) merge config
	cfgPath := createResolveConfigPath(repoPath)
	fileConfig := map[string]any{}
	if data, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(data, &fileConfig) // parse failure → {} like the TS catch
	}
	if opts.Scout {
		scout, _ := fileConfig["scout"].(map[string]any)
		if scout == nil {
			scout = map[string]any{}
		}
		scout["enabled"] = true
		scout["worker"] = scoutWorker
		scout["intervalMinutes"] = intervalMinutes
		fileConfig["scout"] = scout
	}
	if opts.AutoMerge {
		fileConfig["autoMerge"] = true
	}
	if opts.SelfUpdate {
		fileConfig["selfUpdate"] = true
	}
	if opts.Orchestrator {
		orch, _ := fileConfig["orchestrator"].(map[string]any)
		if orch == nil {
			orch = map[string]any{}
		}
		orch["enabled"] = true
		if opts.OrchestratorGoal != "" {
			orch["goal"] = opts.OrchestratorGoal
		}
		fileConfig["orchestrator"] = orch
	}
	// Validate by loading (throws on bad values) before writing.
	// json.MarshalIndent HTML-escapes <>& by default; JSON.stringify does
	// not, so parity requires the escaped-HTML encoder set.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	marshalErr := enc.Encode(fileConfig) // Encode appends the trailing \n
	if marshalErr == nil {
		marshalErr = os.WriteFile(cfgPath, buf.Bytes(), 0o644)
	}
	if marshalErr == nil {
		_, marshalErr = config.Load(repoPath)
	}
	if marshalErr != nil {
		return CreateResult{OK: false, Detail: fmt.Sprintf("config write/validate failed: %v", marshalErr), Dirs: dirs}
	}

	// 3) Orca repo registration (best-effort)
	var orcaWorktrees []string
	if opts.Workers > 0 {
		runner := createRunner(opts)
		// Register repo so worktree create can use path: selector
		integrations.EnsureOrcaRepo(repoPath, runner)
		orcaWorktrees = []string{}
		for i := 0; i < opts.Workers; i++ {
			name := fmt.Sprintf("devagent-worker-%d", i+1)
			if p := integrations.CreateOrcaWorktree(repoPath, name, runner); p != "" {
				orcaWorktrees = append(orcaWorktrees, p)
			}
		}
	}

	// 4) LaunchAgents (macOS only; skip gracefully elsewhere). Never install
	// the persistent user agents for an ephemeral repo: tmp-dir factories
	// would hijack com.devagent.* slots and crash-loop (EX_CONFIG) once the
	// temp checkout is deleted.
	var launchAgentPlists []string
	if (opts.Scout || opts.Tracker || opts.Builder || opts.Orchestrator) && runtime.GOOS == "darwin" && ShouldInstallLaunchAgent(repoPath) {
		specs := RolePlistSpecs(opts, intervalMinutes, scoutWorker, trackIntervalMinutes)
		for _, spec := range specs {
			p, err := installPlist(spec, repoPath)
			if err != nil {
				return CreateResult{OK: false, Detail: fmt.Sprintf("LaunchAgent install failed: %v", err), Dirs: dirs, ConfigPath: cfgPath}
			}
			launchAgentPlists = append(launchAgentPlists, p)
		}
	}
	plistPath := ""
	for _, p := range launchAgentPlists {
		if strings.Contains(p, "scout") {
			plistPath = p
			break
		}
	}

	detail := fmt.Sprintf("factory ready: queue at %s, prds at %s", queue.QueueDir(repoPath), queue.PrdsDir(repoPath))
	if opts.Scout {
		detail += fmt.Sprintf(", scout %s/%dm", scoutWorker, intervalMinutes)
	}
	if opts.Tracker {
		detail += ", tracker agent"
	}
	if opts.Builder {
		detail += ", builder agent"
	}
	if opts.Orchestrator {
		detail += ", orchestrator agent"
		if opts.OrchestratorGoal != "" {
			detail += " (goal set)"
		}
	}
	if len(orcaWorktrees) > 0 {
		detail += fmt.Sprintf(", %d orca worktree(s)", len(orcaWorktrees))
	}
	if len(launchAgentPlists) > 0 {
		detail += fmt.Sprintf(" [%d LaunchAgent(s)]", len(launchAgentPlists))
	}

	res := CreateResult{OK: true, Detail: detail, Dirs: dirs, ConfigPath: cfgPath, LaunchAgentPlist: plistPath, OrcaWorktrees: orcaWorktrees}
	if len(launchAgentPlists) > 0 {
		res.LaunchAgentPlists = launchAgentPlists
	}
	return res
}

// dryRunConfigJSON reproduces the TS dry-run merge-preview object:
// JSON.stringify({ scout: opts.scout ? {...} : undefined, autoMerge,
// selfUpdate }) — undefined scout is omitted; false booleans serialize.
func dryRunConfigJSON(opts CreateOptions, scoutWorker string, intervalMinutes int) string {
	var parts []string
	if opts.Scout {
		worker, _ := json.Marshal(scoutWorker)
		parts = append(parts, fmt.Sprintf(`"scout":{"enabled":true,"worker":%s,"intervalMinutes":%d}`, worker, intervalMinutes))
	}
	parts = append(parts, fmt.Sprintf(`"autoMerge":%t`, opts.AutoMerge))
	parts = append(parts, fmt.Sprintf(`"selfUpdate":%t`, opts.SelfUpdate))
	return "{" + strings.Join(parts, ",") + "}"
}

// truncateRunes mirrors the JS String.prototype.slice(0, n) used for the
// goal preview.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// LaunchAgentPlistContent mirrors launchAgentPlistContent (scout spec only).
func LaunchAgentPlistContent(repoPath string, intervalMinutes int, worker string) string {
	return BuildLaunchAgentPlist(RolePlistSpecs(CreateOptions{
		RepoPath: repoPath,
		Scout:    true,
	}, intervalMinutes, worker, 15)[0])
}

// ShouldInstallLaunchAgent guards the shared com.devagent.scout LaunchAgent
// slot: only repos that live outside the OS temp dir (and exist) may claim it.
func ShouldInstallLaunchAgent(repoPath string) bool {
	if !fileExists(repoPath) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(repoPath)
	if err != nil {
		resolved = repoPath
	}
	// macOS os.TempDir() (/var/folders/...) resolves to /private/var/folders/...
	realTmp := os.TempDir()
	if r, err := filepath.EvalSymlinks(realTmp); err == nil {
		realTmp = r
	}
	return !strings.HasPrefix(resolved, realTmp) && !strings.HasPrefix(resolved, os.TempDir())
}
