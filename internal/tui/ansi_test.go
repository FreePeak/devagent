package tui

import (
	"strings"
	"testing"
)

func TestChipFor(t *testing.T) {
	ok := ChipFor("ok", "git")
	if !strings.Contains(ok, Green+"●"+Reset) {
		t.Fatalf("ok chip missing green dot: %q", ok)
	}
	if !strings.Contains(ok, Green+"git"+Reset) {
		t.Fatalf("ok chip label takes the state color (FR-TUI-P-09): %q", ok)
	}
	failed := ChipFor("failed", "worker")
	if !strings.Contains(failed, Red+"●"+Reset) || !strings.Contains(failed, Red+"worker"+Reset) {
		t.Fatalf("failed chip wrong: %q", failed)
	}
	stale := ChipFor("stale", "old")
	if strings.Contains(stale, "\x1b[35m") {
		t.Fatalf("magenta retired (rainbow color): %q", stale)
	}
	if !strings.Contains(stale, Yellow+"●"+Reset) {
		t.Fatalf("stale chip must use amber, not magenta: %q", stale)
	}
}

func TestTruncate(t *testing.T) {
	if Truncate("short", 18) != "short" {
		t.Fatal("short string altered")
	}
	got := Truncate("a-very-long-worker-name-here", 18)
	if len([]rune(got)) != 18 || !strings.HasSuffix(got, "…") {
		t.Fatalf("truncate = %q", got)
	}
}

func TestDimCyan(t *testing.T) {
	if DimText("x") != Dim+"x"+Reset {
		t.Fatal("dim wrong")
	}
	if CyanText("y") != Cyan+"y"+Reset {
		t.Fatal("cyan wrong")
	}
}
