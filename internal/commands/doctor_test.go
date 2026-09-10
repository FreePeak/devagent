package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/resilience"
)

// healthyOpts assembles DoctorOptions where every seam reports green; tests
// break one seam at a time and assert the failing check is named with a hint.
func healthyOpts(t *testing.T, repo, home string) DoctorOptions {
	t.Helper()
	t.Setenv("DEVAGENT_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	return DoctorOptions{
		RepoPath: repo,
		Version:  "0.9.5",
		LatestTag: func() (string, error) {
			return "v0.9.5", nil
		},
		GitRemote: func() error { return nil },
		GhToken: func() (string, string) {
			return "env GITHUB_TOKEN", "ghp_test"
		},
		TokenValid: func(source, token string) (bool, string) {
			return true, ""
		},
		HerdrCli: fakeHerdrOK{},
		DaemonProbe: func() (int, error) {
			return http.StatusOK, nil
		},
		ProviderProbe: func(cmd string, args []string, dir string) resilience.Probe {
			return resilience.Probe{OK: true}
		},
		Now: func() time.Time { return time.UnixMilli(1_000_000) },
	}
}

type fakeHerdrOK struct{}

func (fakeHerdrOK) HerdrCli(args []string, timeoutMs int) herdr.CliResult {
	return herdr.CliResult{Code: 0, Stdout: "[]"}
}

type fakeHerdrDown struct{}

func (fakeHerdrDown) HerdrCli(args []string, timeoutMs int) herdr.CliResult {
	return herdr.CliResult{Code: -1, Stderr: "connection refused"}
}

func checkOf(t *testing.T, res DoctorResult, name string) DoctorCheck {
	t.Helper()
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check named %s in %+v", name, res.Checks)
	return DoctorCheck{}
}

func TestDoctorHealthyFixtureAllPass(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	res := RunDoctor(healthyOpts(t, repo, home))
	if !res.OK {
		for _, c := range res.Checks {
			if !c.OK {
				t.Errorf("%s failed unexpectedly: %+v", c.Name, c)
			}
		}
		t.Fatal("healthy fixture must pass")
	}
	want := []string{"version", "config", "home", "git-remote", "gh-auth", "herdr", "daemon", "provider", "artifacts"}
	if len(res.Checks) != len(want) {
		t.Fatalf("got %d checks, want %d: %+v", len(res.Checks), len(want), res.Checks)
	}
	for i, name := range want {
		if res.Checks[i].Name != name {
			t.Errorf("check %d = %s, want %s (issue order is the contract)", i, res.Checks[i].Name, name)
		}
	}
}

func TestDoctorVersionMismatchFlaggedWithUnstampedNote(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.Version = versionUnstamped
	opts.LatestTag = func() (string, error) { return "v0.9.6", nil }
	c := checkOf(t, RunDoctor(opts), "version")
	if c.OK {
		t.Fatal("mismatched version must fail")
	}
	if !strings.Contains(c.Detail, "unstamped local build") {
		t.Errorf("unstamped build must be flagged in detail: %q", c.Detail)
	}
	if c.Hint == "" {
		t.Error("failing check must carry a remediation hint")
	}
}

const versionUnstamped = "0.1.0"

func TestDoctorInvalidEnvTokenFlaggedNotMasked(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.GhToken = func() (string, string) { return "env GITHUB_TOKEN", "ghp_bad" }
	opts.TokenValid = func(source, token string) (bool, string) {
		return false, "rejected (HTTP 401)"
	}
	c := checkOf(t, RunDoctor(opts), "gh-auth")
	if c.OK {
		t.Fatal("invalid env token must fail the check")
	}
	if !strings.Contains(c.Detail, "env GITHUB_TOKEN") {
		t.Errorf("detail must name the env source: %q", c.Detail)
	}
	if !strings.Contains(c.Hint, "unset or fix the env value") {
		t.Errorf("hint must point at the env override (env wins over keyring): %q", c.Hint)
	}
}

func TestDoctorKeyringTokenInvalidHintDiffers(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.GhToken = func() (string, string) { return "keyring (gh auth token)", "ghp_stale" }
	opts.TokenValid = func(source, token string) (bool, string) { return false, "rejected (HTTP 401)" }
	c := checkOf(t, RunDoctor(opts), "gh-auth")
	if !strings.Contains(c.Hint, "gh auth login") {
		t.Errorf("keyring failure hint must say gh auth login: %q", c.Hint)
	}
}

func TestDoctorNoTokenFails(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.GhToken = func() (string, string) { return "keyring (gh auth token)", "" }
	c := checkOf(t, RunDoctor(opts), "gh-auth")
	if c.OK || c.Hint == "" {
		t.Errorf("missing token must fail with a hint: %+v", c)
	}
}

