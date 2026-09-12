package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/tui"
	versionpkg "github.com/FreePeak/devagent/internal/version"
	"github.com/spf13/cobra"
)

// cardWidth mirrors src/commands/human-card.ts cardWidth(): the §20.8 card
// budget clamped to [46, 100]; non-tty stdout reads as 100.
func cardWidth() int {
	cols := terminalColumns()
	w := cols
	if w > 100 {
		w = 100
	}
	if w < 46 {
		w = 46
	}
	return w
}

// ledgerCommand mirrors the final `devagent ledger` action (the FR-SIMPLE
// overlay in src/commands/fr-simple-wire.ts replaced the base action):
// failure clusters, summary, or the audit list — §20.8 chips by default,
// --json for scripts.
func ledgerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ledger",
		Short: "Show the orchestration run ledger (persisted audit verdicts); §20.8 chips by default, --json for scripts",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			task, _ := cmd.Flags().GetString("task")
			summary, _ := cmd.Flags().GetBool("summary")
			jsonOut, _ := cmd.Flags().GetBool("json")
			clustersRaw, _ := cmd.Flags().GetString("clusters")
			// pflag's NoOptDefVal covers the bare `--clusters` form but never
			// consumes the following token — commander's optional-value
			// `--clusters [n]` does. The space-separated value therefore
			// lands in args: `ledger --clusters 3` parses as clusters=true
			// with positional "3". Recover it the only way both forms stay
			// observable (flagless command: a positional can only be the
			// clusters value; the `foo --clusters` ordering has no Node
			// analog worth diverging for).
			if clustersRaw == "true" && len(args) > 0 && !strings.HasPrefix(args[0], "-") {
				clustersRaw = args[0]
			}
			if repo == "" {
				repo, _ = os.Getwd()
			}

			// --clusters [n]: commander's optional-value option — the flag
			// arrives as boolean true when bare, a string when valued, and
			// undefined ("" here) when absent. NoOptDefVal is set after flag
			// registration below.
			if clustersRaw != "" {
				var top float64 = 5
				if clustersRaw != "true" {
					top = parseIntPrefix(clustersRaw) // parseInt(raw, 10) semantics
				}
				clusters := ledger.ClusterFailures(repo)
				classes := ledger.ClusterFailureClasses(repo)
				if jsonOut {
					// The JSON branch uses limit 5 whenever the raw value is
					// not a finite positive number (TS: Number.isFinite && > 0).
					limit := 5
					if !math.IsNaN(top) && top > 0 {
						limit = int(top)
					}
					blob, err := ledger.ClustersJSON(clusters, classes, limit)
					if err != nil {
						return err
					}
					fmt.Println(string(blob))
					return nil
				}
				// TS prints the invalid-value line BEFORE any cluster reads;
				// RenderClustersText only ever sees a valid positive top.
				if math.IsNaN(top) || top <= 0 {
					fmt.Printf("Nothing to show for --clusters %s.\n", clustersRaw)
					return nil
				}
				for _, line := range ledger.RenderClustersText(clusters, classes, int(top), clustersRaw) {
					fmt.Println(line)
				}
				return nil
			}

			if summary {
				sum := ledger.SummarizeLedger(repo)
				if jsonOut {
					ts := tui.LedgerSummary(sum)
					fmt.Println(tui.LedgerJSON(&ts, nil))
				} else {
					fmt.Println(tui.RenderLedgerSummaryCard(tui.LedgerSummary(sum), cardWidth()))
				}
				return nil
			}

			records := ledger.ReadLedger(repo, task)
			if jsonOut {
				// ledgerJson(records) stringifies the full audit records (TS
				// readLedger is audit-only). readLedger returns [] even when
				// the ledger is absent; nil here would marshal as null.
				if records == nil {
					records = []ledger.AuditRecord{}
				}
				blob, err := json.MarshalIndent(records, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
				return nil
			}
			list := make([]tui.LedgerListRecord, 0, len(records))
			for _, r := range records {
				list = append(list, tui.LedgerListRecord{
					Kind:       r.Kind,
					Ts:         r.TS,
					TaskID:     r.TaskID,
					Attempt:    r.Attempt,
					Verdict:    r.Verdict,
					Integrity:  r.Integrity,
					UnmetCount: len(r.UnmetCriteria),
					Summary:    r.Summary,
				})
			}
			for _, line := range tui.RenderLedgerListLines(list) {
				fmt.Println(line)
			}
			return nil
		},
	}
	if f := cmd.Flags().Lookup("clusters"); f != nil {
		// commander `.option('--clusters [n]')`: bare flag yields boolean
		// true; pflag models that with a NoOptDefVal sentinel.
		f.NoOptDefVal = "true"
	}
	return cmd
}

// parseIntPrefix mirrors parseInt(s, 10): an optional sign followed by the
// leading run of decimal digits; anything else is NaN.
func parseIntPrefix(s string) float64 {
	s = strings.TrimLeft(s, " \t\n\r\v\f")
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	if digits == 0 {
		return math.NaN()
	}
	n, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return math.NaN()
	}
	return n
}

// logCommand mirrors `devagent log --run <id>` (src/cli.ts): print a run's
// structured JSONL as human lines; missing file exits 1.
func logCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "log",
		Short: "Print the structured JSONL log of a run",
		RunE: func(cmd *cobra.Command, args []string) error {
			runID, _ := cmd.Flags().GetString("run")
			home := os.Getenv("DEVAGENT_HOME")
			if home == "" {
				h := os.Getenv("HOME")
				if h == "" {
					h = "."
				}
				home = filepath.Join(h, ".devagent")
			}
			p := filepath.Join(home, "runs", runID+".jsonl")
			data, err := os.ReadFile(p)
			if err != nil {
				fmt.Fprintf(os.Stderr, "No run log at %s\n", p)
				os.Exit(1)
			}
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var e struct {
					TS      string `json:"ts"`
					Stage   string `json:"stage"`
					Level   string `json:"level"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal([]byte(line), &e); err != nil {
					continue // skip malformed lines
				}
				fmt.Printf("%s [%s] %s: %s\n", e.TS, e.Level, e.Stage, e.Message)
			}
			return nil
		},
	}
}

// recordCommand is the `record` grouping parent (Node prints help); the
// release subcommand carries the real action.
func recordCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "record",
		Short: "Append a structured event to the orchestration run ledger (Q24)",
	}
	release := &cobra.Command{
		Use:   "release",
		Short: "Record a release/tag event as a first-class ledger outcome (Q24)",
		RunE: func(cmd *cobra.Command, args []string) error {
			tag, _ := cmd.Flags().GetString("tag")
			sha, _ := cmd.Flags().GetString("sha")
			repo, _ := cmd.Flags().GetString("repo")
			source, _ := cmd.Flags().GetString("source")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			version := strings.TrimPrefix(tag, "v")
			ledger.AppendReleaseRecord(repo, ledger.ReleaseRecord{
				TS:       time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00"),
				Kind:     "event",
				TaskID:   "release/" + version,
				Attempt:  1,
				Event:    "release-created",
				Version:  version,
				Revision: versionpkg.Revision(),
				Source:   source,
			})
			fmt.Printf("recorded release-created %s (%s @ %s) -> .devagent/runs/orchestration/events.jsonl\n", version, tag, sha)
			return nil
		},
	}
	cmd.AddCommand(release)
	return cmd
}

// parseIntPrefix is used by the ledger clusters parsing; the regexp below is
// retained only to document the accepted shape. (No runtime use.)
var _ = regexp.MustCompile(`^[+-]?[0-9]+`)
