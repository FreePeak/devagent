// Package orchestrator is the Go port of src/orchestrator (FR-GO-07,
// issue #194): a planner agent decomposes a goal into a dependency DAG of
// small precise tasks; executor agents implement them in isolated
// worktrees; the board persists so runs survive restarts.
//
// The run-ledger types live in internal/ledger (FR-GO-04, issue #211);
// this package imports them instead of duplicating the record shapes.
package orchestrator

// WorkerName mirrors the TS WorkerName union (src/types.ts). It is a plain
// string here because worker-name validation belongs to the config port;
// the orchestrator only carries the value.
type WorkerName = string

// TaskStatus mirrors the TS TaskStatus union.
//
// 'untrusted': executor finished and its own gates passed, but no
// independent audit has confirmed completion yet (LongHorizon-Harness
// lesson: executor claims never directly become trusted state). 'ask': a
// decision needs human input before this branch can proceed.
type TaskStatus string

// TaskStatus values mirror the TS literals byte-for-byte.
const (
	TaskStatusPending    TaskStatus = "pending"
	TaskStatusReady      TaskStatus = "ready"
	TaskStatusDispatched TaskStatus = "dispatched"
	TaskStatusUntrusted  TaskStatus = "untrusted"
	TaskStatusDone       TaskStatus = "done"
	TaskStatusFailed     TaskStatus = "failed"
	TaskStatusBlocked    TaskStatus = "blocked"
	TaskStatusAsk        TaskStatus = "ask"
)

// CriterionResult mirrors the TS interface: per-criterion evidence from an
// independent auditor (read-only).
type CriterionResult struct {
	Criterion string `json:"criterion"`
	Met       bool   `json:"met"`
	// Environmental proof: command output, file content, test result.
	Evidence string `json:"evidence"`
}

// AuditVerdict mirrors the TS interface: independent audit verdict. A task
// may only flip to 'done' when verdict is 'pass' AND integrity is 'clean'
// (LH-Harness rule: a mutated-workspace report can never support a
// completed record).
type AuditVerdict struct {
	// 'pass' | 'fail' | 'ask' — 'ask': completion cannot be judged without
	// human input/authorization.
	Verdict string `json:"verdict"`
	// 'clean' | 'suspect' | 'violation'.
	Integrity       string            `json:"integrity"`
	CriteriaResults []CriterionResult `json:"criteriaResults"`
	// Auditor's one-paragraph account of what it actually checked.
	Summary string `json:"summary"`
}

// TaskInterrupt mirrors the TS inline interrupt object (PRD:775): set when
// the executor aborted this task via taskInterrupt — N+ identical trailing
// trail.jsonl failure signatures caused the worker to be aborted and the
// post-mortem threaded into the ledger. The compact evidence survives
// board archive for the next bridge.
type TaskInterrupt struct {
	FailureClass    string `json:"failureClass"`
	LastGateExcerpt string `json:"lastGateExcerpt"`
	Attempts        int    `json:"attempts"`
	TrailHash       string `json:"trailHash"`
}

// OrchestratorTask mirrors the TS OrchestratorTask interface. Optional TS
// fields are pointer types so "absent" round-trips as byte-compatible JSON
// (omitted, not null).
type OrchestratorTask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Precise implementation instructions for the executor worker.
	Prompt string `json:"prompt"`
	// Machine-checkable acceptance criteria. The auditor verifies each
	// criterion independently; itemized criteria make "confidently verified
	// wrong answer" less likely than one freeform expected-output string.
	AcceptanceCriteria []string `json:"acceptanceCriteria,omitempty"`
	// Things the executor must NOT do inside its bounded contract.
	BoundaryConstraints []string `json:"boundaryConstraints,omitempty"`
	// Machine-verifiable completion signal (CrewAI expected_output lesson).
	ExpectedOutput string `json:"expectedOutput,omitempty"`
	// Task ids that must be done before this one becomes ready.
	DependsOn []string   `json:"dependsOn"`
	Status    TaskStatus `json:"status"`
	Attempts  int        `json:"attempts"`
	// Lifetime dispatch counter across every requeue round and recovery
	// contract (Q17/Q36): incremented beside every Attempts += 1, never
	// reset. Both budget-refresh sites (recovery grant, parked requeue)
	// refuse above the configurable cumulative cap so an exhausted task
	// cannot re-burn a fresh maxTaskRetries budget every round (loops 53-55
	// re-burn class). Optional: boards persisted before this field carry no
	// lifetime history.
	TotalAttempts *int   `json:"totalAttempts,omitempty"`
	WorktreePath  string `json:"worktreePath,omitempty"`
	FailureDetail string `json:"failureDetail,omitempty"`
	// Latest independent audit; set once the task leaves 'untrusted' via
	// audit.
	Audit *AuditVerdict `json:"audit,omitempty"`
	// Targeted re-contracting: what the previous attempt failed to prove,
	// so the retry closes the gap instead of redoing blind work.
	EvidenceGaps []string `json:"evidenceGaps,omitempty"`
	// How many planner-written recovery contracts this task has been granted.
	Recoveries *int `json:"recoveries,omitempty"`
	// Consecutive audit failures repeating the same primary gap (SWE-agent
	// lesson: recovery odds decay after repeated failures — escalate to a
	// recovery re-contract instead of burning retries on the same wall).
	RepeatGaps *int `json:"repeatGaps,omitempty"`
	// Executor failure surface (PRD:775).
	Interrupt *TaskInterrupt `json:"interrupt,omitempty"`
	// Per-task PR URL (PR #71 publishTaskPr flow): set when the scheduler
	// published this task's branch as a PR on reaching done. Persisted as
	// published evidence and used to gate the legacy all-done local
	// merge-back so a fully-done board never double-merges the same
	// branches.
	PrURL string `json:"prUrl,omitempty"`
}