func TestDoctorInvalidConfigKeyFailsWithLoadError(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte(`{"worker":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := healthyOpts(t, repo, home)
	opts.ProviderProbe = func(cmd string, args []string, dir string) resilience.Probe {
		return resilience.Probe{OK: true} // degraded default config still probes
	}
	res := RunDoctor(opts)
	c := checkOf(t, res, "config")
	if c.OK {
		t.Fatal("invalid config key must fail")
	}
	// The error string is config.Load's byte-identical contract.
	want := "Invalid worker \"nope\" in config; expected claude-code, opencode, omp, pi, grok, or both"
	if !strings.Contains(c.Detail, want) {
		t.Errorf("detail must carry the Load error: %q", c.Detail)
	}
}

func TestDoctorHomeMissingDirsFailWithMkdirHint(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	// Repoint DEVAGENT_HOME at an empty dir so locks/ and runs/ are absent.
	empty := t.TempDir()
	t.Setenv("DEVAGENT_HOME", empty)
	c := checkOf(t, RunDoctor(opts), "home")
	if c.OK {
		t.Fatal("missing locks/ and runs/ must fail")
	}
	if !strings.Contains(c.Detail, "locks") || !strings.Contains(c.Detail, "runs") {
		t.Errorf("detail must name both missing dirs: %q", c.Detail)
	}
	if !strings.Contains(c.Hint, "mkdir -p") {
		t.Errorf("hint must give the mkdir remediation: %q", c.Hint)
	}
}

func TestDoctorGitRemoteUnreachableFails(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.GitRemote = func() error { return errors.New("dial tcp: timeout") }
	c := checkOf(t, RunDoctor(opts), "git-remote")
	if c.OK || !strings.Contains(c.Detail, "timeout") {
		t.Errorf("unreachable remote must fail with the cause: %+v", c)
	}
}

func TestDoctorHerdrStoppedFailsWhenExpected(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	// herdr enabled in config → a dead session is a real failure.
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte(`{"herdr":{"enabled":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := healthyOpts(t, repo, home)
	opts.HerdrCli = fakeHerdrDown{}
	c := checkOf(t, RunDoctor(opts), "herdr")
	if c.OK || c.Hint == "" {
		t.Errorf("stopped herdr must fail with a hint: %+v", c)
	}
}

func TestDoctorHerdrAbsentNotConfiguredPassesOptional(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	t.Setenv("DEVAGENT_HERDR_BIN", filepath.Join(t.TempDir(), "no-such-herdr"))
	c := checkOf(t, RunDoctor(opts), "herdr")
	if !c.OK {
		t.Errorf("herdr absent and not enabled must pass as optional: %+v", c)
	}
	if !strings.Contains(c.Detail, "optional") {
		t.Errorf("detail must say optional: %q", c.Detail)
	}
}

func TestDoctorDaemonNotRunningIsOptionalPass(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.DaemonProbe = func() (int, error) {
		return 0, errors.New("Get \"http://127.0.0.1:7788/healthz\": dial tcp 127.0.0.1:7788: connect: connection refused")
	}
	c := checkOf(t, RunDoctor(opts), "daemon")
	if !c.OK {
		t.Errorf("daemon down must stay an optional pass: %+v", c)
	}
	if !strings.Contains(c.Detail, "not running") {
		t.Errorf("detail must say not running: %q", c.Detail)
	}
}

func TestDoctorDaemonUnhealthyFails(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.DaemonProbe = func() (int, error) { return 0, errors.New("context deadline exceeded") }
	c := checkOf(t, RunDoctor(opts), "daemon")
	if c.OK || !strings.Contains(c.Detail, "unhealthy") {
		t.Errorf("wedged daemon must fail: %+v", c)
	}
}

// TestDoctorDaemonHealthzProbeUsesUnauthenticatedEndpoint pins the /healthz
// choice: /status sits behind the bearer guard, so a bare probe would 401.
func TestDoctorDaemonHealthzProbeUsesUnauthenticatedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()
	url := strings.TrimPrefix(srv.URL, "http://")
	resp, err := http.Get("http://" + url + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz probe contract broken: %v %+v", err, resp)
	}
	_ = resp.Body.Close()
}

func TestDoctorStaleRunLockFailsWithHint(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	now := time.UnixMilli(10_000_000_000)
	if err := os.MkdirAll(filepath.Join(home, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "locks", "T1.lock"),
		[]byte(fmt.Sprintf(`{"pid":%d,"startedAt":%d}`, os.Getpid(), now.UnixMilli()-ledger.DefaultLockTTL-1)), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := healthyOpts(t, repo, home)
	opts.Now = func() time.Time { return now }
	c := checkOf(t, RunDoctor(opts), "artifacts")
	if c.OK || !strings.Contains(c.Detail, "stale run lock") {
		t.Errorf("stale run lock must fail: %+v", c)
	}
	if !strings.Contains(c.Hint, "loop.lock.d") {
		t.Errorf("hint must cover both remediations: %q", c.Hint)
	}
}

func TestDoctorFreshRunLockPasses(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	now := time.UnixMilli(10_000_000_000)
	if err := os.MkdirAll(filepath.Join(home, "locks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "locks", "T1.lock"),
		[]byte(fmt.Sprintf(`{"pid":%d,"startedAt":%d}`, os.Getpid(), now.UnixMilli()-1000)), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := healthyOpts(t, repo, home)
	opts.Now = func() time.Time { return now }
	if c := checkOf(t, RunDoctor(opts), "artifacts"); !c.OK {
		t.Errorf("fresh lock must pass: %+v", c)
	}
}

func TestDoctorStaleLoopLockDeadPidFails(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	lockDir := filepath.Join(repo, ".selfbuild", "loop.lock.d")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pid nothing can hold: above every real pid on darwin/linux/windows CI.
	if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte("2147483646\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := healthyOpts(t, repo, home)
	c := checkOf(t, RunDoctor(opts), "artifacts")
	if c.OK || !strings.Contains(c.Detail, "dead pid 2147483646") {
		t.Errorf("dead loop.lock pid must fail: %+v", c)
	}
}

func TestDoctorLiveLoopLockPidPasses(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	lockDir := filepath.Join(repo, ".selfbuild", "loop.lock.d")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "pid"), []byte(fmt.Sprint(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := healthyOpts(t, repo, home)
	if c := checkOf(t, RunDoctor(opts), "artifacts"); !c.OK {
		t.Errorf("live holder must pass: %+v", c)
	}
}

func TestDoctorAnyFailureMeansNotOK(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.GitRemote = func() error { return errors.New("boom") }
	res := RunDoctor(opts)
	if res.OK {
		t.Fatal("one failing check must make res.OK false (exit 1 contract)")
	}
}

func TestDoctorJSONShapeRoundTrips(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	res := RunDoctor(healthyOpts(t, repo, home))
	blob := marshalForTest(res)
	var back DoctorResult
	if err := unmarshalForTest(blob, &back); err != nil {
		t.Fatalf("json round-trip: %v", err)
	}
	if back.OK != res.OK || len(back.Checks) != len(res.Checks) {
		t.Errorf("round-trip mismatch: %+v vs %+v", back, res)
	}
}

func TestRenderDoctorReportNamesFailingCheckAndHint(t *testing.T) {
	res := DoctorResult{OK: false, Checks: []DoctorCheck{
		{Name: "gh-auth", OK: false, Detail: "invalid token", Hint: "unset or fix the env value"},
		{Name: "config", OK: true, Detail: "valid"},
	}}
	var lines []string
	RenderDoctorReport(res, func(s string) { lines = append(lines, s) })
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "gh-auth") || !strings.Contains(joined, "unset or fix the env value") {
		t.Errorf("render must name the failing check and hint:\n%s", joined)
	}
	if !strings.Contains(joined, "1 check failed") {
		t.Errorf("summary must count failures (singular):\n%s", joined)
	}
}

func TestDoctorLatestTagDefaultParsesRemoteTags(t *testing.T) {
	// The default seam shells out to git; on a no-origin temp dir it must
	// fall back to the local tag list and never error on empty output.
	repo := t.TempDir()
	run := func(args ...string) {
		if err := execIn(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-m", "init")
	run("tag", "v0.9.5")
	run("tag", "v0.9.4")
	run("tag", "notsemver")
	tag, err := doctorLatestTag(repo)
	if err != nil {
		t.Fatalf("doctorLatestTag: %v", err)
	}
	if tag != "v0.9.5" {
		t.Errorf("tag = %q, want v0.9.5 (numeric max over semver tags only)", tag)
	}
}

func TestDoctorResolveGhTokenEnvWins(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "envtok")
	source, token := doctorResolveGhToken()
	if source != "env GITHUB_TOKEN" || token != "envtok" {
		t.Errorf("env must win: %q %q", source, token)
	}
	t.Setenv("GITHUB_TOKEN", "")
	source, _ = doctorResolveGhToken()
	if !strings.HasPrefix(source, "keyring") {
		t.Errorf("no env → keyring source: %q", source)
	}
}

func TestDoctorConfigDefaultsWhenNoFile(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	c := checkOf(t, RunDoctor(opts), "config")
	if !c.OK || !strings.Contains(c.Detail, "defaults") {
		t.Errorf("missing config must pass with a defaults note: %+v", c)
	}
}

// TestDoctorProviderUsesLoopProbeArgv pins that the provider check probes the
// same argv the loop preflight builds (omp hardening flags included).
func TestDoctorProviderUsesLoopProbeArgv(t *testing.T) {
	var gotCmd string
	var gotArgs []string
	repo, home := t.TempDir(), t.TempDir()
	opts := healthyOpts(t, repo, home)
	opts.ProviderProbe = func(cmd string, args []string, dir string) resilience.Probe {
		gotCmd, gotArgs = cmd, args
		return resilience.Probe{OK: false, Detail: "no answer"}
	}
	c := checkOf(t, RunDoctor(opts), "provider")
	if c.OK {
		t.Fatal("failed probe must fail the check")
	}
	if gotCmd != "omp" {
		t.Errorf("probe cmd = %q, want omp (config default worker)", gotCmd)
	}
	if len(gotArgs) < 2 || gotArgs[0] != "-p" || gotArgs[1] != "OK" {
		t.Errorf("probe args must place the prompt after -p (gate contract): %v", gotArgs)
	}
	if gotArgs[2] != "--mode" {
		t.Errorf("probe args must carry the loop's hardening flags: %v", gotArgs)
	}
}

func marshalForTest(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func unmarshalForTest(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func execIn(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}
