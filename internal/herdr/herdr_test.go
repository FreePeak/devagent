package herdr

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/FreePeak/devagent/internal/ledger"
)

// scriptRunnerCli is a CliRunner that actually executes `pane run` scripts
// through /bin/sh, mirroring the vitest functional stub (test/herdr.test.ts):
// the whole env-file + redirect + marker protocol is exercised end to end.
type scriptRunnerCli struct {
	t           *testing.T
	mu          sync.Mutex
	calls       []string
	envFileKept bool
}

func (s *scriptRunnerCli) HerdrCli(args []string, timeoutMs int) CliResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(args) > 1 && args[0] == "--session" {
		args = args[2:]
	}
	s.calls = append(s.calls, strings.Join(args, " "))
	out := func(o string) CliResult { return CliResult{Code: 0, Stdout: o} }
	switch {
	case len(args) >= 2 && args[0] == "workspace" && args[1] == "list":
		return out(`{"id":"x","result":{"type":"workspace_list","workspaces":[]}}`)
	case len(args) >= 2 && args[0] == "workspace" && args[1] == "create":
		return out(`{"id":"x","result":{"type":"workspace_created","workspace":{"workspace_id":"w1"},"tab":{"tab_id":"w1:t1"},"root_pane":{"pane_id":"w1:p1"}}}`)
	case len(args) >= 2 && args[0] == "pane" && args[1] == "rename":
		return out(`{"id":"x","result":{}}`)
	case len(args) >= 2 && args[0] == "pane" && args[1] == "run":
		// Like the real server: type the command into the pane and return
		// immediately; the pane shell owns the process lifetime.
		script := args[3]
		s.envFileKept = strings.Contains(script, "env.sh") && !strings.Contains(script, "rm -f")
		cmd := exec.Command("/bin/sh", "-c", script)
		_ = cmd.Start()
		go func() { _ = cmd.Wait() }()
		return out(`{"id":"x","result":{}}`)
	case len(args) >= 2 && args[0] == "pane" && args[1] == "send-keys":
		return out(`{"id":"x","result":{}}`)
	case len(args) >= 2 && args[0] == "workspace" && args[1] == "close":
		return out(`{"id":"x","result":{}}`)
	default:
		return CliResult{Code: 2, Stderr: "stub-herdr: unsupported command: " + strings.Join(args, " ")}
	}
}

func (s *scriptRunnerCli) called(arg ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := strings.Join(arg, " ")
	for _, c := range s.calls {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

func (s *scriptRunnerCli) closeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		if strings.HasPrefix(c, "workspace close w1") {
			n++
		}
	}
	return n
}

func TestHerdrServerUpDetectsWorkspaceList(t *testing.T) {
	cli := &scriptRunnerCli{t: t}
	if !HerdrServerUp(cli, "t") {
		t.Fatal("server should be up")
	}
	if !cli.called("workspace list") {
		t.Fatal("workspace list not called")
	}
}

func TestEnsureHerdrServerFalseWhenServerNeverComesUp(t *testing.T) {
	cli := &downCli{inner: &scriptRunnerCli{t: t}}
	if EnsureHerdrServer(cli, "t", "", ServerWait{Attempts: 2, DelayMs: 1}) {
		t.Fatal("server should stay down")
	}
}

