package tui

import "time"

// Loop is the seam for the interactive raw-mode terminal driver
// (FR-TUI alternate screen, key handling, suspend/attach). FR-GO-11 ships
// only the pure rendering + input-parsing half; the loop driving it is
// TODO(FR-GO-13 #202): implement the interactive loop on this seam.
//
// The TS reference is src/tui/tui.ts runInteractive/runTui: poll a snapshot,
// render, diff frames via RenderFrame, decode stdin via DecodeKeys, and map
// keys to state transitions (view switching, selection, overlays, kill
// confirm, quit).
type Loop interface {
	// Run drives the alternate-screen session until the operator quits
	// (q / Ctrl+C). It must restore the screen on exit.
	Run() error
	// Snapshot returns the latest polled snapshot (the render input).
	Snapshot() *Snapshot
	// ApplyKeys feeds one decoded chunk into the loop's state machine
	// (view, selection, overlays, pendingKill) and reports whether the
	// loop should quit.
	ApplyKeys(res DecodeResult) (quit bool)
}

// Transport is the seam over the FR-CTRL daemon API (FR-GO-12): the TUI is a
// pure HTTP + SSE client. TODO(FR-GO-12 #200): implement the Go daemon
// client on this seam; the renderers only need the Snapshot it produces.
type Transport interface {
	// FetchSnapshot polls /status + /agents + /history + /sessions and
	// normalizes the envelopes exactly like the TS fetchSnapshot — every
	// field tolerates a partial failure, never throws.
	FetchSnapshot(opts TuiOptions) (*Snapshot, error)
	// PostJSON issues one POST and returns the note payload for flows like
	// the kill confirm (POST /approve with answer __kill__).
	PostJSON(opts TuiOptions, path string, body any) (status int, ok bool, note string)
}

// FetchedAt returns time.Now truncated like the TS Date.now() ms precision.
func FetchedAt() time.Time { return time.Now() }
