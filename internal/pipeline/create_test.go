package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/spawn"
)

// fltOkRunner mirrors the TS okRunner: orca repo add and worktree create
// succeed, everything else fails.
func fltOkRunner(repo string) func(string, []string, spawn.Options) spawn.Result {
	return func(cmd string, args []string, _ spawn.Options) spawn.Result {
		if cmd == "orca" && fltHasStr(args, "repo") {
			return spawn.Result{ExitCode: 0, Stdout: "{}"}
		}
		if cmd == "orca" && fltHasStr(args, "create") {
			out, _ := json.Marshal(map[string]any{"result": map[string]any{"path": filepath.Join(repo, "wt-worker")}})
			return spawn.Result{ExitCode: 0, Stdout: string(out)}
		}
		return spawn.Result{ExitCode: 1}
	}
}

func fltHasStr(list []string, s string) bool {
	for _, it := range list {
		if it == s {
			return true
		}
	}
	return false
}

func fltFailRunner(string, []string, spawn.Options) spawn.Result {
	return spawn.Result{ExitCode: -1}
}

func fltReadConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return m
}

func TestCreateDryRunPlanDoesNotMutate(t *testing.T) {
	repo := t.TempDir()
	r := RunCreate(CreateOptions{RepoPath: repo, Scout: true, Workers: 2, DryRun: true})
	if !r.OK {
		t.Fatalf("ok = false, detail %q", r.Detail)
	}
	if !strings.Contains(r.Detail, "would ensure") {
		t.Fatalf("detail %q must contain 'would ensure'", r.Detail)
	}
	if _, err := os.Stat(filepath.Join(repo, ".devagent", "queue")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not create the queue dir")
	}
	if _, err := os.Stat(filepath.Join(repo, "devagent.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run must not write devagent.json")
	}
	wantDirs := []string{queue.QueueDir(repo), queue.PrdsDir(repo)}
	if len(r.Dirs) != 2 || r.Dirs[0] != wantDirs[0] || r.Dirs[1] != wantDirs[1] {
		t.Fatalf("dirs = %v, want %v", r.Dirs, wantDirs)
	}
	if r.ConfigPath != "" {
		t.Fatalf("dry-run configPath = %q, want empty", r.ConfigPath)
	}
}

func TestCreateDryRunPlanLines(t *testing.T) {
	repo := t.TempDir()
	goal := "build the thing"
	track := 7
	r := RunCreate(CreateOptions{
		RepoPath: repo, Scout: true, Tracker: true, Builder: true, Orchestrator: true,
		OrchestratorGoal: goal, Workers: 2, AutoMerge: true, SelfUpdate: true,
		ScoutWorker: "opencode", IntervalMinutes: 45, TrackIntervalMinutes: &track, DryRun: true,
	})
	want := []string{
		"would ensure " + queue.QueueDir(repo) + " and " + queue.PrdsDir(repo),
		`would merge ` + filepath.Join(repo, "devagent.json") + ` with {"scout":{"enabled":true,"worker":"opencode","intervalMinutes":45},"autoMerge":true,"selfUpdate":true}`,
		"would install LaunchAgent plist for scout (opencode every 45m)",
		"would install LaunchAgent plist for tracker (every 7m)",
		"would install LaunchAgent plist for builder (scripts/build-loop.sh, poll 300s)",
		`would install LaunchAgent plist for orchestrator (scripts/orchestrate-loop.sh, goal: "build the thing")`,
		"would provision 2 Orca worktree(s) via orca worktree create",
	}
	for _, w := range want {
		if !strings.Contains(r.Detail, w) {
			t.Fatalf("detail %q missing plan line %q", r.Detail, w)
		}
	}
}

func TestCreateDryRunOrchestratorRepoDefault(t *testing.T) {
	repo := t.TempDir()
	r := RunCreate(CreateOptions{RepoPath: repo, Orchestrator: true, DryRun: true})
	if !strings.Contains(r.Detail, "goal: repo default") {
		t.Fatalf("detail %q must contain 'goal: repo default'", r.Detail)
	}
}

func TestCreateRejectsMissingRepoPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-repo")
	r := RunCreate(CreateOptions{RepoPath: missing, DryRun: true})
	if r.OK {
		t.Fatal("ok = true, want false")
	}
	if want := "repoPath does not exist: " + missing; r.Detail != want {
		t.Fatalf("detail = %q, want %q", r.Detail, want)
	}
	if r.Dirs == nil || len(r.Dirs) != 0 {
		t.Fatalf("dirs = %v, want empty", r.Dirs)
	}
}

