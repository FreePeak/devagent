// implstage.go ports src/deps.ts implementStage (worker dispatch inside an
// isolated worktree with the FR-IMPL retry loop), src/workers/fanout.ts
// runFanout, src/resilience/proxy-state.ts recordTransientClass, and
// src/sessionguard/backoff.ts backoffDelay (FR-GO-07, issue #223 batch C).
//
// The full proxy-state port (probe recording, circuit breaker) belongs in
// internal/resilience; only the transient-record slice implementStage needs
// is mirrored here to keep this unit's file ownership disjoint.
//
// Byte-parity: log lines, error strings, and state JSON mirror the
// TypeScript originals exactly; tests pin them.

package pipeline

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/workers"
)

// impResolveSpawner resolves a worker name to its spawn function. A package
// variable so tests can inject a fake worker (the TS suite module-mocks
// getWorker); production always resolves the real registry adapter.
var impResolveSpawner = func(workerName WorkerName) (func(workers.WorkerSpawnOptions) workers.WorkerResult, error) {
	w, err := workers.GetWorker(workerName)
	if err != nil {
		return nil, err
	}
	return w.Spawn, nil
}

// ImplementStage mirrors implementStage (src/deps.ts:207): dispatch a worker
// inside an isolated worktree with the transient-vs-logic retry loop, then
// verify with the repo's own test suite (FR-IMPL-01..04).
func ImplementStage(cfg StageConfig, plan ImplementationPlan, log RunLog) (ImplementResult, error) {
	// Model preflight (Q32): reject an invalid model id BEFORE worktree
	// creation or any worker spend so a bad id fails at the gate in seconds
	// instead of burning attempts mid-board.
	var modelWorkers []WorkerName
	if cfg.Worker == "both" {
		modelWorkers = []WorkerName{"claude-code", "opencode"}
	} else {
		modelWorkers = []WorkerName{cfg.Worker}
	}
	for _, w := range modelWorkers {
		if modelProblem := config.ValidateWorkerModel(w, cfg.Model); modelProblem != "" {
			log.Error("implement", fmt.Sprintf("Dispatch preflight failed: %s", modelProblem), nil)
			resultWorker := w
			if cfg.Worker == "both" {
				resultWorker = "claude-code"
			}
			return ImplementResult{OK: false, Worker: resultWorker, Attempts: 0, FailureClass: "config"}, nil
		}
	}

	// herdr pane runtime: opt-in from config (`herdr.enabled`) or env override.
	herdrCfg, err := config.Load(cfg.RepoPath)
	if err != nil {
		return ImplementResult{}, err
	}
	useHerdr := config.HerdrEnabled(herdrCfg)

	if cfg.Worker == "both" {
		log.Info("implement", "Fan-out mode: dispatching both workers", nil)
		winner, err := impRunFanout(plan, []WorkerName{"claude-code", "opencode"}, log, impFanoutOptions{
			repoPath:        cfg.RepoPath,
			timeoutMs:       cfg.TimeoutMs,
			model:           cfg.Model,
			variant:         cfg.Variant,
			lessonsFile:     cfg.LessonsFile,
			lessonsMaxChars: impLessonsMaxChars(cfg.LessonsMaxChars),
			scoreLeg: func(worktreePath string, timeoutMs int) (*bool, error) {
				g, err := gates.RunTestGate(gates.SpawnRunner{}, worktreePath, timeoutMs)
				if err != nil {
					return nil, err
				}
				return &g.Passed, nil
			},
			spawn: func(workerName WorkerName, opts workers.WorkerSpawnOptions) workers.WorkerResult {
				w, err := impResolveSpawner(workerName)
				if err != nil {
					return workers.WorkerResult{ExitCode: 1, ErrorText: err.Error()}
				}
				return w(opts)
			},
			onSelected: func(usable []workers.FanoutLeg, winner workers.FanoutLeg) {
				impMergeAssistWinner(cfg.RepoPath, plan, usable, winner, log)
			},
		})
		if err != nil {
			return ImplementResult{}, err
		}
		if winner == nil {
			return ImplementResult{OK: false, Worker: "claude-code", Attempts: 1, FailureClass: "worker-error"}, nil
		}
		log.Info("implement", fmt.Sprintf("Fan-out winner: %s (tests %s)", winner.Worker, impPtrBoolJSON(winner.TestsPassed)), nil)
		return ImplementResult{OK: true, Worker: winner.Worker, WorktreePath: winner.WorktreePath, Attempts: 1}, nil
	}

	workerName := cfg.Worker
	spawn, err := impResolveSpawner(workerName)
	if err != nil {
		return ImplementResult{}, err
	}
	lessons := scout.LoadLessons(cfg.RepoPath, cfg.LessonsFile, impLessonsMaxChars(cfg.LessonsMaxChars))
	// Knowledge-context digest for the repair leg (FR-CTX-01): with
	// `context.kg` on, the real LeanKG client runs one 1s-budget call;
	// degraded modes omit the KG layer and land in the run log.
	var kgp *LeanKgProvider
	if cfg.Context != nil && cfg.Context.Kg == "leankg" {
		p := CreateLeanKgProvider(LeanKgClientOptions{
			LeanKgCallOptions: LeanKgCallOptions{
				RepoPath: cfg.RepoPath,
				Query:    fmt.Sprintf("%s %s", plan.Ticket.ID, plan.Ticket.Title),
			},
			Log:   log,
			Stage: ledger.StageImplement,
		})
		kgp = &p
	}
	var knowledgeOptions scout.KnowledgeOptions
	if cfg.LessonsMaxChars != nil {
		maxChars := int(*cfg.LessonsMaxChars)
		knowledgeOptions.MaxChars = &maxChars
	}
	if cfg.Context != nil {
		if cfg.Context.Kg != "" {
			knowledgeOptions.Kg = cfg.Context.Kg
		}
		if cfg.Context.AgentsMd != "" {
			knowledgeOptions.AgentsMd = cfg.Context.AgentsMd
		}
	}
	if kgp != nil {
		knowledgeOptions.KgProvider = kgp.Fn
	}
	knowledge := scout.BuildKnowledgeContext(cfg.RepoPath, knowledgeOptions)
	// Q28: the digest build is the run's only KG contact, so the evidence
	// persisted on merge is captured here from that same reply (never re-queried).
	var kgEvidence any
	if kgp != nil {
		kgEvidence = CaptureKgEvidence(*kgp)
	}
	prompt := scout.BuildImplementationPrompt(plan, lessons)

	maxAttempts := cfg.MaxLoops
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	// Isolated worktree + branch per run (FR-IMPL-01); aborts for git repos
	// instead of silently executing in repo root.
	cwd := cfg.RepoPath
	worktreePath := ""
	wt, wtErr := git.CreateWorktree(cfg.RepoPath, plan.Ticket.ID)
	if wtErr != nil {
		if git.IsGitRepository(cfg.RepoPath) {
			log.Error("implement", fmt.Sprintf("Aborting run: worktree creation failed: %s", wtErr.Error()), nil)
			return ImplementResult{}, fmt.Errorf("worktree creation failed: %s", wtErr.Error())
		}
		log.Warn("implement", fmt.Sprintf("Not a git repository, running in repo root: %s", wtErr.Error()), nil)
	} else {
		cwd = wt.WorktreePath
		worktreePath = wt.WorktreePath
		log.Info("implement", fmt.Sprintf("Worktree ready: %s (branch %s)", wt.WorktreePath, wt.Branch), nil)
	}

	resilienceCfg, err := config.Load(cfg.RepoPath)
	if err != nil {
		return ImplementResult{}, err
	}
	resCfg := config.ResilienceConfig{}
	if resilienceCfg.Resilience != nil {
		resCfg = *resilienceCfg.Resilience
	}
	impl, succeeded, err := impRunRetryLoop(impRetryEnv{
		workerName:     workerName,
		repoPath:       cfg.RepoPath,
		cwd:            cwd,
		worktreePath:   worktreePath,
		prompt:         prompt,
		lessons:        lessons,
		knowledge:      knowledge,
		maxAttempts:    maxAttempts,
		apiMaxAttempts: resCfg.APIMaxAttempts,
		noProgressMs:   impNoProgressMs(resCfg.NoProgressTimeoutMs),
		coldStartMs:    impColdStartMs(resCfg.ColdStartTimeoutMs),
		useHerdr:       useHerdr,
		kgEvidence:     kgEvidence,
		spawn:          spawn,
	}, plan, cfg, log)
	// Cleanup finally block: auto-cleanup removes a succeeded worktree, and
	// the Orca workspace drop is best-effort (never fails the run).
	impFinalizeWorktree(cfg, plan, worktreePath, succeeded, log)
	DropOrcaWorkspaceIfRequested(cfg, log)
	return impl, err
}

