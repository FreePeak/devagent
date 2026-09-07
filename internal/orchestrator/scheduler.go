// Package file mirrors src/orchestrator/scheduler.ts (FR-GO-07, issue #194).
//
// Wave scheduler (sprint-orchestrator lesson): repeatedly execute all ready
// tasks in bounded parallel waves. Failure isolation is wave-scoped — a
// failed task marks itself failed and blocks dependents but never kills
// siblings. Each attempt runs on a fresh worktree (Orca lesson: no context
// contamination across retries).
//
// Evidence-gated completion (LongHorizon-Harness lesson): an executor
// success only moves the task to 'untrusted'; it becomes 'done' solely on
// an independent audit verdict with clean integrity. Failed audits are
// externalized into evidenceGaps so the retry targets the actual gap.
package orchestrator

import (
	"fmt"
	"sync"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
)

// ExecuteTaskResult mirrors the TS ExecuteTaskResult (scheduler.ts) — the
// executor outcome the scheduler consumes. (Struct declared here per the TS
// file layout; ExecuteTask returns it.)
//
// (Definition lives in executor.go, shared by both files' logic.)

// AuditTaskArgs mirrors the TS deps.auditTask args.
type AuditTaskArgs struct {
	Task      *OrchestratorTask
	Board     *ProjectBoard
	RepoPath  string
	TimeoutMs int
	Log       RunLog
}

// AuditTaskResult is the deps.auditTask return: nil verdict = inconclusive
// run (worker crash, unparsable report) — treated as retryable, never as
// pass.
type AuditTaskResult = AuditVerdict

// RecoveryPlan mirrors the TS planRecovery return: the planner-written
// recovery contract granted when a task exhausts its retry budget.
type RecoveryPlan struct {
	Prompt             string
	AcceptanceCriteria []string
}

// PlanRecoveryArgs mirrors the TS deps.planRecovery args.
type PlanRecoveryArgs struct {
	Task  *OrchestratorTask
	Board *ProjectBoard
}

// PublishTaskPrArgs mirrors the TS deps.publishTaskPr args.
type PublishTaskPrArgs struct {
	Task     *OrchestratorTask
	Board    *ProjectBoard
	RepoPath string
	Log      RunLog
}

// SchedulerDeps mirrors the TS SchedulerDeps: dependency-injected executor
// hooks. nil optional fields mean the feature is absent (TS undefined).
type SchedulerDeps struct {
	// ExecuteTask dispatches one task (REQUIRED).
	ExecuteTask func(args ExecuteTaskArgs) (ExecuteTaskResult, error)
	// AuditTask independently audits an 'untrusted' task (read-only).
	// Nil = legacy mode (executor gates are the trust boundary). A nil
	// verdict return = inconclusive → retryable, never pass.
	AuditTask func(args AuditTaskArgs) (*AuditVerdict, error)
	// PlanRecovery grants a planner-written recovery contract when a task
	// exhausts its retry budget (LH manager lesson). Nil = failures go
	// terminal without recovery.
	PlanRecovery func(args PlanRecoveryArgs) (*RecoveryPlan, error)
	// PublishTaskPr publishes a verified task branch as a PR as soon as the
	// task reaches done, instead of waiting for the whole board to finish
	// (loop-69 lesson: task branch pushed but no PR ever created).
	PublishTaskPr func(args PublishTaskPrArgs) (string, error)
}

// GovernorSnapshot mirrors OsSnapshot (src/orchestrator/governor.ts).
// Named locally so the full governor port (sibling scope) can land its own
// OsSnapshot without colliding; runScheduler only consumes this subset.
type GovernorSnapshot struct {
	TotalMem int64     `json:"totalMem"`
	FreeMem  int64     `json:"freeMem"`
	CPUs     int       `json:"cpus"`
	LoadAvg  []float64 `json:"loadAvg,omitempty"`
}

// SchedulerGovernor mirrors the ResourceGovernor subset runScheduler uses
// (src/orchestrator/governor.ts). TODO(FR-GO-07): swap for the full
// governor port when it lands in this package.
type SchedulerGovernor interface {
	GetSnapshotSync() GovernorSnapshot
	EffectiveAuto(snapshot GovernorSnapshot) int
	FormatStatus(concurrencyInput string, effective int, snapshot GovernorSnapshot) string
	GetEstMemPerWorker() int
	// PressureWaitTimeoutMs is the throttle window before proceeding at
	// floor 1 (TS pressureWaitTimeoutMs, default 60_000).
	PressureWaitTimeoutMs() int
}