func TestCreateRealRunWritesConfigAndQueues(t *testing.T) {
	repo := t.TempDir()
	r := RunCreate(CreateOptions{
		RepoPath: repo, Scout: true, Workers: 1, AutoMerge: true,
		IntervalMinutes: 15, ScoutWorker: "opencode", Runner: fltOkRunner(repo),
	})
	if !r.OK {
		t.Fatalf("ok = false, detail %q", r.Detail)
	}
	for _, d := range []string{filepath.Join(repo, ".devagent", "queue"), filepath.Join(repo, ".devagent", "prds")} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("dir %s missing: %v", d, err)
		}
	}
	cfg := fltReadConfig(t, filepath.Join(repo, "devagent.json"))
	scout, _ := cfg["scout"].(map[string]any)
	if scout == nil {
		t.Fatal("config.scout missing")
	}
	if scout["enabled"] != true {
		t.Fatalf("scout.enabled = %v, want true", scout["enabled"])
	}
	if scout["worker"] != "opencode" {
		t.Fatalf("scout.worker = %v, want opencode", scout["worker"])
	}
	if scout["intervalMinutes"] != float64(15) {
		t.Fatalf("scout.intervalMinutes = %v, want 15", scout["intervalMinutes"])
	}
	if cfg["autoMerge"] != true {
		t.Fatalf("autoMerge = %v, want true", cfg["autoMerge"])
	}
	if r.ConfigPath != filepath.Join(repo, "devagent.json") {
		t.Fatalf("configPath = %q", r.ConfigPath)
	}
	if len(r.OrcaWorktrees) != 1 || r.OrcaWorktrees[0] != filepath.Join(repo, "wt-worker") {
		t.Fatalf("orcaWorktrees = %v, want [wt-worker]", r.OrcaWorktrees)
	}
	if !strings.HasPrefix(r.Detail, "factory ready: queue at "+queue.QueueDir(repo)+", prds at "+queue.PrdsDir(repo)) {
		t.Fatalf("detail = %q", r.Detail)
	}
	if !strings.Contains(r.Detail, ", scout opencode/15m") {
		t.Fatalf("detail %q missing scout segment", r.Detail)
	}
	if !strings.Contains(r.Detail, ", 1 orca worktree(s)") {
		t.Fatalf("detail %q missing orca segment", r.Detail)
	}
}

func TestCreateDegradesWhenOrcaMissing(t *testing.T) {
	repo := t.TempDir()
	r := RunCreate(CreateOptions{RepoPath: repo, Workers: 2, Runner: fltFailRunner})
	if !r.OK {
		t.Fatalf("ok = false, detail %q", r.Detail)
	}
	if r.OrcaWorktrees == nil || len(r.OrcaWorktrees) != 0 {
		t.Fatalf("orcaWorktrees = %v, want empty non-nil", r.OrcaWorktrees)
	}
}

func TestCreateMergesExistingConfig(t *testing.T) {
	repo := t.TempDir()
	existing := map[string]any{
		"worker":   "claude-code",
		"maxLoops": float64(5),
		"scout":    map[string]any{"model": "prov/model-x"},
	}
	data, _ := json.Marshal(existing)
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := RunCreate(CreateOptions{RepoPath: repo, Scout: true, Orchestrator: true, OrchestratorGoal: "g", Runner: fltFailRunner})
	if !r.OK {
		t.Fatalf("ok = false, detail %q", r.Detail)
	}
	cfg := fltReadConfig(t, filepath.Join(repo, "devagent.json"))
	if cfg["worker"] != "claude-code" || cfg["maxLoops"] != float64(5) {
		t.Fatalf("existing keys lost: %v", cfg)
	}
	scout, _ := cfg["scout"].(map[string]any)
	if scout["model"] != "prov/model-x" {
		t.Fatalf("scout.model = %v, want preserved", scout["model"])
	}
	if scout["enabled"] != true || scout["worker"] != "omp" || scout["intervalMinutes"] != float64(30) {
		t.Fatalf("scout merge wrong: %v", scout)
	}
	orch, _ := cfg["orchestrator"].(map[string]any)
	if orch == nil || orch["enabled"] != true || orch["goal"] != "g" {
		t.Fatalf("orchestrator merge wrong: %v", orch)
	}
	// The options that stay false must NOT be written (TS: if (opts.autoMerge)).
	if _, present := cfg["autoMerge"]; present {
		t.Fatalf("autoMerge must be absent when not requested: %v", cfg)
	}
	if _, present := cfg["selfUpdate"]; present {
		t.Fatalf("selfUpdate must be absent when not requested: %v", cfg)
	}
}

