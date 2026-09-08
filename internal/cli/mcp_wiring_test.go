package cli

import (
	"strings"
	"testing"
)

// mcpCommandDescription is the TS .description() string for `devagent mcp`
// (src/cli.ts:1277) — the CLI-surface contract for the wired command.
const mcpCommandDescription = "Expose DevAgent as MCP tools over stdio (devagent_dispatch/status/log)"

// TestMcpWired verifies the #251 wiring: `mcp` left the notPortedIssue stub
// map, is registered in the cobra tree with the TS description, and runs a
// real action (no exit-3 stub path) with the flagless frozen surface.
func TestMcpWired(t *testing.T) {
	if _, ok := notPortedIssue["mcp"]; ok {
		t.Error("notPortedIssue still carries \"mcp\" — the command is wired, remove the stub entry")
	}
	root := NewRoot()
	mcp, _, err := root.Find([]string{"mcp"})
	if err != nil || mcp == nil {
		t.Fatalf("devagent mcp not found in the command tree: %v", err)
	}
	if mcp.Short != mcpCommandDescription {
		t.Errorf("mcp Short = %q, want the TS description %q", mcp.Short, mcpCommandDescription)
	}
	if mcp.RunE == nil && mcp.Run == nil {
		t.Error("mcp has no action: it would degrade to cobra help instead of serving")
	}
	if strings.Contains(mcp.Short, "not yet ported") {
		t.Error("mcp still carries the stub help text")
	}
	if mcp.Flags().Lookup("repo") != nil {
		t.Error("mcp must be flagless (frozen surface), but a --repo flag is registered")
	}
}
