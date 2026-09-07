// Consume loop: Go port of src/consume.ts (the parts not already in
// internal/gates) plus the self-update hook of src/self-update.ts.
//
// One queued task per call: claim → synthetic ticket → pipeline → publish →
// optional auto-merge (STRIDE gate, per-branch regression oracle, merged-
// result oracle) → fenced queue writes. Transient infra failures requeue
// instead of failing terminally, with a repo-scoped stale-worker sweep.

package pipeline

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/integrations"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/lessons"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/sessionguard"
	"github.com/FreePeak/devagent/internal/spawn"
)

// ConsumeOptions mirrors the TS ConsumeOptions interface.
type ConsumeOptions struct {
	RepoPath  string `json:"repoPath"`
	AutoPr    bool   `json:"autoPr"`
	AutoMerge bool   `json:"autoMerge"`
	MaxLoops  int    `json:"maxLoops"`
	TimeoutMs int    `json:"timeoutMs"`
	// WorkerID is the claim identity for the queue file.
	WorkerID string `json:"workerId,omitempty"`
}

// ConsumeResult mirrors the TS ConsumeResult interface.
type ConsumeResult struct {
	OK     bool   `json:"ok"`
	TaskID string `json:"taskId,omitempty"`
	Detail string `json:"detail"`
	PRURL  string `json:"prUrl,omitempty"`
	Merged bool   `json:"merged,omitempty"`
}

// consumeTransientPattern mirrors the TS /timed.?out|watchdog|no-progress/i.
var consumeTransientPattern = regexp.MustCompile(`(?i)timed.?out|watchdog|no-progress`)

// IsConsumeTransient mirrors isConsumeTransient: watchdog / timeout wording
// counts as transient, plus the shared provider-error classifier.
func IsConsumeTransient(detail string) bool {
	if consumeTransientPattern.MatchString(detail) {
		return true
	}
	return resilience.IsTransientProviderError(&detail)
}

// KG evidence constants (PRD Q28), verbatim with the TS originals.
const (
	// kgEvidencePredictedImpact is the `predictedImpact` text for
	// machine-persisted KG evidence (guard gate 1).
	kgEvidencePredictedImpact = "cuts repeat KG re-queries on future runs: fresh structural evidence is already in the digest"
	// kgEvidenceLessonPrefix is the lessons-line prefix marking a
	// machine-persisted KG excerpt.
	kgEvidenceLessonPrefix = "KG digest evidence persisted on merge:"
)

// KgEvidenceRecordOptions mirrors the TS recordMergedKgEvidence opts.
type KgEvidenceRecordOptions struct {
	// Log is the run logger; nil = silent.
	Log RunLog
	// LessonsFile is a repo-relative override; "" = config.lessonsFile or
	// the .devagent default.
	LessonsFile string
	// Threshold overrides the dedupe similarity; nil = config value or the
	// guard default.
	Threshold *float64
}

// RecordMergedKgEvidence mirrors recordMergedKgEvidence (PRD Q28): persist
// the merged run's verbatim KG provenance excerpt into the lessons digest,
// freshness-gated per FR-CTX-05. Only a `fresh` stamp lands; the write goes
// through the eval guard with the held-out must-beat tier off (the
// candidate is machine-captured evidence, not a proposal). Returns the
// guard's verdict, or nil when the freshness gate omitted the append. A
// config-load error surfaces like the TS throw; the guard itself never
// fails a merge that already landed.
func RecordMergedKgEvidence(repoPath string, evidence *KgEvidence, opts *KgEvidenceRecordOptions) (*lessons.LessonsDedupeResult, error) {
	if opts == nil {
		opts = &KgEvidenceRecordOptions{}
	}
	if !IsFreshKgEvidence(evidence) {
		return nil, nil
	}
	cfg, err := config.Load(repoPath)
	if err != nil {
		return nil, err
	}
	lessonsFile := opts.LessonsFile
	if lessonsFile == "" {
		lessonsFile = cfg.LessonsFile
	}
	if lessonsFile == "" {
		lessonsFile = lessons.LessonsPath
	}
	threshold := opts.Threshold
	if threshold == nil && cfg.LessonsDedupeSimilarity != nil {
		threshold = cfg.LessonsDedupeSimilarity
	}
	// The TS wraps the append in try/catch ("never throws"): a panic from
	// the guard degrades to the warn path instead of failing the merge.
	var result *lessons.LessonsDedupeResult
	func() {
		defer func() {
			if r := recover(); r != nil {
				if opts.Log != nil {
					opts.Log.Warn(ledger.StageConsume,
						fmt.Sprintf("KG lesson evidence append failed: %v", r), nil)
				}
				result = nil
			}
		}()
		mustBeat := false
		entry := fmt.Sprintf("%s %s", kgEvidenceLessonPrefix, evidence.Excerpt)
		out := lessons.AppendLessonGuarded(repoPath, entry, &lessons.AppendLessonGuardedOpts{
			LessonsFile:     lessonsFile,
			Threshold:       threshold,
			PredictedImpact: kgEvidencePredictedImpact,
			SuiteTimeoutMs:  lessons.DefaultLessonsSuiteTimeoutMs,
			MustBeat:        &mustBeat,
		})
		result = &out
	}()
	if result == nil {
		return nil, nil
	}
	if opts.Log != nil {
		opts.Log.Info(ledger.StageConsume,
			fmt.Sprintf("KG lesson evidence %s", result.Reason),
			[]ledger.KV{
				{Key: "freshness", Value: evidence.Freshness},
				{Key: "lessonsFile", Value: lessonsFile},
				{Key: "similarity", Value: result.Similarity},
				{Key: "suite", Value: result.Suite},
			})
	}
	return result, nil
}