// impLessonsMaxChars maps the optional *float64 lessonsMaxChars to the int
// maxChars scout.LoadLessons takes (nil = default budget).
func impLessonsMaxChars(v *float64) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

// impNoProgressMs maps the optional *float64 noProgressTimeoutMs config to
// its effective value (TS `?? 10 * 60_000`).
func impNoProgressMs(v *float64) int {
	if v == nil {
		return 600000
	}
	return int(*v)
}

// impColdStartMs maps the optional *float64 coldStartTimeoutMs config to its
// effective value (TS `?? 90_000`).
func impColdStartMs(v *float64) int {
	if v == nil {
		return 90000
	}
	return int(*v)
}

// impPtrBoolJSON renders TS `winner.testsPassed` inside the fanout-winner
// log template: "true", "false", or "null" (nil).
func impPtrBoolJSON(v *bool) string {
	if v == nil {
		return "null"
	}
	if *v {
		return "true"
	}
	return "false"
}

// impMergeAssistWinner mirrors the fanout merge-assist closure: make the
// winning leg publishable through the normal pushBranch/createPr path.
// Every step is best-effort; a cleanup failure never fails the run.
func impMergeAssistWinner(repoPath string, plan ImplementationPlan, usable []workers.FanoutLeg, winner workers.FanoutLeg, log RunLog) {
	if winner.WorktreePath == "" {
		return
	}
	canonicalBranch := fmt.Sprintf("devagent/%s", plan.Ticket.ID)
	committed, err := git.CommitAllChanges(winner.WorktreePath, fmt.Sprintf("devagent(%s): fan-out winner (%s)", plan.Ticket.ID, winner.Worker))
	if err != nil {
		log.Warn("implement", fmt.Sprintf("Fan-out winner commit failed: %s", err.Error()), nil)
	} else if !committed {
		log.Info("implement", "Fan-out winner had nothing to commit", nil)
	}
	if err := git.RenameCurrentBranch(winner.WorktreePath, canonicalBranch); err != nil {
		log.Warn("implement", fmt.Sprintf("Fan-out winner branch rename failed: %s", err.Error()), nil)
	} else {
		log.Info("implement", fmt.Sprintf("Fan-out winner branch renamed to %s", canonicalBranch), nil)
	}
	for _, leg := range usable {
		if leg.Worker == winner.Worker || leg.WorktreePath == "" {
			continue
		}
		err := func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			git.RemoveWorktree(repoPath, filepath.Base(leg.WorktreePath))
			if leg.Branch != "" {
				git.DeleteBranch(repoPath, leg.Branch)
			}
			return nil
		}()
		if err != nil {
			log.Warn("implement", fmt.Sprintf("Fan-out loser cleanup failed (%s): %s", leg.Worker, err.Error()), nil)
		} else {
			log.Info("implement", fmt.Sprintf("Fan-out loser cleaned up: %s", leg.Worker), nil)
		}
	}
}

