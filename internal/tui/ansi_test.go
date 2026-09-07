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
	if !strings.Contains(ok, Dim+"git"+Reset) {
		t.Fatalf("ok chip label should be dim (statusColor default): %q", ok)
	}
	failed := ChipFor("failed", "worker")
	if !strings.Contains(failed, Red+"●"+Reset) || !strings.Contains(failed, Red+"worker"+Reset) {
		t.Fatalf("failed chip wrong: %q", failed)
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