// AttemptSuffix mirrors the TS attemptSuffix: branch/worktree attempt
// suffix; recovery grants extend it to stay collision-free.
func AttemptSuffix(attempts int, recoveries int) string {
	suffix := "a" + itoa(attempts)
	if recoveries > 0 {
		suffix += "r" + itoa(recoveries)
	}
	return suffix
}

// ProjectBoardRoles mirrors the TS roles object.
type ProjectBoardRoles struct {
	Planner  WorkerName `json:"planner"`
	Executor WorkerName `json:"executor"`
	// Independent auditor role; absent boards predate evidence gating.
	Auditor WorkerName `json:"auditor,omitempty"`
}

// ProjectBoard mirrors the TS ProjectBoard interface.
type ProjectBoard struct {
	Goal      string             `json:"goal"`
	CreatedAt string             `json:"createdAt"`
	UpdatedAt string             `json:"updatedAt"`
	Roles     ProjectBoardRoles  `json:"roles"`
	Tasks     []OrchestratorTask `json:"tasks"`
	// Cross-board retry memory (Q27): executor failure class carried from
	// the prior archived board when the same goal is re-bridged, so the
	// scout deprioritizes the goal until the root cause ships instead of
	// burning a fresh attempt budget on each requeue round. Mirrors the
	// ExecutorFailureClass taxonomy from src/types.ts (stored as string
	// like task.interrupt.failureClass).
	FailureClass string `json:"failureClass,omitempty"`
}

// BoardFile mirrors the TS BOARD_FILE constant.
const BoardFile = ".devagent-project.json"

// RecomputeReadiness mirrors the TS recomputeReadiness: tasks whose
// dependencies are all done become ready.
//
// 'pending' and dependency-induced 'blocked' are both derived states:
// re-derive every pass so a task blocked by an upstream 'ask'/'blocked' is
// promoted once that upstream reaches 'done' (loop-55: T2 answered -> T3
// stayed blocked forever because only 'pending' was recomputed).
func RecomputeReadiness(tasks []OrchestratorTask) []OrchestratorTask {
	byID := make(map[string]OrchestratorTask, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	out := make([]OrchestratorTask, len(tasks))
	for i, t := range tasks {
		if t.Status != TaskStatusPending && t.Status != TaskStatusBlocked {
			out[i] = t
			continue
		}
		dangling := false
		// deps.every(...) is TRUE for an empty array: a no-deps pending
		// task promotes to 'ready' (TS parity).
		allDone := true
		neverRunnable := false
		for _, d := range t.DependsOn {
			dep, ok := byID[d]
			if !ok {
				// dangling dependency reference: block rather than run blind
				dangling = true
				break
			}
			if dep.Status != TaskStatusDone {
				allDone = false
			}
			if dep.Status == TaskStatusFailed || dep.Status == TaskStatusBlocked || dep.Status == TaskStatusAsk {
				// upstream failed permanently or paused on human input ->
				// this can never run
				neverRunnable = true
			}
		}
		switch {
		case dangling:
			t.Status = TaskStatusBlocked
		case allDone:
			t.Status = TaskStatusReady
		case neverRunnable:
			t.Status = TaskStatusBlocked
		}
		out[i] = t
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