// impRetryEnv bundles everything the retry loop needs, including the test
// seams (spawn/testGate/backoff; nil seams = production defaults).
type impRetryEnv struct {
	workerName     WorkerName
	repoPath       string
	cwd            string
	worktreePath   string
	prompt         string
	lessons        string
	knowledge      string
	maxAttempts    int
	apiMaxAttempts *float64
	noProgressMs   int
	coldStartMs    int
	useHerdr       bool
	kgEvidence     any
	spawn          func(workers.WorkerSpawnOptions) workers.WorkerResult
	testGate       func(cwd string, timeoutMs int) (gates.GateResult, error)
	backoff        func(attempt int) int
}

// impRunRetryLoop mirrors the FR-IMPL retry loop (src/deps.ts:368-443):
// transient infra failures (watchdog timeouts, provider outages) never
// consume the logic retry budget; only bounded maxAttempts covers real
// implementation failures. Returns the result, whether the loop ended with
// a gate-passed attempt, and any error that aborts the stage (test-gate
// failure to launch propagates like the TS throw).
func impRunRetryLoop(env impRetryEnv, plan ImplementationPlan, cfg StageConfig, log RunLog) (ImplementResult, bool, error) {
	succeeded := false
	if env.spawn == nil {
		spawn, err := impResolveSpawner(env.workerName)
		if err != nil {
			return ImplementResult{}, false, err
		}
		env.spawn = spawn
	}
	testGate := env.testGate
	if testGate == nil {
		testGate = func(cwd string, timeoutMs int) (gates.GateResult, error) {
			return gates.RunTestGate(gates.SpawnRunner{}, cwd, timeoutMs)
		}
	}
	backoff := env.backoff
	if backoff == nil {
		backoff = impBackoffDelay
	}

	repairPrompt := env.prompt
	logicAttempts := 0
	lastFailureClass := ExecutorFailureClass("worker-error")
	infraRetries := 0
	const maxInfraBurst = 200 // safety cap even with Infinity
	for {
		infiniteMode := env.apiMaxAttempts == nil || math.IsInf(*env.apiMaxAttempts, 1)
		if !infiniteMode && logicAttempts >= env.maxAttempts {
			break
		}
		if infiniteMode && infraRetries >= maxInfraBurst {
			break
		}
		if infiniteMode && logicAttempts >= env.maxAttempts && infraRetries == 0 {
			break
		}
		displayAttempt := logicAttempts + 1
		attemptPrompt := env.prompt
		if logicAttempts != 0 {
			attemptPrompt = repairPrompt
		}
		maxDisp := fmt.Sprintf("%d", env.maxAttempts)
		if infiniteMode {
			maxDisp = "∞"
		}
		log.Info("implement", fmt.Sprintf("Worker %s attempt %d/%s", env.workerName, displayAttempt, maxDisp), nil)
		spawnOpts := workers.WorkerSpawnOptions{
			Prompt:    attemptPrompt,
			Cwd:       env.cwd,
			TimeoutMs: cfg.TimeoutMs,
		}
		if cfg.Model != "" {
			spawnOpts.Model = cfg.Model
		}
		if cfg.Variant != "" {
			spawnOpts.Variant = cfg.Variant
		}
		if env.apiMaxAttempts != nil && !math.IsInf(*env.apiMaxAttempts, 1) {
			v := int(*env.apiMaxAttempts)
			spawnOpts.APIMaxAttempts = &v
		}
		noProgress := env.noProgressMs
		spawnOpts.NoProgressTimeoutMs = &noProgress
		if env.coldStartMs != 0 {
			spawnOpts.ColdStartTimeoutMs = env.coldStartMs
		}
		if env.useHerdr {
			herdr := true
			spawnOpts.Herdr = &herdr
		}
		// Q34: ledger identity from the dispatcher; rows land under the main
		// repo, never the ephemeral worktree cwd.
		spawnOpts.WatchdogLedger = &workers.WatchdogLedgerContext{
			RepoPath: env.repoPath,
			TaskId:   plan.Ticket.ID,
			Attempt:  displayAttempt,
			Worker:   env.workerName,
		}
		result := env.spawn(spawnOpts)
		// Reap any stale provider workers that may be idling after a hung
		// call (observability must never break the retry).
		func() {
			defer func() { _ = recover() }()
			olderThan := env.noProgressMs
			if olderThan == 0 {
				olderThan = 60000
			}
			stale := FindStaleWorkerPids(olderThan, nil)
			for _, s := range stale {
				KillStaleProcessTree(s.Pid)
			}
		}()
		log.Info("implement", fmt.Sprintf("Attempt %d finished", displayAttempt), []ledger.KV{
			{Key: "exitCode", Value: result.ExitCode},
			{Key: "timedOut", Value: result.TimedOut},
			{Key: "durationMs", Value: result.DurationMs},
			{Key: "events", Value: len(result.Events)},
		})
		if result.TimedOut || result.ExitCode != 0 {
			if impIsInfraTransient(result) {
				infraRetries++
				func() {
					defer func() { _ = recover() }()
					text := result.ResultText
					if result.TimedOut {
						if text != "" {
							text += " watchdog timeout"
						} else {
							text = "watchdog timeout"
						}
					}
					impRecordTransientClass(env.repoPath, text)
				}()
				data := []ledger.KV{}
				if result.ResultText != "" {
					data = append(data, ledger.KV{Key: "resultText", Value: impRuneTruncate(result.ResultText, 120)})
				}
				log.Warn("implement", fmt.Sprintf("Transient infra failure, retrying (infra retry %d)", infraRetries), data)
				time.Sleep(time.Duration(backoff(infraRetries)) * time.Millisecond)
				continue
			}
			detail := result.ResultText
			if detail == "" {
				detail = fmt.Sprintf("worker exited %d", result.ExitCode)
			}
			repairPrompt = scout.BuildRepairPrompt(plan, logicAttempts+1, detail, env.lessons, env.knowledge)
			lastFailureClass = "worker-error"
			logicAttempts++
			continue
		}
		// Worker reports success: verify with the repo's own test suite before
		// accepting.
		g1, err := testGate(env.cwd, cfg.TimeoutMs)
		if err != nil {
			return ImplementResult{}, false, err
		}
		gateState := "failed"
		if g1.Passed {
			gateState = "passed"
		}
		data := []ledger.KV{}
		if g1.Detail != "" {
			data = append(data, ledger.KV{Key: "detail", Value: firstLine(g1.Detail)})
		}
		log.Info("implement", fmt.Sprintf("Attempt %d test gate: %s", displayAttempt, gateState), data)
		if g1.Passed {
			succeeded = true
			return ImplementResult{OK: true, Worker: env.workerName, Attempts: displayAttempt, WorktreePath: env.worktreePath, KgEvidence: env.kgEvidence}, true, nil
		}
		detail := g1.Detail
		if detail == "" {
			detail = "test suite failed"
		}
		repairPrompt = scout.BuildRepairPrompt(plan, logicAttempts+1, detail, env.lessons, env.knowledge)
		lastFailureClass = "test-gate"
		logicAttempts++
	}
	attempts := logicAttempts
	if attempts == 0 {
		attempts = 1
	}
	return ImplementResult{OK: false, Worker: env.workerName, Attempts: attempts, WorktreePath: env.worktreePath, FailureClass: lastFailureClass}, succeeded, nil
}