// RegressionOracleResult is the shared oracle shape (per-branch and
// merged-result oracles return the same object in the TS).
type RegressionOracleResult = gates.RegressionOracleResult

// MergedResultOracleOptions mirrors the TS runMergedResultOracle opts plus
// the test seams.
type MergedResultOracleOptions struct {
	TimeoutMs int
	// Enabled overrides the orchestrate.regressionOracle knob when non-nil.
	Enabled *bool
	// Log mirrors the TS RunLogger: level is "info" or "warn". Nil = no-op.
	Log func(level, message string, extra map[string]any)
	// ListPrs is the injectable board enumeration (defaults to
	// orchestrator.ListOpenPrs over DefaultRunGh); for tests.
	ListPrs func() ([]orchestrator.PrStatus, error)
}

// defaultListOpenPrs is the board-enumeration seam (tests override).
var defaultListOpenPrs = func(repoPath string) ([]orchestrator.PrStatus, error) {
	return orchestrator.ListOpenPrs(repoPath, orchestrator.DefaultRunGh)
}

// cnsRandToken6 mirrors the TS Math.random().toString(36).slice(2, 8):
// six lowercase base-36 characters for staging-path uniqueness.
func cnsRandToken6() string {
	const chars = "0123456789abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 6)
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = chars[(uint64(seed)>>33)%uint64(len(chars))]
	}
	return string(b)
}

