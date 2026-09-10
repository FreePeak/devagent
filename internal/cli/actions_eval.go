package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/eval"
	"github.com/FreePeak/devagent/internal/integrations"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/workers"
	"github.com/spf13/cobra"
)

// newEvalCmd is the `eval` grouping parent of the FR-VAL-05 quality-drift
// ratchet (issue #293). Scoring measures; the drift view gates:
//
//	devagent eval score --pr 305            # judge one shipped PR, write a row
//	devagent eval score --last 10           # the nightly work list
//	devagent eval drift --max-drop 15       # gate past a 15-point drop (exit 1)
//
// The ratchet verdict also rides `devagent ledger --clusters`, which the
// self-build driver already captures into its research/PO prompts — a shipped
// artifact scoring below its class's recent best therefore reaches the next pick
// through the plumbing that exists, with no second telemetry path.
//
// The ledger is per-checkout (.devagent/runs/orchestration/events.jsonl), so the
// checkout scored must be the checkout the loop reads; the score output prints
// the resolved path so a mis-wired nightly is visible in its own log instead of
// silently producing no drift forever.
func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Quality-drift ratchet over shipped artifacts (FR-VAL-05): LLM-judge rubric scoring recorded as eval-score ledger rows",
	}
	cmd.AddCommand(evalScoreCommand(), evalDriftCommand())
	return cmd
}

// evalScoreCommand scores shipped PRs against the checked-in rubric and appends
// one `eval-score` row per score to the existing orchestration ledger. The judge
// is the configured worker/model, dispatched like the audit gate's auditor.
// Nothing scored (unreadable rubric, no usable PR) exits nonzero — a nightly
// gate must never pass on an empty run.
func evalScoreCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "score",
		Short: "Score shipped PR(s) against the rubric and record eval-score ledger rows (--repo must be the checkout the loop reads)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			pr, _ := cmd.Flags().GetInt("pr")
			last, _ := cmd.Flags().GetInt("last")
			rubricPath, _ := cmd.Flags().GetString("rubric")
			workerFlag, _ := cmd.Flags().GetString("worker")
			modelFlag, _ := cmd.Flags().GetString("model")
			timeoutMins, _ := cmd.Flags().GetInt("timeout")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if pr <= 0 && last <= 0 {
				return fmt.Errorf("eval score: pass --pr <n> and/or --last <n> — nothing to score")
			}
			// A zero/negative wall clock would leave the judge dispatch unbounded
			// (TimeoutMs 0 means no deadline in every adapter), so a mistyped
			// nightly flag could hang the job instead of failing it.
			if timeoutMins <= 0 {
				timeoutMins = judgeTimeoutDefaultMins
			}
			worker, model, variant := workerFlag, modelFlag, ""
			if worker == "" || model == "" {
				cfg, err := config.Load(repo)
				if err != nil {
					return err
				}
				variant = cfg.Variant
				if worker == "" {
					worker = cfg.Worker
				}
				if model == "" {
					model = cfg.Model
				}
			}
			adapter, err := workers.GetWorker(worker)
			if err != nil {
				return err
			}
			judgeName := worker + "@" + model
			if model == "" {
				judgeName = worker + "@default"
			}
			timeoutMs := timeoutMins * 60_000
			judge := func(prompt string) (string, error) {
				r := adapter.Spawn(workers.WorkerSpawnOptions{
					Prompt: prompt, Cwd: repo, TimeoutMs: timeoutMs, Model: model, Variant: variant,
				})
				if r.TimedOut || r.ExitCode != 0 {
					return "", fmt.Errorf("judge %s exited %d (timedOut=%v): %s",
						judgeName, r.ExitCode, r.TimedOut, strings.TrimSpace(r.ErrorText))
				}
				return r.ResultText, nil
			}
			var prs []int
			if pr > 0 {
				prs = []int{pr}
			}
			fmt.Printf("[eval] ledger %s\n", filepath.Join(repo, ledger.LedgerDir, "events.jsonl"))
			rows, err := eval.ScoreShipped(eval.Options{
				RepoPath:   repo,
				RubricPath: rubricPath,
				PRs:        prs,
				Last:       last,
				GH:         ghRunner(),
				Judge:      judge,
				JudgeName:  judgeName,
				Log:        func(format string, a ...any) { fmt.Println("[eval] " + fmt.Sprintf(format, a...)) },
			})
			if err != nil {
				return err
			}
			if jsonOut {
				blob, err := json.MarshalIndent(rows, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
				return nil
			}
			for _, r := range rows {
				fmt.Printf("[eval] PR #%d (%s, rubric %s+%s, judge %s) — %d/%d\n",
					r.PR, r.GoalClass, r.RubricVersion, r.RubricDigest, r.Judge, r.Total, r.Max)
				for _, c := range r.Criteria {
					fmt.Printf("  - %-22s %3d/%d\n", c.Criterion, c.Score, c.Max)
				}
				if r.Notes != "" {
					fmt.Printf("  note: %s\n", r.Notes)
				}
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "target repository owning the rubric and the ledger — must be the checkout `devagent ledger --clusters` reads (default cwd)")
	cmd.Flags().Int("pr", 0, "score this PR number")
	cmd.Flags().Int("last", 0, "also score the last N merged PRs (the nightly work list)")
	cmd.Flags().String("rubric", "", "rubric path (default: .devagent/eval/rubric.md, else docs/eval/rubric.md)")
	cmd.Flags().String("worker", "", "judge worker CLI (default from repo config)")
	cmd.Flags().String("model", "", "judge model id (default from repo config)")
	cmd.Flags().Int("timeout", judgeTimeoutDefaultMins, "wall-clock minutes per judge dispatch")
	cmd.Flags().Bool("json", false, "print the written ledger rows as JSON")
	return cmd
}

