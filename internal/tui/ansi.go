// Package tui holds the Go port of the §20.8 card/chip ANSI helpers from
// src/tui/tui.ts that the human-readable surfaces share (init checklist,
// queue/validate/ledger cards). Seeds FR-GO-11 — the full TUI port reuses
// this package.
package tui

import (
	"fmt"
	"os"
	"strings"
)

// Muted dashboard palette (FR-TUI-P-09, pilot's chart colors):
//
//	running → steel  #7eb8da      ok → sage  #7ec699
//	fail    → rose   #d48a8a      warn → amber #e0af68
//	border  → slate  #3d4450      dim/accents → gray
//
// Truecolor when COLORTERM advertises it (truecolor / 24bit), a 16-color
// fallback otherwise, and a monochrome palette when the terminal says it
// cannot or should not receive color: NO_COLOR set (the no-color.org
// convention every researched TUI honors — k9s, htop -C, opencode "none")
// or TERM=dumb. The exported names keep their historical slots so
// every call site (and the §20.8 human cards that share this package) keeps
// one visual language; only the resolved values differ per mode:
//
//	Green  → sage        Red    → rose       Yellow → amber
//	Cyan   → accent (steel)         Steel → running       Border → slate
//	Dim    → gray           Magenta is retired (rainbow color).
//
// Mono keeps the structural attributes (Bold / Dim-faint / Inverse) —
// htop's -C does the same — and empties every color; state still reads
// through the glyphs (● / ▸) and labels.
var (
	Reset   = "\x1b[0m"
	Dim     = "\x1b[2m"
	Bold    = "\x1b[1m"
	Green   = "\x1b[32m"
	Yellow  = "\x1b[33m"
	Red     = "\x1b[31m"
	Cyan    = "\x1b[36m"
	Steel   = "\x1b[36m"
	Border  = "\x1b[2m"
	Inverse = "\x1b[7m"
)

// TruecolorSGR renders a hex color (#rrggbb) as an SGR truecolor prefix.
func TruecolorSGR(hex string) string {
	if len(hex) != 7 || hex[0] != '#' {
		return ""
	}
	var r, g, b int
	_, _ = fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, b)
}

// applyPalette rebinds the SGR vars for one palette mode.
func applyPalette(truecolor bool) {
	if monoMode {
		applyMono()
		return
	}
	if truecolor {
		Steel = TruecolorSGR("#7eb8da")
		Green = TruecolorSGR("#7ec699")
		Red = TruecolorSGR("#d48a8a")
		Yellow = TruecolorSGR("#e0af68")
		Cyan = TruecolorSGR("#7eb8da") // accent tracks the running steel
		Dim = TruecolorSGR("#828a97")
		Border = TruecolorSGR("#3d4450")
		return
	}
	// 16-color fallback: nearest ANSI slots (steel ≈ cyan, amber ≈ yellow,
	// slate ≈ bright black, gray ≈ faint).
	Steel = "\x1b[36m"
	Green = "\x1b[32m"
	Red = "\x1b[31m"
	Yellow = "\x1b[33m"
	Cyan = "\x1b[36m"
	Dim = "\x1b[2m"
	Border = "\x1b[2m"
}

// applyMono empties every color var, keeping Bold/Dim/Inverse structure.
func applyMono() {
	Steel, Green, Red, Yellow, Cyan, Border = "", "", "", "", "", ""
	Dim = ""
}

// monoFor pins the mono decision for one env lookup (pure, testable):
// NO_COLOR non-empty (the no-color.org convention every researched TUI
// honors — k9s, htop -C, opencode "none") or TERM=dumb.
func monoFor(getenv func(string) string) bool {
	return getenv("NO_COLOR") != "" || getenv("TERM") == "dumb"
}

var monoMode = false

// paletteFor pins the palette for one env lookup (pure, testable).
func paletteFor(getenv func(string) string) bool {
	return getenv("COLORTERM") == "truecolor" || getenv("COLORTERM") == "24bit"
}

func init() {
	monoMode = monoFor(os.Getenv)
	applyPalette(paletteFor(os.Getenv))
}

// SetMono forces the monochrome palette (tests); the returned func restores
// the process-detected mode.
func SetMono(on bool) (restore func()) {
	prev := monoMode
	monoMode = on
	applyPalette(paletteFor(os.Getenv))
	return func() {
		monoMode = prev
		applyPalette(paletteFor(os.Getenv))
	}
}

// StatusColor maps a state onto the muted palette. "paused" is the
// approval-needed aggregate (amber — the operator must act; the
// opencode/crush convention: permission-needed is never silent).
func StatusColor(status string) string {
	switch strings.ToLower(status) {
	case "running":
		return Steel
	case "ok", "pass", "done":
		return Green
	case "stale", "warn", "paused":
		return Yellow
	case "failed", "fail":
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

// DimText wraps s in the dim (gray) color.
func DimText(s string) string { return Dim + s + Reset }

// CyanText wraps s in the muted accent (attach hints and other §20.8
// emphasis).
func CyanText(s string) string { return Cyan + s + Reset }

// ChipFor renders the status chip: colored dot + label, e.g. "● running"
// (Pilot-style), colored from the muted map.
func ChipFor(state string, label string) string {
	var dot string
	switch state {
	case "running":
		dot = Steel
	case "ok", "done":
		dot = Green
	case "failed":
		dot = Red
	case "stale", "warn":
		dot = Yellow
	default:
		dot = Yellow
	}
	if label == "" {
		label = state
	}
	return dot + "●" + Reset + " " + StatusColor(state) + Truncate(label, 18) + Reset
}