// impIsInfraTransient mirrors the isInfraTransient closure (Q31): a cold-start
// kill is transient infra alongside the watchdog timeout — a wedged CLI init
// is not the worker's fault.
func impIsInfraTransient(r workers.WorkerResult) bool {
	if r.TimedOut || r.ColdStart {
		return true
	}
	t := r.ResultText
	if t != "" && resilience.IsNonRetryableApiError(t) {
		return false
	}
	if t == "" {
		return false
	}
	return resilience.IsTransientProviderError(&t)
}

// impFinalizeWorktree mirrors the cleanup finally block: auto-cleanup snaps
// the winning tree onto a run branch and removes the worktree; keep and
// failed auto runs preserve the tree for inspection.
func impFinalizeWorktree(cfg StageConfig, plan ImplementationPlan, worktreePath string, succeeded bool, log RunLog) {
	if worktreePath == "" {
		return
	}
	mode := cfg.Cleanup
	if mode == "" {
		mode = "auto"
	}
	shouldRemove := mode == "always" || (mode == "auto" && succeeded)
	finMode := git.ModePreserve
	if shouldRemove {
		finMode = git.ModeRemove
	}
	fin := git.FinalizeRunWorktree(git.FinalizeWorktreeOptions{
		RepoPath:     cfg.RepoPath,
		WorktreePath: worktreePath,
		TicketID:     plan.Ticket.ID,
		Mode:         finMode,
	})
	if fin.Action == "removed" {
		var notes []string
		if fin.Committed {
			notes = append(notes, "changes snapshotted to run branch")
		}
		if fin.Pushed {
			notes = append(notes, "run branch pushed to origin")
		}
		detail := ""
		if len(notes) > 0 {
			detail = fmt.Sprintf(" (%s)", strings.Join(notes, ", "))
		}
		log.Info("implement", fmt.Sprintf("Auto-cleanup: worktree removed%s: %s", detail, worktreePath), nil)
	} else if fin.Error != "" {
		log.Warn("implement", fmt.Sprintf("Auto-cleanup failed, tree preserved (%s): %s", fin.Error, worktreePath), nil)
	} else {
		log.Info("implement", fmt.Sprintf("Worktree preserved for inspection: %s", worktreePath), nil)
	}
}

