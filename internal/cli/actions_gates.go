package cli

// actions_gates.go wires the orchestrator-era commands landed by FR-GO-07
// (#221) and FR-GO-08 (#215) to their Go implementations, porting the
// src/cli.ts .action bodies faithfully: same output shapes, same exit codes.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/devagent/internal/commands"
	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/lessons"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/spf13/cobra"
)

// commandExitCode carries a process.exitCode assignment from a RunE body:
// RunE must return nil (not an error) for these, and Execute applies the
// code after a successful dispatch. This mirrors commander's
// process.exitCode semantics (exit without killing pending output).
var commandExitCode *int

// setExitCode records the process exit code for this invocation.
func setExitCode(code int) { commandExitCode = &code }

// ---------------------------------------------------------------------------
// selfbuild-gate
// ---------------------------------------------------------------------------

func newSelfbuildGateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "selfbuild-gate",
		Short: "Machine-readable selfbuild starvation + Q27 re-burn gates (PRD:888): the starved() and already_shipped() decisions typed and tested; the `devagent loop` driver and the sibling bash drivers (build-loop.sh, orchestrator-loop.sh, warroom-loop.sh) are thin callers reaching the same seam via --extra-productive / --ledger instead of their own awk copies. Same exit-code contract as backlog-check: 0 = continue (not starved / goal not shipped), 1 = gate verdict (starved — halt; already shipped — skip), 2 = unresolved (neither/both gate flags given — caller misuse). A missing/unreadable ledger reads as continue, matching the shell fallback. A crashed CLI also exits 1, so callers must only honor rc 1 when the verdict word (\"starved:\" / \"already shipped\") is in the output.",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			ledger, _ := cmd.Flags().GetString("ledger")
			starved, _ := cmd.Flags().GetBool("starved")
			limit, _ := cmd.Flags().GetInt("limit")
			extra, _ := cmd.Flags().GetString("extra-productive")
			goal, _ := cmd.Flags().GetString("already-shipped")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if starved == (goal != "") {
				fmt.Fprintln(os.Stderr, "selfbuild-gate: pass exactly one of --starved or --already-shipped <goal>")
				setExitCode(2)
				return nil
			}
			path := ledger
			if path == "" {
				path = filepath.Join(repo, ".selfbuild", "ledger.jsonl")
			}
			lines := orchestrator.ReadLedgerLines(path)
			if starved {
				var extraList []string
				for _, s := range strings.Split(extra, ",") {
					if s = strings.TrimSpace(s); s != "" {
						extraList = append(extraList, s)
					}
				}
				v := orchestrator.EvaluateStarvation(lines, limit, extraList...)
				if v.Starved {
					fmt.Printf("starved: %d consecutive non-productive iterations >= limit %d — halt\n", v.Count, v.Limit)
					setExitCode(1)
				} else {
					fmt.Printf("productive: %d consecutive non-productive iterations < limit %d — continue\n", v.Count, v.Limit)
					setExitCode(0)
				}
				return nil
			}
			v := orchestrator.AlreadyShipped(goal, lines)
			if v.Shipped {
				fmt.Printf("already shipped (%s match on a productive ledger row) — skip\n", v.Reason)
				setExitCode(1)
			} else {
				fmt.Println("not shipped — dispatch ok")
				setExitCode(0)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "target repository owning .selfbuild/ledger.jsonl")
	cmd.Flags().String("ledger", "", "ledger JSONL to evaluate (default <repo>/.selfbuild/ledger.jsonl; warroom-loop points this at .warroom/progress.jsonl)")
	cmd.Flags().Bool("starved", false, "starvation gate: consecutive non-productive rows since the last productive row reach --limit")
	cmd.Flags().Int("limit", 5, "starvation limit (default 5)")
	cmd.Flags().String("extra-productive", "", "comma-separated statuses that additionally break the starvation streak (per-driver ledger dialects, e.g. judge-done,spec-refined; --starved only)")
	cmd.Flags().String("already-shipped", "", "Q27 re-burn guard: a productive ledger row already carries this goal")
	return cmd
}