// GovernorAdapter adapts the full ResourceGovernor port (governor.go) to
// the SchedulerGovernor seam runScheduler consumes.
type GovernorAdapter struct{ G *ResourceGovernor }

func (a GovernorAdapter) GetSnapshotSync() GovernorSnapshot {
	snap := a.G.GetSnapshotSync()
	if snap == nil {
		return GovernorSnapshot{}
	}
	return governorToScheduler(*snap)
}

func (a GovernorAdapter) EffectiveAuto(snapshot GovernorSnapshot) int {
	return a.G.EffectiveAuto(governorFromScheduler(snapshot))
}

func (a GovernorAdapter) FormatStatus(concurrencyInput string, effective int, snapshot GovernorSnapshot) string {
	auto := concurrencyInput == "auto"
	conv := governorFromScheduler(snapshot)
	// The sample-age segment only renders for the governor's own cached
	// snapshot pointer; the seam snapshot mirrors it, so pass the cached
	// one when they agree field-by-field (LoadAvg included).
	if s := a.G.GetSnapshotSync(); s != nil && sameSnapshot(*s, conv) {
		return a.G.FormatStatus(ConcurrencyValue{Auto: auto, Value: 0}, effective, s)
	}
	return a.G.FormatStatus(ConcurrencyValue{Auto: auto, Value: 0}, effective, &conv)
}

func (a GovernorAdapter) GetEstMemPerWorker() int { return int(a.G.GetEstMemPerWorker()) }

func (a GovernorAdapter) PressureWaitTimeoutMs() int {
	return a.G.pressureWaitTimeoutMs
}

func governorToScheduler(s OsSnapshot) GovernorSnapshot {
	return GovernorSnapshot{TotalMem: s.TotalMem, FreeMem: s.FreeMem, CPUs: s.Cpus, LoadAvg: s.LoadAvg}
}

func governorFromScheduler(s GovernorSnapshot) OsSnapshot {
	return OsSnapshot{TotalMem: s.TotalMem, FreeMem: s.FreeMem, Cpus: s.CPUs, LoadAvg: s.LoadAvg}
}

// SchedulerConcurrency mirrors the TS `concurrency: number | 'auto'`.
type SchedulerConcurrency struct {
	Auto bool
	N    int
}

// SchedulerOptions mirrors the TS SchedulerOptions.
type SchedulerOptions struct {
	RepoPath string
	// Repo-local lessons file override (see loadLessons).
	LessonsFile string
	// Character budget for injected lessons (see loadLessons).
	LessonsMaxChars *float64
	Executor        WorkerName
	Concurrency     SchedulerConcurrency
	// Governor for auto mode. When nil and concurrency is auto, falls back
	// to 2.
	Governor SchedulerGovernor
	// Test hook: inject a fixed snapshot instead of live OS.
	GovernorSnapshot *GovernorSnapshot
	MaxTaskRetries   int
	// Recovery-contract grants per task before a failure goes terminal
	// (nil = 1).
	MaxRecoveries *int
	// Cumulative lifetime cap on dispatches per task across requeue rounds
	// and recovery contracts (Q17/Q36): nil/0 = unbounded (legacy).
	MaxTotalAttempts *int
	// Consecutive audits repeating the same primary gap that trigger early
	// recovery escalation (nil = 2 — SWE-agent decay data says a third
	// identical attempt rarely recovers).
	RepeatGapThreshold *int
	TimeoutMs          int
	// Hard cap on dispatch waves (SWE-agent L1). When reached, unfinished
	// tasks stay pending and the run ends with a resumable board.
	MaxWaves *int
	// OnWavePersisted is called after each wave with terminal task
	// transitions persisted (LangGraph pending-writes lesson).
	OnWavePersisted func(board *ProjectBoard)
}