// impFanoutOptions mirrors TS FanoutOptions (src/workers/fanout.ts).
type impFanoutOptions struct {
	repoPath        string
	timeoutMs       int
	model           string
	variant         string
	lessonsFile     string
	lessonsMaxChars int
	// scoreLeg returns the gate verdict; nil *bool = could not score
	// (TS Promise<boolean | null>). A non-nil error propagates like the
	// TS throw.
	scoreLeg   func(worktreePath string, timeoutMs int) (*bool, error)
	onSelected func(legs []workers.FanoutLeg, winner workers.FanoutLeg)
	// spawn defaults to the worker registry; tests inject a fake.
	spawn func(workerName WorkerName, opts workers.WorkerSpawnOptions) workers.WorkerResult
}

// impRunFanout mirrors runFanout (src/workers/fanout.ts): dispatch one leg
// per worker (parallel, like Promise.all), rerun flaky tests once, and pick
// the winner by rank. Returns nil when no leg produced usable work.
func impRunFanout(plan ImplementationPlan, names []WorkerName, log RunLog, opts impFanoutOptions) (*workers.FanoutLeg, error) {
	prompt := scout.BuildImplementationPrompt(plan, scout.LoadLessons(opts.repoPath, opts.lessonsFile, opts.lessonsMaxChars))
	legs := make([]workers.FanoutLeg, len(names))
	errs := make([]error, len(names))
	var wg sync.WaitGroup
	for i, workerName := range names {
		wg.Add(1)
		go func(i int, workerName WorkerName) {
			defer wg.Done()
			leg, err := impRunFanoutLeg(plan, workerName, i, prompt, log, opts)
			legs[i], errs[i] = leg, err
		}(i, workerName)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	usable := workers.FilterUsableLegs(legs)
	if len(usable) == 0 {
		return nil, nil
	}
	winner := workers.SelectFanoutWinner(usable)
	if opts.onSelected != nil {
		opts.onSelected(usable, winner)
	}
	return &winner, nil
}

// impRunFanoutLeg is one Promise.all leg body.
func impRunFanoutLeg(plan ImplementationPlan, workerName WorkerName, index int, prompt string, log RunLog, opts impFanoutOptions) (workers.FanoutLeg, error) {
	spawn := opts.spawn
	if spawn == nil {
		w, err := impResolveSpawner(workerName)
		if err != nil {
			return workers.FanoutLeg{}, err
		}
		spawn = func(_ WorkerName, spawnOpts workers.WorkerSpawnOptions) workers.WorkerResult {
			return w(spawnOpts)
		}
	}
	cwd := opts.repoPath
	worktreePath := ""
	branch := ""
	wt, err := git.CreateWorktree(opts.repoPath, fmt.Sprintf("%s-%s", plan.Ticket.ID, workers.WorkerSuffix(workerName, index)))
	if err != nil {
		log.Warn("implement", fmt.Sprintf("Fanout %s: worktree failed (%s)", workerName, err.Error()), nil)
	} else {
		cwd = wt.WorktreePath
		worktreePath = wt.WorktreePath
		branch = wt.Branch
	}
	spawnOpts := workers.WorkerSpawnOptions{Prompt: prompt, Cwd: cwd, TimeoutMs: opts.timeoutMs}
	if opts.model != "" {
		spawnOpts.Model = opts.model
	}
	if opts.variant != "" {
		spawnOpts.Variant = opts.variant
	}
	result := spawn(workerName, spawnOpts)
	ok := !result.TimedOut && result.ExitCode == 0
	log.Info("implement", fmt.Sprintf("Fanout %s finished", workerName), []ledger.KV{
		{Key: "exitCode", Value: result.ExitCode},
		{Key: "timedOut", Value: result.TimedOut},
		{Key: "durationMs", Value: result.DurationMs},
	})
	// Flaky guard (PRD section 17 Phase 4): one rerun before condemning a
	// leg — nondeterministic suites must not discard otherwise good work.
	testsPassed, scored := workers.ScoreFanoutLeg(ok, worktreePath, func(wtPath string, timeoutMs int) *bool {
		if opts.scoreLeg == nil {
			return nil
		}
		v, err := opts.scoreLeg(wtPath, timeoutMs)
		if err != nil {
			return nil
		}
		return v
	})
	if scored && opts.scoreLeg != nil {
		if _, err := opts.scoreLeg(worktreePath, opts.timeoutMs); err != nil {
			return workers.FanoutLeg{}, err
		}
	}
	flaky := false
	if testsPassed != nil && !*testsPassed && worktreePath != "" && opts.scoreLeg != nil {
		rerun, err := opts.scoreLeg(worktreePath, opts.timeoutMs)
		if err != nil {
			return workers.FanoutLeg{}, err
		}
		testsPassed = rerun
		if rerun != nil && *rerun {
			flaky = true
			log.Warn("implement", fmt.Sprintf("Fanout %s: tests failed then passed on rerun (flaky)", workerName), nil)
		}
	}
	failureClass := ""
	if !ok {
		if result.TimedOut {
			failureClass = "timeout"
		} else {
			failureClass = "worker-error"
		}
	} else if testsPassed != nil && !*testsPassed {
		failureClass = "test-gate"
	}
	return workers.FanoutLeg{
		Worker:       workerName,
		WorktreePath: worktreePath,
		Branch:       branch,
		Ok:           ok,
		TestsPassed:  testsPassed,
		Flaky:        flaky,
		DurationMs:   0,
		FailureClass: failureClass,
	}, nil
}

// impBackoffDelay mirrors backoffDelay (src/sessionguard/backoff.ts) with
// its default options: base 2000ms, factor 2, cap 60000ms, ±25% jitter.
// Duplicated locally because internal/sessionguard is a sibling unit and the
// function is not part of the pinned cross-unit surface.
func impBackoffDelay(attempt int) int {
	const (
		baseDelayMs = 2000
		factor      = 2
		maxDelayMs  = 60000
	)
	if attempt < 1 {
		attempt = 1
	}
	raw := float64(baseDelayMs)
	for i := 1; i < attempt; i++ {
		raw *= factor
		if raw >= maxDelayMs {
			raw = maxDelayMs
			break
		}
	}
	if raw > maxDelayMs {
		raw = maxDelayMs
	}
	jitter := 1 + (rand.Float64()*0.5 - 0.25)
	return int(math.Max(0, math.Round(raw*jitter)))
}

// impProxyProbeRecord mirrors TS ProxyProbeRecord.
type impProxyProbeRecord struct {
	Ok     bool   `json:"ok"`
	At     string `json:"at"`
	Detail string `json:"detail,omitempty"`
}

// impTransientRecord mirrors TS TransientRecord.
type impTransientRecord struct {
	Class   string `json:"class"`
	At      string `json:"at"`
	Excerpt string `json:"excerpt"`
}

// impProxyState mirrors TS ProxyState.
type impProxyState struct {
	Circuit          string               `json:"circuit"`
	CircuitChangedAt string               `json:"circuitChangedAt"`
	LastProbe        *impProxyProbeRecord `json:"lastProbe,omitempty"`
	LastTransient    *impTransientRecord  `json:"lastTransient,omitempty"`
	UpdatedAt        string               `json:"updatedAt"`
}

// impProxyStatePath mirrors proxyStatePath (src/resilience/proxy-state.ts).
func impProxyStatePath(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "proxy-state.json")
}

