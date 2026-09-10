package resilience

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// requireSh skips the probe-spawn tests off POSIX: they drive /bin/sh as
// the fake worker CLI.
func requireSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake worker CLI needs /bin/sh")
	}
}

// TestRunPreflightProbeCompletesOnStreamedMarker: #306 acceptance. A
// worker that prints the OK marker and then hangs forever (standing in
// for omp's session/advisor/teardown tail) must be classified ok well
// inside the cap — the old buffered probe waited for process exit and
// failed this at the cap.
func TestRunPreflightProbeCompletesOnStreamedMarker(t *testing.T) {
	requireSh(t)
	start := time.Now()
	p := RunPreflightProbe("/bin/sh", []string{
		"-c", `echo '{"role":"assistant","content":[{"type":"text","text":"OK"}]}'; sleep 60`,
	}, t.TempDir(), 15_000)
	elapsed := time.Since(start)
	if !p.OK {
		t.Fatalf("probe = %+v after %s, want ok on the streamed marker", p, elapsed)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe completed in %s, want well inside the 15s cap", elapsed)
	}
}

// TestRunPreflightProbeGrokMarkerShape: the grok streaming-json chunk
// shape completes the probe too, on a natural quick exit.
func TestRunPreflightProbeGrokMarkerShape(t *testing.T) {
	requireSh(t)
	p := RunPreflightProbe("/bin/sh", []string{
		"-c", `echo '{"type":"text","data":"OK"}'`,
	}, t.TempDir(), 15_000)
	if !p.OK {
		t.Fatalf("probe = %+v, want ok on the grok data marker", p)
	}
}

// TestRunPreflightProbeDegradesAtCapWhenNeverAnswered: the wall cap stays
// the backstop — a worker that never prints the marker degrades in bounded
// time, with the group kill returning promptly (the sleep would run 60s
// untouched) and the buffered path's empty-output detail shape.
func TestRunPreflightProbeDegradesAtCapWhenNeverAnswered(t *testing.T) {
	requireSh(t)
	start := time.Now()
	p := RunPreflightProbe("/bin/sh", []string{"-c", "sleep 60"}, t.TempDir(), 500)
	elapsed := time.Since(start)
	if p.OK {
		t.Fatalf("probe ok, want degraded without a marker")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe took %s, want the 500ms cap to kill and return", elapsed)
	}
	if p.Detail != "exit -1" {
		t.Errorf("detail = %q, want the empty-output %q shape", p.Detail, "exit -1")
	}
}

// TestRunPreflightProbeExitZeroWithoutMarkerDegrades: the old rule that a
// clean exit without an answer is degraded still holds.
func TestRunPreflightProbeExitZeroWithoutMarkerDegrades(t *testing.T) {
	requireSh(t)
	p := RunPreflightProbe("/bin/sh", []string{"-c", "echo hello"}, t.TempDir(), 5_000)
	if p.OK {
		t.Fatalf("probe ok, want degraded on exit 0 without a marker")
	}
	if !strings.Contains(p.Detail, "hello") {
		t.Errorf("detail = %q, want the captured output excerpt", p.Detail)
	}
}

// TestRunPreflightProbeSpawnFailureShape: an unspawnable command degrades
// with the buffered path's ENOENT shape (exit -1, no output).
func TestRunPreflightProbeSpawnFailureShape(t *testing.T) {
	p := RunPreflightProbe("devagent-probe-nonexistent-cli", nil, t.TempDir(), 5_000)
	if p.OK {
		t.Fatal("probe ok, want degraded on spawn failure")
	}
	if p.Detail != "exit -1" {
		t.Errorf("detail = %q, want %q", p.Detail, "exit -1")
	}
}

// TestProbeRetryCapOverlayBoundsInProcessRetries: #306 requirement 3 —
// every probe launch carries a run-scoped config overlay capping the
// CLI's own retry budget (default 2; DEVAGENT_PROBE_API_MAX_RETRIES
// overrides; the user's config file is never touched).
func TestProbeRetryCapOverlayBoundsInProcessRetries(t *testing.T) {
	path, err := writeProbeRetryCapOverlay()
	if err != nil {
		t.Fatalf("writeProbeRetryCapOverlay: %v", err)
	}
	defer os.Remove(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, "retry:") || !strings.Contains(s, "maxRetries: 2") {
		t.Fatalf("overlay %q, want the retry group with the default cap", s)
	}

	t.Setenv(ProbeAPIRetryCapEnv, "7")
	path2, err := writeProbeRetryCapOverlay()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path2)
	b2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b2), "maxRetries: 7") {
		t.Fatalf("overlay %q, want the env override cap", b2)
	}
}

// TestProbeOverlayEnvAppendsToExisting: an operator's PI_CONFIG_FILES is
// preserved with the probe overlay last (later files win).
func TestProbeOverlayEnvAppendsToExisting(t *testing.T) {
	t.Setenv("PI_CONFIG_FILES", "/tmp/operator-a.yml")
	env := probeOverlayEnv("/tmp/probe-overlay.yml")
	if env["PI_CONFIG_FILES"] != "/tmp/operator-a.yml"+string(os.PathListSeparator)+"/tmp/probe-overlay.yml" {
		t.Fatalf("PI_CONFIG_FILES = %q, want operator value with the overlay appended", env["PI_CONFIG_FILES"])
	}
	if probeOverlayEnv("") != nil {
		t.Fatal("empty overlay must add no env")
	}
}

// TestRunPreflightProbePassesRetryOverlayToChild: end-to-end — the fake
// worker sees the overlay path in its environment (per-invocation; on the
// degraded path the echo lands in the detail).
func TestRunPreflightProbePassesRetryOverlayToChild(t *testing.T) {
	requireSh(t)
	p := RunPreflightProbe("/bin/sh", []string{"-c", `echo "PI=$PI_CONFIG_FILES"; exit 3`}, t.TempDir(), 5_000)
	if p.OK {
		t.Fatal("probe ok, want degraded on exit 3 without a marker")
	}
	if !strings.Contains(p.Detail, "devagent-probe-retry-overlay") {
		t.Fatalf("detail = %q, want the PI_CONFIG_FILES overlay path the child received", p.Detail)
	}
}