func TestRunCommandInHerdrPaneCapturesStdoutAndExitCode(t *testing.T) {
	dir := t.TempDir()
	cli := &scriptRunnerCli{t: t}
	res, err := RunCommandInHerdrPane(cli, "echo", []string{"hello"}, PaneRunOptions{Dir: dir, TimeoutMs: 10_000})
	if err != nil || res == nil {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if res.TimedOut || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "hello" {
		t.Fatalf("res = %+v", res)
	}
	if !cli.called("workspace create") || !cli.called("pane rename w1:p1") {
		t.Fatalf("pane protocol calls = %v", cli.calls)
	}
	if cli.closeCalls() != 1 {
		t.Fatalf("workspace close calls = %d, want 1 (hygiene default)", cli.closeCalls())
	}
	if !cli.called("pane run w1:p1") {
		t.Fatalf("pane run missing: %v", cli.calls)
	}
}

func TestRunCommandInHerdrPanePropagatesNonzeroExit(t *testing.T) {
	dir := t.TempDir()
	cli := &scriptRunnerCli{t: t}
	res, err := RunCommandInHerdrPane(cli, "sh", []string{"-c", "echo boom >&2; exit 3"}, PaneRunOptions{Dir: dir, TimeoutMs: 10_000})
	if err != nil || res == nil {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if res.ExitCode != 3 || !strings.Contains(res.Stderr, "boom") {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunCommandInHerdrPaneEnvInjectionWithoutLeak(t *testing.T) {
	dir := t.TempDir()
	secretOut := filepath.Join(dir, "secret.out")
	cli := &scriptRunnerCli{t: t}
	res, err := RunCommandInHerdrPane(cli, "sh", []string{"-c", "echo $SECRET_V > " + shQuote(secretOut)}, PaneRunOptions{
		Dir:       dir,
		Env:       map[string]string{"SECRET_V": "s3cret-value"},
		TimeoutMs: 10_000,
	})
	if err != nil || res == nil {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d", res.ExitCode)
	}
	data, rerr := os.ReadFile(secretOut)
	if rerr != nil || strings.TrimSpace(string(data)) != "s3cret-value" {
		t.Fatalf("secret.out = %q, err = %v", data, rerr)
	}
	// The secret reaches the pane via a sourced env file, never argv/scrollback
	// (the TS test asserts the joined call log does not contain the VALUE — the
	// test's own `echo $SECRET_V` argv references the name, not the value).
	joined := strings.Join(cli.calls, "\n")
	if strings.Contains(joined, "s3cret-value") {
		t.Fatalf("secret value leaked onto a command line: %q", joined)
	}
	// The pane-run script must source env.sh before use and remove it after:
	// `set -a; . <env.sh>; set +a` ... `rm -f <env.sh>`.
	var runScript string
	for _, c := range cli.calls {
		if strings.HasPrefix(c, "pane run ") {
			runScript = c
		}
	}
	if !strings.Contains(runScript, "set -a; . ") || !strings.Contains(runScript, "; set +a;") || !strings.Contains(runScript, "rm -f ") {
		t.Fatalf("pane script does not source+remove env.sh: %q", runScript)
	}
	if cli.envFileKept {
		t.Fatal("env.sh not removed by the pane script")
	}
}

func TestRunCommandInHerdrPaneTimeoutTeardown(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	dir := t.TempDir()
	cli := &scriptRunnerCli{t: t}
	res, err := RunCommandInHerdrPane(cli, "sleep", []string{"30"}, PaneRunOptions{Dir: dir, TimeoutMs: 700})
	if err != nil || res == nil {
		t.Fatalf("res = %v, err = %v", res, err)
	}
	if !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("res = %+v", res)
	}
	if !cli.called("send-keys w1:p1 ctrl+c") {
		t.Fatal("ctrl+c not sent")
	}
	if cli.closeCalls() == 0 {
		t.Fatal("workspace not torn down")
	}
}

func TestRunCommandInHerdrPaneWatchdogHealthCleanRun(t *testing.T) {
	repo := t.TempDir()
	dir := t.TempDir()
	cli := &scriptRunnerCli{t: t}
	res, err := RunCommandInHerdrPane(cli, "sh", []string{"-c", `echo '{"type":"tool_execution_start"}'`}, PaneRunOptions{
		Dir:                 dir,
		TimeoutMs:           10_000,
		NoProgressTimeoutMs: 5_000,
		Watchdog:            &WatchdogContext{RepoPath: repo, TaskID: "Q34-2", Attempt: 1, Worker: "omp"},
	})
	if err != nil || res == nil || res.TimedOut {
		t.Fatalf("res = %v, err = %v", res, err)
	}

	rows := readLedgerRows(t, repo)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	for k, want := range map[string]any{
		"kind":                "event",
		"event":               "watchdog-health",
		"site":                "herdr-pane",
		"taskId":              "Q34-2",
		"worker":              "omp",
		"noProgressTimeoutMs": float64(5_000),
		"watchdogFired":       false,
		"runtime":             "herdr-pane",
		"visible":             true,
		"visibility":          "herdr-pane",
	} {
		if row[k] != want {
			t.Errorf("row[%q] = %v, want %v", k, row[k], want)
		}
	}
	if cr, ok := row["clockResets"].(float64); !ok || cr < 1 {
		t.Errorf("clockResets = %v, want >= 1", row["clockResets"])
	}
	if mb, ok := row["meaningfulBytes"].(float64); !ok || mb <= 0 {
		t.Errorf("meaningfulBytes = %v, want > 0", row["meaningfulBytes"])
	}
}

// readLedgerRows parses the events.jsonl ledger into maps.
func readLedgerRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad ledger row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRunCommandInHerdrPaneNoRowWithoutLedgerOrClock(t *testing.T) {
	repo := t.TempDir()
	dir := t.TempDir()
	cli := &scriptRunnerCli{t: t}
	// No ledger context.
	if _, err := RunCommandInHerdrPane(cli, "echo", []string{"x"}, PaneRunOptions{Dir: dir, TimeoutMs: 10_000, NoProgressTimeoutMs: 5_000}); err != nil {
		t.Fatal(err)
	}
	// Ledger context but clock disabled.
	if _, err := RunCommandInHerdrPane(cli, "echo", []string{"x"}, PaneRunOptions{Dir: dir, TimeoutMs: 10_000, Watchdog: &WatchdogContext{RepoPath: repo, TaskID: "T", Attempt: 1, Worker: "omp"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ledger written without armed clock: %v", err)
	}
}

func TestRunCommandInHerdrPaneKeepsPanesWhenEnvSet(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_KEEP_PANES", "1")
	dir := t.TempDir()
	cli := &scriptRunnerCli{t: t}
	if _, err := RunCommandInHerdrPane(cli, "echo", []string{"kept"}, PaneRunOptions{Dir: dir, TimeoutMs: 10_000}); err != nil {
		t.Fatal(err)
	}
	if cli.closeCalls() != 0 {
		t.Fatalf("workspace close calls = %d, want 0", cli.closeCalls())
	}
}

func TestRenderEnvFileSkipsInvalidIdentifiers(t *testing.T) {
	text := RenderEnvFile(map[string]string{
		"PATH":     "/usr/bin",
		"SECRET_V": "ok",
		// npm injects npm_config_node_pre_gyp:cache; dash/colon keys abort
		// `source` under zsh.
		"npm_config_node_pre_gyp:cache": "x",
		"A-DASH":                        "y",
		"_OK_1":                         "z",
	})
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %v", lines)
	}
	// Cross-runtime divergence: TS emits insertion order, Go sorts; the sourced
	// result is identical.
	if !strings.HasPrefix(lines[0], "export PATH=") || !strings.Contains(lines[0], `'/usr/bin'`) {
		t.Errorf("line0 = %q", lines[0])
	}
	for _, l := range lines {
		if strings.Contains(l, ":") || strings.Contains(l, "A-DASH") {
			t.Errorf("invalid identifier exported: %q", l)
		}
	}
	if !strings.Contains(text, "export _OK_1='z'") {
		t.Errorf("missing _OK_1: %q", text)
	}
}

func TestRenderEnvFileQuotesSingleQuotes(t *testing.T) {
	text := RenderEnvFile(map[string]string{"MSG": "it's"})
	if !strings.Contains(text, `'it'\''s'`) {
		t.Fatalf("quoting wrong: %q", text)
	}
}

func TestProgressClassifier(t *testing.T) {
	progress := []string{
		`{"type":"tool_execution_start","name":"bash"}`,
		`{"type":"tool_execution_end"}`,
		`{"type":"text_end"}`,
		`{"type":"toolcall_start"}`,
		`{"type":"tool_call"}`,
		`{"type":"tool_call_update"}`,
		`{"type":"text","data":"hi"}`,
	}
	for _, line := range progress {
		if !IsNdjsonProgressLine(line) {
			t.Errorf("expected progress: %s", line)
		}
	}
	thinking := []string{
		`{"type":"thinking_delta","delta":"..."}`,
		`some preamble "thinking_delta" inside`,
		`{"type":"thinking_start"}`,
		`{"type":"thought"}`,
		``,
		`{"type":"message_start"}`,
	}
	for _, line := range thinking {
		if IsNdjsonProgressLine(line) {
			t.Errorf("expected non-progress: %s", line)
		}
	}
}

func TestPaneNameForCwdAndParseInt(t *testing.T) {
	if got := paneNameForCwd("/repo/.devagent-worktrees/T1-a1"); got != "T1-a1" {
		t.Errorf("pane name = %q", got)
	}
	if got := paneNameForCwd("/"); got != "devagent worker" {
		t.Errorf("empty base = %q", got)
	}
	if n, ok := parseJsInt("  42\n"); !ok || n != 42 {
		t.Errorf("parseJsInt(42) = %d, %v", n, ok)
	}
	if n, ok := parseJsInt("42x"); !ok || n != 42 {
		t.Errorf("JS parseInt tolerates trailing junk: %d, %v", n, ok)
	}
	if _, ok := parseJsInt("x42"); ok {
		t.Error("parseJsInt(x42) should fail")
	}
	if _, ok := parseJsInt(""); ok {
		t.Error("parseJsInt('') should fail")
	}
}

func TestSessionResolution(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "")
	if ResolveSession("") != "devagent" {
		t.Error("default session should be devagent")
	}
	t.Setenv("DEVAGENT_HERDR_SESSION", "other")
	if ResolveSession("") != "other" {
		t.Error("env session ignored")
	}
	if ResolveSession("explicit") != "explicit" {
		t.Error("explicit session ignored")
	}
	t.Setenv("DEVAGENT_HERDR_BIN", "")
	if HerdrBin() != "herdr" {
		t.Error("default bin should be herdr")
	}
	t.Setenv("DEVAGENT_HERDR_BIN", "/tmp/stub-herdr")
	if HerdrBin() != "/tmp/stub-herdr" {
		t.Error("bin override ignored")
	}
}

func TestPaneForegroundWorkerContract(t *testing.T) {
	cli := newFakeCli(t)
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	if !PaneForegroundWorker(cli, "wX:p1") {
		t.Error("omp foreground should read as live")
	}
	cli.procInfo["wX:p2"] = "testdata/sweep/process-info-zsh.json"
	if PaneForegroundWorker(cli, "wX:p2") {
		t.Error("zsh foreground should read as idle")
	}
	if PaneForegroundWorker(cli, "") {
		t.Error("empty pane id should be false")
	}
	if PaneForegroundWorker(cli, "wX:missing") {
		t.Error("unsupported reply should be false (best-effort)")
	}
}

func TestParseCliJSONJunk(t *testing.T) {
	if ParseCliJSON("not json") != nil {
		t.Error("junk should parse to nil")
	}
	if ParseCliJSON(`[1,2]`) != nil {
		t.Error("array should parse to nil (object envelope contract)")
	}
	if ParseCliJSON(`{"result":{"type":"x"}}`) == nil {
		t.Error("object should parse")
	}
}

func TestLedgerRowFieldOrder(t *testing.T) {
	repo := t.TempDir()
	runtime := "herdr-pane"
	visible := true
	visibility := "herdr-pane"
	ledger.AppendWatchdogHealthRecord(repo, ledger.WatchdogHealthRecord{
		TS: "2026-09-07T00:00:00.000Z", Kind: "event", TaskID: "T1", Attempt: 1,
		Event: "watchdog-health", Site: "herdr-pane", Worker: "omp",
		NoProgressTimeoutMs: 1000, WallClockMs: 5, ClockResets: 2,
		MeaningfulBytes: 30, IdleMs: 3,
		Runtime: &runtime, Visible: &visible, Visibility: &visibility,
	})
	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(data))
	// Struct field order of ledger.WatchdogHealthRecord is the JSONL contract
	// (internal/ledger owns it; pinned here so a herdr-pane row never drifts).
	want := `{"ts":"2026-09-07T00:00:00.000Z","kind":"event","taskId":"T1","attempt":1,"event":"watchdog-health","site":"herdr-pane","worker":"omp","noProgressTimeoutMs":1000,"watchdogFired":false,"coldStartFired":false,"wallClockMs":5,"clockResets":2,"meaningfulBytes":30,"idleMs":3,"runtime":"herdr-pane","visible":true,"visibility":"herdr-pane"}`
	if line != want {
		t.Fatalf("row = %s\nwant = %s", line, want)
	}
}

func TestLedgerNowShape(t *testing.T) {
	ts := ledger.NowISO()
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).MatchString(ts) {
		t.Fatalf("ts = %q, want ISO with millis + Z", ts)
	}
}

func TestPidSeamsDirect(t *testing.T) {
	orphanSeams(t, "4242\n0\nbogus\n", map[string][]string{"4242": {"bash scripts/selfbuild-loop.sh"}})
	got := psPidsMatching("ignored")
	if len(got) != 1 || got[0] != 4242 {
		t.Fatalf("pids = %v, want [4242]", got)
	}
	if PaneRunOwnerOrphaned("wX:p9") {
		t.Fatal("owner with live driver ancestry should NOT be orphaned")
	}
	if !HasLoopDriverAncestor([]string{"timeout", "bash scripts/selfbuild-loop.sh"}) {
		t.Error("loop driver ancestor undetected")
	}
	if HasLoopDriverAncestor([]string{"timeout 7200 devagent-go task"}) {
		t.Error("false driver ancestor")
	}
	if !HasLoopDriverAncestor([]string{"timeout", "./devagent-go loop"}) {
		t.Error("Go loop driver ancestor undetected")
	}
	if !HasLoopDriverAncestor([]string{"nohup devagent loop"}) {
		t.Error("installed-binary loop driver ancestor undetected")
	}
	// Missing ancestry entry = no evidence = orphaned.
	orphanSeams(t, "4242\n", map[string][]string{})
	if !PaneRunOwnerOrphaned("wX:p9") {
		t.Fatal("detached owner should be orphaned")
	}
}

func TestResolveSweepSettingsFailsClosedOnBadConfig(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.WriteFile(filepath.Join(dir, "devagent.json"), []byte(`{"herdr":{"sweep":{"denySessions":["Bad Name"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sweep := ResolveSweepSettings()
	if sweep.Enabled {
		t.Fatalf("invalid config must fail closed, got %+v", sweep)
	}
}

func TestResolveSweepSettingsDefaults(t *testing.T) {
	dir := t.TempDir()
	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	sweep := ResolveSweepSettings()
	if !sweep.Enabled || sweep.Orphans != nil || len(sweep.DenySessions) != 0 {
		t.Fatalf("defaults = %+v, want enabled, orphans unset, no denies", sweep)
	}
}

func TestSweepFailsClosedWhenPaneListJunk(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "not json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("junk pane list = %v, want empty", got)
	}
	cli.paneList = `{"id":"x","result":{"type":"pane_list"}}` // no panes key
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("empty panes = %v, want empty", got)
	}
}

func TestFakeCliUnsupportedCommand(t *testing.T) {
	cli := newFakeCli(t)
	r := cli.HerdrCli([]string{"bogus"}, 0)
	if r.Code != 2 {
		t.Fatalf("code = %d, want 2", r.Code)
	}
}

func TestFixtureFilesExist(t *testing.T) {
	for _, f := range []string{
		"testdata/sweep/pane-list-live-worker.json",
		"testdata/sweep/pane-list-idle-worktree.json",
		"testdata/sweep/pane-list-scratch.json",
		"testdata/sweep/pane-list-agentless.json",
		"testdata/sweep/pane-list-two-idle.json",
		"testdata/sweep/process-info-omp.json",
		"testdata/sweep/process-info-zsh.json",
		"testdata/sweep/agent-list-running.json",
		"testdata/sweep/agent-list-idle.json",
		"testdata/roster/agent-list-two-pane.json",
		"testdata/roster/agent-list-recovery-suffix.json",
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing fixture %s", f)
		}
	}
}
