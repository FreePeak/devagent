// Package file mirrors src/orchestrator/executor.ts (FR-GO-07, issue #194).
//
// Executor role: implements one small precise task in its own worktree
// (branch devagent/<task-id>), verifies with the repo's test suite (G1),
// and reports done/failed with evidence. Fresh worktree per retry.
//
// Failure surface (PRD:775): each failed attempt appends a normalized
// failure signature to a per-task trail.jsonl under the repo (survives
// worktree cleanup — the trail outlives any single attempt). When the task
// exhausts attempts with N+ identical trailing signatures, the executor
// marks taskInterrupt, aborts the worker, and returns the compact
// post-mortem (failure class, last gate excerpt, attempts, trail hash) so
// the scheduler can thread it into the ledger on board archive (closing the
// loop-57/58 diagnostic gap: loops died on the same goal with only
// `attempts: 3`-style evidence).
//
// Live side effects are seamed: worker dispatch through WorkerDispatcher
// (the workers CLI adapter is a sibling port), stale-worker reaping and
// operator-attach tracing through optional funcs (best-effort by design,
// no-op defaults). Everything else — trail JSONL, signature normalization,
// preflight guards, the repair loop, the commit step — is ported directly
// and mirrors the TS control flow.

package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	mathrandv2 "math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/resilience"
	"github.com/FreePeak/devagent/internal/scout"
	"github.com/FreePeak/devagent/internal/spawn"
	"github.com/FreePeak/devagent/internal/trust"
)

// ExecutorFailureClass mirrors the TS ExecutorFailureClass union
// (src/types.ts): the taxonomy stored in board.failureClass and
// task.interrupt.failureClass.
type ExecutorFailureClass = string

const (
	// Repo test suite failed on the gate (G1) after the worker reported done.
	ExecutorFailureClassTestGate ExecutorFailureClass = "test-gate"
	// Worker exited non-zero or produced an error the classifier cannot retry.
	ExecutorFailureClassWorkerError ExecutorFailureClass = "worker-error"
	// Worker timed out (wall-clock budget exhausted, no progress).
	ExecutorFailureClassTimeout ExecutorFailureClass = "timeout"
	// Git commit of gate-passed work failed.
	ExecutorFailureClassCommit ExecutorFailureClass = "commit"
	// Worktree creation failed (repo/branch-level problem).
	ExecutorFailureClassWorktree ExecutorFailureClass = "worktree"
	// Transient provider failure that would normally be retried, not terminal.
	ExecutorFailureClassTransientProvider ExecutorFailureClass = "transient-provider"
	// Dispatch preflight rejected the run config (e.g. model id invalid).
	ExecutorFailureClassConfig ExecutorFailureClass = "config"
	// Dispatch preflight refused an oversized prescriptive prompt (Q18).
	ExecutorFailureClassPromptOversized ExecutorFailureClass = "prompt-oversized"
	// Failure we could not classify into a known class.
	ExecutorFailureClassUnknown ExecutorFailureClass = "unknown"
)

// TrailRoot mirrors the TS TRAIL_ROOT: trail files under the repo; same dir
// the ledger uses so it survives resets.
const TrailRoot = ".devagent/runs/orchestration"

// DefaultMaxPromptBytes mirrors the TS DEFAULT_MAX_PROMPT_BYTES: default
// byte ceiling for a task's own prescriptive instruction payload
// (Q18 / PRD:915). The 08-29 stuck board burned 3 salvage attempts on a
// ~5 KB step-by-step prompt; 4 KB sits below that so a dense mega-prompt is
// refused and forced into a plan-split, while ordinary task contracts stay
// well under.
const DefaultMaxPromptBytes = 4096

// DefaultInterruptThreshold mirrors the TS evaluateTrailInterrupt default
// threshold (PRD:775 — N defaults to 3).
const DefaultInterruptThreshold = 3

const (
	executorMaxLogicAttempts = 2   // in-worker repair loop; scheduler owns cross-wave retries
	executorMaxInfraRetries  = 200 // transient provider retry ceiling
	commitStepTimeoutMs      = 30_000
	noProgressDefaultMs      = 10 * 60_000 // resilience.noProgressTimeoutMs default
	coldStartDefaultMs       = 90_000      // resilience.coldStartTimeoutMs default (Q31)
)