// RunScheduler mirrors the TS runScheduler: wave loop over the board with
// bounded parallel dispatch. The board is mutated in place and returned
// (matching the TS caller contract).
func RunScheduler(board *ProjectBoard, opts SchedulerOptions, deps SchedulerDeps, log RunLog) *ProjectBoard {
	if log == nil {
		log = NoopRunLog{}
	}
	maxRecoveries := 1
	if opts.MaxRecoveries != nil {
		maxRecoveries = *opts.MaxRecoveries
	}
	repeatGapThreshold := 2
	if opts.RepeatGapThreshold != nil {
		repeatGapThreshold = *opts.RepeatGapThreshold
	}
	maxTotalAttempts := 0
	if opts.MaxTotalAttempts != nil {
		maxTotalAttempts = *opts.MaxTotalAttempts
	}

	// Q17/Q36: lifetime dispatches at/above the cap — the budget must not refresh.
	cumulativeCapped := func(task *OrchestratorTask) bool {
		return maxTotalAttempts > 0 && derefInt(task.TotalAttempts) >= maxTotalAttempts
	}

	// Grant one planner-written re-contract before a failure goes terminal.
	grantRecovery := func(task *OrchestratorTask) bool {
		if deps.PlanRecovery == nil || derefInt(task.Recoveries) >= maxRecoveries {
			return false
		}
		// Refuse the fresh per-round budget before spending planner work: the
		// task stays terminal (caller flips it to 'failed').
		if cumulativeCapped(task) {
			log.Warn(ledger.StageTask, fmt.Sprintf("%s recovery refused: %d lifetime attempt(s) >= cumulative cap %d", task.ID, derefInt(task.TotalAttempts), maxTotalAttempts), nil)
			return false
		}
		rec, err := deps.PlanRecovery(PlanRecoveryArgs{Task: task, Board: board})
		if err != nil {
			log.Warn(ledger.StageTask, fmt.Sprintf("%s recovery planning crashed: %s", task.ID, err.Error()), nil)
			return false
		}
		if rec == nil || rec.Prompt == "" {
			return false
		}
		task.Recoveries = intPtr(derefInt(task.Recoveries) + 1)
		task.Prompt = rec.Prompt
		if len(rec.AcceptanceCriteria) > 0 {
			cut := len(rec.AcceptanceCriteria)
			if cut > 10 {
				cut = 10
			}
			task.AcceptanceCriteria = rec.AcceptanceCriteria[:cut]
		}
		// fresh budget on the new contract; attemptSuffix keeps worktrees collision-free
		task.Attempts = 0
		task.Status = TaskStatusPending
		task.EvidenceGaps = nil
		task.Audit = nil
		task.FailureDetail = ""
		task.RepeatGaps = intPtr(0) // new contract, fresh streak

		log.Info(ledger.StageTask, fmt.Sprintf("%s granted recovery contract #%d", task.ID, derefInt(task.Recoveries)), nil)
		return true
	}

	isAuto := opts.Concurrency.Auto
	resolveEffective := func() int {
		if !isAuto {
			n := opts.Concurrency.N
			return maxInt(1, n)
		}
		if opts.Governor == nil {
			return 2
		}
		snap := opts.Governor.GetSnapshotSync()
		if opts.GovernorSnapshot != nil {
			snap = *opts.GovernorSnapshot
		}
		return opts.Governor.EffectiveAuto(snap)
	}

	var mu sync.Mutex
	activeWorkers := 0
	wave := 0
	for {
		board.Tasks = RecomputeReadiness(board.Tasks)
		var queue []*OrchestratorTask
		for i := range board.Tasks {
			t := &board.Tasks[i]
			if t.Status == TaskStatusReady ||
				(t.Status == TaskStatusFailed && t.Attempts < opts.MaxTaskRetries && !cumulativeCapped(t)) {
				queue = append(queue, t)
			}
		}
		if len(queue) == 0 {
			break
		}
		// Budget ceiling: stop dispatching, leave the board resumable
		if opts.MaxWaves != nil && wave >= *opts.MaxWaves {
			ids := make([]string, 0, len(queue))
			for _, t := range queue {
				ids = append(ids, t.ID)
			}
			log.Warn(ledger.StageTask,
				fmt.Sprintf("wave budget exhausted after %d wave(s); %d task(s) still queued", wave, len(queue)),
				[]ledger.KV{{Key: "queued", Value: ids}})
			break
		}
		wave++

		effective := resolveEffective()
		poolSize := maxInt(1, minInt(effective, len(queue)))
		if isAuto && opts.Governor != nil {
			snap := opts.Governor.GetSnapshotSync()
			if opts.GovernorSnapshot != nil {
				snap = *opts.GovernorSnapshot
			}
			line := opts.Governor.FormatStatus("auto", effective, snap)
			log.Info(ledger.StageGovernor,
				fmt.Sprintf("wave %d: %s (queue %d -> pool %d)", wave, line, len(queue), poolSize),
				[]ledger.KV{
					{Key: "effective", Value: effective},
					{Key: "queueLen", Value: len(queue)},
					{Key: "freeMem", Value: snap.FreeMem},
					{Key: "totalMem", Value: snap.TotalMem},
					{Key: "cpus", Value: snap.CPUs},
					{Key: "estPerWorker", Value: opts.Governor.GetEstMemPerWorker()},
				})
		}

		// Shared work queue pulled by poolSize workers (the TS closure-over-
		// queue.shift() pattern).
		qi := 0
		var wg sync.WaitGroup
		for w := 0; w < poolSize; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					// Governor throttle: wait for headroom before pulling next task
					if isAuto && opts.Governor != nil {
						pressureStart := time.Now()
						timeoutMs := opts.Governor.PressureWaitTimeoutMs()
						if timeoutMs == 0 {
							timeoutMs = 60_000
						}
						for {
							curEff := resolveEffective()
							mu.Lock()
							headroom := activeWorkers < curEff
							mu.Unlock()
							if headroom {
								break
							}
							if time.Since(pressureStart).Milliseconds() >= int64(timeoutMs) {
								mu.Lock()
								active := activeWorkers
								mu.Unlock()
								log.Info(ledger.StageGovernor,
									fmt.Sprintf("pressure wait timeout after %dms; proceeding at floor 1", timeoutMs),
									[]ledger.KV{{Key: "activeWorkers", Value: active}, {Key: "curEff", Value: curEff}})
								break
							}
							time.Sleep(50 * time.Millisecond)
						}
					}

					mu.Lock()
					if qi >= len(queue) {
						mu.Unlock()
						return
					}
					task := queue[qi]
					qi++
					activeWorkers++
					mu.Unlock()

					task.Status = TaskStatusDispatched
					task.Attempts++
					// lifetime counter (Q17/Q36): never reset by recovery grants or requeue
					task.TotalAttempts = intPtr(derefInt(task.TotalAttempts) + 1)
					log.Info(ledger.StageTask, fmt.Sprintf("Dispatching %s: %s", task.ID, task.Title), []ledger.KV{{Key: "attempt", Value: task.Attempts}})

					runDispatch(task, board, opts, deps, grantRecovery, repeatGapThreshold, maxRecoveries, log)

					mu.Lock()
					if activeWorkers > 0 {
						activeWorkers--
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if opts.OnWavePersisted != nil {
			opts.OnWavePersisted(board) // crash mid-run loses nothing already done
		}
	}
	board.Tasks = RecomputeReadiness(board.Tasks)
	return board
}

// runDispatch mirrors the TS per-task dispatch body: the legacy/audited/
// interrupted/failure branches including evidence-gap externalization,
// repeat-gap escalation, and the taskInterrupt ledger thread.
func runDispatch(task *OrchestratorTask, board *ProjectBoard, opts SchedulerOptions, deps SchedulerDeps, grantRecovery func(*OrchestratorTask) bool, repeatGapThreshold int, maxRecoveries int, log RunLog) {
	r, execErr := func() (r ExecuteTaskResult, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				// Convert panics into the TS exception channel.
				err = fmt.Errorf("%v", rec)
			}
		}()
		return deps.ExecuteTask(ExecuteTaskArgs{
			Task:            task,
			Board:           board,
			RepoPath:        opts.RepoPath,
			TimeoutMs:       opts.TimeoutMs,
			LessonsFile:     opts.LessonsFile,
			LessonsMaxChars: opts.LessonsMaxChars,
			Executor:        opts.Executor,
			Log:             log,
		})
	}()
	if execErr != nil {
		task.Status = TaskStatusFailed
		task.FailureDetail = execErr.Error()
		log.Error(ledger.StageTask, fmt.Sprintf("%s crashed: %s", task.ID, execErr.Error()), nil)
		return
	}

	if r.OK && deps.AuditTask == nil {
		// legacy mode: no auditor configured — executor gates are the trust boundary
		task.Status = TaskStatusDone
		task.FailureDetail = ""
		log.Info(ledger.StageTask, fmt.Sprintf("%s done", task.ID), nil)
		publishTaskPr(deps, task, board, opts, log)
		return
	}

	if r.OK && deps.AuditTask != nil {
		// Evidence gate: executor success is only a claim until audited
		task.Status = TaskStatusUntrusted
		var v *AuditVerdict
		av, auditErr := deps.AuditTask(AuditTaskArgs{Task: task, Board: board, RepoPath: opts.RepoPath, TimeoutMs: opts.TimeoutMs, Log: log})
		if auditErr != nil {
			log.Warn(ledger.StageAudit, fmt.Sprintf("%s audit crashed: %s", task.ID, auditErr.Error()), nil)
		}
		v = av
		if v != nil && v.Verdict == "pass" && v.Integrity == "clean" {
			task.Audit = v
			task.EvidenceGaps = nil
			task.Status = TaskStatusDone
			task.FailureDetail = ""
			log.Info(ledger.StageTask, fmt.Sprintf("%s done (audited)", task.ID), []ledger.KV{{Key: "criteria", Value: len(v.CriteriaResults)}})
			publishTaskPr(deps, task, board, opts, log)
			return
		}
		if v != nil && v.Verdict == "ask" {
			// Not a failure and not retryable: the branch waits for a human
			// answer via `orchestrate --resume --answer <id>=<text>`
			task.Audit = v
			task.Status = TaskStatusAsk
			task.FailureDetail = fmt.Sprintf("needs human input: %s", truncRunes(v.Summary, 200))
			log.Warn(ledger.StageTask, fmt.Sprintf("%s paused for human input", task.ID), []ledger.KV{{Key: "question", Value: truncRunes(v.Summary, 200)}})
			return
		}
		// Externalize the failure into state so the retry targets the
		// gap instead of redoing blind work (LH recovery lesson)
		gaps := []string{}
		if v != nil && v.Verdict == "fail" {
			for _, c := range v.CriteriaResults {
				if !c.Met {
					gaps = append(gaps, fmt.Sprintf("unmet: %s — %s", c.Criterion, truncRunes(c.Evidence, 200)))
				}
			}
		}
		if v != nil && v.Integrity != "clean" {
			gaps = append(gaps, fmt.Sprintf("integrity %s: workspace mutation or provenance concern", v.Integrity))
		}
		if v == nil {
			gaps = append(gaps, "audit inconclusive: worker crashed or report unparsable")
		}
		task.EvidenceGaps = gaps
		if v != nil {
			task.Audit = v
		}
		// L2 (SWE-agent §B.3.3): recovery odds decay when the same gap
		// repeats — escalate to a recovery re-contract early instead of
		// burning the remaining retry budget against the same wall.
		primaryGap := ""
		if len(gaps) > 0 {
			primaryGap = gaps[0]
		}
		prevPrimary := task.FailureDetail
		if primaryGap != "" && primaryGap == prevPrimary {
			task.RepeatGaps = intPtr(derefInt(task.RepeatGaps) + 1)
		} else {
			task.RepeatGaps = intPtr(1)
		}
		earlyEscalation := derefInt(task.RepeatGaps) >= repeatGapThreshold && derefInt(task.Recoveries) < maxRecoveries
		task.Status = TaskStatusPending
		if !(task.Attempts < opts.MaxTaskRetries && !earlyEscalation) {
			if grantRecovery(task) {
				task.Status = TaskStatusPending
			} else {
				task.Status = TaskStatusFailed
			}
		}

		task.FailureDetail = "audit failed"
		if len(gaps) > 0 {
			task.FailureDetail = gaps[0]
		}
		verdictLabel := "inconclusive"
		if v != nil {
			verdictLabel = v.Verdict + "/" + v.Integrity
		}
		log.Warn(ledger.StageTask, fmt.Sprintf("%s audit rejected (%s)", task.ID, verdictLabel), []ledger.KV{
			{Key: "attempt", Value: task.Attempts},
			{Key: "gaps", Value: len(gaps)},
		})
		return
	}

	if r.Interrupted {
		// Executor failure surface (PRD:775): N+ identical trailing trail.jsonl
		// failure signatures — taskInterrupt. Terminal; no retry budget, no
		// recovery grant. The compact post-mortem (goal, failure class, last
		// gate excerpt, attempts, trail hash) is threaded into the ledger (Q24
		// taxonomy mirror, PR #100) so archived boards carry replayable evidence
		// for the next bridge (closing the loop-57/58 diagnostic gap).
		task.Status = TaskStatusFailed
		task.FailureDetail = r.Detail
		if task.FailureDetail == "" {
			task.FailureDetail = r.FailureClass
		}
		if task.FailureDetail == "" {
			task.FailureDetail = "task interrupted"
		}
		interrupt := TaskInterrupt{
			FailureClass:    r.FailureClass,
			LastGateExcerpt: r.LastGateExcerpt,
			Attempts:        r.Attempts,
			TrailHash:       r.TrailHash,
		}
		if interrupt.FailureClass == "" {
			interrupt.FailureClass = "unknown"
		}
		if interrupt.Attempts == 0 {
			interrupt.Attempts = task.Attempts
		}
		task.Interrupt = &interrupt
		detail := truncRunes(r.Detail, 200)
		record := ledger.TaskInterruptRecord{
			TS:              queue.NowIso(),
			Kind:            "event",
			Event:           "taskInterrupt",
			TaskID:          task.ID,
			Attempt:         task.Attempts,
			Goal:            board.Goal,
			FailureClass:    interrupt.FailureClass,
			LastGateExcerpt: truncRunes(interrupt.LastGateExcerpt, 200),
			Attempts:        interrupt.Attempts,
			TrailHash:       interrupt.TrailHash,
		}
		if r.Detail != "" {
			record.Detail = &detail
		}
		ledger.AppendTaskInterruptRecord(opts.RepoPath, record)
		warnDetail := r.Detail
		if warnDetail == "" {
			warnDetail = r.FailureClass
		}
		log.Warn(ledger.StageTask, fmt.Sprintf("%s taskInterrupt: %s", task.ID, truncRunes(warnDetail, 200)), []ledger.KV{
			{Key: "failureClass", Value: r.FailureClass},
			{Key: "trailHash", Value: r.TrailHash},
		})
		return
	}

	// Plain failure: retryable within budget -> pending for the next wave;
	// else one recovery re-contract, then terminal failure
	task.FailureDetail = r.Detail
	task.Status = TaskStatusPending
	if task.Attempts >= opts.MaxTaskRetries {
		if grantRecovery(task) {
			task.Status = TaskStatusPending
		} else {
			task.Status = TaskStatusFailed
		}
	}
	data := []ledger.KV{}
	if r.Detail != "" {
		data = append(data, ledger.KV{Key: "detail", Value: truncRunes(r.Detail, 200)})
	}
	log.Warn(ledger.StageTask, fmt.Sprintf("%s failed (attempt %d)", task.ID, task.Attempts), data)
}

