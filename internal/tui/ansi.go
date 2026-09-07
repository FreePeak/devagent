// Package tui holds the Go port of the §20.8 card/chip ANSI helpers from
// src/tui/tui.ts that the human-readable surfaces share (init checklist,
// queue/validate/ledger cards). Seeds FR-GO-11 — the full TUI port reuses
// this package.
package tui

import "strings"

// ANSI colors: dim lines are the quiet majority (pilot-style dashboard).
// Byte-identical to the C block in src/tui/tui.ts.
const (
	Reset   = "\x1b[0m"
	Dim     = "\x1b[2m"
	Bold    = "\x1b[1m"
	Green   = "\x1b[32m"
	Yellow  = "\x1b[33m"
	Red     = "\x1b[31m"
	Cyan    = "\x1b[36m"
	Magenta = "\x1b[35m"
	Inverse = "\x1b[7m"
)

// StatusColor mirrors statusColor().
func StatusColor(status string) string {
	switch strings.ToLower(status) {
	case "running":
		return Green
	case "idle":
		return Yellow
	case "stale":
		return Magenta
	case "failed":
		return Red
	default:
		return Dim
	}
}

// Truncate mirrors truncate(): runes, with an ellipsis at the cap.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		n = 1
	}
	return string(r[:n-1]) + "…"
}

// DimText wraps s in the dim color.
func DimText(s string) string { return Dim + s + Reset }

// CyanText wraps s in the cyan accent (attach hints and other §20.8
// emphasis).
func CyanText(s string) string { return Cyan + s + Reset }

// ChipFor renders the status chip: colored dot + label, e.g. "● running"
// (Pilot-style).
func ChipFor(state string, label string) string {
	var dot string
	switch state {
	case "running", "ok":
		dot = Green
	case "failed":
		dot = Red
	case "stale":
		dot = Magenta
	default:
		dot = Yellow
	}
	if label == "" {
		label = state
	}
	return dot + "●" + Reset + " " + StatusColor(state) + Truncate(label, 18) + Reset
}