// ---------------------------------------------------------------------------
// board-recovery
// ---------------------------------------------------------------------------

func newBoardRecoveryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board-recovery",
		Short: "Machine-readable orchestrator board-recovery gate (PRD:888 Q19): the board-recovery decisions from scripts/orchestrate-loop.sh (requeue_parked, completed-board archive, stuck/undispatchable archive thresholds, re-bridge fall-through) folded into src/orchestrator/board-recovery.ts so they are typed and tested; the shell is a thin caller. The gate performs the action it verdicts (requeue write, board archive, merged-worktree prune) and prints exactly one verdict line \"<wait|requeue|archive>: <reason>\": wait = sleep and continue, requeue = parked tasks reset (sleep and continue), archive = stuck board archived (fall through to the queue bridge this cycle). Exit 0 = verdict printed; exit 2 = unresolved (bad flags or a board that vanished mid-cycle) — callers must fall back to wait, never act on a crashed gate.",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			parkedPolls, _ := cmd.Flags().GetInt("parked-polls")
			requeueAfter, _ := cmd.Flags().GetInt("requeue-after")
			pollSecs, _ := cmd.Flags().GetInt("poll-secs")
			maxTotal, _ := cmd.Flags().GetInt("max-total-attempts")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if parkedPolls < 0 || requeueAfter < 0 || pollSecs < 0 || maxTotal < 0 {
				fmt.Fprintln(os.Stderr, "board-recovery: --parked-polls/--requeue-after/--poll-secs/--max-total-attempts must be non-negative numbers")
				setExitCode(2)
				return nil
			}
			verdict, err := orchestrator.RunBoardRecovery(repo, orchestrator.RunBoardRecoveryOptions{
				RecoveryOptions: orchestrator.RecoveryOptions{
					ParkedPolls:      parkedPolls,
					RequeueAfter:     requeueAfter,
					PollSecs:         pollSecs,
					MaxTotalAttempts: maxTotal,
				},
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "board-recovery: %v\n", err)
				setExitCode(2)
				return nil
			}
			fmt.Println(orchestrator.FormatVerdict(verdict))
			return nil
		},
	}
	cmd.Flags().String("repo", "", "target repository owning .devagent-project.json")
	cmd.Flags().Int("parked-polls", 0, "consecutive parked cycles including this one (driver-side counter)")
	cmd.Flags().Int("requeue-after", 6, "requeue threshold in parked cycles (0 = never requeue)")
	cmd.Flags().Int("poll-secs", 600, "loop sleep quoted in wait/requeue verdict text")
	cmd.Flags().Int("max-total-attempts", 0, "cumulative lifetime dispatch cap per task (Q17/Q36): failed/blocked tasks whose totalAttempts reaches it are refused the requeue reset and stay terminal (0 = unbounded)")
	return cmd
}

// ---------------------------------------------------------------------------
// preflight
// ---------------------------------------------------------------------------

func newPreflightCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Operator-role provider preflight (Q40): probe the configured worker CLI; on failure write an operator-degraded ledger row, open the circuit, and exit nonzero so the calling loop skips its agent dispatch this cycle (opt-outs ORCHESTRATOR_MODEL_PROBE=0 or OPERATOR_PROBE_DISABLED=1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			role, _ := cmd.Flags().GetString("role")
			workerFlag, _ := cmd.Flags().GetString("worker")
			modelFlag, _ := cmd.Flags().GetString("model")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			roleOK := false
			for _, r := range resilience.PreflightRoles {
				if string(r) == role {
					roleOK = true
					break
				}
			}
			if !roleOK {
				fmt.Fprintf(os.Stderr, "[preflight] unknown role %q; expected one of: %s\n", role, "prd-curator, po, selfbuild, warroom, reviewer")
				setExitCode(1)
				return nil
			}
			if os.Getenv("OPERATOR_PROBE_DISABLED") == "1" {
				fmt.Printf("[preflight] disabled by OPERATOR_PROBE_DISABLED=1 — skipping probe for role=%s\n", role)
				return nil
			}
			if os.Getenv("ORCHESTRATOR_MODEL_PROBE") == "0" {
				fmt.Println("[preflight] probe disabled for role=" + role + " (ORCHESTRATOR_MODEL_PROBE=0) — cycle proceeds unprobed")
				return nil
			}
			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			worker := workerFlag
			if worker == "" {
				worker = cfg.Worker
			}
			rawModel := modelFlag
			if rawModel == "" {
				rawModel = cfg.Model
			}
			decision, err := resilience.RunPreflightGate(resilience.PreflightGateArgs{
				RepoPath: repo,
				Role:     resilience.PreflightRole(role),
				Worker:   worker,
				Model:    rawModel,
				Argv:     commands.BuildProbeArgvFor(worker, rawModel),
				Cwd:      repo,
			})
			if err != nil {
				return err
			}
			if decision.OK {
				model := rawModel
				if model == "" {
					model = "(default)"
				}
				fmt.Printf("[preflight] ok role=%s worker=%s model=%s attempts=%d\n", decision.Role, worker, model, decision.Attempts)
				return nil
			}
			fmt.Printf("[preflight] degraded role=%s worker=%s model=%s attempts=%d — skip agent dispatch this cycle\n", decision.Role, worker, rawModel, decision.Attempts)
			if decision.Detail != "" {
				fmt.Fprintf(os.Stderr, "[preflight] last probe: %s\n", decision.Detail)
			}
			if decision.Paged {
				fmt.Fprintln(os.Stderr, "[preflight] paged operator webhook (resilience.degradeWebhookUrl)")
			}
			setExitCode(1)
			return nil
		},
	}
	cmd.Flags().String("role", "", "operator role to gate (prd-curator | po | selfbuild | warroom | reviewer)")
	_ = cmd.MarkFlagRequired("role")
	cmd.Flags().String("repo", "", "target repository owning the ledger/proxy state")
	cmd.Flags().String("worker", "", "worker CLI to probe (default from repo config)")
	cmd.Flags().String("model", "", "model id passed to the probe (default from repo config)")
	return cmd
}

// ---------------------------------------------------------------------------
// page-degrade-breach
// ---------------------------------------------------------------------------

func newPageDegradeBreachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "page-degrade-breach",
		Short: "Page the operator webhook for a degradation streak the preflight gate did not cause (Q41 doc-sync surface): read the trailing streak from .devagent/runs/orchestration/events.jsonl and POST one provider-degraded-breach alert to resilience.degradeWebhookUrl, but only on the cycle where the streak equals the threshold — mid-streak cycles stay silent. Best-effort by design: an unset webhook, a broken devagent.json or a transport throw still exits 0, so paging can never fail the calling loop",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			source, _ := cmd.Flags().GetString("source")
			role, _ := cmd.Flags().GetString("role")
			workerFlag, _ := cmd.Flags().GetString("worker")
			modelFlag, _ := cmd.Flags().GetString("model")
			detail, _ := cmd.Flags().GetString("detail")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if !resilience.IsDegradeBreachSource(source) {
				fmt.Fprintf(os.Stderr, "[page-degrade-breach] unknown source %q; expected one of: %s\n", source, "preflight, doc-sync")
				setExitCode(1)
				return nil
			}
			// Worker/model are alert context only: a config this CLI cannot
			// read must degrade to empty fields, never to a failed command.
			worker, model := workerFlag, modelFlag
			if worker == "" || model == "" {
				if cfg, err := config.Load(repo); err == nil {
					if worker == "" {
						worker = cfg.Worker
					}
					if model == "" {
						model = cfg.Model
					}
				}
			}
			paged := resilience.PageDegradeBreach(resilience.PageDegradeBreachArgs{
				RepoPath: repo,
				Source:   resilience.DegradeBreachSource(source),
				Role:     role,
				Worker:   worker,
				Model:    model,
				Detail:   detail,
			})
			if paged {
				fmt.Println("[page-degrade-breach] breach alert posted (streak == threshold)")
			} else {
				fmt.Println("[page-degrade-breach] no page (streak below threshold, webhook unset, or transport failed) — best-effort")
			}
			return nil
		},
	}
	cmd.Flags().String("source", "", "surface that recorded the streak-completing row (preflight | doc-sync)")
	_ = cmd.MarkFlagRequired("source")
	cmd.Flags().String("repo", "", "target repository owning the ledger and config")
	cmd.Flags().String("role", "", "role carried onto the alert (the loop role that paused)")
	cmd.Flags().String("worker", "", "worker CLI carried onto the alert (default from repo config)")
	cmd.Flags().String("model", "", "model id carried onto the alert (default from repo config)")
	cmd.Flags().String("detail", "", "human-readable why carried onto the alert")
	return cmd
}