func TestCreateConfigValidationErrorByteParity(t *testing.T) {
	repo := t.TempDir()
	// An invalid pre-existing value create.ts never touches survives the
	// merge and fails the loadConfig validation pass.
	data := []byte(`{"scout":{"maxQueued":0}}`)
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	r := RunCreate(CreateOptions{RepoPath: repo, Scout: true, Runner: fltFailRunner})
	if r.OK {
		t.Fatal("ok = true, want false")
	}
	want := `config write/validate failed: Invalid scout.maxQueued "0"; expected >= 1`
	if r.Detail != want {
		t.Fatalf("detail = %q, want %q", r.Detail, want)
	}
	if len(r.Dirs) != 2 {
		t.Fatalf("dirs = %v, want the two queue dirs", r.Dirs)
	}
}

func TestBuildLaunchAgentPlistContent(t *testing.T) {
	xml := LaunchAgentPlistContent("/tmp/myrepo", 45, "claude-code")
	for _, want := range []string{"com.devagent.scout", "/tmp/myrepo", "45", "claude-code", "<?xml", "Label", "ProgramArguments", "RunAtLoad", "KeepAlive", "ThrottleInterval"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("plist missing %q:\n%s", want, xml)
		}
	}
}

func TestBuildLaunchAgentPlistEscaping(t *testing.T) {
	spec := plistSpec{
		label:       "com.devagent.test",
		logName:     "test.log",
		repoPath:    "/tmp/r",
		programArgs: []string{"/bin/bash", "/tmp/r/scripts/a&b.sh"},
	}
	xml := BuildLaunchAgentPlist(spec)
	if !strings.Contains(xml, "a&amp;b.sh") {
		t.Fatalf("plist must XML-escape &: %s", xml)
	}
}

func TestRolePlistSpecs(t *testing.T) {
	track := 20
	specs := RolePlistSpecs(CreateOptions{
		RepoPath: "/tmp/selfbuild-test", Scout: true, Tracker: true, Builder: true, Orchestrator: true,
		OrchestratorGoal: "go", ScoutWorker: "claude-code", IntervalMinutes: 45, TrackIntervalMinutes: &track,
	}, 45, "claude-code", 20)
	if len(specs) != 4 {
		t.Fatalf("specs = %d, want 4", len(specs))
	}
	labels := []string{specs[0].label, specs[1].label, specs[2].label, specs[3].label}
	wantLabels := []string{"com.devagent.scout", "com.devagent.tracker", "com.devagent.builder", "com.devagent.orchestrator"}
	for i := range wantLabels {
		if labels[i] != wantLabels[i] {
			t.Fatalf("label[%d] = %q, want %q", i, labels[i], wantLabels[i])
		}
	}
	if got := strings.Join(specs[0].programArgs, " "); !strings.Contains(got, "--interval 45") || !strings.Contains(got, "--worker claude-code") || !strings.Contains(got, "--timeout 30") {
		t.Fatalf("scout args = %q", got)
	}
	if got := strings.Join(specs[1].programArgs, " "); !strings.Contains(got, "--interval 20") {
		t.Fatalf("tracker args = %q", got)
	}
	if got := strings.Join(specs[2].programArgs, " "); got != "/bin/bash /tmp/selfbuild-test/scripts/build-loop.sh" {
		t.Fatalf("builder args = %q", got)
	}
	if specs[3].envVals["ORCHESTRATOR_REPO"] != "/tmp/selfbuild-test" || specs[3].envVals["ORCHESTRATOR_GOAL"] != "go" {
		t.Fatalf("orchestrator env = %v", specs[3].envVals)
	}
}

func TestShouldInstallLaunchAgent(t *testing.T) {
	tmpRepo := t.TempDir()
	if ShouldInstallLaunchAgent(tmpRepo) {
		t.Fatal("tmp repo must not install the shared LaunchAgent slot")
	}
	if ShouldInstallLaunchAgent(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("missing repo must not install the shared LaunchAgent slot")
	}
	if !ShouldInstallLaunchAgent("/usr") {
		t.Fatal("a real non-temp path must be allowed")
	}
}
