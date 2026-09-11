package tui

import (
	"strings"
	"testing"
)

// TestNewlineKeysInsideGoalInput pins the multi-line goal input: Ctrl+N and
// Alt+Enter (ESC CR) insert a line break, Enter still submits the whole draft,
// and Backspace across the break joins the lines back. Regression for the
// one-line sheet where a multi-line goal was impossible to type.
func TestNewlineKeysInsideGoalInput(t *testing.T) {
	l := newTestLoop(t, TuiOptions{})
	l.ApplyKeys(DecodeKeys("n", false)) // dispatch sheet
	if l.overlay == nil || l.overlay.Kind != "dispatch" {
		t.Fatalf("n must open the dispatch sheet, got %+v", l.overlay)
	}
	l.ApplyKeys(DecodeKeys("first line", false))
	l.ApplyKeys(DecodeKeys("\x0e", false)) // Ctrl+N
	l.ApplyKeys(DecodeKeys("second line", false))
	if got := l.overlay.Input; got != "first line\nsecond line" {
		t.Fatalf("Ctrl+N must insert a newline, got %q", got)
	}
	l.ApplyKeys(DecodeKeys("\x1b\r", false)) // Alt+Enter
	if got := l.overlay.Input; got != "first line\nsecond line\n" {
		t.Fatalf("Alt+Enter must insert a newline, got %q", got)
	}
	// Backspace trims the trailing newline (the break is an ordinary rune).
	l.ApplyKeys(DecodeKeys("\x7f", false))
	if got := l.overlay.Input; got != "first line\nsecond line" {
		t.Fatalf("backspace must remove the line break, got %q", got)
	}
	// Enter submits rather than inserting another break.
	l.ApplyKeys(DecodeKeys("\r", false))
	if l.overlay != nil {
		t.Fatalf("Enter must submit and close the sheet, overlay still %+v", l.overlay)
	}
}

// TestDecodeNewlineKeys pins the decoder contract for the two bindings: \n
// stays Enter (several terminals deliver Enter as 0x0a — remapping it would
// break submit), while Ctrl+N and ESC+CR are the explicit break keys.
func TestDecodeNewlineKeys(t *testing.T) {
	if k := DecodeKeys("\r", false).Keys; len(k) != 1 || k[0].Kind != KeyEnter {
		t.Fatalf("CR must stay Enter, got %v", k)
	}
	if k := DecodeKeys("\n", false).Keys; len(k) != 1 || k[0].Kind != KeyEnter {
		t.Fatalf("LF must stay Enter, got %v", k)
	}
	if k := DecodeKeys("\x0e", false).Keys; len(k) != 1 || k[0].Kind != KeyNewline {
		t.Fatalf("Ctrl+N must decode to KeyNewline, got %v", k)
	}
	if k := DecodeKeys("\x1b\r", false).Keys; len(k) != 1 || k[0].Kind != KeyNewline {
		t.Fatalf("Alt+Enter must decode to KeyNewline, got %v", k)
	}
}

// TestGoalInputBoxFixedSize pins the window-derived box: the rendered input
// viewport keeps its height as the text grows (no content-sized jumping), it
// scrolls to show the tail being typed, and the box width tracks the terminal.
func TestGoalInputBoxFixedSize(t *testing.T) {
	rows := 60 // 60/6 = 10 input rows
	want := goalInputRows(rows)
	if want != 10 {
		t.Fatalf("goalInputRows(60) = %d, want 10", want)
	}
	one := goalInputLines("hello", 40, want)
	many := goalInputLines(strings.Repeat("line\n", 40)+"tail", 40, want)
	if len(one) != want || len(many) != want {
		t.Fatalf("input viewport must stay %d rows, got %d and %d", want, len(one), len(many))
	}
	if !strings.Contains(strings.Join(many, "\n"), "tail") {
		t.Fatalf("scrolled viewport must show the tail being typed:\n%s", strings.Join(many, "\n"))
	}
	// The window width drives the box width.
	narrow := dispatchOverlayLines(&Overlay{Kind: "dispatch"}, 60, rows)
	wide := dispatchOverlayLines(&Overlay{Kind: "dispatch"}, 160, rows)
	if len(narrow) == 0 || len(wide) == 0 {
		t.Fatal("dispatch overlay rendered nothing")
	}
	if len(narrow[0]) >= len(wide[0]) {
		t.Fatalf("box width must track the terminal: narrow %d vs wide %d", len(narrow[0]), len(wide[0]))
	}
}
