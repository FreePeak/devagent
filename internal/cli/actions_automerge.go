package cli

// actions_automerge.go wires `automerge` (src/cli.ts) to the FR-GO-07
// autopr port (internal/orchestrator). Outcome rows print exactly like the
// Node body; a non-merge outcome without --dry-run sets exit 1.

import (
	"fmt"
	"os"
	"strconv"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/spf13/cobra"
)

func automergeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "automerge",
		Short: "Auto review + merge open PRs against objective gates (CI green, mergeable, hazard scan)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			base, _ := cmd.Flags().GetString("base")
			method, _ := cmd.Flags().GetString("method")
			timeout, _ := cmd.Flags().GetInt("timeout")
			graceHours := gatesFlagIntOpt(cmd, "grace-hours")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			pr32s, _ := cmd.Flags().GetStringArray("pr")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			var grace *float64
			if graceHours != nil {
				f := float64(*graceHours)
				grace = &f
			}
			var waitSec *int
			if cmd.Flags().Changed("timeout") {
				waitSec = &timeout
			}
			var prNumbers []int
			for _, v := range pr32s {
				if n, err := strconv.Atoi(v); err == nil {
					prNumbers = append(prNumbers, n)
				}
			}
			outcomes := orchestrator.AutoReviewAndMerge(repo, orchestrator.AutoReviewAndMergeBatchOpts{
				AutoReviewAndMergeOptions: orchestrator.AutoReviewAndMergeOptions{
					BaseBranch:       base,
					Method:           method,
					DryRun:           dryRun,
					WaitForChecksSec: waitSec,
					GraceHours:       grace,
				},
				PrNumbers: prNumbers,
				Log:       func(msg string) { fmt.Println(msg) },
			}, orchestrator.DefaultRunGh)
			merged := 0
			for _, o := range outcomes {
				fmt.Printf("#%d %s (%s)\n", o.PR, o.Action, o.Detail)
				if o.Action == "merged" {
					merged++
				}
			}
			fmt.Printf("\n%d/%d merged\n", merged, len(outcomes))
			if merged < len(outcomes) && !dryRun {
				setExitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "target repository (used for gh context)")
	cmd.Flags().StringArray("pr", nil, "specific PR number (repeat to target a set)")
	cmd.Flags().String("base", "", "only PRs targeting this base branch")
	cmd.Flags().String("method", "squash", "squash | merge | rebase")
	cmd.Flags().Int("timeout", 300, "max seconds to wait for pending checks")
	cmd.Flags().Float64("grace-hours", -1, "hours a PR may stay red before the queue skips it (default from config prHygiene.graceHours)")
	cmd.Flags().Bool("dry-run", false, "evaluate and print verdicts without reviewing or merging")
	return cmd
}

// ensure config is referenced even if future edits drop its use here.
var _ = config.Load