// ---------------------------------------------------------------------------
// queue list / show / bridge
// ---------------------------------------------------------------------------

func newQueueCmd() *cobra.Command {
	queueCmd := &cobra.Command{
		Use:   "queue",
		Short: "Queue operations (FR-QUEUE-01)",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List queued tasks",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			status, _ := cmd.Flags().GetString("status")
			asJSON, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			var filter queue.QueuedTaskStatus
			if status != "" {
				filter = queue.QueuedTaskStatus(status)
			}
			tasks := queue.ListTasks(repo, filter)
			if asJSON {
				blob, err := json.MarshalIndent(tasks, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
				return nil
			}
			if len(tasks) == 0 {
				suffix := ""
				if status != "" {
					suffix = " with status " + status
				}
				fmt.Println("No tasks" + suffix + ".")
				return nil
			}
			for _, t := range tasks {
				lastErr := ""
				if t.LastError != nil {
					lastErr = " — " + truncateStr(*t.LastError, 60)
				}
				fmt.Printf("%-8s %s  %s%s\n", t.Status, t.ID, t.Title, lastErr)
			}
			return nil
		},
	}
	listCmd.Flags().String("repo", "", "repository")
	listCmd.Flags().String("status", "", "filter: pending | claimed | done | failed")
	listCmd.Flags().Bool("json", false, "emit JSON")
	queueCmd.AddCommand(listCmd)

	showCmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one queued task + PRD head",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			asJSON, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			t := queue.ReadTask(repo, args[0])
			if t == nil {
				fmt.Fprintf(os.Stderr, "Task %s not found\n", args[0])
				setExitCode(1)
				return nil
			}
			if asJSON {
				blob, err := json.MarshalIndent(t, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
				return nil
			}
			fmt.Printf("%s %s: %s\n", t.Status, t.ID, t.Title)
			fmt.Printf("  goal: %s\n", t.Goal)
			if len(t.AcceptanceCriteria) > 0 {
				fmt.Printf("  criteria: %s\n", strings.Join(t.AcceptanceCriteria, "; "))
			}
			if _, ok := queue.ReadPrd(repo, t.ID); ok {
				prd, _ := queue.ReadPrd(repo, t.ID)
				fmt.Printf("\n--- PRD head ---\n%s\n", truncateStr(prd, 600))
			}
			return nil
		},
	}
	showCmd.Flags().String("repo", "", "repository")
	showCmd.Flags().Bool("json", false, "emit JSON")
	queueCmd.AddCommand(showCmd)

	bridgeCmd := &cobra.Command{
		Use:   "bridge",
		Short: "Bridge the oldest pending queued goal into an orchestrator board (idempotent; no-op when a board exists)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			r := orchestrator.BridgeIfQueued(repo, nil)
			if r == nil {
				fmt.Println("nothing to bridge: no pending queued tasks")
				return nil
			}
			if r.Idempotent || !r.Created {
				fmt.Printf("board already exists at %s (%d task(s)) — no-op\n", r.BoardPath, r.TasksWritten)
			} else {
				fmt.Printf("bridged %d task(s) into %s\n", r.TasksWritten, r.BoardPath)
			}
			return nil
		},
	}
	bridgeCmd.Flags().String("repo", "", "repository")
	queueCmd.AddCommand(bridgeCmd)
	return queueCmd
}