// InstructionPayloadBytes mirrors the TS instructionPayloadBytes: byte
// length of the task's own instruction payload (prompt + boundary
// constraints + evidence gaps, Q18). Deliberately excludes the lessons/KG
// digest and the acceptance criteria — the digest is bounded separately by
// lessonsMaxChars (4000) and is routinely large, so counting it would make
// the size guard fire on every dispatch. This measures only the prescriptive
// text the executor is being asked to follow.
func InstructionPayloadBytes(task OrchestratorTask) int {
	parts := []string{task.Prompt}
	parts = append(parts, task.BoundaryConstraints...)
	parts = append(parts, task.EvidenceGaps...)
	return len(strings.Join(parts, "\n"))
}

// TrailSignature mirrors the TS TrailSignature: one failure signature row
// in a task's trail.jsonl. Field order = TS writer key order.
type TrailSignature struct {
	TS           string `json:"ts"`
	Attempt      int    `json:"attempt"`
	Signature    string `json:"signature"`
	FailureClass string `json:"failureClass"`
	Excerpt      string `json:"excerpt"`
}

// TrailSignatureInput mirrors the TS Omit<TrailSignature, 'ts'> append
// record (the writer adds ts last, matching the TS spread order).
type TrailSignatureInput struct {
	Attempt      int    `json:"attempt"`
	Signature    string `json:"signature"`
	FailureClass string `json:"failureClass"`
	Excerpt      string `json:"excerpt"`
}

// TaskTrailPath mirrors the TS taskTrailPath: per-task trail path
// (repo-scoped so it persists across fresh worktrees).
func TaskTrailPath(repoPath string, taskID string) string {
	return filepath.Join(repoPath, TrailRoot, "trail-"+git.SanitizeTicketID(taskID)+".jsonl")
}

