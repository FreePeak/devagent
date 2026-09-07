package scantext

import (
	"strings"
	"testing"
)

func TestBuildAdjacentCategoryScanText(t *testing.T) {
	got := BuildAdjacentCategoryScanText()
	want := strings.Join([]string{
		"GRADIENT selection prior — when picking or justifying a backlog item, look beyond agent products.",
		"Adjacent categories to consider: sensors, MCP servers, harness tooling.",
		FunnelMissNote,
		ArchitectureGateHint,
	}, "\n")
	if got != want {
		t.Fatalf("scan text drifted:\n got: %q\nwant: %q", got, want)
	}
	// The categories line must carry all three adjacent categories in order.
	if !strings.Contains(got, "sensors, MCP servers, harness tooling") {
		t.Error("adjacent categories changed")
	}
}