// cnsTailLines mirrors the TS excerpt builder: `${stdout}${stderr}`
// trimEnd, split on newlines, last 15 lines joined.
func cnsTailLines(parts ...string) string {
	joined := strings.Join(parts, "")
	lines := strings.Split(strings.TrimRightFunc(joined, unicode.IsSpace), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}

// RunMergedResultOracle mirrors runMergedResultOracle (PRD §17 Phase 4,
// curation run 24): the per-branch regression oracle judges each PR in
// isolation, so a board of individually-green PRs can still merge to a red
// result. Before an auto-merge, rebuild the whole board in a throwaway
// worktree — check out the candidate's base, merge every open devagent PR
// head sharing that base (including the candidate), then run the repo's
// full suite on the merged tree. A merge conflict or a red suite blocks the
// merge (the PR survives); a skipped gate (no runnable command / knob off /
// failed worktree or install) lets auto-merge proceed. The worktree is
// always removed.
func RunMergedResultOracle(repoPath, branch string, opts MergedResultOracleOptions) (RegressionOracleResult, error) {
	lg := func(level, message string, extra map[string]any) {
		if opts.Log != nil {
			opts.Log(level, message, extra)
		}
	}
	enabled := opts.Enabled
	if enabled == nil {
		if cfg, err := config.Load(repoPath); err == nil && cfg.Orchestrate != nil && cfg.Orchestrate.RegressionOracle != nil {
			enabled = cfg.Orchestrate.RegressionOracle
		}
	}
	if enabled != nil && !*enabled {
		return RegressionOracleResult{Passed: true, Skipped: true, Reason: gates.ReasonDisabled}, nil
	}

	// Enumerate the board: every open PR sharing the candidate's base, plus
	// the candidate itself. A failed enumeration (gh unavailable) degrades
	// to a candidate-only board on the default base rather than blocking the
	// merge.
	base := "main"
	heads := []string{branch}
	listPrs := opts.ListPrs
	if listPrs == nil {
		listPrs = func() ([]orchestrator.PrStatus, error) { return defaultListOpenPrs(repoPath) }
	}
	prs, err := listPrs()
	if err != nil {
		lg("warn", "merged-result oracle PR enumeration failed; falling back to candidate-only", map[string]any{
			"gate":  "merged-regression",
			"error": err.Error(),
		})
	} else {
		for _, p := range prs {
			if p.HeadRefName == branch && p.BaseRefName != "" {
				base = p.BaseRefName
				break
			}
		}
		for _, p := range prs {
			if p.BaseRefName == base && p.HeadRefName != "" && !containsStr(heads, p.HeadRefName) {
				heads = append(heads, p.HeadRefName)
			}
		}
	}

	worktreesRoot := filepath.Join(repoPath, ".devagent-worktrees")
	if err := os.MkdirAll(worktreesRoot, 0o755); err != nil {
		return RegressionOracleResult{}, err
	}
	staging := filepath.Join(worktreesRoot, fmt.Sprintf("merged-%d-%s", time.Now().UnixMilli(), cnsRandToken6()))
	add := spawn.RunCli("git", []string{"worktree", "add", "--detach", staging, base},
		spawn.Options{Dir: repoPath, TimeoutMs: 60_000})
	if add.ExitCode != 0 {
		lg("warn", "merged-result oracle worktree add failed; skipping gate", map[string]any{
			"gate":   "merged-regression",
			"base":   base,
			"stderr": cnsTruncateRunes(add.Stderr, 200),
		})
		return RegressionOracleResult{Passed: true, Skipped: true, Reason: gates.ReasonWorktreeFailed}, nil
	}
	// The worktree is always removed.
	defer func() {
		remove := spawn.RunCli("git", []string{"worktree", "remove", "--force", staging},
			spawn.Options{Dir: repoPath, TimeoutMs: 60_000})
		if remove.ExitCode != 0 {
			lg("warn", "merged-result oracle worktree remove failed", map[string]any{
				"gate":   "merged-regression",
				"path":   staging,
				"stderr": cnsTruncateRunes(remove.Stderr, 200),
			})
		}
	}()

	// Merge every board head onto the base. A conflict aborts the merge and
	// blocks auto-merge (git reports conflicts on stdout, errors on stderr).
	for _, head := range heads {
		m := spawn.RunCli("git", []string{"merge", "--no-edit", head}, spawn.Options{Dir: staging, TimeoutMs: 60_000})
		if m.ExitCode != 0 {
			_ = spawn.RunCli("git", []string{"merge", "--abort"}, spawn.Options{Dir: staging, TimeoutMs: 60_000})
			lg("warn", "merged-result oracle blocked merge: conflict", map[string]any{
				"gate": "merged-regression",
				"head": head,
			})
			return RegressionOracleResult{
				Passed:  false,
				Skipped: false,
				Reason:  gates.ReasonMergeConflict,
				Excerpt: cnsTailLines(m.Stdout, m.Stderr),
			}, nil
		}
	}

	testCommand, err := gates.DetectTestCommand(staging)
	if err != nil {
		return RegressionOracleResult{}, err
	}
	if testCommand == nil {
		return RegressionOracleResult{Passed: true, Skipped: true, Reason: gates.ReasonNoTestCommand}, nil
	}
	// Fresh worktrees lack node_modules; npm suites with a lockfile get an
	// install first, and a failed install skips the gate (fail-open).
	if testCommand.Cmd == "npm" {
		hasLockfile := false
		for _, f := range []string{"package-lock.json", "npm-shrinkwrap.json"} {
			if _, err := os.Stat(filepath.Join(staging, f)); err == nil {
				hasLockfile = true
				break
			}
		}
		if hasLockfile {
			install := spawn.RunCli("npm", []string{"ci", "--ignore-scripts"},
				spawn.Options{Dir: staging, TimeoutMs: opts.TimeoutMs})
			if install.ExitCode != 0 {
				lg("warn", "regression oracle skipped: dependency install failed", map[string]any{
					"gate":   "regression",
					"stderr": cnsTruncateRunes(install.Stderr, 200),
				})
				return RegressionOracleResult{Passed: true, Skipped: true, Reason: gates.ReasonInstallFailed}, nil
			}
		}
	}
	run := spawn.RunCli(testCommand.Cmd, testCommand.Args, spawn.Options{Dir: staging, TimeoutMs: opts.TimeoutMs})
	if run.ExitCode == 0 {
		lg("info", "merged-result oracle passed", map[string]any{
			"gate":  "merged-regression",
			"heads": heads,
		})
		return RegressionOracleResult{Passed: true, Skipped: false}, nil
	}
	lg("warn", "merged-result oracle blocked merge: suite failed on merged result", map[string]any{
		"gate":     "merged-regression",
		"exitCode": run.ExitCode,
	})
	return RegressionOracleResult{
		Passed:  false,
		Skipped: false,
		Excerpt: cnsTailLines(run.Stdout, run.Stderr),
	}, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Test seams (tests override; nil falls back to the production default).
var (
	// cnsBuildDeps is the PipelineDeps constructor (pinned:
	// pipeline.BuildDeps from the implstage port). Tests inject fake deps.
	cnsBuildDeps func(creds config.Credentials, cfg StageConfig, log RunLog) PipelineDeps
	// cnsNewRunLog is the run-logger constructor. Tests inject a recorder.
	cnsNewRunLog func() RunLog
	// cnsSleep is the backoff wait. Tests no-op it.
	cnsSleep = func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	// cnsAutoMergePr is the auto-merge step. Tests fake it (no gh).
	cnsAutoMergePr = func(repoPath, prURL string) error {
		_, err := integrations.AutoMergePr(repoPath, prURL, "squash", integrations.GitHubOptions{})
		return err
	}
	// cnsSelfUpdateRunner is the self-update CLI runner seam; nil =
	// spawn.RunCli.
	cnsSelfUpdateRunner func(cmd string, args []string, opts spawn.Options) spawn.Result
)

// cnsBuildDepsDefault wires the pinned BuildDeps symbol.
func cnsBuildDepsDefault(creds config.Credentials, cfg StageConfig, log RunLog) PipelineDeps {
	return BuildDeps(creds, cfg, log)
}

// cnsNewRunLogDefault mirrors the TS `new RunLogger()`.
func cnsNewRunLogDefault() RunLog {
	lg, err := ledger.NewRunLogger("")
	if err != nil {
		return cnsNoopRunLog{}
	}
	return lg
}

// cnsNoopRunLog is the silent RunLog fallback.
type cnsNoopRunLog struct{}

func (cnsNoopRunLog) Info(ledger.RunStage, string, []ledger.KV)  {}
func (cnsNoopRunLog) Warn(ledger.RunStage, string, []ledger.KV)  {}
func (cnsNoopRunLog) Error(ledger.RunStage, string, []ledger.KV) {}

// queuedRunResult mirrors the TS runQueuedTask return shape.
type queuedRunResult struct {
	OK     bool
	Detail string
	PRURL  string
	Merged bool
}

// ConsumeOnce mirrors consumeOnce: claim one pending task and run it
// through the pipeline -> PR -> optional auto-merge. Like the TS, every
// failure is folded into the returned result; the error return stays nil
// except where the TS would reject outright (e.g. an unreadable config in
// the failure paths).
func ConsumeOnce(opts ConsumeOptions) (ConsumeResult, error) {
	workerID := opts.WorkerID
	if workerID == "" {
		workerID = fmt.Sprintf("consume-%d", os.Getpid())
	}
	task := queue.ClaimNextPending(opts.RepoPath, workerID, nil)
	if task == nil {
		return ConsumeResult{OK: true, Detail: "no pending tasks"}, nil
	}

	newRunLog := cnsNewRunLog
	if newRunLog == nil {
		newRunLog = cnsNewRunLogDefault
	}
	log := newRunLog()
	log.Info(ledger.StageConsume, fmt.Sprintf("Claimed %s: %s", task.ID, task.Title),
		[]ledger.KV{{Key: "workerId", Value: workerID}})
	// Fencing token issued by the claim (FR-VIS-09 queue claims): every
	// write below carries it, so a lease reclaimed by another worker
	// mid-run makes our own completion/failure writes refuse instead of
	// clobbering the new owner.
	generation := int64(0)
	if task.LeaseGeneration != nil {
		generation = int64(*task.LeaseGeneration)
	}

	result, err := runQueuedTask(task, opts, log)
	if err != nil {
		return cnsConsumeCrash(task, opts, generation, err.Error(), log)
	}
	if result.OK {
		if queue.CompleteTask(opts.RepoPath, task.ID, generation, nil) == nil {
			log.Warn(ledger.StageConsume,
				fmt.Sprintf("Completion of %s refused — lease moved on (generation %d)", task.ID, generation), nil)
		}
		return ConsumeResult{OK: true, TaskID: task.ID, Detail: result.Detail, PRURL: result.PRURL, Merged: result.Merged}, nil
	}

	// Infinite retry for transient infra failures: requeue as pending
	// instead of terminal failed so the next consume loop retries after
	// backoff. Bounded failures (test gate, lint) go to failed as before.
	cfg, cfgErr := config.Load(opts.RepoPath)
	if cfgErr != nil {
		// The TS loadConfig throw lands in the catch block below.
		return cnsConsumeCrash(task, opts, generation, cfgErr.Error(), log)
	}
	if cnsInfiniteRetries(cfg) && IsConsumeTransient(result.Detail) {
		cnsSweepStaleWorkers(opts.RepoPath, cfg, task)
		// Backoff before requeueing so we don't hot-loop a flapping
		// endpoint.
		attempts := 0
		if task.Attempts != nil {
			attempts = *task.Attempts
		}
		cnsSleep(sessionguard.BackoffDelay(attempts+1, sessionguard.DEFAULT_BACKOFF, nil))
		if queue.RequeueTask(opts.RepoPath, task.ID, generation, result.Detail, nil) == nil {
			log.Warn(ledger.StageConsume,
				fmt.Sprintf("Requeue of %s refused — lease moved on (generation %d)", task.ID, generation), nil)
		}
		log.Warn(ledger.StageConsume,
			fmt.Sprintf("Transient infra failure for %s, requeued as pending", task.ID),
			[]ledger.KV{{Key: "detail", Value: cnsTruncateRunes(result.Detail, 120)}})
		return ConsumeResult{
			OK:     false,
			TaskID: task.ID,
			Detail: result.Detail + " (transient — requeued)",
			PRURL:  result.PRURL,
			Merged: result.Merged,
		}, nil
	}
	if queue.FailTask(opts.RepoPath, task.ID, generation, result.Detail, nil) == nil {
		log.Warn(ledger.StageConsume,
			fmt.Sprintf("Failure record for %s refused — lease moved on (generation %d)", task.ID, generation), nil)
	}
	return ConsumeResult{OK: false, TaskID: task.ID, Detail: result.Detail, PRURL: result.PRURL, Merged: result.Merged}, nil
}

// cnsInfiniteRetries mirrors the TS resilience.apiMaxAttempts undefined /
// Infinity check.
func cnsInfiniteRetries(cfg config.Config) bool {
	return cfg.Resilience == nil || cfg.Resilience.APIMaxAttempts == nil || math.IsInf(*cfg.Resilience.APIMaxAttempts, 1)
}

// cnsSweepStaleWorkers mirrors the inline reaper sweep: reap only stale
// workers scoped to this repo's worktrees — never interactive sessions or
// workers for other projects.
func cnsSweepStaleWorkers(repoPath string, cfg config.Config, task *queue.QueuedTask) {
	staleMs := 10 * 60_000
	if cfg.Resilience != nil && cfg.Resilience.NoProgressTimeoutMs != nil {
		staleMs = int(*cfg.Resilience.NoProgressTimeoutMs)
	}
	stale := FindStaleWorkerPids(staleMs, &ReapOptions{CWDPrefix: filepath.Join(repoPath, ".devagent-worktrees")})
	for _, s := range stale {
		if strings.Contains(s.Command, task.ID) {
			reapKillTree(s.Pid)
		}
	}
}

// cnsConsumeCrash mirrors the TS catch block of consumeOnce.
func cnsConsumeCrash(task *queue.QueuedTask, opts ConsumeOptions, generation int64, msg string, log RunLog) (ConsumeResult, error) {
	cfg, cfgErr := config.Load(opts.RepoPath)
	if cfgErr != nil {
		// The TS catch block's own loadConfig throw escapes consumeOnce.
		return ConsumeResult{}, cfgErr
	}
	if cnsInfiniteRetries(cfg) && IsConsumeTransient(msg) {
		// No hard 60_000 threshold — respect configured noProgress timeout.
		cnsSweepStaleWorkers(opts.RepoPath, cfg, task)
		cnsSleep(sessionguard.BackoffDelay(1, sessionguard.DEFAULT_BACKOFF, nil))
		if queue.RequeueTask(opts.RepoPath, task.ID, generation, msg, nil) == nil {
			log.Warn(ledger.StageConsume,
				fmt.Sprintf("Requeue of %s refused — lease moved on (generation %d)", task.ID, generation), nil)
		}
		return ConsumeResult{OK: false, TaskID: task.ID,
			Detail: fmt.Sprintf("crashed: %s (transient — requeued)", msg)}, nil
	}
	if queue.FailTask(opts.RepoPath, task.ID, generation, msg, nil) == nil {
		log.Warn(ledger.StageConsume,
			fmt.Sprintf("Failure record for %s refused — lease moved on (generation %d)", task.ID, generation), nil)
	}
	log.Error(ledger.StageConsume, fmt.Sprintf("Task %s crashed: %s", task.ID, msg), nil)
	return ConsumeResult{OK: false, TaskID: task.ID, Detail: fmt.Sprintf("crashed: %s", msg)}, nil
}

// runQueuedTask mirrors the TS runQueuedTask: synthetic ticket from the
// queued task, pipeline run, then the auto-merge gate ladder. Errors
// surface to the consumeOnce crash path like the TS throw.
func runQueuedTask(task *queue.QueuedTask, opts ConsumeOptions, log RunLog) (queuedRunResult, error) {
	description := task.Goal
	if task.Description != nil && *task.Description != "" {
		description += "\n\n" + *task.Description
	}
	ticket := TicketSpec{
		ID:                 task.ID,
		Title:              task.Title,
		Description:        description,
		Labels:             []string{},
		AcceptanceCriteria: task.AcceptanceCriteria,
		URL:                "",
		TrackerInternalID:  task.ID,
	}

	cfg, err := config.Load(opts.RepoPath)
	if err != nil {
		return queuedRunResult{}, err
	}
	cleanup := cfg.Cleanup
	if cleanup == "" {
		cleanup = "auto"
	}
	dropOrca := cfg.DropOrcaWorkspace != nil && *cfg.DropOrcaWorkspace
	runCfg := RunConfig{
		TicketID:          task.ID,
		RepoPath:          opts.RepoPath,
		Worker:            cfg.Worker,
		AutoPr:            opts.AutoPr,
		Interactive:       false,
		MaxLoops:          opts.MaxLoops,
		TimeoutMs:         opts.TimeoutMs,
		DryRun:            false,
		Cleanup:           CleanupMode(cleanup),
		DropOrcaWorkspace: dropOrca,
	}

	// Use buildDeps-equivalent but with fetchTicket stubbed to the queued
	// ticket (no tracker fetch).
	buildDeps := cnsBuildDeps
	if buildDeps == nil {
		buildDeps = cnsBuildDepsDefault
	}
	deps := buildDeps(config.LoadCredentials(), StageConfig{
		RepoPath:          opts.RepoPath,
		MaxLoops:          opts.MaxLoops,
		TimeoutMs:         opts.TimeoutMs,
		Worker:            cfg.Worker,
		AutoPr:            opts.AutoPr,
		Cleanup:           CleanupMode(cleanup),
		DropOrcaWorkspace: dropOrca,
	}, log)
	deps.FetchTicket = func(string) (TicketSpec, error) { return ticket, nil }

	outcomes, err := RunPipeline(runCfg, deps, log)
	if err != nil {
		return queuedRunResult{}, err
	}

	var failed, publish, implement *StageOutcome
	for i := range outcomes {
		switch outcomes[i].Stage {
		case "failed":
			if failed == nil {
				failed = &outcomes[i]
			}
		case "publish":
			if publish == nil {
				publish = &outcomes[i]
			}
		case "implement":
			if implement == nil {
				implement = &outcomes[i]
			}
		}
	}
	if failed != nil {
		return queuedRunResult{OK: false, Detail: failed.Reason}, nil
	}

	prURL := ""
	if publish != nil {
		prURL = publish.PRURL
	}

	if opts.AutoMerge && prURL != "" {
		// G5 STRIDE gate (PRD section 11): static review of the branch diff
		// before auto-merge. HIGH/CRITICAL findings block the merge;
		// MEDIUM/LOW are advisory. A missing/failed diff is treated as an
		// empty diff (pass).
		diff := ""
		var kgEvidence *KgEvidence
		branch := ""
		if implement != nil {
			branch = implement.Branch
			switch ev := implement.KgEvidence.(type) {
			case *KgEvidence:
				kgEvidence = ev
			case KgEvidence:
				k := ev
				kgEvidence = &k
			}
		}
		if branch == "" {
			branch = parsePrBranch(prURL)
		}
		if branch != "" {
			for _, base := range []string{"main", "origin/main"} {
				res := spawn.RunCli("git", []string{"diff", base + "..." + branch},
					spawn.Options{Dir: opts.RepoPath, TimeoutMs: 60_000})
				if res.ExitCode == 0 && strings.TrimSpace(res.Stdout) != "" {
					diff = res.Stdout
					break
				}
			}
		}

		// Per-path allowlist (PRD Q25): the PR may commit
		// .devagent/stride-allowlist.json so findings in fixture/test files
		// do not stall autoMerge. The file is read from the PR branch itself
		// (so it is reviewable in the diff), never from the base; an absent,
		// unreadable, or malformed allowlist fails closed (no suppression).
		var allowlistPaths []string
		if branch != "" {
			res := spawn.RunCli("git", []string{"show", branch + ":" + gates.StrideAllowlistPath},
				spawn.Options{Dir: opts.RepoPath, TimeoutMs: 15_000})
			if res.ExitCode == 0 {
				parsed, ok := gates.ParseStrideAllowlist(res.Stdout)
				if ok {
					allowlistPaths = parsed
				} else {
					log.Warn(ledger.StageValidate,
						fmt.Sprintf("G5 STRIDE allowlist ignored (malformed): %s", gates.StrideAllowlistPath), nil)
				}
			}
		}

		evaluation := gates.EvaluateStride(gates.StrideInput{Diff: diff, AllowlistPaths: allowlistPaths})
		blocked := false
		for _, f := range evaluation.Findings {
			if f.Severity == "HIGH" || f.Severity == "CRITICAL" {
				blocked = true
				break
			}
		}
		state := "passed"
		if blocked {
			state = "blocked"
		}
		findings := make([]cnsStrideFindingKV, 0, len(evaluation.Findings))
		for _, f := range evaluation.Findings {
			findings = append(findings, cnsStrideFindingKV{
				Category: f.Category, Severity: f.Severity, File: f.File, Line: f.Line,
			})
		}
		allowlistKV := allowlistPaths
		if allowlistKV == nil {
			allowlistKV = []string{}
		}
		log.Info(ledger.StageValidate, fmt.Sprintf("G5 STRIDE gate %s", state), []ledger.KV{
			{Key: "gate", Value: "stride"},
			{Key: "severityMax", Value: evaluation.SeverityMax},
			{Key: "allowlist", Value: allowlistKV},
			{Key: "findings", Value: findings},
		})

		if blocked {
			return queuedRunResult{
				OK: true,
				Detail: fmt.Sprintf("done: %s -> %s (stride gate blocked merge: %s findings: %d)",
					task.ID, prURL, evaluation.SeverityMax, len(evaluation.Findings)),
				PRURL:  prURL,
				Merged: false,
			}, nil
		}

		if branch != "" {
			// Regression oracle (PRD §17 Phase 4): full test suite on the PR
			// branch in a throwaway worktree. A red suite blocks the merge;
			// skipped gates (no test command / knob off) proceed to
			// auto-merge.
			regression, err := gates.RunRegressionOracle(opts.RepoPath, branch, gates.RegressionOracleOptions{
				TimeoutMs: opts.TimeoutMs,
				Log:       cnsGateLog(log),
			})
			if err != nil {
				return queuedRunResult{}, err
			}
			if !regression.Passed {
				excerpt := ""
				if regression.Excerpt != "" {
					excerpt = ": " + regression.Excerpt
				}
				return queuedRunResult{
					OK:     true,
					Detail: fmt.Sprintf("done: %s -> %s (regression-failed%s)", task.ID, prURL, excerpt),
					PRURL:  prURL,
					Merged: false,
				}, nil
			}
		}

		if branch != "" {
			// Merged-result oracle (PRD §17 Phase 4, curation run 24): the
			// per-branch gate above only sees this PR alone. Rebuild the
			// whole board (base + every open PR head) in a throwaway
			// worktree and run the suite there; a merge conflict or a red
			// merged suite blocks auto-merge (the PR survives).
			merged, err := RunMergedResultOracle(opts.RepoPath, branch, MergedResultOracleOptions{
				TimeoutMs: opts.TimeoutMs,
				Log:       cnsGateLog(log),
			})
			if err != nil {
				return queuedRunResult{}, err
			}
			if !merged.Passed {
				kind := "merged-regression-failed"
				if merged.Reason == gates.ReasonMergeConflict {
					kind = "merge-conflict"
				}
				excerpt := ""
				if merged.Excerpt != "" {
					excerpt = ": " + merged.Excerpt
				}
				return queuedRunResult{
					OK:     true,
					Detail: fmt.Sprintf("done: %s -> %s (%s%s)", task.ID, prURL, kind, excerpt),
					PRURL:  prURL,
					Merged: false,
				}, nil
			}
		}

		mergeErr := cnsAutoMergePr(opts.RepoPath, prURL)
		if mergeErr == nil {
			// PRD Q28: the merge is the moment the run's structural evidence
			// becomes durable. Freshness-gated inside, so a stale reply is
			// never persisted.
			if _, kgErr := RecordMergedKgEvidence(opts.RepoPath, kgEvidence, &KgEvidenceRecordOptions{Log: log}); kgErr != nil {
				mergeErr = kgErr
			}
		}
		if mergeErr == nil {
			return queuedRunResult{
				OK:     true,
				Detail: fmt.Sprintf("done: %s -> %s (auto-merged)", task.ID, prURL),
				PRURL:  prURL,
				Merged: true,
			}, nil
		}
		return queuedRunResult{
			OK:     true,
			Detail: fmt.Sprintf("done: %s -> %s (auto-merge failed: %s)", task.ID, prURL, mergeErr.Error()),
			PRURL:  prURL,
			Merged: false,
		}, nil
	}

	// Self-update hook: after green merge, optionally pull+build
	if prURL != "" {
		if cfg.SelfUpdate != nil && *cfg.SelfUpdate {
			if err := cnsRunSelfUpdate(opts.RepoPath, log); err != nil {
				log.Warn(ledger.StageConsume, fmt.Sprintf("self-update skipped: %s", err.Error()), nil)
			}
		}
	}

	if prURL != "" {
		return queuedRunResult{OK: true, Detail: fmt.Sprintf("done: %s -> %s", task.ID, prURL), PRURL: prURL}, nil
	}
	if implement != nil && !implement.OK {
		return queuedRunResult{OK: false, Detail: fmt.Sprintf("implementation failed for %s", task.ID)}, nil
	}
	return queuedRunResult{OK: true, Detail: fmt.Sprintf("done: %s (no PR: missing GITHUB_TOKEN or branch)", task.ID)}, nil
}

// cnsStrideFindingKV renders one G5 finding in the TS data-object shape
// (category, severity, file, line).
type cnsStrideFindingKV struct {
	Category string `json:"category"`
	Severity string `json:"severity"`
	File     string `json:"file"`
	Line     *int   `json:"line"`
}

// cnsGateLog adapts the RunLog to the gates oracle log seam (stage
// "validate", level "info"/"warn").
func cnsGateLog(log RunLog) func(level, message string, extra map[string]any) {
	return func(level, message string, extra map[string]any) {
		if log == nil {
			return
		}
		kvs := cnsKvsFromMap(extra)
		if level == "warn" {
			log.Warn(ledger.StageValidate, message, kvs)
			return
		}
		log.Info(ledger.StageValidate, message, kvs)
	}
}

// cnsKvsFromMap renders the TS data object as ordered KVs (sorted by key
// for deterministic run logs; the oracle seams receive plain maps here).
func cnsKvsFromMap(extra map[string]any) []ledger.KV {
	if len(extra) == 0 {
		return nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sortStrings(keys)
	kvs := make([]ledger.KV, 0, len(keys))
	for _, k := range keys {
		kvs = append(kvs, ledger.KV{Key: k, Value: extra[k]})
	}
	return kvs
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// parsePrBranch mirrors the TS parsePrBranch: derive the PR head branch
// from a PR URL (/pull/<n> form), or "" when unknown.
func parsePrBranch(prURL string) string {
	m := prBranchPattern.FindStringSubmatch(prURL)
	if m == nil {
		return ""
	}
	return "devagent/pull-" + m[1]
}

var prBranchPattern = regexp.MustCompile(`/pull/(\d+)`)

// ---------------------------------------------------------------------------
// Self-update hook: port of src/self-update.ts (the consume loop's
// post-merge hook). The TS `runner` seam becomes cnsSelfUpdateRunner.

var selfUpdateRedactions = []struct {
	re *regexp.Regexp
	to string
}{
	{regexp.MustCompile(`(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{10,}`), "<redacted>"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{10,}`), "<redacted>"},
	{regexp.MustCompile(`glpat-[A-Za-z0-9_-]{10,}`), "<redacted>"},
	{regexp.MustCompile(`(https?://)[^/@\s]+:[^@\s]+@`), "${1}<redacted>@"},
	{regexp.MustCompile(`(?i)x-access-token:[^\s@]+@`), "x-access-token:<redacted>@"},
}

// cnsRedactSecrets mirrors redactSecrets: redact credential-looking
// substrings (tokens, basic-auth URLs) before they reach logs or returned
// details. Git errors can embed authenticated remote URLs.
func cnsRedactSecrets(text string) string {
	for _, r := range selfUpdateRedactions {
		text = r.re.ReplaceAllString(text, r.to)
	}
	return text
}

// cnsRunSelfUpdate mirrors runSelfUpdate: pull latest main, rebuild, and
// (when scout LaunchAgent is installed) kickstart it. Never runs with a
// dirty worktree; callers should check before invoking.
func cnsRunSelfUpdate(repoPath string, log RunLog) error {
	steps := []string{}
	run := func(cmd string, args []string, timeoutMs int) spawn.Result {
		if cnsSelfUpdateRunner != nil {
			return cnsSelfUpdateRunner(cmd, args, spawn.Options{Dir: repoPath, TimeoutMs: timeoutMs})
		}
		return spawn.RunCli(cmd, args, spawn.Options{Dir: repoPath, TimeoutMs: timeoutMs})
	}

	// 1) git status must be clean (no staged/unstaged changes); untracked
	// .devagent/* is ok
	status := run("git", []string{"status", "--porcelain"}, 10_000)
	if status.TimedOut || status.ExitCode != 0 {
		return fmt.Errorf("self-update: git status failed")
	}
	dirty := 0
	for _, l := range strings.Split(status.Stdout, "\n") {
		if strings.TrimSpace(l) != "" && !strings.Contains(l, ".devagent/") && !strings.Contains(l, ".selfbuild/") {
			dirty++
		}
	}
	if dirty > 0 {
		return fmt.Errorf("self-update skipped: dirty worktree (%d file(s))", dirty)
	}

	// 2) git pull --ff-only
	pull := run("git", []string{"pull", "--ff-only"}, 30_000)
	if pull.TimedOut || pull.ExitCode != 0 {
		msg := cnsRedactSecrets(cnsTruncateRunes(pull.Stdout+pull.Stderr, 400))
		if log != nil {
			log.Warn(ledger.StageSelfUpdate, fmt.Sprintf("pull failed: %s", msg), nil)
		}
		return fmt.Errorf("self-update: pull failed: %s", strings.TrimSpace(cnsTruncateRunes(msg, 200)))
	}
	steps = append(steps, "pull")

	// 3) npm ci (or npm install fallback) + build
	install := run("npm", []string{"ci", "--ignore-scripts"}, 120_000)
	if install.TimedOut || install.ExitCode != 0 {
		install = run("npm", []string{"install", "--ignore-scripts"}, 120_000)
		if install.TimedOut || install.ExitCode != 0 {
			return fmt.Errorf("self-update: npm install failed")
		}
	}
	steps = append(steps, "install")

	build := run("npm", []string{"run", "build"}, 60_000)
	if build.TimedOut || build.ExitCode != 0 {
		return fmt.Errorf("self-update: build failed")
	}
	steps = append(steps, "build")

	// 4) kickstart scout LaunchAgent if present (macOS)
	if runtime.GOOS == "darwin" {
		label := "com.devagent.scout"
		uid := os.Getuid()
		kick := run("launchctl", []string{"kickstart", fmt.Sprintf("gui/%d/%s", uid, label)}, 5_000)
		if !kick.TimedOut && kick.ExitCode == 0 {
			steps = append(steps, "scout kickstart")
		}
	}

	detail := fmt.Sprintf("self-update ok: %s", strings.Join(steps, " -> "))
	if log != nil {
		log.Info(ledger.StageSelfUpdate, detail, nil)
	}
	return nil
}