// publishTaskPr mirrors the inline `if (deps.publishTaskPr)` publish blocks:
// PR URL persisted as published evidence; publish failure is non-fatal.
func publishTaskPr(deps SchedulerDeps, task *OrchestratorTask, board *ProjectBoard, opts SchedulerOptions, log RunLog) {
	if deps.PublishTaskPr == nil {
		return
	}
	prURL, err := deps.PublishTaskPr(PublishTaskPrArgs{Task: task, Board: board, RepoPath: opts.RepoPath, Log: log})
	if err != nil {
		log.Warn(ledger.StagePublish, fmt.Sprintf("%s PR publish failed (non-fatal): %s", task.ID, err.Error()), nil)
		return
	}
	if prURL != "" {
		task.PrURL = prURL
	}
}

func maxRecoveriesOf(opts SchedulerOptions) int {
	if opts.MaxRecoveries != nil {
		return *opts.MaxRecoveries
	}
	return 1
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sameSnapshot(a, b OsSnapshot) bool {
	if a.TotalMem != b.TotalMem || a.FreeMem != b.FreeMem || a.Cpus != b.Cpus || len(a.LoadAvg) != len(b.LoadAvg) {
		return false
	}
	for i := range a.LoadAvg {
		if a.LoadAvg[i] != b.LoadAvg[i] {
			return false
		}
	}
	return true
}