var (
	trailISORe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ][\d:.Z+-]+`)
	trailWSRe  = regexp.MustCompile(`\s+`)
)

// FailureSignature mirrors the TS failureSignature: normalize a failure
// excerpt into a stable signature — trim + collapse whitespace + lowercase
// so trivial churn (quoting, line wrapping) does not fragment
// otherwise-identical failures into distinct signatures. ISO timestamps are
// stripped so worker errors that embed the clock still collide across
// attempts (identical failure, different minute).
func FailureSignature(excerpt string) string {
	normalized := trailWSRe.ReplaceAllString(trailISORe.ReplaceAllString(excerpt, " <ts> "), " ")
	normalized = strings.ToLower(strings.TrimSpace(normalized))
	sum := sha256.Sum256([]byte(normalized))
	return hex.EncodeToString(sum[:])[:16]
}

// AppendTrailSignature mirrors the TS appendTrailSignature: append one
// failure signature to the task trail. Best-effort by design.
func AppendTrailSignature(repoPath string, taskID string, record TrailSignatureInput) {
	file := TaskTrailPath(repoPath, taskID)
	if err := os.MkdirAll(filepath.Join(repoPath, TrailRoot), 0o755); err != nil {
		return // best-effort observability only
	}
	// Field order = TS writer order: {attempt, signature, failureClass,
	// excerpt, ts} (the TS spread adds ts last). EscapeHTML off: JSON.stringify
	// does not escape <, > or &.
	line, err := marshalOrderedJSON(record, queue.NowIso())
	if err != nil {
		return // best-effort observability only
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return // best-effort observability only
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(line)
}

// marshalOrderedJSON encodes record then appends the ts key last, matching
// the TS `{...record, ts}` spread key order. HTML escaping is disabled to
// match JSON.stringify.
func marshalOrderedJSON(record TrailSignatureInput, ts string) ([]byte, error) {
	var buf strings.Builder
	buf.WriteString(`{"attempt":`)
	a, err := json.Marshal(record.Attempt)
	if err != nil {
		return nil, err
	}
	buf.Write(a)
	buf.WriteString(`,"signature":`)
	s, err := json.Marshal(record.Signature)
	if err != nil {
		return nil, err
	}
	buf.Write(s)
	buf.WriteString(`,"failureClass":`)
	c, err := json.Marshal(record.FailureClass)
	if err != nil {
		return nil, err
	}
	buf.Write(c)
	buf.WriteString(`,"excerpt":`)
	e, err := json.Marshal(record.Excerpt)
	if err != nil {
		return nil, err
	}
	buf.Write(e)
	buf.WriteString(`,"ts":`)
	t, err := json.Marshal(ts)
	if err != nil {
		return nil, err
	}
	buf.Write(t)
	buf.WriteString("}\n")
	return []byte(buf.String()), nil
}

// ReadTrailSignatures mirrors the TS readTrailSignatures: read a task trail,
// oldest first; [] when absent/corrupt.
func ReadTrailSignatures(repoPath string, taskID string) []TrailSignature {
	file := TaskTrailPath(repoPath, taskID)
	data, err := os.ReadFile(file)
	if err != nil {
		return []TrailSignature{}
	}
	out := []TrailSignature{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var sig TrailSignature
		if err := json.Unmarshal([]byte(line), &sig); err != nil {
			continue // skip corrupt lines; a trail is data, not truth
		}
		out = append(out, sig)
	}
	return out
}

// DuplicateTrailingSignatures mirrors the TS duplicateTrailingSignatures:
// the trailing run of identical signatures, if it is at least `n` long.
// Returns the trailing slice on a hit, nil otherwise (PRD:775 — "N+
// identical trailing trail.jsonl failure signatures").
func DuplicateTrailingSignatures(signatures []TrailSignature, n int) []TrailSignature {
	if len(signatures) < n {
		return nil
	}
	tail := signatures[len(signatures)-n:]
	first := tail[0].Signature
	for _, s := range tail {
		if s.Signature != first {
			return nil
		}
	}
	return tail
}

// InterruptDecision mirrors the TS InterruptDecision: decision payload
// returned by the executor on a taskInterrupt.
type InterruptDecision struct {
	Interrupted     bool                 `json:"interrupted"`
	FailureClass    ExecutorFailureClass `json:"failureClass"`
	LastGateExcerpt string               `json:"lastGateExcerpt"`
	Attempts        int                  `json:"attempts"`
	TrailHash       string               `json:"trailHash"`
	// Human summary (identical count + class + hash).
	Detail string `json:"detail"`
}

// EvaluateTrailInterrupt mirrors the TS evaluateTrailInterrupt: append the
// failure signature to the task trail, then decide whether the trail now
// ends with N+ identical signatures (PRD:775; TS default threshold 3 — the
// Go port takes it explicitly, pass DefaultInterruptThreshold). On a hit,
// returns the taskInterrupt decision payload (failure class, last gate
// excerpt, attempts, trail hash); nil means keep retrying.
func EvaluateTrailInterrupt(repoPath string, taskID string, attempts int, excerpt string, failureClass ExecutorFailureClass, threshold int) *InterruptDecision {
	sig := FailureSignature(excerpt)
	AppendTrailSignature(repoPath, taskID, TrailSignatureInput{
		Attempt:      attempts,
		Signature:    sig,
		FailureClass: failureClass,
		Excerpt:      truncRunes(excerpt, 200),
	})
	trail := ReadTrailSignatures(repoPath, taskID)
	dupes := DuplicateTrailingSignatures(trail, threshold)
	if dupes == nil {
		return nil
	}
	trailHash := sha256.Sum256([]byte(strings.Join(mapJoin(dupes, func(d TrailSignature) string { return d.Signature }), "|")))
	hash := hex.EncodeToString(trailHash[:])[:16]
	return &InterruptDecision{
		Interrupted:     true,
		FailureClass:    failureClass,
		LastGateExcerpt: dupes[len(dupes)-1].Excerpt,
		Attempts:        attempts,
		TrailHash:       hash,
		Detail:          fmt.Sprintf("%d identical %s failures (trail hash %s)", len(dupes), failureClass, hash),
	}
}

func mapJoin[T any](items []T, fn func(T) string) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, fn(it))
	}
	return out
}

// truncRunes delegates to merge.go sliceUTF16 — the TS
// String.prototype.slice(0, n) semantic (UTF-16 code-unit truncation), the
// byte-parity contract for `.slice(0, 200)` evidence cuts. Name kept from
// the first cut; do not add a second truncation helper.
func truncRunes(s string, n int) string {
	return sliceUTF16(s, n)
}

// WorkerDispatchRequest mirrors the spawn opts object executeTask and
// localCiFixer hand to the worker adapter (src/workers/index.ts getWorker
// spawn). Optional TS fields are zero-valued = unset.
type WorkerDispatchRequest struct {
	Prompt    string
	Cwd       string
	TimeoutMs int
	Attempt   int // 1-based dispatch attempt (Q34 watchdog identity)
	// Q34: ledger identity from the dispatcher, never derived from the
	// (ephemeral) worktree cwd the worker runs in.
	WatchdogRepoPath string
	WatchdogTaskID   string
	WatchdogWorker   WorkerName

	Model               string // "" = unset
	Variant             string // "" = unset
	APIMaxAttempts      int    // 0 = unset
	NoProgressTimeoutMs int    // 0 = unset
	ColdStartTimeoutMs  int    // 0 = unset
	Herdr               bool
}

// WorkerDispatchResult mirrors the worker spawn result surface the executor
// consumes (resultText/errorText/exitCode/timedOut/noProgress/coldStart).
type WorkerDispatchResult struct {
	ExitCode   int
	ResultText string
	ErrorText  string
	TimedOut   bool
	NoProgress bool
	ColdStart  bool
}

// WorkerDispatcher mirrors the worker spawn seam executeTask and
// localCiFixer dispatch through.
//
// TODO(FR-GO-05 #190): replace with sibling port when the workers adapter
// lands; until then callers inject a dispatcher (tests use fakes).
type WorkerDispatcher interface {
	Dispatch(req WorkerDispatchRequest) WorkerDispatchResult
}

// RunLog mirrors the RunLogger surface executeTask/runScheduler consume
// (src/logger.ts). *ledger.RunLogger satisfies it; tests use a no-op.
type RunLog interface {
	Info(stage ledger.RunStage, message string, data []ledger.KV)
	Warn(stage ledger.RunStage, message string, data []ledger.KV)
	Error(stage ledger.RunStage, message string, data []ledger.KV)
}

// NoopRunLog is a silent RunLog for hermetic tests.
type NoopRunLog struct{}

func (NoopRunLog) Info(ledger.RunStage, string, []ledger.KV)  {}
func (NoopRunLog) Warn(ledger.RunStage, string, []ledger.KV)  {}
func (NoopRunLog) Error(ledger.RunStage, string, []ledger.KV) {}

// ExecutorDeps carries the live side-effect seams ExecuteTask needs.
// Every field is optional unless documented: nil means best-effort no-op
// (the TS defaults swallow these errors the same way).
type ExecutorDeps struct {
	// Dispatcher is REQUIRED: the worker adapter executeTask dispatches
	// through.
	//
	// TODO(FR-GO-05 #190): replace with sibling port.
	Dispatcher WorkerDispatcher
	// ReapStaleWorkers mirrors findStaleWorkerPids(idleMs, {cwdPrefix}) —
	// repo-scoped stale worker pid discovery. nil = skip (best-effort).
	// TODO(FR-GO-05 #190): replace with the reaper port.
	ReapStaleWorkers func(cwdPrefix string, idleMs int) []int
	// KillProcessTree mirrors killStaleProcessTree(pid, reason). nil = skip.
	KillProcessTree func(pid int, reason string)
	// OperatorAttachTrace mirrors herdr operatorAttachTrace(taskId) — only
	// consulted when herdr is enabled. nil = skip (visibility only).
	OperatorAttachTrace func(taskID string) bool
	// ResolveSession mirrors herdr resolveSession(). nil = "".
	ResolveSession func() string
}

// ExecuteTaskArgs mirrors the TS executeTask args object.
type ExecuteTaskArgs struct {
	Task      *OrchestratorTask
	Board     *ProjectBoard
	RepoPath  string
	TimeoutMs int
	// Repo-local lessons file override (see loadLessons).
	LessonsFile string
	// Character budget for injected lessons (see loadLessons); nil = config.
	LessonsMaxChars *float64
	Executor        WorkerName
	Log             RunLog
	Deps            ExecutorDeps
}

// ExecuteTaskResult mirrors the TS executeTask return shape (declared in
// scheduler.ts as ExecuteTaskResult). JSON tags keep the TS result shape.
type ExecuteTaskResult struct {
	OK              bool   `json:"ok"`
	WorktreePath    string `json:"worktreePath,omitempty"`
	Detail          string `json:"detail,omitempty"`
	Interrupted     bool   `json:"interrupted,omitempty"`
	FailureClass    string `json:"failureClass,omitempty"`
	LastGateExcerpt string `json:"lastGateExcerpt,omitempty"`
	Attempts        int    `json:"attempts,omitempty"`
	TrailHash       string `json:"trailHash,omitempty"`
}

// ExecuteTask mirrors the TS executeTask: preflight guards, fresh worktree,
// bounded repair loop with transient-infra retries, G1 test gate, commit,
// and the PRD:775 trail-interrupt failure surface. The returned error is
// the TS "thrown exception" channel (config load failure) — the scheduler
// catches it and marks the task failed.
func ExecuteTask(args ExecuteTaskArgs) (ExecuteTaskResult, error) {
	task := args.Task
	log := args.Log
	if log == nil {
		log = NoopRunLog{}
	}
	cfg, err := config.Load(args.RepoPath)
	if err != nil {
		return ExecuteTaskResult{}, err
	}

	// Model preflight (PRD Phase 4 "Provider model-id validation at dispatch",
	// Q32): reject an invalid config.model BEFORE worktree creation or any
	// worker spend so a bad id fails at the gate in seconds instead of
	// burning attempts mid-board (loop 58: `--model coding` exit-1-in-12s
	// repeated 3x).
	modelProblem := config.ValidateWorkerModel(args.Executor, cfg.Model)
	if modelProblem != "" {
		log.Error(ledger.StageTask, fmt.Sprintf("%s dispatch preflight failed: %s", task.ID, modelProblem), nil)
		return ExecuteTaskResult{
			OK:           false,
			Detail:       "dispatch preflight: " + modelProblem,
			FailureClass: ExecutorFailureClassConfig,
		}, nil
	}

	// Prompt-size preflight (Q18 / PRD:915): refuse an oversized prescriptive
	// prompt BEFORE worktree creation or any worker spend. Measure only the
	// task's own instruction payload (never the lessons/KG digest).
	// resilience.maxPromptBytes (env DEVAGENT_MAX_PROMPT_BYTES) sets the
	// ceiling; 0 disables the guard.
	maxPromptBytes := DefaultMaxPromptBytes
	if cfg.Resilience != nil && cfg.Resilience.MaxPromptBytes != nil {
		maxPromptBytes = int(*cfg.Resilience.MaxPromptBytes)
	}
	promptBytes := InstructionPayloadBytes(*task)
	if maxPromptBytes > 0 && promptBytes > maxPromptBytes {
		detail := fmt.Sprintf("prompt oversized: %d bytes > resilience.maxPromptBytes=%d — "+
			"split this into smaller tasks (plan-split): each subtask must carry its own "+
			"instruction payload under %d bytes rather than one dense prescriptive prompt",
			promptBytes, maxPromptBytes, maxPromptBytes)
		log.Error(ledger.StageTask, fmt.Sprintf("%s dispatch preflight failed: %s", task.ID, detail), nil)
		return ExecuteTaskResult{
			OK:           false,
			Detail:       detail,
			FailureClass: ExecutorFailureClassPromptOversized,
		}, nil
	}

	criteria := append([]string{}, task.AcceptanceCriteria...)
	if task.ExpectedOutput != "" {
		criteria = append(criteria, task.ExpectedOutput)
	}
	description := buildTaskDescription(*task)
	plan := scout.ImplementationPlan{
		Ticket: scout.TicketSpec{
			ID:                 task.ID,
			Title:              task.Title,
			Description:        description,
			Labels:             []string{"orchestrated"},
			AcceptanceCriteria: criteria,
			TrackerInternalID:  task.ID,
		},
		Classification: scout.ClassEndpointOnly,
		Tasks:          []string{},
	}

	// branch devagent/<TASKID>-a<attempt>[r<recovery>] keeps retries isolated
	// (fresh tree); recovery grants extend the suffix to avoid branch reuse
	wt, err := git.CreateWorktree(args.RepoPath, git.SanitizeTicketID(task.ID)+"-"+AttemptSuffix(task.Attempts, derefInt(task.Recoveries)))
	if err != nil {
		return ExecuteTaskResult{
			OK:           false,
			Detail:       "worktree creation failed: " + err.Error(),
			FailureClass: ExecutorFailureClassWorktree,
		}, nil
	}
	worktreePath := wt.WorktreePath

	lessonsMaxChars := 0 // 0 = scout.LoadLessons default budget
	if args.LessonsMaxChars != nil {
		lessonsMaxChars = int(*args.LessonsMaxChars)
	} else if cfg.LessonsMaxChars != nil {
		lessonsMaxChars = int(*cfg.LessonsMaxChars)
	}
	lessons := scout.LoadLessons(args.RepoPath, args.LessonsFile, lessonsMaxChars)
	prompt := scout.BuildImplementationPrompt(plan, lessons)

	// Knowledge-context digest for the in-worker repair leg (FR-CTX-01/03):
	// assembled orchestrator-side, spliced into the repair prompt only — the
	// worker adapter receives a plain prompt string (FR-CTX-04).
	knowledge := buildKnowledgeDigest(args.RepoPath, cfg, args.LessonsMaxChars)

	noProgressTimeoutMs := noProgressDefaultMs
	coldStartTimeoutMs := coldStartDefaultMs
	var apiMaxAttempts int
	if cfg.Resilience != nil {
		if cfg.Resilience.NoProgressTimeoutMs != nil {
			noProgressTimeoutMs = int(*cfg.Resilience.NoProgressTimeoutMs)
		}
		if cfg.Resilience.ColdStartTimeoutMs != nil {
			coldStartTimeoutMs = int(*cfg.Resilience.ColdStartTimeoutMs)
		}
		if cfg.Resilience.APIMaxAttempts != nil {
			apiMaxAttempts = int(*cfg.Resilience.APIMaxAttempts)
		}
	}
	useHerdr := config.HerdrEnabled(cfg)

	deps := args.Deps
	reaperIdleMs := noProgressTimeoutMs
	if reaperIdleMs == 0 {
		reaperIdleMs = 60_000
	}

	attempt := 1
	logicAttempts := 0
	infraRetries := 0
	for logicAttempts < executorMaxLogicAttempts || infraRetries < executorMaxInfraRetries {
		reqPrompt := prompt
		if attempt > 1 {
			reqPrompt = scout.BuildRepairPrompt(plan, attempt-1, "previous attempt failed the test gate", lessons, knowledge)
		}
		result := deps.Dispatcher.Dispatch(WorkerDispatchRequest{
			Prompt:              reqPrompt,
			Cwd:                 worktreePath,
			TimeoutMs:           args.TimeoutMs,
			Attempt:             attempt,
			WatchdogRepoPath:    args.RepoPath,
			WatchdogTaskID:      task.ID,
			WatchdogWorker:      args.Executor,
			Model:               cfg.Model,
			Variant:             cfg.Variant,
			APIMaxAttempts:      apiMaxAttempts,
			NoProgressTimeoutMs: noProgressTimeoutMs,
			ColdStartTimeoutMs:  coldStartTimeoutMs,
			Herdr:               useHerdr,
		})

		// Repo-scoped stale worker reaping: only headless workers whose cwd
		// is inside this repo's worktrees may be reaped — never the user's
		// interactive sessions. Skipped for herdr panes (herdr watchdog owns
		// those).
		if deps.ReapStaleWorkers != nil && deps.KillProcessTree != nil && !useHerdr {
			for _, pid := range deps.ReapStaleWorkers(filepath.Join(args.RepoPath, ".devagent-worktrees"), reaperIdleMs) {
				deps.KillProcessTree(pid, "")
			}
		}

		// FR-VIS-03 (partial): after a herdr-path spawn, check whether an
		// operator is attached to the task's pane and record it. Best-effort
		// only — visibility bookkeeping must never break the spawn pipeline.
		if useHerdr && deps.OperatorAttachTrace != nil && deps.OperatorAttachTrace(task.ID) {
			session := ""
			if deps.ResolveSession != nil {
				session = deps.ResolveSession()
			}
			ledger.AppendOperatorAttachRecord(args.RepoPath, ledger.OperatorAttachRecord{
				TS:      queue.NowIso(),
				Kind:    "event",
				Event:   "operator-attached",
				TaskID:  task.ID,
				Attempt: attempt,
				PaneID:  "",
				Session: session,
			})
		}

		if result.TimedOut || result.ExitCode != 0 || result.ErrorText != "" || result.NoProgress || result.ColdStart {
			// Inspect both resultText and errorText: the proxy surfaces
			// transient provider outages (rate-limited empty streams) as
			// stderr-only with no .result field.
			text := strings.Join(nonEmpty([]string{result.ResultText, result.ErrorText}), "\n")
			var errText *string
			if result.ErrorText != "" {
				errText = &result.ErrorText
			}
			if result.TimedOut || result.NoProgress ||
				// Q31: a cold-start kill is the same infra class as a
				// silence-clock fire — retry cheaply, do not burn a logic
				// attempt.
				result.ColdStart ||
				(text != "" && !resilience.IsNonRetryableApiError(text) &&
					(resilience.IsTransientProviderError(&text) || resilience.IsTransientProviderError(errText))) {
				infraRetries++
				recordTransientClass(args.RepoPath, strings.Join(nonEmpty([]string{
					text,
					classLabel(result),
				}), " "))
				sleepMs(backoffDelay(infraRetries))
				attempt++
				continue
			}
			logicAttempts++
			attempt++

			// Executor failure surface (PRD:775): record the failure
			// signature and abort the worker once N+ identical trailing
			// trail signatures appear.
			excerpt := text
			if excerpt == "" {
				excerpt = fmt.Sprintf("worker exited %d", result.ExitCode)
			}
			if interrupt := EvaluateTrailInterrupt(args.RepoPath, task.ID, task.Attempts, excerpt, ExecutorFailureClassWorkerError, DefaultInterruptThreshold); interrupt != nil {
				abortWorker(deps, worktreePath)
				return interruptResult(worktreePath, interrupt), nil
			}
			if logicAttempts >= executorMaxLogicAttempts {
				break
			}
			continue
		}

		g1, gateErr := gates.RunTestGate(gates.SpawnRunner{}, worktreePath, args.TimeoutMs)
		if gateErr != nil {
			return ExecuteTaskResult{}, gateErr // TS: detectTestCommand throw bubbles
		}
		if g1.Passed {
			// Commit the work: uncommitted changes never reach merge-back
			// (live-smoke lesson: gate passed in-tree but merge integrated
			// nothing).
			_ = spawn.RunCli("git", []string{"add", "-A"}, spawn.Options{Dir: worktreePath, TimeoutMs: commitStepTimeoutMs})
			commit := spawn.RunCli("git",
				[]string{"commit", "-m", fmt.Sprintf("task %s: %s", task.ID, task.Title), "--no-verify"},
				spawn.Options{Dir: worktreePath, TimeoutMs: commitStepTimeoutMs})
			if commit.ExitCode != 0 && !nothingToCommitRe.MatchString(commit.Stderr+commit.Stdout) {
				return ExecuteTaskResult{
					OK:           false,
					WorktreePath: worktreePath,
					Detail:       "commit failed: " + truncRunes(commit.Stderr, 200),
					FailureClass: ExecutorFailureClassCommit,
				}, nil
			}
			return ExecuteTaskResult{OK: true, WorktreePath: worktreePath}, nil
		}
		log.Warn(ledger.StageTask, fmt.Sprintf("%s G1 failed on executor attempt %d", task.ID, attempt), nil)

		// Executor failure surface (PRD:775): record the failure signature and
		// abort the worker once N+ identical trailing trail signatures appear.
		gateExcerpt := g1.Detail
		if gateExcerpt == "" {
			gateExcerpt = "test gate failed"
		}
		if gateInterrupt := EvaluateTrailInterrupt(args.RepoPath, task.ID, task.Attempts, gateExcerpt, ExecutorFailureClassTestGate, DefaultInterruptThreshold); gateInterrupt != nil {
			abortWorker(deps, worktreePath)
			return interruptResult(worktreePath, gateInterrupt), nil
		}

		logicAttempts++
		attempt++
		if logicAttempts >= executorMaxLogicAttempts {
			break
		}
	}
	return ExecuteTaskResult{
		OK:           false,
		WorktreePath: worktreePath,
		Detail:       "test gate failed after executor attempts",
		FailureClass: ExecutorFailureClassTestGate,
	}, nil
}

func interruptResult(worktreePath string, d *InterruptDecision) ExecuteTaskResult {
	return ExecuteTaskResult{
		OK:              false,
		WorktreePath:    worktreePath,
		Interrupted:     true,
		FailureClass:    d.FailureClass,
		LastGateExcerpt: d.LastGateExcerpt,
		Attempts:        d.Attempts,
		TrailHash:       d.TrailHash,
		Detail:          "taskInterrupt: " + d.Detail,
	}
}

// buildTaskDescription mirrors the TS ticket description assembly in
// executeTask.
func buildTaskDescription(task OrchestratorTask) string {
	parts := []string{task.Prompt}
	if len(task.BoundaryConstraints) > 0 {
		parts = append(parts, "\nBoundary constraints (must respect):\n"+strings.Join(mapJoin(task.BoundaryConstraints, func(c string) string { return "- " + c }), "\n"))
	}
	// Targeted re-contracting: retry closes the audited gap, not blind redo
	if len(task.EvidenceGaps) > 0 {
		parts = append(parts, "\nPrevious attempt failed independent audit. Close these specific gaps:\n"+strings.Join(mapJoin(task.EvidenceGaps, func(g string) string { return "- " + g }), "\n"))
	}
	return strings.Join(parts, "")
}

// buildKnowledgeDigest mirrors the FR-CTX knowledge-context assembly in
// executeTask: config-gated KG layer, lessonsMaxChars-bounded digest.
func buildKnowledgeDigest(repoPath string, cfg config.Config, lessonsMaxChars *float64) string {
	maxChars := 0
	if lessonsMaxChars != nil {
		maxChars = int(*lessonsMaxChars)
	} else if cfg.LessonsMaxChars != nil {
		maxChars = int(*cfg.LessonsMaxChars)
	}
	opts := scout.KnowledgeOptions{MaxChars: &maxChars}
	if cfg.Context != nil {
		opts.Kg = cfg.Context.Kg
		opts.AgentsMd = trust.Mode(cfg.Context.AgentsMd)
	}
	return scout.BuildKnowledgeContext(repoPath, opts)
}

// derefInt dereferences an optional int field, 0 when nil (TS `?? 0`).
func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// intPtr returns a pointer to n (TS optional-field write sites).
func intPtr(n int) *int {
	v := n
	return &v
}

func classLabel(result WorkerDispatchResult) string {
	switch {
	case result.ColdStart:
		return "cold-start deadline exceeded"
	case result.TimedOut:
		return "no-progress watchdog timeout"
	default:
		return ""
	}
}

// recordTransientClass mirrors the TS recordTransientClass call site
// (src/resilience/proxy-state.ts): durable provider health observability,
// best-effort by design — a write failure never changes the executor
// outcome.
func recordTransientClass(repoPath string, text string) {
	_ = resilience.RecordTransientClass(repoPath, text)
}

func nonEmpty(items []string) []string {
	out := []string{}
	for _, s := range items {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

var nothingToCommitRe = regexp.MustCompile(`nothing to commit`)

// abortWorker mirrors the TS abortWorker: best-effort kill of any devagent
// headless worker whose cwd is inside this task's worktree. The seam funcs
// are optional; absent reaper = no-op (the spawn already returned by the
// time the interrupt fires, so this only covers lingering orphans).
func abortWorker(deps ExecutorDeps, worktreePath string) {
	if deps.ReapStaleWorkers == nil || deps.KillProcessTree == nil {
		return // best-effort abort only
	}
	for _, pid := range deps.ReapStaleWorkers(worktreePath, 0) {
		deps.KillProcessTree(pid, "taskInterrupt")
	}
}

// backoffDelay mirrors src/sessionguard/backoff.ts backoffDelay with the
// DEFAULT_BACKOFF options: exponential with +/-25% jitter so parallel
// guards do not sync up. Attempt is 1-based; attempt 1 waits ~2s.
func backoffDelay(attempt int) int {
	const baseDelayMs = 2_000
	const maxDelayMs = 60_000
	const factor = 2
	capped := attempt
	if capped < 1 {
		capped = 1
	}
	raw := float64(baseDelayMs)
	for i := 1; i < capped; i++ {
		raw *= factor
		if raw >= float64(maxDelayMs) {
			raw = float64(maxDelayMs)
			break
		}
	}
	if raw > float64(maxDelayMs) {
		raw = float64(maxDelayMs)
	}
	jitter := 1 + (mathrandv2.Float64()*0.5 - 0.25)
	delay := math.Round(raw * jitter)
	if delay < 0 {
		delay = 0
	}
	return int(delay)
}

// sleepMs pauses between transient-infra retries (the TS
// `new Promise(r => setTimeout(r, delay))`). Tests drive their own
// dispatcher fakes and never hit this path with real waits; the ceiling is
// bounded by maxDelayMs = 60s.
func sleepMs(ms int) {
	if ms <= 0 {
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}