// judgeTimeoutDefaultMins bounds one judge dispatch. Ten minutes is generous for
// a read-only rubric pass and short enough that a wedged provider cannot hold the
// nightly.
const judgeTimeoutDefaultMins = 10

// evalDriftCommand reports the ratchet: shipped PRs scoring below the best their
// own goal class recently achieved, each with the criterion that lost the most
// ground. With --max-drop <points> it becomes a gate (exit 1 past the budget);
// the default 0 keeps the first release alert-only, per issue #293.
func evalDriftCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Report quality-drift ratchet violations from eval-score ledger rows (--max-drop gates)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			window, _ := cmd.Flags().GetInt("window")
			top, _ := cmd.Flags().GetInt("top")
			maxDrop, _ := cmd.Flags().GetInt("max-drop")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			scores := ledger.ReadEvalScores(repo)
			drift := ledger.QualityDriftFromScores(scores, window)
			if jsonOut {
				payload := drift
				if payload == nil {
					payload = []ledger.QualityDrift{}
				}
				blob, err := json.MarshalIndent(payload, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
			} else if len(drift) == 0 {
				if len(scores) < 2 {
					fmt.Printf("[eval] insufficient baseline: %d eval-score row(s) in %s — the ratchet needs 2+ scores in one goal class\n",
						len(scores), ledger.LedgerDir)
				} else {
					fmt.Printf("[eval] no quality drift: %d eval-score row(s), nothing below its class's trailing best\n", len(scores))
				}
			} else {
				limit := top
				if limit <= 0 {
					limit = len(drift)
				}
				for _, line := range ledger.RenderClustersText(nil, nil, drift, limit, "") {
					fmt.Println(line)
				}
				fmt.Printf("[eval] %d violation(s) across %d eval-score row(s)\n", len(drift), len(scores))
			}
			if maxDrop > 0 {
				for _, d := range drift {
					if d.Drop > maxDrop {
						fmt.Fprintf(os.Stderr,
							"[eval] drift beyond budget: PR #%d (%s) dropped %d point(s) > --max-drop %d; weakest criterion: %s %d/%d\n",
							d.PR, d.GoalClass, d.Drop, maxDrop, d.Criterion, d.CriterionScore, d.CriterionMax)
						setExitCode(1)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "target repository owning the ledger (default cwd)")
	cmd.Flags().Int("window", ledger.DriftWindow, "trailing window per goal class holding the best-so-far")
	cmd.Flags().Int("top", 0, "cap reported violations (0 = all)")
	cmd.Flags().Int("max-drop", 0, "fail (exit 1) when any drop exceeds this many points (0 = alert-only)")
	cmd.Flags().Bool("json", false, "print the violations as JSON")
	return cmd
}

// ghRunner is the `gh` seam eval reads PR evidence through. It reuses the
// orchestrator's hardened gh exec (fallback PATH, GhError carrying stderr) and
// the integrations rate-limit retry, so a secondary-limit blip during a
// `--last 10` run retries once instead of silently shrinking the scored set —
// a thin run must not read as a clean ratchet.
func ghRunner() eval.Runner {
	return func(args []string, cwd string) (string, error) {
		r, err := integrations.WithRateLimitRetry(func() (*orchestrator.GhResult, error) {
			return orchestrator.DefaultRunGh(args, cwd)
		}, func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) })
		if err != nil {
			return "", err
		}
		return r.Stdout, nil
	}
}
