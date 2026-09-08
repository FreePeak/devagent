package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/FreePeak/devagent/internal/curator"
	"github.com/spf13/cobra"
)

// prdAuditCommand mirrors `devagent prd-audit` (src/cli.ts:173-188): the
// advisory-only curator PRD-coverage audit (PRD §18 Q15). It scans
// <repo>/docs/prds/*.md against the queue and warns about PRDs no task
// covers (unqueued) and PRDs whose covering task is still open past the
// mtime threshold (stale). Q15 resolved advisory-only: the curator never
// writes the queue, so it stays decoupled from the queue schema and the
// next scout cycle acts on the warning. Warnings go to stderr; the exit
// code is ALWAYS 0 — a finding is information, not a cycle failure, and
// scripts/prd-curator.sh depends on that when it pipes this into the
// curation log.
func prdAuditCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "prd-audit",
		Short: "Advisory-only curator PRD-coverage audit (PRD Q15): scan <repo>/docs/prds/*.md against the queue and warn (unqueued/stale) on stderr; exit code is always 0 — a finding is information, not a cycle failure (Q15: no enqueue)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			asJSON, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			report := curator.AuditPrdCoverage(repo, curator.AuditOptions{})
			if asJSON {
				// TS JSON.stringify does not HTML-escape; Go's default
				// marshaling turns ">" into \u003e. Disable that so the
				// --json bytes match the Node output. (Encode appends the
				// trailing newline that fmt.Println added.)
				enc := json.NewEncoder(os.Stdout)
				enc.SetEscapeHTML(false)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return err
				}
				return nil
			}
			// Summary line on stdout, each warning on stderr — the TS
			// console.log/console.error split that prd-curator.sh consumes.
			fmt.Printf("[prd-audit] scanned %d PRD(s) in %s against %d queue task(s) — %d warning(s), advisory only (Q15: no enqueue)\n",
				report.Scanned, report.PrdsDir, report.Tasks, len(report.Findings))
			for _, f := range report.Findings {
				fmt.Fprintf(os.Stderr, "[prd-audit] warn %s %s\n", f.Kind, f.Warning)
			}
			return nil
		},
	}
}
