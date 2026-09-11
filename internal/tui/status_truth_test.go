package tui

import (
	"strings"
	"testing"
)

// TestLiveRunsFieldTrustsDaemonCount pins the status truthfulness fix,
// verified on the real surface 2026-09-11 16:4x: the TUI painted "runs 0a"
// while the daemon reported runs.active 1 with a live task, because the field
// counted herdr panes and workers are not herdr panes (roster empty). The
// daemon's lock-derived count must win.
func TestLiveRunsFieldTrustsDaemonCount(t *testing.T) {
	status := &StatusPayload{Runs: &RunsPayload{Active: f64(3)}}
	got := liveRunsField(status, nil)
	if !strings.Contains(got, "3a") {
		t.Fatalf("live runs must show the daemon's active count, got %q", got)
	}

	// Payload without a runs block keeps the pane fallback.
	paneStatus := &StatusPayload{}
	panes := []TuiPane{{State: "running"}, {State: "idle"}}
	if got := liveRunsField(paneStatus, panes); !strings.Contains(got, "1a") {
		t.Fatalf("pane fallback must count running panes, got %q", got)
	}
}

// TestHeroShowsInFlightRunsInsteadOfIdleCue pins the hero half: with an empty
// pane roster but live runs, the hero must not paint the idle next-action cue.
func TestHeroShowsInFlightRunsInsteadOfIdleCue(t *testing.T) {
	snap := &Snapshot{Status: &StatusPayload{Runs: &RunsPayload{Active: f64(1)}}}
	lines := heroLines(snap, RenderOptions{View: ViewWorkers}, nil)
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "next:") {
		t.Fatalf("idle cue painted while a run is in flight:\n%s", joined)
	}
	if !strings.Contains(joined, "run(s) in flight") {
		t.Fatalf("hero must report the in-flight run:\n%s", joined)
	}

	// With no runs and no panes the idle cue still applies.
	idle := strings.Join(heroLines(&Snapshot{Status: &StatusPayload{}}, RenderOptions{View: ViewWorkers}, nil), "\n")
	if !strings.Contains(idle, "next:") {
		t.Fatalf("idle hero must keep its next-action cue:\n%s", idle)
	}
}
