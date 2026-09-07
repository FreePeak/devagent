package herdr

import (
	"testing"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/spawn"
)

// ---------- FR-VIS visibility resolution ----------

func TestShouldUseHerdrExplicitFlagWins(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR", "0")
	t.Setenv("DEVAGENT_VISIBILITY", "visible")
	if v := true; !func() bool { r, _ := ShouldUseHerdr(&v); return r }() {
		t.Error("explicit true ignored")
	}
	if v := false; func() bool { r, _ := ShouldUseHerdr(&v); return r }() {
		t.Error("explicit false ignored")
	}
	_ = config.SpawnVisibility // linked
}

func TestShouldUseHerdrEnvBeatsVisibility(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR", "1")
	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	if r, err := ShouldUseHerdr(nil); err != nil || !r {
		t.Fatalf("HERDR=1 should win: %v %v", r, err)
	}
	t.Setenv("DEVAGENT_HERDR", "0")
	t.Setenv("DEVAGENT_VISIBILITY", "visible")
	if r, err := ShouldUseHerdr(nil); err != nil || r {
		t.Fatalf("HERDR=0 should win: %v %v", r, err)
	}
}

func TestShouldUseHerdrVisibilityEnvForcesDirect(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR", "")
	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	if r, err := ShouldUseHerdr(nil); err != nil || r {
		t.Fatalf("headless should force direct: %v %v", r, err)
	}
	t.Setenv("DEVAGENT_VISIBILITY", "visible")
	if r, err := ShouldUseHerdr(nil); err != nil || !r {
		t.Fatalf("visible should route to panes: %v %v", r, err)
	}
}

func TestShouldUseHerdrDefaultsVisible(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR", "")
	t.Setenv("DEVAGENT_VISIBILITY", "")
	dir := t.TempDir()
	chdir(t, dir)
	if r, err := ShouldUseHerdr(nil); err != nil || !r {
		t.Fatalf("FR-VIS-01 default should be visible: %v %v", r, err)
	}
}

func TestShouldUseHerdrConfigVisibility(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR", "")
	t.Setenv("DEVAGENT_VISIBILITY", "")
	dir := t.TempDir()
	chdir(t, dir)
	writeFile(t, dir, "devagent.json", `{"worker":"omp","spawn":{"visibility":"headless"}}`)
	if r, err := ShouldUseHerdr(nil); err != nil || r {
		t.Fatalf("config headless should force direct: %v %v", r, err)
	}
}

func TestShouldUseHerdrInvalidConfigPropagates(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR", "")
	t.Setenv("DEVAGENT_VISIBILITY", "")
	dir := t.TempDir()
	chdir(t, dir)
	writeFile(t, dir, "devagent.json", `{"herdr":{"sweep":{"enabled":"no"}}}`)
	if _, err := ShouldUseHerdr(nil); err == nil {
		t.Fatal("invalid config should propagate an error (outside the fallback try/catch in TS)")
	}
}

func TestSpawnVisibilityPrecedence(t *testing.T) {
	t.Setenv("DEVAGENT_VISIBILITY", "")
	cfg := config.Config{Spawn: &config.SpawnConfig{Visibility: "headless"}}
	if config.SpawnVisibility(cfg) != "headless" {
		t.Error("config headless lost")
	}
	if config.SpawnVisibility(config.Config{}) != "visible" {
		t.Error("default should be visible")
	}
	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	cfgVisible := config.Config{Spawn: &config.SpawnConfig{Visibility: "visible"}}
	if config.SpawnVisibility(cfgVisible) != "headless" {
		t.Error("env should win over config")
	}
}

func TestPaneEnvOpAttachName(t *testing.T) {
	if PaneEnvOpAttach != "DEVAGENT_OPERATOR_ATTACHED" {
		t.Fatalf("PANE_ENV_OP_ATTACH = %q", PaneEnvOpAttach)
	}
}

// ---------- FR-VIS loud fallbacks ----------

func TestFallbackToDirectSpawnWhenHerdrDown(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	t.Setenv("DEVAGENT_HERDR_BIN", "devagent-no-such-herdr-binary")
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	t.Setenv("DEVAGENT_HERDR", "1")
	var warnings []string
	SetFallbackSink(func(site, message string) { warnings = append(warnings, site+": "+message) })
	t.Cleanup(func() { SetFallbackSink(nil) })
	res, err := RunWorkerCli(ExecRunner{}, "echo", []string{"direct-fallback"}, RunWorkerOptions{
		Dir:       dir,
		TimeoutMs: 10_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout == "" {
		t.Fatalf("direct fallback failed: %+v", res)
	}
	if len(warnings) != 1 || warnings[0] != "echo: herdr session not reachable; running echo directly" {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestFallbackWarnsOncePerSite(t *testing.T) {
	SetFallbackSink(func(site, message string) {})
	t.Cleanup(func() { SetFallbackSink(nil) })
	WarnFallbackOnce("omp", "first")
	WarnFallbackOnce("omp", "second")
	WarnFallbackOnce("pi", "first-pi")
	var got []string
	SetFallbackSink(func(site, message string) { got = append(got, site+": "+message) })
	WarnFallbackOnce("omp", "after-reset")
	if len(got) != 1 || got[0] != "omp: after-reset" {
		t.Fatalf("sink warnings = %v (dedupe state must reset on setFallbackSink)", got)
	}
}

// ---------- shouldUseHerdr headless semantics (FR-VIS-05): RunWorkerCli
// routes by the same decision ----------

func TestRunWorkerCliHeadlessGoesDirect(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	t.Setenv("DEVAGENT_HERDR", "")
	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	var warnings []string
	SetFallbackSink(func(site, message string) { warnings = append(warnings, message) })
	t.Cleanup(func() { SetFallbackSink(nil) })
	// CliRunner would fail everything, but headless must never touch it.
	cli := &scriptRunnerCli{t: t}
	res, err := RunWorkerCli(cli, "echo", []string{"headless"}, RunWorkerOptions{Dir: dir, TimeoutMs: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("direct spawn failed: %+v", res)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("headless run touched herdr: %v", cli.calls)
	}
	if len(warnings) != 0 {
		t.Fatalf("headless run warned: %v", warnings)
	}
}

func TestRunWorkerCliVisibleRoutesThroughPane(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	t.Setenv("DEVAGENT_HERDR", "1")
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := &scriptRunnerCli{t: t}
	res, err := RunWorkerCli(cli, "echo", []string{"visible-run"}, RunWorkerOptions{Dir: dir, TimeoutMs: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout == "" {
		t.Fatalf("pane run failed: %+v", res)
	}
	if !cli.called("pane run w1:p1") {
		t.Fatalf("expected a pane run, calls = %v", cli.calls)
	}
}

// ---------- parity of Result shapes ----------

func TestSpawnResultParity(t *testing.T) {
	// spawn.Result is the shared shape (SpawnCliResult); herdr results must
	// convert losslessly.
	r := spawn.Result{ExitCode: 3, Stdout: "o", Stderr: "e", TimedOut: true}
	hr := Result{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr, TimedOut: r.TimedOut}
	if hr.ExitCode != 3 || hr.Stdout != "o" || hr.Stderr != "e" || !hr.TimedOut {
		t.Fatalf("conversion mismatch: %+v", hr)
	}
}
