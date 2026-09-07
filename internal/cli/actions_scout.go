package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/spf13/cobra"
)

// scoutCommand mirrors `devagent scout` for the read-only surface the scout
// package ports (--replay: replay captured worker-output fixtures through
// extractScoutPayload and diff against golden.json). Live dispatch
// (runScoutOnce / runScoutLoop) depends on the queue + worker runtime —
// FR-GO-04 #194 — so every other mode keeps the exit-3 not-ported contract.
func scoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "scout",
		Short: "24/7 opencode scout: research backlog -> PRD -> queue (FR-SCOUT-01)",
		RunE: func(cmd *cobra.Command, args []string) error {
			replay, _ := cmd.Flags().GetBool("replay")
			if !replay {
				return &notPortedError{msg: "devagent scout: not yet ported to Go (FR-GO #192) — only --replay is wired; live dispatch needs the queue + worker runtime (FR-GO-04 #194)"}
			}
			results, err := scout.ReplayScoutFixtures("")
			if err != nil {
				return err
			}
			failures := 0
			for _, r := range results {
				status := "FAIL"
				if r.Pass {
					status = "PASS"
				} else {
					failures++
					fmt.Printf("%s %s\n", status, r.Name)
					fmt.Printf("  expected: %s\n", jsJSONString(r.Expected))
					fmt.Printf("  actual:   %s\n", jsJSONString(r.Actual))
					continue
				}
				fmt.Printf("%s %s\n", status, r.Name)
			}
			fmt.Printf("%d/%d fixtures match golden\n", len(results)-failures, len(results))
			if failures > 0 {
				os.Exit(1)
			}
			return nil
		},
	}
}

// jsJSONString mirrors JSON.stringify of a string|null: quoted JSON string
// or the literal null.
func jsJSONString(s *string) string {
	if s == nil {
		return "null"
	}
	blob, err := json.Marshal(*s)
	if err != nil {
		return "null"
	}
	return string(blob)
}

// scoutStatusCommand mirrors `devagent scout-status`: heartbeat + queue
// depth (read-only). Queue counts come from the local read-only queue glue
// (state_glue.go) until the queue package lands.
func scoutStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "scout-status",
		Short: "Show scout heartbeat + queue depth",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			hb := scout.ReadHeartbeat(repo)
			counts := queueCounts(repo)
			if jsonOut {
				payload := struct {
					Heartbeat *scout.ScoutHeartbeat `json:"heartbeat"`
					Queue     queueJSON             `json:"queue"`
				}{hb, queueJSON{
					Total:   counts.Total,
					Pending: counts.Pending,
					Claimed: counts.Claimed,
					Done:    counts.Done,
					Failed:  counts.Failed,
				}}
				fmt.Println(marshalIndent(payload))
				return nil
			}
			if hb == nil {
				fmt.Println("No scout heartbeat yet. Run: devagent scout --once --dry-run")
			} else {
				ageMs := timeNowMs() - parseISOMs(hb.LastRunAt)
				var age string
				if ageMs < 60_000 {
					age = fmt.Sprintf("%ds ago", roundDiv(ageMs, 1000))
				} else {
					age = fmt.Sprintf("%dm ago", roundDiv(ageMs, 60_000))
				}
				fmt.Printf("Scout: %s interval %sm last %s %s\n",
					jsOptStr(hb.Worker), jsOptNum(hb.IntervalMinutes), jsOptStr(hb.LastStatus), age)
				lastTask := "(none)"
				if hb.LastTaskID != nil {
					lastTask = *hb.LastTaskID
				}
				fmt.Printf("  lastTask: %s  detail: %s\n", lastTask, jsOptStr(hb.LastDetail))
				fmt.Printf("  at: %s\n", hb.LastRunAt)
			}
			fmt.Printf("Queue: total %d (pending:%d claimed:%d done:%d failed:%d)\n",
				counts.Total, counts.Pending, counts.Claimed, counts.Done, counts.Failed)
			return nil
		},
	}
}

// queueJSON serializes the taskCount record with the TS key order
// (total, pending, claimed, done, failed).
type queueJSON struct {
	Total   int `json:"total"`
	Pending int `json:"pending"`
	Claimed int `json:"claimed"`
	Done    int `json:"done"`
	Failed  int `json:"failed"`
}

// jsOptStr mirrors a TS template-literal interpolation of string|undefined:
// undefined prints as "undefined".
func jsOptStr(s *string) string {
	if s == nil {
		return "undefined"
	}
	return *s
}

// jsOptNum mirrors a TS template-literal interpolation of number|undefined.
func jsOptNum(f *float64) string {
	if f == nil {
		return "undefined"
	}
	return strconvFormatNumber(*f)
}

// parseISOMs mirrors Date.parse for the ISO-8601 stamps the writers emit;
// 0 when unparseable (callers treat the heartbeat as missing first).
func parseISOMs(ts string) int64 {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t, err = time.Parse(time.RFC3339, ts)
		if err != nil {
			return 0
		}
	}
	return t.UnixMilli()
}

// roundDiv mirrors Math.round(x / div) for non-negative values.
func roundDiv(v, div int64) int64 {
	return (v + div/2) / div
}

var _ = tui.CardWidth // referenced by the shared card renderers
