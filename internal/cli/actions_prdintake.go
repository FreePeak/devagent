package cli

// actions_prdintake.go wires `devagent prd-intake` (issue #370): the one
// sanctioned route from docs/PRD.md into the loop's deterministic lane.
//
// The PRD stays a state document — prose, blockquotes and completed
// checkboxes are never work — but an OPEN checkbox (`- [ ] …`) is an
// explicit operator request, and until now nothing read it: an empty lane
// made the driver fall back to LLM self-selection and ship loop plumbing
// (issue #355). Intake is deterministic (no LLM, no network) and idempotent
// (a row's queue id is a hash of its text), so it also runs unattended at
// the head of every loop iteration.

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/FreePeak/devagent/internal/prdintake"
	"github.com/spf13/cobra"
)

func newPrdIntakeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "prd-intake",
		Short: "Queue the operator's open `- [ ]` items in docs/PRD.md as loop work (issue #370): deterministic, idempotent, no LLM",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			asJSON, _ := cmd.Flags().GetBool("json")
			maxItems, _ := cmd.Flags().GetInt("max")

			report, err := prdintake.Ingest(prdintake.Options{
				RepoPath: repo,
				MaxItems: maxItems,
				DryRun:   dryRun,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "prd-intake: %v\n", err)
				setExitCode(1)
				return nil
			}
			if asJSON {
				blob, jerr := json.MarshalIndent(report, "", "  ")
				if jerr != nil {
					return jerr
				}
				fmt.Println(string(blob))
				return nil
			}
			verb := "queued"
			if dryRun {
				verb = "would queue"
			}
			fmt.Printf("PRD %s: %d open item(s), %s %d, already known %d\n",
				report.PRDPath, report.Open, verb, len(report.Queued), len(report.Known))
			for _, it := range report.Queued {
				fmt.Printf("  + %s  %s\n", it.ID, it.Title)
			}
			for _, it := range report.Known {
				fmt.Printf("  = %s  %s\n", it.ID, it.Title)
			}
			if report.Open == 0 {
				fmt.Println("Nothing to build: write `- [ ] <what you want>` anywhere in docs/PRD.md and commit it — the next iteration picks it up.")
			} else {
				fmt.Printf("Queue depth: %d. Next: `devagent up` (or `devagent loop`) builds the top of the queue.\n", report.QueueDepth)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "repository whose docs/PRD.md is ingested (default cwd)")
	cmd.Flags().Bool("dry-run", false, "report what would be queued without writing queue rows")
	cmd.Flags().Bool("json", false, "machine-readable report")
	cmd.Flags().Int("max", 0, fmt.Sprintf("cap new rows per pass (0 = default %d, negative = unbounded)", prdintake.DefaultMaxItems))
	return cmd
}
