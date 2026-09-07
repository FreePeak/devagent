package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/FreePeak/devagent/internal/resilience"
)

// okProbe: hermetic stub for the provider probe — no live CLI in tests
// (the TS suite injects the same seam).
func okProbe(string, []string, string) resilience.Probe {
	return resilience.Probe{OK: true}
}

// fakeWorkerBin writes a no-op `omp` onto a fresh PATH dir so the worker
// required-check passes on CI runners without the real CLI.
func fakeWorkerBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "omp"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestRunInitFreshRepo(t *testing.T) {
	fakeWorkerBin(t)
	repo := t.TempDir()
	res, err := RunInit(InitOptions{RepoPath: repo, Probe: okProbe})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("init should pass on required checks only; got %+v", res)
	}
	if !res.Created {
		t.Error("fresh repo should report created=true")
	}
	if res.ConfigPath != filepath.Join(repo, "devagent.json") {
		t.Fatalf("configPath = %q", res.ConfigPath)
	}
	// Required checks: git + worker (omp on this machine).
	for _, c := range res.Checks {
		if c.Name == "git" && !c.OK {
			t.Error("git should be found in test env")
		}
		if c.Name == "LINEAR_API_KEY" && c.Required {
			t.Error("credentials must stay optional")
		}
	}
	// Sane defaults written.
	raw, _ := os.ReadFile(res.ConfigPath)
	for _, want := range []string{`"worker": "omp"`, `"maxLoops": 3`, `"timeoutMinutes": 30`, `"githubBaseBranch": "main"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("config missing %s: %s", want, raw)
		}
	}
}

func TestRunInitIdempotentMerge(t *testing.T) {
	fakeWorkerBin(t)
	repo := t.TempDir()
	_ = os.WriteFile(filepath.Join(repo, "devagent.json"), []byte("{\"worker\": \"pi\", \"maxLoops\": 9}\n"), 0o644)
	res, err := RunInit(InitOptions{RepoPath: repo, Probe: okProbe})
	if err != nil {
		t.Fatal(err)
	}
	if res.Created {
		t.Error("existing config must report created=false")
	}
	raw, _ := os.ReadFile(res.ConfigPath)
	if !bytes.Contains(raw, []byte(`"worker": "pi"`)) || !bytes.Contains(raw, []byte(`"maxLoops": 9`)) {
		t.Fatalf("existing choices must win: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"githubBaseBranch": "main"`)) {
		t.Fatalf("absent defaults must be filled: %s", raw)
	}
}

func TestRunInitBrokenConfigReplaced(t *testing.T) {
	fakeWorkerBin(t)
	repo := t.TempDir()
	_ = os.WriteFile(filepath.Join(repo, "devagent.json"), []byte("{broken"), 0o644)
	res, err := RunInit(InitOptions{RepoPath: repo, Probe: okProbe})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("init should succeed: %+v", res)
	}
	raw, _ := os.ReadFile(res.ConfigPath)
	if bytes.Contains(raw, []byte("broken")) {
		t.Fatalf("broken file not replaced: %s", raw)
	}
}

func TestRunInitSmoke(t *testing.T) {
	fakeWorkerBin(t)
	repo := t.TempDir()
	res, err := RunInit(InitOptions{RepoPath: repo, Smoke: true, Probe: okProbe})
	if err != nil {
		t.Fatal(err)
	}
	if res.Smoke == nil || !res.Smoke.OK {
		t.Fatalf("hermetic smoke should pass: %+v", res.Smoke)
	}
	if _, err := os.Stat(filepath.Join(repo, ".devagent", "smoke", "last.json")); err != nil {
		t.Fatal("smoke receipt not written")
	}
}

func TestRenderInitReportShape(t *testing.T) {
	fakeWorkerBin(t)
	var buf bytes.Buffer
	res, err := RunInit(InitOptions{RepoPath: t.TempDir(), Probe: okProbe})
	if err != nil {
		t.Fatal(err)
	}
	RenderInitReport(res, func(s string) { buf.WriteString(s + "\n") })
	out := buf.String()
	for _, want := range []string{"DevAgent setup —", "git found", "Next: state your goal in one sentence"} {
		if !bytes.Contains([]byte(out), []byte(want)) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	// Chips render in the §20.8 language (colored dot + label + advice on
	// failure). The exact pass/fail split depends on the machine's env
	// (LINEAR_API_KEY etc.), so assert the shape, not a specific verdict.
	if !bytes.Contains([]byte(out), []byte("git")) {
		t.Error("checklist missing the git row")
	}
}