// impReadProxyState mirrors readProxyState: nil when the state file is
// missing or corrupt (a corrupt file resets, never wedges).
func impReadProxyState(repoPath string) *impProxyState {
	raw, err := os.ReadFile(impProxyStatePath(repoPath))
	if err != nil {
		return nil
	}
	var state impProxyState
	if json.Unmarshal(raw, &state) != nil {
		return nil
	}
	return &state
}

// impNowIso mirrors nowIso: `new Date().toISOString()`.
func impNowIso() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

var impWhitespaceRe = regexp.MustCompile(`\s+`)

// impRecordTransientClass mirrors recordTransientClass (the transient-record
// slice of proxy-state.ts): on a transient provider error, log the coarse
// class in .devagent/proxy-state.json (never throws).
func impRecordTransientClass(repoPath, text string) *impTransientRecord {
	class := resilience.TransientErrorClass(&text)
	if class == "" {
		return nil
	}
	now := impNowIso()
	excerpt := strings.TrimSpace(impWhitespaceRe.ReplaceAllString(text, " "))
	if r := []rune(excerpt); len(r) > 200 {
		excerpt = string(r[:200])
	}
	record := &impTransientRecord{Class: class, At: now, Excerpt: excerpt}
	next := impProxyState{
		Circuit:          "closed",
		CircuitChangedAt: now,
		LastTransient:    record,
		UpdatedAt:        now,
	}
	if prev := impReadProxyState(repoPath); prev != nil {
		next.Circuit = prev.Circuit
		next.CircuitChangedAt = prev.CircuitChangedAt
		next.LastProbe = prev.LastProbe
	}
	raw, err := json.Marshal(next)
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(impProxyStatePath(repoPath)), 0o755)
		_ = os.WriteFile(impProxyStatePath(repoPath), raw, 0o644)
	}
	return record
}

// impRuneTruncate truncates to n runes (the repo-wide Go equivalent of the
// TS string slice).
func impRuneTruncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
