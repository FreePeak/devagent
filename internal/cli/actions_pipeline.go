package cli

// Pipeline-family command wiring (FR-GO-04 #194 / FR-GO-05 #190): the cobra
// bodies for run/fleet/task/orchestrate/project/create/consume/backlog-check/
// reap-stale, byte-parity with the src/cli.ts action bodies, on top of the
// landed internal/pipeline + internal/orchestrator ports.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/integrations"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/pipeline"
	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/workers"
	"github.com/spf13/cobra"
)

var autoMergePRRe = regexp.MustCompile(`pull/(\d+)`)

// resolveCleanupFlag mirrors resolveCleanup (src/cli.ts): the CLI --cleanup
// flag wins over devagent.json; the final default is 'auto'.
func resolveCleanupFlag(flagValue, fileConfig string) (pipeline.CleanupMode, error) {
	value := fileConfig
	if flagValue != "" {
		value = flagValue
	}
	if value == "" {
		value = "auto"
	}
	switch value {
	case "auto", "keep", "always":
		return pipeline.CleanupMode(value), nil
	}
	return "", fmt.Errorf("Invalid --cleanup %q; expected auto, keep, or always", value)
}

// parseConcurrencyCLI mirrors parseConcurrency (src/cli.ts): "auto" stays the
// sentinel; anything else must parse as a finite number >= 1 and floors.
func parseConcurrencyCLI(raw string) (orchestrator.ConcurrencyValue, error) {
	if raw == "auto" || strings.ToLower(raw) == "auto" {
		return orchestrator.ConcurrencyValue{Auto: true}, nil
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 {
		return orchestrator.ConcurrencyValue{}, fmt.Errorf("Invalid --concurrency %q; expected positive integer or %q", raw, "auto")
	}
	return orchestrator.ConcurrencyValue{Value: int(math.Floor(n))}, nil
}

// printOutcomes mirrors printOutcomes (src/cli.ts): one line per pipeline
// stage outcome, byte-identical.
func printOutcomes(outcomes []pipeline.StageOutcome) {
	for _, o := range outcomes {
		switch o.Stage {
		case "plan":
			fmt.Printf("Plan (%s):\n", o.Summary)
			for _, t := range o.Tasks {
				fmt.Printf("  - %s\n", t)
			}
		case "clarify":
			fmt.Printf("Needs clarification: %s\n", o.Question)
		case "implement":
			state := "failed"
			if o.OK {
				state = "ok"
			}
			fmt.Printf("Implement (%s): %s after %d attempt(s)\n", o.Worker, state, o.Attempts)
		case "validate":
			verdict := "FAILED"
			if o.Passed {
				verdict = "PASSED"
			}
			fmt.Printf("Validation gate: %s\n", verdict)
		case "publish":
			if o.PRURL != "" {
				fmt.Printf("PR opened: %s\n", o.PRURL)
			} else {
				fmt.Printf("Publish: %s\n", o.Note)
			}
		case "failed":
			fmt.Fprintf(os.Stderr, "Run failed: %s\n", o.Reason)
			setExitCode(1)
		}
	}
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

func runCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Execute the full pipeline for one ticket",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			ticket, _ := cmd.Flags().GetString("ticket")
			workerFlag, _ := cmd.Flags().GetString("worker")
			modelFlag, _ := cmd.Flags().GetString("model")
			variantFlag, _ := cmd.Flags().GetString("variant")
			cleanupFlag, _ := cmd.Flags().GetString("cleanup")
			dropOrcaFlag, _ := cmd.Flags().GetBool("drop-orca-workspace")
			autoPr, _ := cmd.Flags().GetBool("auto-pr")
			interactiveFlag, _ := cmd.Flags().GetBool("interactive")
			maxLoopsFlag, _ := cmd.Flags().GetInt("max-loops")
			timeoutFlag, _ := cmd.Flags().GetInt("timeout")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			autoMergeFlag, _ := cmd.Flags().GetBool("auto-merge")
			if repo == "" {
				repo, _ = os.Getwd()
			}

			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			creds := config.LoadCredentials()
			logger, err := ledger.NewRunLogger("")
			if err != nil {
				return err
			}

			// `x || config.x || undefined` mapping: a false flag falls
			// through to the config pointer, else unset.
			autoMerge := autoMergeFlag || (cfg.AutoMerge != nil && *cfg.AutoMerge)
			dropOrca := dropOrcaFlag || (cfg.DropOrcaWorkspace != nil && *cfg.DropOrcaWorkspace)
			worker := workerFlag
			if worker == "" {
				worker = cfg.Worker
			}
			model := modelFlag
			if model == "" {
				model = cfg.Model
			}
			variant := variantFlag
			if variant == "" {
				variant = cfg.Variant
			}
			maxLoops := cfg.MaxLoops
			if cmd.Flags().Changed("max-loops") {
				maxLoops = maxLoopsFlag
			}
			timeoutMinutes := cfg.TimeoutMinutes
			if cmd.Flags().Changed("timeout") {
				timeoutMinutes = timeoutFlag
			}
			cleanup, cerr := resolveCleanupFlag(cleanupFlag, cfg.Cleanup)
			if cerr != nil {
				return cerr
			}

			// Dry-run plans offline against a synthetic ticket; no
			// credentials needed.
			if !dryRun && creds.LinearAPIKey == "" {
				fmt.Fprintln(os.Stderr, "LINEAR_API_KEY is not set. Ticket fetching is unavailable.")
				setExitCode(1)
				return nil
			}

			// Latest-wins dedup: one active run per ticket across processes.
			lock := pipeline.TryAcquireRun(devagentHome(), ticket)
			if lock == nil {
				fmt.Fprintf(os.Stderr, "Run for %s already active\n", ticket)
				setExitCode(1)
				return nil
			}
			defer lock.Release()

			// Tracker selection: Jira when JIRA_* env present, else Linear.
			// (The TS fetchForTracker closure is dead code — buildDeps builds
			// its own fetch ticket incl. the env routing; only the log
			// annotation is observable here.)
			useJira := os.Getenv("JIRA_DOMAIN") != "" && os.Getenv("JIRA_EMAIL") != "" && os.Getenv("JIRA_API_TOKEN") != ""
			tracker := "linear"
			if useJira {
				tracker = "jira"
			}
			logger.Info(ledger.StageFetch, fmt.Sprintf("Run %s starting", logger.RunID()), []ledger.KV{
				{Key: "ticket", Value: ticket}, {Key: "worker", Value: worker},
				{Key: "dryRun", Value: dryRun}, {Key: "tracker", Value: tracker},
			})

			runCfg := pipeline.RunConfig{
				TicketID: ticket, RepoPath: repo, Worker: pipeline.WorkerName(worker),
				Model: model, Variant: variant,
				AutoMerge: autoMerge, AutoPr: autoPr, Interactive: interactiveFlag || !autoPr,
				MaxLoops: maxLoops, TimeoutMs: timeoutMinutes * 60000, DryRun: dryRun,
				Cleanup: cleanup, DropOrcaWorkspace: dropOrca,
			}
			var deps pipeline.PipelineDeps
			if dryRun {
				deps = pipeline.BuildDryRunDeps()
			} else {
				deps = pipeline.BuildDeps(creds, pipeline.StageConfig{
					RepoPath: repo, MaxLoops: maxLoops, TimeoutMs: runCfg.TimeoutMs,
					Worker: pipeline.WorkerName(worker), AutoPr: autoPr, Cleanup: cleanup,
					DropOrcaWorkspace: dropOrca, LessonsFile: cfg.LessonsFile,
					LessonsMaxChars: cfg.LessonsMaxChars, Context: cfg.Context,
					Model: model, Variant: variant,
				}, logger)
			}
			outcomes, perr := pipeline.RunPipeline(runCfg, deps, logger)
			if perr != nil {
				logger.Error(ledger.StageFetch, perr.Error(), nil)
				fmt.Fprintln(os.Stderr, perr.Error())
				setExitCode(1)
				return nil
			}
			printOutcomes(outcomes)
			fmt.Printf("Run log: %s\n", logger.Path())
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// fleet
// ---------------------------------------------------------------------------

func fleetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "fleet",
		Short: "Run tickets across multiple repositories with bounded concurrency",
		RunE: func(cmd *cobra.Command, args []string) error {
			repoEntries, _ := cmd.Flags().GetStringArray("repo")
			tickets, _ := cmd.Flags().GetStringArray("ticket")
			concurrencyRaw, _ := cmd.Flags().GetString("concurrency")
			workerFlag, _ := cmd.Flags().GetString("worker")
			cleanupFlag, _ := cmd.Flags().GetString("cleanup")
			dropOrcaFlag, _ := cmd.Flags().GetBool("drop-orca-workspace")
			autoPr, _ := cmd.Flags().GetBool("auto-pr")
			maxLoopsFlag, _ := cmd.Flags().GetInt("max-loops")
			cwd, _ := os.Getwd()

			cfg, err := config.Load(cwd)
			if err != nil {
				return err
			}

			entries := []pipeline.FleetEntry{}
			for _, raw := range repoEntries {
				eq := strings.Index(raw, "=")
				if eq <= 0 {
					fmt.Fprintf(os.Stderr, "Invalid --repo entry %q (expected name=path)\n", raw)
					setExitCode(1)
					return nil
				}
				entries = append(entries, pipeline.FleetEntry{Name: raw[:eq], Path: raw[eq+1:]})
			}

			creds := config.LoadCredentials()
			if creds.LinearAPIKey == "" {
				fmt.Fprintln(os.Stderr, "LINEAR_API_KEY is not set.")
				setExitCode(1)
				return nil
			}

			conc, cerr := parseConcurrencyCLI(concurrencyRaw)
			if cerr != nil {
				fmt.Fprintln(os.Stderr, "Error: "+cerr.Error())
				setExitCode(1)
				return nil
			}
			var governor *orchestrator.ResourceGovernor
			if conc.Auto {
				governor = orchestrator.NewResourceGovernor(nil)
				snap := governor.GetSnapshotSync()
				eff := governor.EffectiveAuto(*snap)
				fmt.Printf("[governor] fleet auto -> %d (%s)\n", eff, governor.FormatStatus(orchestrator.ConcurrencyValue{Auto: true}, eff, snap))
			}

			worker := workerFlag
			if worker == "" {
				worker = cfg.Worker
			}
			maxLoops := cfg.MaxLoops
			if cmd.Flags().Changed("max-loops") {
				maxLoops = maxLoopsFlag
			}
			cleanup, cerr := resolveCleanupFlag(cleanupFlag, cfg.Cleanup)
			if cerr != nil {
				return cerr
			}
			dropOrca := dropOrcaFlag || (cfg.DropOrcaWorkspace != nil && *cfg.DropOrcaWorkspace)

			result := pipeline.RunFleet(pipeline.FleetOptions{
				TicketIDs:   tickets,
				Entries:     entries,
				Concurrency: conc,
				Governor:    governor,
				TimeoutMs:   cfg.TimeoutMinutes * 60000,
				Worker:      pipeline.WorkerName(worker),
				AutoPr:      autoPr,
				MaxLoops:    maxLoops,
				RunOne: func(a pipeline.FleetRunArgs) pipeline.FleetRunResult {
					runCfg := pipeline.RunConfig{
						TicketID: a.TicketID, RepoPath: a.RepoPath, Worker: a.Worker,
						AutoPr: a.AutoPr, Interactive: !a.AutoPr, MaxLoops: a.MaxLoops,
						TimeoutMs: a.TimeoutMs, DryRun: false, Cleanup: cleanup,
						DropOrcaWorkspace: dropOrca,
					}
					// TS fleet cfg carries no autoMerge/model/variant — the
					// StageConfig omits them identically.
					stageCfg := pipeline.StageConfig{
						RepoPath: a.RepoPath, MaxLoops: a.MaxLoops, TimeoutMs: a.TimeoutMs,
						Worker: a.Worker, AutoPr: a.AutoPr, Cleanup: cleanup,
						DropOrcaWorkspace: dropOrca,
					}
					outcomes, perr := pipeline.RunPipeline(runCfg, pipeline.BuildDeps(creds, stageCfg, a.Log), a.Log)
					if perr != nil {
						return pipeline.FleetRunResult{OK: false, Summary: perr.Error()}
					}
					reason := "completed"
					for _, o := range outcomes {
						if o.Stage == "failed" {
							reason = o.Reason
							break
						}
					}
					return pipeline.FleetRunResult{OK: reason == "completed", Summary: reason}
				},
			})

			fmt.Println("\nFleet results:")
			for _, item := range result.Items {
				mark := "✗"
				if item.OK {
					mark = "✓"
				}
				line := fmt.Sprintf("  %s %s/%s: %s", mark, item.Entry, item.TicketID, item.Summary)
				if item.LogPath != "" {
					line += fmt.Sprintf(" (log %s)", item.LogPath)
				}
				fmt.Println(line)
			}
			fmt.Printf("%d succeeded, %d failed\n", result.Succeeded, result.Failed)
			if result.Failed > 0 {
				setExitCode(1)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// task
// ---------------------------------------------------------------------------

func taskCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "task",
		Short: "Run one prompt-driven task headlessly (orchestrator integration mode); --pick resolves a PRD Phase 4 backlog item and cross-checks it before dispatch",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			promptFlag, _ := cmd.Flags().GetString("prompt")
			pick, _ := cmd.Flags().GetString("pick")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			id, _ := cmd.Flags().GetString("id")
			workerFlag, _ := cmd.Flags().GetString("worker")
			modelFlag, _ := cmd.Flags().GetString("model")
			variantFlag, _ := cmd.Flags().GetString("variant")
			cleanupFlag, _ := cmd.Flags().GetString("cleanup")
			dropOrcaFlag, _ := cmd.Flags().GetBool("drop-orca-workspace")
			autoPr, _ := cmd.Flags().GetBool("auto-pr")
			autoMergeFlag, _ := cmd.Flags().GetBool("auto-merge")
			maxLoopsFlag, _ := cmd.Flags().GetInt("max-loops")
			remote, _ := cmd.Flags().GetString("remote")
			if repo == "" {
				repo, _ = os.Getwd()
			}

			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			creds := config.LoadCredentials()
			logger, err := ledger.NewRunLogger("")
			if err != nil {
				return err
			}

			// PRD backlog reconciliation at pick time: validate a --pick
			// before any dispatch; shipped picks are rejected and struck
			// (dry-run validates only, no writes).
			prompt := promptFlag
			if pick != "" {
				prdPath := filepath.Join(repo, "docs", "PRD.md")
				if _, serr := os.Stat(prdPath); serr != nil {
					fmt.Fprintf(os.Stderr, "task: no docs/PRD.md in %s\n", repo)
					setExitCode(1)
					return nil
				}
				prd, rerr := os.ReadFile(prdPath)
				if rerr != nil {
					return rerr
				}
				mergedTitles := pipeline.ListMergedPrTitles(repo)
				check := pipeline.CheckBacklogPick(pick, string(prd), mergedTitles, nil)
				if !dryRun && len(check.StruckIDs) > 0 {
					_ = os.WriteFile(prdPath, []byte(pipeline.StrikeBacklogItems(string(prd), check.StruckIDs)), 0o644)
					if check.Shipped {
						fmt.Printf("[task] struck confirmed-shipped backlog items: %s\n", strings.Join(check.StruckIDs, ", "))
					}
				}
				if !check.OK {
					fmt.Fprintln(os.Stderr, check.Message)
					setExitCode(1)
					return nil
				}
				if dryRun {
					fmt.Printf("[dry-run] pick %s: %s\n", pick, check.Message)
					return nil
				}
				prompt = check.Prompt
			}
			if prompt == "" {
				fmt.Fprintln(os.Stderr, "task: --prompt or --pick is required")
				setExitCode(1)
				return nil
			}

			// Remote execution: the shared host owns its checkout, workers
			// and credentials; this process is a thin SSH client.
			if remote != "" {
				logger.Info(ledger.StageTask, fmt.Sprintf("Task run %s starting (remote: %s)", logger.RunID(), remote), nil)
				cwd, _ := os.Getwd()
				result := pipeline.RunRemoteTask(pipeline.RunRemoteTaskOptions{
					Target: remote, Prompt: prompt, TaskID: id, Worker: workerFlag,
					TimeoutMs: cfg.TimeoutMinutes * 60000, Log: logger,
				}, pipeline.RemoteDeps{Run: func(argv []string, timeoutMs int) pipeline.RemoteRunOutcome {
					r := workers.SpawnCli(argv[0], argv[1:], workers.SpawnCliOptions{Dir: cwd, TimeoutMs: timeoutMs})
					return pipeline.RemoteRunOutcome{ExitCode: r.ExitCode, Stdout: r.Stdout}
				}})
				if result.OK {
					fmt.Println(result.Note)
				} else {
					fmt.Printf("remote task failed: %s\n", result.Note)
				}
				if result.PRURL != "" {
					fmt.Printf("PR: %s\n", result.PRURL)
				}
				if !result.OK {
					setExitCode(1)
				}
				return nil
			}

			autoMerge := autoMergeFlag || (cfg.AutoMerge != nil && *cfg.AutoMerge)
			dropOrca := dropOrcaFlag || (cfg.DropOrcaWorkspace != nil && *cfg.DropOrcaWorkspace)
			worker := workerFlag
			if worker == "" {
				worker = cfg.Worker
			}
			model := modelFlag
			if model == "" {
				model = cfg.Model
			}
			variant := variantFlag
			if variant == "" {
				variant = cfg.Variant
			}
			maxLoops := cfg.MaxLoops
			if cmd.Flags().Changed("max-loops") {
				maxLoops = maxLoopsFlag
			}
			cleanup, cerr := resolveCleanupFlag(cleanupFlag, cfg.Cleanup)
			if cerr != nil {
				return cerr
			}
			taskID := id
			if taskID == "" {
				taskID = os.Getenv("DEVAGENT_TASK_ID")
			}

			logger.Info(ledger.StageTask, fmt.Sprintf("Task run %s starting", logger.RunID()), []ledger.KV{
				{Key: "repo", Value: repo}, {Key: "autoPr", Value: autoPr},
			})

			// Durable-state bootstrap: best-effort, never fatal.
			if st, serr := git.EnsureStateBranch(repo, nil); serr != nil {
				logger.Info(ledger.StageTask, fmt.Sprintf("ensureStateBranch failed (continuing): %s", serr.Error()), nil)
			} else if st.Action == "created" {
				logger.Info(ledger.StageTask, "created selfbuild/state branch on origin", nil)
			}

			synthID := taskID
			if synthID == "" {
				synthID = "TASK"
			}
			stageCfg := pipeline.StageConfig{
				RepoPath: repo, MaxLoops: maxLoops, TimeoutMs: cfg.TimeoutMinutes * 60000,
				Worker: pipeline.WorkerName(worker), AutoPr: autoPr,
				LessonsFile: cfg.LessonsFile, LessonsMaxChars: cfg.LessonsMaxChars,
				Context: cfg.Context, Model: model, Variant: variant,
				Cleanup: cleanup, DropOrcaWorkspace: dropOrca,
			}
			// TS implementStage can reject; the Go seam has no error channel
			// so the closure captures the error and the caller surfaces it
			// exactly like the TS catch path.
			var implErr, pubErr error
			deps := pipeline.TaskDeps{
				RunPipelineDeps: pipeline.PipelineDeps{
					FetchTicket: func(string) (pipeline.TicketSpec, error) {
						return pipeline.TicketSpec{ID: synthID, Labels: []string{}, AcceptanceCriteria: []string{}}, nil
					},
					RunGateG3: func(rp string, classification pipeline.TicketClass) pipeline.GateCheck {
						r := gates.RunMigrationStaticGate(gates.GateContext{RepoPath: rp, Classification: string(classification)})
						return pipeline.GateCheck{Passed: r.Passed, Findings: r.Findings, Detail: r.Detail}
					},
				},
				ImplementStage: func(c pipeline.TaskOptions, ticket pipeline.TicketSpec, lg pipeline.RunLog) pipeline.TaskImplResult {
					plan := scout.ImplementationPlan{Ticket: ticket, Classification: "endpoint-only", Tasks: []string{}}
					res, ierr := pipeline.ImplementStage(stageCfg, plan, lg)
					if ierr != nil {
						implErr = ierr
						return pipeline.TaskImplResult{}
					}
					return pipeline.TaskImplResult{OK: res.OK, Worker: res.Worker, Attempts: res.Attempts, WorktreePath: res.WorktreePath}
				},
				PublishStage: func(c pipeline.TaskOptions, ticket pipeline.TicketSpec, impl pipeline.PublishImpl) string {
					if impl.WorktreePath == "" || creds.GithubToken == "" {
						return ""
					}
					base := cfg.GithubBaseBranch
					if base == "" {
						base = "main"
					}
					prURL, perr := pipeline.PublishTaskBranch(pipeline.TaskPublishOptions{
						RepoPath: c.RepoPath, Prompt: prompt, BaseBranch: base, Log: logger,
					}, impl, pipeline.TaskPublishDeps{
						CommitAllChanges: git.CommitAllChanges,
						CurrentBranch:    git.CurrentBranch,
						ListChangedFiles: git.ListChangedFiles,
						PushBranch: func(repoPath, branch string) error {
							return integrations.PushBranch(repoPath, branch, integrations.GitHubOptions{})
						},
						CreatePr: func(o pipeline.TaskPublishPrRequest) (string, error) {
							return integrations.CreatePr(integrations.CreatePrOptions{
								RepoPath: o.RepoPath, Branch: o.Branch, Title: o.Title, Body: o.Body, BaseBranch: base,
							}, integrations.GitHubOptions{})
						},
					})
					if perr != nil {
						pubErr = perr
						return ""
					}
					if prURL == "" {
						return ""
					}
					if c.AutoMerge {
						if m := autoMergePRRe.FindStringSubmatch(prURL); m != nil {
							n, _ := strconv.Atoi(m[1])
							go func() {
								baseBranch := "main"
								if lcfg, lerr := config.Load(repo); lerr == nil && lcfg.GithubBaseBranch != "" {
									baseBranch = lcfg.GithubBaseBranch
								}
								o := orchestrator.AutoReviewAndMergeOne(repo, n, orchestrator.AutoReviewAndMergeOneOpts{AutoReviewAndMergeOptions: orchestrator.AutoReviewAndMergeOptions{BaseBranch: baseBranch}}, nil)
								logger.Info(ledger.StageTask, fmt.Sprintf("auto-merge PR #%d: %s (%s)", n, o.Action, truncateStr(o.Detail, 120)), nil)
							}()
						}
					}
					return prURL
				},
			}

			result := pipeline.RunTask(pipeline.TaskOptions{
				Prompt: prompt, RepoPath: repo, AutoPr: autoPr, AutoMerge: autoMerge,
				MaxLoops: maxLoops, TimeoutMs: cfg.TimeoutMinutes * 60000,
				Cleanup: cleanup, DropOrcaWorkspace: dropOrca, Log: logger, TaskID: taskID,
			}, deps)
			if implErr != nil {
				fmt.Fprintln(os.Stderr, implErr.Error())
				setExitCode(1)
				return nil
			}
			if pubErr != nil {
				fmt.Fprintln(os.Stderr, pubErr.Error())
				setExitCode(1)
				return nil
			}
			fmt.Println(result.Note)
			if !result.OK {
				setExitCode(1)
			}
			fmt.Printf("Run log: %s\n", logger.Path())
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// orchestrate
// ---------------------------------------------------------------------------

func orchestrateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "orchestrate",
		Short: "Decompose a goal into a task board and implement it in dependency waves",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			goal, _ := cmd.Flags().GetString("goal")
			plannerFlag, _ := cmd.Flags().GetString("planner")
			executorFlag, _ := cmd.Flags().GetString("executor")
			auditorFlag, _ := cmd.Flags().GetString("auditor")
			noAudit, _ := cmd.Flags().GetBool("no-audit")
			answers, _ := cmd.Flags().GetStringArray("answer")
			concurrencyRaw, _ := cmd.Flags().GetString("concurrency")
			maxTaskRetries, _ := cmd.Flags().GetInt("max-task-retries")
			maxRecoveries, _ := cmd.Flags().GetInt("max-recoveries")
			maxTotalAttempts, _ := cmd.Flags().GetInt("max-total-attempts")
			planOnly, _ := cmd.Flags().GetBool("plan-only")
			maxWavesFlag, _ := cmd.Flags().GetInt("max-waves")
			resume, _ := cmd.Flags().GetBool("resume")
			noMerge, _ := cmd.Flags().GetBool("no-merge")
			if repo == "" {
				repo, _ = os.Getwd()
			}

			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			logger, err := ledger.NewRunLogger("")
			if err != nil {
				return err
			}
			logger.Info(ledger.StageTask, fmt.Sprintf("Orchestration run %s starting", logger.RunID()), []ledger.KV{
				{Key: "repo", Value: repo},
			})

			// The TS body runs inside one try/catch: any error prints the
			// bare message to stderr and exits 1 (no cobra "Error:" prefix).
			fail := func(err error) {
				fmt.Fprintln(os.Stderr, err.Error())
				setExitCode(1)
			}

			// Durable-state bootstrap: best-effort, never fatal.
			if st, serr := git.EnsureStateBranch(repo, nil); serr != nil {
				logger.Info(ledger.StageTask, fmt.Sprintf("ensureStateBranch failed (continuing): %s", serr.Error()), nil)
			} else if st.Action == "created" {
				logger.Info(ledger.StageTask, "created selfbuild/state branch on origin", nil)
			}

			plannerName := plannerFlag
			if plannerName == "" {
				plannerName = cfg.Worker
			}
			executorName := executorFlag
			if executorName == "" {
				executorName = cfg.Worker
			}
			// Evidence gate on by default: only an independent audit makes
			// executor success trusted state.
			auditorName := auditorFlag
			if auditorName == "" {
				auditorName = executorName
			}
			if noAudit {
				auditorName = ""
			}
			timeoutMs := cfg.TimeoutMinutes * 60000

			board := (*orchestrator.ProjectBoard)(nil)
			if resume {
				board = orchestrator.LoadBoard(repo)
			}
			if board == nil {
				if resume {
					fmt.Fprintln(os.Stderr, "No existing board found; planning fresh.")
				}
				tasks, perr := pipeline.RunPlanner(goal, repo, pipeline.WorkerName(plannerName), timeoutMs,
					pipeline.PlannerOptions{Model: cfg.Model, Variant: cfg.Variant})
				if perr != nil {
					fail(perr)
					return nil
				}
				board = orchestrator.CreateBoard(goal, tasks, orchestrator.ProjectBoardRoles{
					Planner:  orchestrator.WorkerName(plannerName),
					Executor: orchestrator.WorkerName(executorName),
					Auditor:  orchestrator.WorkerName(auditorName),
				})
				orchestrator.SaveBoard(repo, board)
				fmt.Printf("Plan (%d task(s)):\n", len(tasks))
				for _, t := range tasks {
					line := fmt.Sprintf("  %s: %s", t.ID, t.Title)
					if len(t.DependsOn) > 0 {
						line += fmt.Sprintf(" (after %s)", strings.Join(t.DependsOn, ","))
					}
					fmt.Println(line)
				}
			}

			// Validate-before-spend: show the full contracts, stop before
			// executor spend.
			if planOnly {
				fmt.Println(orchestrator.FormatPlanOnly(board))
				fmt.Printf("\nPlan-only: board saved at %s. Execute later with --resume.\n", filepath.Join(repo, ".devagent-project.json"))
				return nil
			}

			// Human-in-the-loop: resolve tasks paused with verdict 'ask'.
			for _, a := range answers {
				eq := strings.Index(a, "=")
				if eq <= 0 {
					fmt.Fprintf(os.Stderr, "--answer %s: expected <taskId>=<answer>; ignored\n", a)
					continue
				}
				r := orchestrator.ApplyHumanAnswer(board, a[:eq], a[eq+1:])
				if r.OK {
					fmt.Println(r.Note)
				} else {
					fmt.Fprintf(os.Stderr, "--answer %s: %s\n", a, r.Note)
				}
			}

			// Resource governor for auto concurrency.
			conc, cerr := parseConcurrencyCLI(concurrencyRaw)
			if cerr != nil {
				fail(cerr)
				return nil
			}
			var governor *orchestrator.ResourceGovernor
			if conc.Auto {
				governor = orchestrator.NewResourceGovernor(nil)
				snap := governor.GetSnapshotSync()
				eff := governor.EffectiveAuto(*snap)
				fmt.Printf("[governor] %s\n", governor.FormatStatus(orchestrator.ConcurrencyValue{Auto: true}, eff, snap))
				logger.Info(ledger.StageGovernor, fmt.Sprintf("auto -> %d", eff), []ledger.KV{
					{Key: "freeMem", Value: snap.FreeMem}, {Key: "totalMem", Value: snap.TotalMem},
					{Key: "cpus", Value: snap.Cpus}, {Key: "estPerWorker", Value: governor.GetEstMemPerWorker()},
				})
			}

			schedOpts := orchestrator.SchedulerOptions{
				RepoPath:         repo,
				LessonsFile:      cfg.LessonsFile,
				LessonsMaxChars:  cfg.LessonsMaxChars,
				Executor:         orchestrator.WorkerName(executorName),
				Concurrency:      orchestrator.SchedulerConcurrency{Auto: conc.Auto, N: conc.Value},
				MaxTaskRetries:   maxTaskRetries,
				MaxRecoveries:    &maxRecoveries,
				MaxTotalAttempts: &maxTotalAttempts,
				TimeoutMs:        timeoutMs,
				OnWavePersisted: func(b *orchestrator.ProjectBoard) {
					orchestrator.SaveBoard(repo, b)
				},
			}
			if governor != nil {
				schedOpts.Governor = orchestrator.GovernorAdapter{G: governor}
			}
			if cmd.Flags().Changed("max-waves") {
				v := maxWavesFlag
				schedOpts.MaxWaves = &v
			}

			deps := orchestrator.SchedulerDeps{
				ExecuteTask: func(a orchestrator.ExecuteTaskArgs) (orchestrator.ExecuteTaskResult, error) {
					a.Executor = orchestrator.WorkerName(executorName)
					a.Deps = orchestrator.ExecutorDeps{
						Dispatcher: cliWorkerDispatcher{},
						ReapStaleWorkers: func(cwdPrefix string, idleMs int) []int {
							stale := pipeline.FindStaleWorkerPids(idleMs, &pipeline.ReapOptions{CWDPrefix: cwdPrefix})
							pids := make([]int, 0, len(stale))
							for _, s := range stale {
								pids = append(pids, s.Pid)
							}
							return pids
						},
						KillProcessTree: func(pid int, _ string) { _ = pipeline.KillStaleProcessTree(pid) },
					}
					return orchestrator.ExecuteTask(a)
				},
			}
			if auditorName != "" {
				deps.AuditTask = func(a orchestrator.AuditTaskArgs) (*orchestrator.AuditVerdict, error) {
					wt := a.Task.WorktreePath
					if wt == "" {
						wt = a.RepoPath
					}
					runner := func(prompt string) (string, error) {
						w, werr := workers.GetWorker(auditorName)
						if werr != nil {
							return "", werr
						}
						awc, lerr := config.Load(wt)
						model, variant := "", ""
						if lerr == nil {
							model, variant = awc.Model, awc.Variant
						}
						r := w.Spawn(workers.WorkerSpawnOptions{
							Prompt: prompt, Cwd: wt, TimeoutMs: a.TimeoutMs, Model: model, Variant: variant,
						})
						if r.TimedOut || r.ExitCode != 0 {
							return "", fmt.Errorf("auditor %s exited %d (timedOut=%v)", auditorName, r.ExitCode, r.TimedOut)
						}
						return r.ResultText, nil
					}
					return orchestrator.RunAudit(orchestrator.RunAuditArgs{
						Board: *a.Board, Task: *a.Task, WorktreePath: wt,
						TimeoutMs: a.TimeoutMs, Auditor: orchestrator.WorkerName(auditorName),
					}, runner, nil), nil
				}
			}
			if maxRecoveries > 0 {
				deps.PlanRecovery = func(a orchestrator.PlanRecoveryArgs) (*orchestrator.RecoveryPlan, error) {
					contract := pipeline.RunRecoveryPlanner(struct {
						Goal          string
						Task          orchestrator.OrchestratorTask
						RepoPath      string
						PlannerWorker pipeline.WorkerName
						TimeoutMs     int
						Opts          pipeline.PlannerOptions
					}{
						Goal: board.Goal, Task: *a.Task, RepoPath: repo,
						PlannerWorker: pipeline.WorkerName(plannerName), TimeoutMs: timeoutMs,
						Opts: pipeline.PlannerOptions{Model: cfg.Model, Variant: cfg.Variant},
					})
					if contract == nil {
						return nil, nil
					}
					return &orchestrator.RecoveryPlan{Prompt: contract.Prompt, AcceptanceCriteria: contract.AcceptanceCriteria}, nil
				}
			}
			deps.PublishTaskPr = func(a orchestrator.PublishTaskPrArgs) (string, error) {
				task := a.Task
				recoveries := 0
				if task.Recoveries != nil {
					recoveries = *task.Recoveries
				}
				branch := fmt.Sprintf("devagent/%s-%s", task.ID, orchestrator.AttemptSuffix(task.Attempts, recoveries))
				if err := integrations.PushBranch(a.RepoPath, branch, integrations.GitHubOptions{}); err != nil {
					return "", err
				}
				base := cfg.GithubBaseBranch
				if base == "" {
					base = "main"
				}
				body := task.Title
				if task.Prompt != "" {
					body = firstNLines(task.Prompt, 30)
				}
				body = strings.Join([]string{fmt.Sprintf("## Task %s", task.ID), "", body}, "\n")
				prURL, err := integrations.CreatePr(integrations.CreatePrOptions{
					RepoPath: a.RepoPath, Branch: branch,
					Title: fmt.Sprintf("[%s] %s", task.ID, task.Title), Body: body, BaseBranch: base,
				}, integrations.GitHubOptions{})
				if err != nil {
					return "", err
				}
				a.Log.Info("publish", fmt.Sprintf("opened PR %s for %s", prURL, task.ID), nil)
				return prURL, nil
			}

			result := orchestrator.RunScheduler(board, schedOpts, deps, logger)
			orchestrator.SaveBoard(repo, result)

			done, failed, blocked, untrusted := 0, 0, 0, 0
			for _, t := range result.Tasks {
				switch t.Status {
				case "done":
					done++
				case "failed":
					failed++
				case "blocked":
					blocked++
				case "untrusted":
					untrusted++
				}
			}
			statusLine := fmt.Sprintf("\nProject: %d/%d done, %d failed, %d blocked", done, len(result.Tasks), failed, blocked)
			if untrusted > 0 {
				statusLine += fmt.Sprintf(", %d awaiting audit", untrusted)
			}
			fmt.Println(statusLine)
			for _, t := range result.Tasks {
				line := fmt.Sprintf("  [%s] %s: %s", t.Status, t.ID, t.Title)
				if t.FailureDetail != "" {
					line += fmt.Sprintf(" — %s", truncateStr(t.FailureDetail, 100))
				}
				fmt.Println(line)
			}
			if failed > 0 || blocked > 0 {
				setExitCode(1)
			}
			fmt.Printf("Board: %s/.devagent-project.json (resume with --resume)\n", repo)
			fmt.Printf("Run log: %s\n", logger.Path())

			// Merge-back when every task is done and not a dry inspection.
			allDone := len(result.Tasks) > 0
			for _, t := range result.Tasks {
				if t.Status != "done" {
					allDone = false
					break
				}
			}
			if !allDone || noMerge {
				return nil
			}

			// Zombie-PR hygiene on the all-done path (best-effort).
			hygiene := orchestrator.SweepTaskPrHygiene(repo, orchestrator.PrHygieneOptions{
				AutoMerge: boolPtr(true),
				Log:       func(m string) { logger.Info(ledger.StageTask, m, nil) },
			}, nil)
			for _, o := range hygiene.Outcomes {
				if o.Action != "untouched" && o.Action != "skipped" {
					logger.Info(ledger.StageTask, fmt.Sprintf("pr-hygiene #%d %s (%s): %s", o.PR, o.Action, o.Reason, truncateStr(o.Detail, 160)), nil)
				}
			}
			if hygiene.SkipAutoMerge {
				logger.Warn(ledger.StageTask, "merge-back skipped: red-across-grace TASK PR(s) present; re-run when CI is green", nil)
				fmt.Println("\nMerge-back skipped: red-across-grace TASK PR(s) present; re-run with --resume when CI is green.")
				return nil
			}

			// Per-task PRs own integration; the local merge-back would
			// double-merge the same branches.
			if orchestrator.PerTaskPrPublished(*result) {
				prTasks := 0
				for _, t := range result.Tasks {
					if t.Status == "done" && t.PrURL != "" {
						prTasks++
					}
				}
				note := fmt.Sprintf("Merge-back skipped: %d done task(s) already published per-task PR(s); integration is owned by those PRs.", prTasks)
				logger.Info(ledger.StageTask, note, nil)
				fmt.Printf("\n%s\n", note)
				return nil
			}

			baseCfg, berr := config.Load(repo)
			if berr != nil {
				fail(berr)
				return nil
			}
			base := baseCfg.GithubBaseBranch
			if base == "" {
				base = "main"
			}
			fmt.Printf("\nAll tasks done — merging into %s...\n", base)
			stashSha, serr := git.StashMainWorktree(repo, "devagent auto-stash before merge")
			if serr != nil {
				fail(serr)
				return nil
			}
			if stashSha != "" {
				fmt.Printf("Auto-stashed uncommitted changes as %s\n", stashSha)
			}
			restore := func() {
				if stashSha == "" {
					return
				}
				if !orchestrator.RestoreAutoStash(repo, stashSha) {
					fmt.Fprintf(os.Stderr, "Warning: could not restore stash %s; stash kept for manual recovery.\n", stashSha)
				}
			}
			if aerr := git.AssertCleanMainWorktree(repo, base); aerr != nil {
				restore()
				fail(aerr)
				return nil
			}
			mr := orchestrator.MergeProjectBranches(repo, *result, base, logger, nil)
			if mr.Ok {
				fmt.Printf("Integrated: %s\n", strings.Join(mr.Merged, ", "))
			} else {
				fmt.Fprintf(os.Stderr, "Integration failed at %s (%s): %s\n", mr.Failure.TaskID, mr.Failure.Stage, mr.Failure.Detail)
				fmt.Fprintln(os.Stderr, "Board preserved; fix and re-run with --resume.")
				setExitCode(1)
			}
			restore()
			return nil
		},
	}
}

// firstNLines mirrors `text.split('\n').slice(0, 30).join('\n')`.
func firstNLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// project
// ---------------------------------------------------------------------------

func projectCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "project",
		Short: "Show orchestrator project board status for a repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			board := orchestrator.LoadBoard(repo)
			if board == nil {
				fmt.Println(`No project board. Start one: devagent orchestrate --goal "..."`)
				return nil
			}
			// Counts in insertion order (JS Map semantics).
			order := []string{}
			counts := map[string]int{}
			for _, t := range board.Tasks {
				s := string(t.Status)
				if _, ok := counts[s]; !ok {
					order = append(order, s)
				}
				counts[s]++
			}
			fmt.Printf("Goal: %s\n", board.Goal)
			roleLine := fmt.Sprintf("Roles: planner=%s executor=%s", board.Roles.Planner, board.Roles.Executor)
			if board.Roles.Auditor != "" {
				roleLine += fmt.Sprintf(" auditor=%s", board.Roles.Auditor)
			}
			fmt.Println(roleLine)
			bar := make([]string, 0, len(order))
			for _, k := range order {
				bar = append(bar, fmt.Sprintf("%s:%d", k, counts[k]))
			}
			fmt.Printf("Tasks: %d (%s)\n", len(board.Tasks), strings.Join(bar, " "))
			for _, t := range board.Tasks {
				mark := "·"
				switch t.Status {
				case "done":
					mark = "✓"
				case "failed":
					mark = "✗"
				case "dispatched":
					mark = "▶"
				case "blocked":
					mark = "⛔"
				case "untrusted":
					mark = "~"
				case "ask":
					mark = "?"
				}
				auditNote := ""
				if t.Audit != nil {
					unmet := 0
					for _, c := range t.Audit.CriteriaResults {
						if !c.Met {
							unmet++
						}
					}
					auditNote = fmt.Sprintf(" [audit %s/%s", t.Audit.Verdict, t.Audit.Integrity)
					if unmet > 0 {
						auditNote += fmt.Sprintf(", unmet: %d", unmet)
					}
					auditNote += "]"
				}
				gapNote := ""
				if len(t.EvidenceGaps) > 0 {
					gapNote = fmt.Sprintf(" — gaps: %s", truncateStr(t.EvidenceGaps[0], 80))
				}
				line := fmt.Sprintf(" %s [%s] %s: %s%s%s", mark, t.Status, t.ID, t.Title, auditNote, gapNote)
				if gapNote == "" && t.FailureDetail != "" {
					line += fmt.Sprintf(" — %s", truncateStr(t.FailureDetail, 80))
				}
				fmt.Println(line)
				// Ledger evidence history: verdict trends at a glance.
				tail := ledger.LedgerTailFor(repo, t.ID, 3)
				if len(tail) > 1 || (len(tail) == 1 && tail[0].Verdict != "pass") {
					segs := make([]string, 0, len(tail))
					for _, r := range tail {
						segs = append(segs, fmt.Sprintf("%s/%s@a%d", r.Verdict, r.Integrity, r.Attempt))
					}
					fmt.Printf("    history: %s\n", strings.Join(segs, " -> "))
				}
			}
			fmt.Printf("Updated: %s\n", board.UpdatedAt)
			allDone := len(board.Tasks) > 0
			for _, t := range board.Tasks {
				if t.Status != "done" {
					allDone = false
					break
				}
			}
			if allDone {
				fmt.Println(`Ready to integrate: devagent orchestrate --goal "" --resume`)
			} else {
				fmt.Println(`Resume: devagent orchestrate --goal "" --resume`)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

func createCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "create",
		Short: "Bootstrap the factory: queue dirs, config, optional scout LaunchAgent + Orca worktrees (FR-CREATE-01)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			scoutFlag, _ := cmd.Flags().GetBool("scout")
			trackerFlag, _ := cmd.Flags().GetBool("tracker")
			builderFlag, _ := cmd.Flags().GetBool("builder")
			orchestratorFlag, _ := cmd.Flags().GetBool("orchestrator")
			orchestratorGoal, _ := cmd.Flags().GetString("orchestrator-goal")
			workersFlag, _ := cmd.Flags().GetInt("workers")
			autoMergeFlag, _ := cmd.Flags().GetBool("auto-merge")
			selfUpdateFlag, _ := cmd.Flags().GetBool("self-update")
			intervalFlag, _ := cmd.Flags().GetInt("interval")
			trackIntervalFlag, _ := cmd.Flags().GetInt("track-interval")
			scoutWorkerFlag, _ := cmd.Flags().GetString("scout-worker")
			dryRun, _ := cmd.Flags().GetBool("dry-run")

			if orchestratorFlag && orchestratorGoal == "" {
				if _, err := os.Stat(filepath.Join(repo, ".devagent", "orchestrator-goal.txt")); err != nil {
					fmt.Fprintln(os.Stderr, "--orchestrator requires --orchestrator-goal (or .devagent/orchestrator-goal.txt)")
					setExitCode(1)
					return nil
				}
			}
			var trackInterval *int
			if trackIntervalFlag != 0 {
				trackInterval = &trackIntervalFlag
			}
			r := pipeline.RunCreate(pipeline.CreateOptions{
				RepoPath:             repo,
				Scout:                scoutFlag,
				Tracker:              trackerFlag,
				Builder:              builderFlag,
				Orchestrator:         orchestratorFlag,
				OrchestratorGoal:     orchestratorGoal,
				Workers:              workersFlag,
				AutoMerge:            autoMergeFlag,
				SelfUpdate:           selfUpdateFlag,
				DryRun:               dryRun,
				IntervalMinutes:      intervalFlag,
				ScoutWorker:          scoutWorkerFlag,
				TrackIntervalMinutes: trackInterval,
			})
			fmt.Println(r.Detail)
			if r.ConfigPath != "" {
				fmt.Printf("config: %s\n", r.ConfigPath)
			}
			for _, p := range r.LaunchAgentPlists {
				fmt.Printf("LaunchAgent: %s\n", p)
			}
			if len(r.LaunchAgentPlists) == 0 && r.LaunchAgentPlist != "" {
				fmt.Printf("LaunchAgent: %s\n", r.LaunchAgentPlist)
			}
			for _, p := range r.OrcaWorktrees {
				fmt.Printf("  worktree: %s\n", p)
			}
			if !r.OK {
				setExitCode(1)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// consume
// ---------------------------------------------------------------------------

func consumeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "consume",
		Short: "Claim one queued task and run it through devagent task + validation -> PR (FR-WORKER-02)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			autoPr, _ := cmd.Flags().GetBool("auto-pr")
			autoMergeFlag, _ := cmd.Flags().GetBool("auto-merge")
			maxLoopsFlag, _ := cmd.Flags().GetInt("max-loops")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			cfg, err := config.Load(repo)
			if err != nil {
				return err
			}
			autoMerge := autoMergeFlag || (cfg.AutoMerge != nil && *cfg.AutoMerge)
			maxLoops := cfg.MaxLoops
			if cmd.Flags().Changed("max-loops") {
				maxLoops = maxLoopsFlag
			}
			r, cerr := pipeline.ConsumeOnce(pipeline.ConsumeOptions{
				RepoPath: repo, AutoPr: autoPr, AutoMerge: autoMerge,
				MaxLoops: maxLoops, TimeoutMs: cfg.TimeoutMinutes * 60000,
			})
			if cerr != nil {
				fmt.Fprintln(os.Stderr, cerr.Error())
				setExitCode(1)
				return nil
			}
			fmt.Println(r.Detail)
			if r.PRURL != "" {
				fmt.Printf("PR: %s\n", r.PRURL)
			}
			if !r.OK && r.TaskID != "" {
				setExitCode(1)
			}
			if r.TaskID == "" {
				fmt.Println("No pending tasks.")
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// backlog-check
// ---------------------------------------------------------------------------

func backlogCheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "backlog-check <pick-id>",
		Short: "Cross-check a Phase 4 backlog ref against merged PR titles + completion notes + ledger goals before dispatch (PRD:889)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			ledgerFlag, _ := cmd.Flags().GetString("ledger")
			strike, _ := cmd.Flags().GetBool("strike")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			pickID := args[0]
			prdPath := filepath.Join(repo, "docs", "PRD.md")
			if _, serr := os.Stat(prdPath); serr != nil {
				fmt.Fprintf(os.Stderr, "backlog-check: no docs/PRD.md in %s\n", repo)
				setExitCode(2)
				return nil
			}
			prd, rerr := os.ReadFile(prdPath)
			if rerr != nil {
				return rerr
			}
			mergedTitles := pipeline.ListMergedPrTitles(repo)
			ledgerPath := ledgerFlag
			if ledgerPath == "" {
				ledgerPath = filepath.Join(repo, ".selfbuild", "ledger.jsonl")
			}
			ledgerGoals := orchestrator.ProductiveGoals(orchestrator.ReadLedgerLines(ledgerPath))
			check := pipeline.CheckBacklogPick(pickID, string(prd), mergedTitles, ledgerGoals)
			if strike && len(check.StruckIDs) > 0 {
				_ = os.WriteFile(prdPath, []byte(pipeline.StrikeBacklogItems(string(prd), check.StruckIDs)), 0o644)
				fmt.Printf("struck: %s\n", strings.Join(check.StruckIDs, " "))
			}
			fmt.Println(check.Message)
			// An unresolved check must not silently supersede the caller's
			// own guard: exit 2 tells the driver to fall back to the ledger
			// heuristic.
			switch {
			case check.OK && len(mergedTitles) == 0:
				setExitCode(2)
			case check.OK:
				setExitCode(0)
			case check.Shipped:
				setExitCode(1)
			default:
				setExitCode(2)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// reap-stale
// ---------------------------------------------------------------------------

func reapStaleCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "reap-stale",
		Short: "Find and kill stale opencode/claude/omp worker processes (infinite-retry reaper)",
		RunE: func(cmd *cobra.Command, args []string) error {
			olderThan, _ := cmd.Flags().GetInt("older-than")
			repo, _ := cmd.Flags().GetString("repo")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			if olderThan == 0 {
				olderThan = 600000
			}
			var opts *pipeline.ReapOptions
			if repo != "" {
				opts = &pipeline.ReapOptions{CWDPrefix: filepath.Join(repo, ".devagent-worktrees")}
			}
			if dryRun {
				stale := pipeline.FindStaleWorkerPids(olderThan, opts)
				if len(stale) == 0 {
					fmt.Println("No stale workers.")
					return nil
				}
				for _, s := range stale {
					fmt.Printf("%d %ds %s\n", s.Pid, int64(math.Round(float64(s.ElapsedMs)/1000)), s.Command)
				}
				return nil
			}
			killed := pipeline.ReapStaleWorkers(olderThan, false, opts)
			if len(killed) == 0 {
				fmt.Println("No stale workers.")
				return nil
			}
			for _, s := range killed {
				fmt.Printf("killed %d %ds %s\n", s.Pid, int64(math.Round(float64(s.ElapsedMs)/1000)), s.Command)
			}
			return nil
		},
	}
}

// ---------------------------------------------------------------------------
// worker dispatcher bridge (orchestrator <-> workers adapters)
// ---------------------------------------------------------------------------

// mapDispatchRequest maps an orchestrator.WorkerDispatchRequest onto the
// production worker adapter spawn options (workers.GetWorker), carrying the
// Q34 watchdog ledger identity and the resilience knobs the executor
// computed. Factored out of cliWorkerDispatcher so the mapping is testable
// without spawning anything.
func mapDispatchRequest(req orchestrator.WorkerDispatchRequest) (workers.WorkerAdapter, workers.WorkerSpawnOptions, error) {
	w, err := workers.GetWorker(string(req.WatchdogWorker))
	if err != nil {
		return nil, workers.WorkerSpawnOptions{}, err
	}
	opts := workers.WorkerSpawnOptions{
		Prompt: req.Prompt, Cwd: req.Cwd, TimeoutMs: req.TimeoutMs,
		Model: req.Model, Variant: req.Variant,
		ColdStartTimeoutMs: req.ColdStartTimeoutMs,
		WatchdogLedger: &workers.WatchdogLedgerContext{
			RepoPath: req.WatchdogRepoPath, TaskId: req.WatchdogTaskID,
			Attempt: req.Attempt, Worker: string(req.WatchdogWorker),
		},
	}
	if req.APIMaxAttempts > 0 {
		v := req.APIMaxAttempts
		opts.APIMaxAttempts = &v
	}
	if req.NoProgressTimeoutMs > 0 {
		v := req.NoProgressTimeoutMs
		opts.NoProgressTimeoutMs = &v
	}
	if req.Herdr {
		opts.Herdr = boolPtr(true)
	}
	return w, opts, nil
}

// cliWorkerDispatcher bridges orchestrator.WorkerDispatcher to the named
// worker adapter (mirrors the TS dynamic getWorker dispatch in executor.ts).
type cliWorkerDispatcher struct{}

func (cliWorkerDispatcher) Dispatch(req orchestrator.WorkerDispatchRequest) orchestrator.WorkerDispatchResult {
	w, opts, err := mapDispatchRequest(req)
	if err != nil {
		return orchestrator.WorkerDispatchResult{ExitCode: 1, ErrorText: err.Error()}
	}
	r := w.Spawn(opts)
	return orchestrator.WorkerDispatchResult{
		ExitCode: r.ExitCode, ResultText: r.ResultText, ErrorText: r.ErrorText,
		TimedOut: r.TimedOut, NoProgress: r.NoProgress, ColdStart: r.ColdStart,
	}
}
