package cli

import (
	"fmt"
	"os"

	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/spf13/cobra"
)

// herdrSweepCommand mirrors `devagent herdr-sweep` — the sweep body lives in
// internal/herdr.RunHerdrSweep (byte-parity port of the TS action); the CLI
// only resolves the session flag and delegates.
func herdrSweepCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "herdr-sweep",
		Short: "Close idle/agentless stale panes in the devagent herdr session (session-scoped; bounded by herdr.sweep — enabled toggle, denySessions list, operator-attach exemption)",
		RunE: func(cmd *cobra.Command, args []string) error {
			session, _ := cmd.Flags().GetString("session")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			orphans, _ := cmd.Flags().GetBool("orphans")
			os.Exit(herdr.RunHerdrSweep(herdr.ExecRunner{}, session, dryRun, orphans))
			return nil
		},
	}
}

// sessionsCommand mirrors `devagent sessions` (src/commands/sessions.ts):
// list live worker panes as a table or raw JSON.
func sessionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List live worker panes in the herdr session (FR-VIS-02)",
		RunE: func(cmd *cobra.Command, args []string) error {
			jsonOut, _ := cmd.Flags().GetBool("json")
			os.Exit(herdr.RunSessions(herdr.ExecRunner{}, jsonOut))
			return nil
		},
	}
}

// attachCommand mirrors `devagent attach <task>` (src/commands/sessions.ts):
// print (or with --exec, run) the jump-in command for a worker pane,
// recording the attach in the ledger.
func attachCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "attach",
		Short: "Print (or with --exec, run) the jump-in command for a worker pane (FR-VIS-02)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			execMode, _ := cmd.Flags().GetBool("exec")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			os.Exit(herdr.RunAttach(herdr.ExecRunner{}, repo, args[0], execMode))
			return nil
		},
	}
}

// paneRunCommand mirrors `devagent pane-run` — required flags (--cwd,
// --timeout, --out, --err, --done) plus the <cmd> [args...] passthrough.
func paneRunCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "pane-run",
		Short: "Run one command inside a herdr pane of the devagent session (FR-VIS-04): research/PO/headless phases become operator-visible; falls back to a direct child when herdr is down",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := cmd.Flags().GetString("cwd")
			timeoutSecs, _ := cmd.Flags().GetInt("timeout")
			outPath, _ := cmd.Flags().GetString("out")
			errPath, _ := cmd.Flags().GetString("err")
			donePath, _ := cmd.Flags().GetString("done")
			session, _ := cmd.Flags().GetString("session")
			os.Exit(herdr.RunPaneRun(herdr.ExecRunner{}, cwd, timeoutSecs, outPath, errPath, donePath, session, args[0], args[1:]))
			return nil
		},
	}
}

var _ = fmt.Sprintf // retain fmt while the delegate bodies stay thin
