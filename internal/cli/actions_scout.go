package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/spf13/cobra"
)

// scoutCommand mirrors `devagent scout`: --replay replays the captured
// worker-output fixtures through extractScoutPayload and diffs against
// golden.json; every other mode runs the live FR-SCOUT-01 cycle
// (scout.RunOnce / scout.RunLoop). The exit-3 not-ported stub retired with
// FR-GO-04 #194 — the LaunchAgent (`com.devagent.scout`, installed by
// `devagent create --scout`) launches `devagent scout --repo … --interval …`
// 24/7, and until this wiring landed those launches died silently on exit 3
// while the queue writer starved (2026-09-14 audit).
func scoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "scout",
		Short: "24/7 opencode scout: research backlog -> PRD -> queue (FR-SCOUT-01)",
		RunE: func(cmd *cobra.Command, args []string) error {
			replay, _ := cmd.Flags().GetBool("replay")
			if !replay {
				return runScoutLive(cmd)
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

// runScoutLive wires the frozen scout flag surface (--repo --worker
// --interval --timeout --once --dry-run) onto the cycle. Units match
// scripts/install-scout-launchagent.sh: --interval and --timeout are
// MINUTES. A bare run (neither --once nor --interval) performs ONE cycle,
// not a foreground daemon: an operator typing `devagent scout` must not be
// surprised into a Ctrl+C-or-SIGHUP session, while the LaunchAgent always
// passes --interval explicitly. --dry-run dispatches nothing (no AI call,
// docs/SCOUT.md) and prints the prompt length + what it would enqueue.
func runScoutLive(cmd *cobra.Command) error {
	repo, _ := cmd.Flags().GetString("repo")
	if repo == "" {
		repo, _ = os.Getwd() // same cwd fallback as scout-status
	}
	worker, _ := cmd.Flags().GetString("worker")
	interval, _ := cmd.Flags().GetInt("interval")
	timeout, _ := cmd.Flags().GetInt("timeout")
	once, _ := cmd.Flags().GetBool("once")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	opts := scout.RunOptions{RepoPath: repo, Worker: worker, DryRun: dryRun}
	if timeout > 0 {
		opts.Timeout = time.Duration(timeout) * time.Minute
	}
	if once || interval <= 0 {
		res, err := scout.RunOnce(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "scout: %v\n", err)
			setExitCode(1)
			return nil
		}
		fmt.Printf("[scout] %s %s\n", res.Status, res.Detail)
		if !res.OK {
			setExitCode(1)
			return nil
		}
		setExitCode(0)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	scout.RunLoop(repo, time.Duration(interval)*time.Minute, ctx.Done(), opts)
	setExitCode(0)
	return nil
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
