package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

// Regression (2026-09-08 loop-167 hang): pane-run's --timeout was wired as a
// plain String flag (pane-run missing from wiredTypedFlags), so the action's
// Flags().GetInt("timeout") silently failed and returned 0 — RunPaneRun then
// armed TimeoutMs=0, which is the TS "undefined timeout = no wall cap"
// semantics. Every pane-run dispatch therefore waited on the done-marker
// forever and the loop driver blocked indefinitely inside paneRunDispatch.
func TestPaneRunTimeoutFlagIsInt(t *testing.T) {
	cmd := paneRunCommand()
	addFlags(cmd, frozenSurface["pane-run"].Flags, requiredFlags["pane-run"])
	f := cmd.Flags().Lookup("timeout")
	if f == nil {
		t.Fatal("--timeout not registered")
	}
	// The typed branches in addFlags previously skipped MarkFlagRequired
	// entirely, so typing --timeout as Int silently dropped its required
	// marking and a missing flag degraded to timeout=0 = no wall cap.
	if len(cmd.Flags().Lookup("timeout").Annotations[cobra.BashCompOneRequiredFlag]) != 1 {
		t.Error("--timeout lost its required marking")
	}
	if f.Value.Type() != "int" {
		t.Fatalf("--timeout type = %q, want int (GetInt silently yields 0 otherwise)", f.Value.Type())
	}
}