func truncateStr(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ---------------------------------------------------------------------------
// lessons (guarded append) + scores
// ---------------------------------------------------------------------------

func newLessonsCmd() *cobra.Command {
	lessonsCmd := &cobra.Command{
		Use:   "lessons",
		Short: "Append a lesson behind the eval guard (PRD Phase 4 \"Lessons eval guard\", evaluate→accept slice). Machine appends must go through this: a candidate needs a non-empty --predicted-impact, must clear the dedupe gate, and must leave the repo regression suite green against the proposed lessons-file state (red reverts the file). Exactly one lessons-eval ledger row is written per gated append.",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			entry, _ := cmd.Flags().GetString("entry")
			impact, _ := cmd.Flags().GetString("predicted-impact")
			lessonsFileFlag, _ := cmd.Flags().GetString("lessons-file")
			thresholdFlag := gatesFlagFloat(cmd, "threshold")
			suiteTimeout := gatesFlagInt(cmd, "suite-timeout-ms")
			loopFlag := gatesFlagIntOpt(cmd, "loop")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if entry == "" || impact == "" {
				fmt.Fprintln(os.Stderr, "--entry and --predicted-impact are required for the append action.")
				setExitCode(1)
				return nil
			}
			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			lessonsFile := lessonsFileFlag
			if lessonsFile == "" {
				lessonsFile = cfg.LessonsFile
			}
			if lessonsFile == "" {
				lessonsFile = ".selfbuild/lessons.md"
			}
			opts := &lessons.AppendLessonGuardedOpts{
				LessonsFile:     lessonsFile,
				PredictedImpact: impact,
				SuiteTimeoutMs:  suiteTimeout,
			}
			if thresholdFlag != nil {
				opts.Threshold = thresholdFlag
			} else if cfg.LessonsDedupeSimilarity != nil {
				opts.Threshold = cfg.LessonsDedupeSimilarity
			}
			if loopFlag != nil {
				opts.Loop = loopFlag
			}
			if dryRun {
				opts.DryRun = true
			}
			r := lessons.AppendLessonGuarded(repo, entry, opts)
			out := map[string]any{
				"appended":     r.OK,
				"reason":       r.Reason,
				"suite":        r.Suite,
				"similarity":   r.Similarity,
				"threshold":    r.Threshold,
				"matchedEntry": r.MatchedEntry,
			}
			if dryRun {
				out["dryRun"] = true
				out["heldOut"] = r.HeldOut
				out["mustBeat"] = r.MustBeat
				out["mustBeatScore"] = r.MustBeatScore
				blob, _ := json.Marshal(out)
				fmt.Println(string(blob))
				// Dry-run never writes; exit 0 only when every checkable gate
				// passed (see the Node body).
				if r.Reason != "accepted" {
					setExitCode(1)
				}
				return nil
			}
			blob, err := json.Marshal(out)
			if err != nil {
				return err
			}
			fmt.Println(string(blob))
			if r.Reason != "accepted" {
				setExitCode(1)
			}
			return nil
		},
	}
	lessonsCmd.Flags().String("repo", "", "repository containing the lessons file")
	lessonsCmd.Flags().String("entry", "", "lesson entry to append (one line)")
	lessonsCmd.Flags().String("predicted-impact", "", "predictedImpact field required by the eval guard (AHE/Meta-Harness propose→evaluate→accept precedent); empty values are rejected")
	lessonsCmd.Flags().String("lessons-file", "", "repo-relative lessons file path (default .selfbuild/lessons.md)")
	lessonsCmd.Flags().Float64("threshold", -1, "similarity reject threshold in [0,1] (default 0.8)")
	lessonsCmd.Flags().Int("suite-timeout-ms", 600000, "evaluate-step wall-clock budget in ms (default 600000)")
	lessonsCmd.Flags().Int("loop", 0, "self-build loop number; written to the lessons-eval ledger row so impact scoring can join it to the loop-result row (Q39)")
	lessonsCmd.Flags().Bool("dry-run", false, "validate the append (dedupe + held-out must-beat check) without staging the lessons file, running the suite, or writing ledger rows; prints what a real append would do")

	scoresCmd := &cobra.Command{
		Use:   "scores",
		Short: "Show measured lesson impact scores (Q39): per-excerptHash accept rate, repeat-failure delta, and composite score aggregated from the orchestration ledger (lessons-eval rows joined with loop-result rows).",
		RunE: func(cmd *cobra.Command, args []string) error {
			// --repo is declared on the parent `lessons` command; commander
			// binds it to the parent even when given after the subcommand
			// name (v12 behavior) — mirror that via cmd.Parent().
			repo, _ := cmd.Parent().Flags().GetString("repo")
			asJSON, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			events := lessons.ReadEvents(repo)
			scores := lessons.ComputeLessonScores(events)
			if len(scores) == 0 {
				fmt.Println("No lesson impact scores: no lessons-eval rows in the orchestration ledger yet.")
				return nil
			}
			type row struct {
				hash string
				s    lessons.LessonScore
			}
			rows := make([]row, 0, len(scores))
			for h, s := range scores {
				rows = append(rows, row{h, s})
			}
			for i := 1; i < len(rows); i++ {
				for j := i; j > 0 && rows[j].s.Score > rows[j-1].s.Score; j-- {
					rows[j], rows[j-1] = rows[j-1], rows[j]
				}
			}
			if asJSON {
				out := map[string]lessons.LessonScore{}
				for _, r := range rows {
					out[r.hash] = r.s
				}
				blob, err := json.MarshalIndent(out, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
				return nil
			}
			fmt.Println("excerptHash      score   acceptRate  delta   evals  loopFailRate")
			for _, r := range rows {
				fmt.Printf("%-16s %6.3f  %10.3f    %5.3f  %5d  %10.3f\n",
					r.hash, r.s.Score, r.s.AcceptRate, r.s.Delta, r.s.EvalCount, r.s.LessonLoopFailureRate)
			}
			return nil
		},
	}
	scoresCmd.Flags().Bool("json", false, "emit JSON")
	lessonsCmd.AddCommand(scoresCmd)
	return lessonsCmd
}

func gatesFlagFloat(cmd *cobra.Command, name string) *float64 {
	f, err := cmd.Flags().GetFloat64(name)
	if err != nil || f < 0 {
		return nil
	}
	return &f
}

func gatesFlagInt(cmd *cobra.Command, name string) int {
	v, err := cmd.Flags().GetInt(name)
	if err != nil {
		return 0
	}
	return v
}

func gatesFlagIntOpt(cmd *cobra.Command, name string) *int {
	if !cmd.Flags().Changed(name) {
		return nil
	}
	v, err := cmd.Flags().GetInt(name)
	if err != nil {
		return nil
	}
	return &v
}

// ---------------------------------------------------------------------------
// pr-hygiene / autosweep / automerge
// ---------------------------------------------------------------------------

func newPrHygieneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr-hygiene",
		Short: "Zombie-PR hygiene for devagent/TASK-* PRs: close base-superseded, flag red-across-grace (skips autoMerge until green)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			graceHours := gatesFlagIntOpt(cmd, "grace-hours")
			autoMerge, _ := cmd.Flags().GetBool("auto-merge")
			apply, _ := cmd.Flags().GetBool("apply")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			dryRun := true
			if cfg.PRHygiene != nil && cfg.PRHygiene.DryRun != nil {
				dryRun = *cfg.PRHygiene.DryRun
			}
			dryRunEff := !apply || dryRun
			autoMergeEff := autoMerge || (cfg.AutoMerge != nil && *cfg.AutoMerge)
			opts := orchestrator.PrHygieneOptions{
				DryRun:    &dryRunEff,
				AutoMerge: &autoMergeEff,
				Log:       func(msg string) { fmt.Println(msg) },
			}
			if graceHours != nil {
				f := float64(*graceHours)
				opts.GraceHours = &f
			} else if cfg.PRHygiene != nil && cfg.PRHygiene.GraceHours != nil {
				opts.GraceHours = cfg.PRHygiene.GraceHours
			}
			res := orchestrator.SweepTaskPrHygiene(repo, opts, orchestrator.DefaultRunGh)
			for _, o := range res.Outcomes {
				fmt.Printf("#%d %s (%s): %s\n", o.PR, o.Action, o.Reason, o.Detail)
			}
			acted := 0
			for _, o := range res.Outcomes {
				if o.Action == "closed" || o.Action == "flagged" {
					acted++
				}
			}
			suffix := ""
			if !apply {
				suffix = " (dry-run; pass --apply to act)"
			}
			fmt.Printf("\n%d/%d PR(s) %s%s\n", acted, len(res.Outcomes), tern(apply, "swept", "flagged"), suffix)
			if res.SkipAutoMerge {
				fmt.Println("autoMerge skipped: red-across-grace PR(s) present")
			}
			if res.SkipAutoMerge {
				setExitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "target repository (used for gh context)")
	cmd.Flags().Float64("grace-hours", -1, "hours a PR may stay red before being flagged; also the auto-close age floor, capped at 24h (default from config prHygiene.graceHours)")
	cmd.Flags().Bool("auto-merge", false, "report skipAutoMerge when a red-across-grace PR would have merged (default from config autoMerge)")
	cmd.Flags().Bool("apply", false, "comment and close for real (default: config prHygiene.dryRun, itself defaulting to dry-run)")
	return cmd
}

func newAutoSweepCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "autosweep",
		Short: "Zombie-PR hygiene sweep: skip superseded PRs and auto-close PRs red across the grace window",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			stale, _ := cmd.Flags().GetBool("stale-prs")
			graceDays := gatesFlagIntOpt(cmd, "grace-days")
			apply, _ := cmd.Flags().GetBool("apply")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if !stale {
				fmt.Fprintln(os.Stderr, "nothing to sweep: pass --stale-prs")
				setExitCode(1)
				return nil
			}
			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			dryRun := true
			if cfg.ZombiePrs != nil && cfg.ZombiePrs.DryRun != nil {
				dryRun = *cfg.ZombiePrs.DryRun
			}
			opts := orchestrator.ZombiePrOptions{
				DryRun: boolPtr(!apply || dryRun),
				Log:    func(msg string) { fmt.Println(msg) },
			}
			if graceDays != nil {
				f := float64(*graceDays)
				opts.GraceDays = &f
			} else if cfg.ZombiePrs != nil && cfg.ZombiePrs.GraceDays != nil {
				opts.GraceDays = cfg.ZombiePrs.GraceDays
			}
			outcomes := orchestrator.SweepStalePrs(repo, opts, orchestrator.DefaultRunGh)
			for _, o := range outcomes {
				fmt.Printf("#%d %s: %s\n", o.PR, o.Action, o.Detail)
			}
			acted := 0
			for _, o := range outcomes {
				if o.Action == "superseded" || o.Action == "closed" {
					acted++
				}
			}
			fmt.Printf("\n%d/%d PR(s) %s\n", acted, len(outcomes), tern(apply, "swept", "flagged (dry-run; pass --apply to act)"))
			return nil
		},
	}
	cmd.Flags().Bool("stale-prs", false, "sweep stale open PRs (superseded-base skip + grace-window close)")
	cmd.Flags().String("repo", "", "target repository (used for gh context)")
	cmd.Flags().Float64("grace-days", -1, "days a PR may stay red before auto-close (default from config zombiePrs.graceDays)")
	cmd.Flags().Bool("apply", false, "comment and close for real (default: config zombiePrs.dryRun, itself defaulting to dry-run)")
	return cmd
}

func tern(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

func boolPtr(b bool) *bool { return &b }
