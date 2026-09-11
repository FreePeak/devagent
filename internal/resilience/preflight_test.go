package resilience

import (
	"path/filepath"
	"testing"
	"time"
)

// TestRunPreflightProbeCompletesOnStreamedMarker pins the #308 production
// failure: the model answered (`"text":"OK"` streamed, stopReason stop) while
// the CLI process lingered past the 70s kill, so the exit-waiting probe
// stamped a healthy provider provider-degraded. The probe must return OK the
// moment the marker streams — well before the timeout — even though the fake
// CLI never exits.
func TestRunPreflightProbeCompletesOnStreamedMarker(t *testing.T) {
	// Both captured marker shapes must stay verbatim: omp's `--mode json`
	// event stream and grok's `--output-format streaming-json` text chunk.
	// Each is followed by the lingering post-answer machinery the fix
	// exists for.
	for _, tc := range []struct{ name, marker string }{
		{"omp", `{"type":"assistant","text":"OK","stopReason":"stop"}`},
		{"grok", `{"type":"text","data":"OK"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := `printf '%s' '` + tc.marker + `'; sleep 600`
			start := time.Now()
			probe := RunPreflightProbe("/bin/sh", []string{"-c", script}, t.TempDir(), 5000)
			elapsed := time.Since(start)
			if !probe.OK {
				t.Fatalf("probe degraded despite streamed %s marker: %+v", tc.name, probe)
			}
			if elapsed >= 4500*time.Millisecond {
				t.Fatalf("probe waited %s (timeout 5s) instead of completing on the marker", elapsed)
			}
		})
	}
}

// TestRunPreflightProbeExitWithoutMarkerStaysDegraded pins the kept rule: a
// run that ends cleanly but never streams an OK marker is degraded, with the
// captured output as the detail.
func TestRunPreflightProbeExitWithoutMarkerStaysDegraded(t *testing.T) {
	script := `printf '%s' '{"type":"text","data":"NOPE"}'; exit 0`
	probe := RunPreflightProbe("/bin/sh", []string{"-c", script}, filepath.Dir(t.TempDir()), 5000)
	if probe.OK {
		t.Fatalf("probe OK despite no marker: %+v", probe)
	}
	if probe.Detail == "" {
		t.Fatal("degraded probe must carry a detail excerpt")
	}
}
