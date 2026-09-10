package commands

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/version"
)

// doctorFixture builds a hermetic doctor run: a temp repo with a valid
// devagent.json, a temp DEVAGENT_HOME with locks/ and runs/, a fake herdr
// binary (so check 6's LookPath never reads the host PATH), and every
// external touchpoint (network, gh, herdr, daemon, pid liveness) behind a
// green seam that individual tests override to inject the fault.
type doctorFixture struct {
	t        *testing.T
	repo     string
	home     string
	opts     DoctorOptions
	restores []func()
}

func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	repo := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte(`{"worker":"omp","model":"onegw/free"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"locks", "runs"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fakeHerdr := filepath.Join(home, "herdr-fake")
	if err := os.WriteFile(fakeHerdr, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_HERDR_BIN", fakeHerdr)
	f := &doctorFixture{t: t, repo: repo, home: home}
	f.opts = DoctorOptions{
		RepoPath:         repo,
		LatestReleaseTag: func() (string, error) { return "v1.2.3", nil },
		GhToken:          func() GhTokenInfo { return GhTokenInfo{Token: "tok", Source: "keyring"} },
		GhTokenValid:     func(string) (bool, string) { return true, "" },
		GitRemoteProbe:   func(string) (bool, string) { return true, "origin reachable" },
		HerdrList:        func() herdr.CliResult { return herdr.CliResult{Code: 0, Stdout: `{"result":{"agents":[]}}`} },
		DaemonStatus:     func() (daemonState, string) { return daemonOK, "daemon /status healthy (authenticated)" },
		Probe: func(string, []string, string) resilience.Probe {
			return resilience.Probe{OK: true}
		},
		ProcessAlive: func(int) bool { return true },
	}
	t.Cleanup(func() {
		for _, r := range f.restores {
			r()
		}
	})
	return f
}

// stampVersion pins version.Version for the run (the package var is the
// stamp surface the Makefile -ldflags writes).
func (f *doctorFixture) stampVersion(v string) {
	f.t.Helper()
	old := version.Version
	version.Version = v
	f.restores = append(f.restores, func() { version.Version = old })
}

func (f *doctorFixture) run() DoctorResult {
	f.t.Helper()
	res, err := RunDoctor(f.opts)
	if err != nil {
		f.t.Fatalf("RunDoctor: %v", err)
	}
	return res
}

func checkByName(t *testing.T, res DoctorResult, name string) DoctorCheck {
	t.Helper()
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q missing from result", name)
	return DoctorCheck{}
}

// TestDoctorHealthyBoxPasses: the acceptance criterion — a healthy box
// exits 0 (result.OK true) and all nine checks answer.
func TestDoctorHealthyBoxPasses(t *testing.T) {
	f := newDoctorFixture(t)
	f.stampVersion("1.2.3")
	res := f.run()
	if !res.OK {
		t.Fatalf("healthy box reported ok=false: %+v", res)
	}
	want := []string{"version", "config", "home", "git-remote", "gh-auth", "herdr", "daemon", "provider", "artifacts"}
	if len(res.Checks) != len(want) {
		t.Fatalf("got %d checks, want %d: %+v", len(res.Checks), len(want), res.Checks)
	}
	for i, c := range res.Checks {
		if c.Name != want[i] || !c.OK {
			t.Errorf("check %d = %s ok=%v, want %s ok=true", i, c.Name, c.OK, want[i])
		}
	}
}

// TestDoctorUnstampedBuildWarnsNotFails: an unstamped local build is
// cosmetic — one warn row, exit still 0.
func TestDoctorUnstampedBuildWarnsNotFails(t *testing.T) {
	f := newDoctorFixture(t) // version.Version stays the 0.1.0 build default
	res := f.run()
	c := checkByName(t, res, "version")
	if !c.OK || !c.Warn {
		t.Fatalf("unstamped build: ok=%v warn=%v, want ok=true warn=true", c.OK, c.Warn)
	}
	if !strings.Contains(c.Detail, "unstamped") {
		t.Errorf("detail %q does not flag the unstamped build", c.Detail)
	}
	if !res.OK {
		t.Errorf("warn row must not fail the run")
	}
}

// TestDoctorStaleReleaseWarns: a stamped binary older than the latest
// release warns with the self-update hint.
func TestDoctorStaleReleaseWarns(t *testing.T) {
	f := newDoctorFixture(t)
	f.stampVersion("1.0.0")
	res := f.run()
	c := checkByName(t, res, "version")
	if !c.Warn || !strings.Contains(c.Detail, "1.0.0") || !strings.Contains(c.Detail, "v1.2.3") {
		t.Fatalf("stale release warn wrong: %+v", c)
	}
	if !res.OK {
		t.Errorf("stale release must not fail the run")
	}
}

// TestDoctorBadConfigKey: an invalid config key is named with a remediation
// hint (fault fixture: bad worker).
func TestDoctorBadConfigKey(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.WriteFile(filepath.Join(f.repo, "devagent.json"), []byte(`{"worker":"nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.run()
	c := checkByName(t, res, "config")
	if c.OK {
		t.Fatal("invalid worker passed the config check")
	}
	if !strings.Contains(c.Detail, "Invalid worker") {
		t.Errorf("detail does not name the fault: %q", c.Detail)
	}
	if c.Hint == "" || !strings.Contains(c.Hint, "devagent.json") {
		t.Errorf("missing remediation hint: %q", c.Hint)
	}
	if res.OK {
		t.Errorf("a failed check must fail the run")
	}
}

// TestDoctorMissingConfig: no devagent.json is a named failure.
func TestDoctorMissingConfig(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.Remove(filepath.Join(f.repo, "devagent.json")); err != nil {
		t.Fatal(err)
	}
	res := f.run()
	c := checkByName(t, res, "config")
	if c.OK || !strings.Contains(c.Hint, "devagent init") {
		t.Fatalf("missing config: %+v", c)
	}
}

// TestDoctorInvalidEnvTokenNotMasked: the 2026-09-09 incident — an invalid
// env GITHUB_TOKEN must be flagged with the shadowing hint, never masked by
// a healthy keyring behind it.
func TestDoctorInvalidEnvTokenNotMasked(t *testing.T) {
	f := newDoctorFixture(t)
	f.opts.GhToken = func() GhTokenInfo { return GhTokenInfo{Token: "bad", Source: "env"} }
	f.opts.GhTokenValid = func(string) (bool, string) { return false, "Bad credentials" }
	res := f.run()
	c := checkByName(t, res, "gh-auth")
	if c.OK {
		t.Fatal("invalid env token passed")
	}
	if !strings.Contains(c.Detail, "env") || !strings.Contains(c.Detail, "INVALID") {
		t.Errorf("detail does not report source + invalidity: %q", c.Detail)
	}
	if !strings.Contains(c.Hint, "GITHUB_TOKEN") || !strings.Contains(c.Hint, "shadows") {
		t.Errorf("hint does not carry the shadowing lesson: %q", c.Hint)
	}
}

// TestDoctorInvalidKeyringToken: same failure from the keyring gets the
// gh-auth-login hint instead.
func TestDoctorInvalidKeyringToken(t *testing.T) {
	f := newDoctorFixture(t)
	f.opts.GhTokenValid = func(string) (bool, string) { return false, "Bad credentials" }
	res := f.run()
	c := checkByName(t, res, "gh-auth")
	if !strings.Contains(c.Hint, "gh auth login") {
		t.Fatalf("keyring hint wrong: %q", c.Hint)
	}
}

// TestDoctorHerdrBinaryMissing: no herdr binary resolvable is a named
// failure (the fixture's fake binary is pointed at a nonexistent path).
func TestDoctorHerdrBinaryMissing(t *testing.T) {
	f := newDoctorFixture(t)
	t.Setenv("DEVAGENT_HERDR_BIN", filepath.Join(f.home, "no-such-herdr"))
	res := f.run()
	c := checkByName(t, res, "herdr")
	if c.OK || !strings.Contains(c.Detail, "not on PATH") {
		t.Fatalf("missing herdr binary: %+v", c)
	}
}

// TestDoctorHerdrSessionStopped: binary present but the session RPC fails
// is a named failure with a start-the-session hint.
func TestDoctorHerdrSessionStopped(t *testing.T) {
	f := newDoctorFixture(t)
	f.opts.HerdrList = func() herdr.CliResult { return herdr.CliResult{Code: -1} }
	res := f.run()
	c := checkByName(t, res, "herdr")
	if c.OK || !strings.Contains(c.Detail, "agent list failed") {
		t.Fatalf("stopped herdr session: %+v", c)
	}
	if !strings.Contains(c.Hint, "session create") {
		t.Errorf("hint wrong: %q", c.Hint)
	}
}

// TestDoctorDaemonStates: the four verdicts — down is an informational
// pass; unauthorized and unhealthy fail with distinct hints.
func TestDoctorDaemonStates(t *testing.T) {
	cases := []struct {
		state    daemonState
		wantOK   bool
		wantHint string
	}{
		{daemonDown, true, ""},
		{daemonOK, true, ""},
		{daemonUnauthorized, false, "daemon-token"},
		{daemonUnhealthy, false, "restart the daemon"},
	}
	for _, tc := range cases {
		f := newDoctorFixture(t)
		f.opts.DaemonStatus = func() (daemonState, string) { return tc.state, "detail " + string(tc.state) }
		res := f.run()
		c := checkByName(t, res, "daemon")
		if c.OK != tc.wantOK {
			t.Errorf("%s: ok=%v, want %v (detail %q)", tc.state, c.OK, tc.wantOK, c.Detail)
		}
		if tc.wantHint != "" && !strings.Contains(c.Hint, tc.wantHint) {
			t.Errorf("%s: hint %q does not carry %q", tc.state, c.Hint, tc.wantHint)
		}
	}
}

// TestDoctorHomeMissingDirs: a writable home without locks/ and runs/ fails
// with the mkdir hint; creating them clears it.
func TestDoctorHomeMissingDirs(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.Remove(filepath.Join(f.home, "runs")); err != nil {
		t.Fatal(err)
	}
	res := f.run()
	c := checkByName(t, res, "home")
	if c.OK || !strings.Contains(c.Hint, "mkdir -p") {
		t.Fatalf("missing runs/: %+v", c)
	}
	if err := os.MkdirAll(filepath.Join(f.home, "runs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if again := f.run(); !checkByName(t, again, "home").OK {
		t.Fatalf("home still failing after mkdir: %+v", checkByName(t, again, "home"))
	}
}

// TestDoctorGitRemoteUnreachable: a dead remote fails with the BatchMode
// hint (fault fixture).
func TestDoctorGitRemoteUnreachable(t *testing.T) {
	f := newDoctorFixture(t)
	f.opts.GitRemoteProbe = func(string) (bool, string) { return false, "ssh git@github.com unreachable (BatchMode)" }
	res := f.run()
	c := checkByName(t, res, "git-remote")
	if c.OK || !strings.Contains(c.Hint, "BatchMode") {
		t.Fatalf("unreachable remote: %+v", c)
	}
}

// TestDoctorProviderFail: the loop's bare probe (single attempt, no gate
// side effects) failing is a named failure with a provider hint.
func TestDoctorProviderFail(t *testing.T) {
	f := newDoctorFixture(t)
	f.opts.Probe = func(string, []string, string) resilience.Probe {
		return resilience.Probe{OK: false, Detail: "exit 1"}
	}
	res := f.run()
	c := checkByName(t, res, "provider")
	if c.OK || !strings.Contains(c.Detail, "omp") || !strings.Contains(c.Hint, "provider login") {
		t.Fatalf("failed provider: %+v", c)
	}
}

// TestDoctorStaleArtifacts: TTL-expired run locks warn (self-heal), a
// dead-pid run lock fails, and a loop.lock.d with a dead pid fails with the
// rm hint. pid 42 is the live driver in the fixture.
func TestDoctorStaleArtifacts(t *testing.T) {
	f := newDoctorFixture(t)
	f.opts.ProcessAlive = func(pid int) bool { return pid == 42 }
	old := strconv.FormatInt(time.Now().Add(-2*time.Hour).UnixMilli(), 10)
	fresh := strconv.FormatInt(time.Now().UnixMilli(), 10)

	// TTL-expired lock with a live pid: warn only.
	if err := os.WriteFile(filepath.Join(f.home, "locks", "T-1.lock"), []byte(`{"pid":42,"startedAt":`+old+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.run()
	c := checkByName(t, res, "artifacts")
	if !c.OK || !c.Warn || !strings.Contains(c.Detail, "older than TTL") {
		t.Fatalf("TTL lock: %+v", c)
	}
	if !res.OK {
		t.Errorf("self-healing lock must not fail the run")
	}

	// Fresh lock whose holder pid is gone: fail.
	if err := os.WriteFile(filepath.Join(f.home, "locks", "T-2.lock"), []byte(`{"pid":7,"startedAt":`+fresh+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	res = f.run()
	c = checkByName(t, res, "artifacts")
	if c.OK || !strings.Contains(c.Detail, "pid 7 is gone") {
		t.Fatalf("zombie lock: %+v", c)
	}

	// Wedged loop.lock.d (dead driver pid): fail with the rm -rf hint.
	lockD := filepath.Join(f.repo, ".selfbuild", "loop.lock.d")
	if err := os.MkdirAll(lockD, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockD, "pid"), []byte("7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = f.run()
	c = checkByName(t, res, "artifacts")
	if c.OK || !strings.Contains(c.Hint, "rm -rf") {
		t.Fatalf("wedged loop.lock.d: %+v", c)
	}

	// Live driver pid: back to pass, pid named (the dead-pid lock removed
	// first so the phase asserts its own verdict, not the leftover's).
	if err := os.Remove(filepath.Join(f.home, "locks", "T-2.lock")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockD, "pid"), []byte("42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = f.run()
	c = checkByName(t, res, "artifacts")
	if !c.OK || !strings.Contains(c.Detail, "active driver pid 42") {
		t.Fatalf("live driver pid: %+v", c)
	}
}

// TestRenderDoctorReport: the human surface prints the checklist rows and
// the remediation hint under failures.
func TestRenderDoctorReport(t *testing.T) {
	res := DoctorResult{RepoPath: "/repo", OK: false, Checks: []DoctorCheck{
		{Name: "config", OK: true, Detail: "devagent.json parses and validates"},
		{Name: "herdr", Detail: "agent list failed", Hint: "start the session"},
	}}
	var lines []string
	RenderDoctorReport(res, func(s string) { lines = append(lines, s) })
	out := strings.Join(lines, "\n")
	if !strings.Contains(out, "herdr") || !strings.Contains(out, "start the session") {
		t.Fatalf("report missing failure + hint:\n%s", out)
	}
	if !strings.Contains(out, "one or more checks failed") {
		t.Fatalf("verdict line missing:\n%s", out)
	}
}
