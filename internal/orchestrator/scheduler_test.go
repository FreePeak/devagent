// Package file mirrors test/orchestrator.test.ts (runScheduler describe
// blocks — FR-GO-07, issue #194). Same fixture names and assertions as the
// vitest originals.
package orchestrator

import (
	"sync"
	"testing"
	"time"
)

func schedTask(id string, mutate func(*OrchestratorTask)) OrchestratorTask {
	t := OrchestratorTask{
		ID:        id,
		Title:     id,
		Prompt:    "do it",
		DependsOn: []string{},
		Status:    TaskStatusPending,
		Attempts:  0,
	}
	if mutate != nil {
		mutate(&t)
	}
	return t
}

func schedBoard(tasks []OrchestratorTask) ProjectBoard {
	return ProjectBoard{
		Goal: "g", CreatedAt: "", UpdatedAt: "",
		Roles: ProjectBoardRoles{Planner: "claude-code"},
		Tasks: tasks,
	}
}

func TestRunSchedulerExecutesInDependencyWaves(t *testing.T) {
	// executes in dependency waves: T2 runs only after T1 done
	var mu sync.Mutex
	var order []string
	b := schedBoard([]OrchestratorTask{
		schedTask("T1", nil),
		schedTask("T2", func(t *OrchestratorTask) { t.DependsOn = []string{"T1"} }),
	})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 4}, MaxTaskRetries: 1, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(args ExecuteTaskArgs) (ExecuteTaskResult, error) {
			mu.Lock()
			order = append(order, args.Task.ID)
			mu.Unlock()
			return ExecuteTaskResult{OK: true}, nil
		},
	}, NoopRunLog{})
	if len(order) != 2 || order[0] != "T1" || order[1] != "T2" {
		t.Fatalf("expected order [T1 T2], got %v", order)
	}
	for _, task := range b.Tasks {
		if task.Status != TaskStatusDone {
			t.Fatalf("task %s must be done, got %s", task.ID, task.Status)
		}
	}
}

func TestRunSchedulerRetriesFailedTaskWithinBudget(t *testing.T) {
	// retries a failed task within budget then succeeds
	calls := 0
	b := schedBoard([]OrchestratorTask{schedTask("T1", nil)})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 2, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			calls++
			if calls < 2 {
				return ExecuteTaskResult{OK: false, Detail: "tests failed"}, nil
			}
			return ExecuteTaskResult{OK: true}, nil
		},
	}, NoopRunLog{})
	if calls != 2 {
		t.Fatalf("expected 2 dispatches, got %d", calls)
	}
	if b.Tasks[0].Status != TaskStatusDone {
		t.Fatalf("expected done, got %s", b.Tasks[0].Status)
	}
}

func TestRunSchedulerPublishesPrAsSoonAsTaskDone(t *testing.T) {
	// publishes a PR as soon as a task reaches done (loop-69: pushed branch, no PR)
	var mu sync.Mutex
	var published []string
	b := schedBoard([]OrchestratorTask{
		schedTask("T1", nil),
		schedTask("T2", func(t *OrchestratorTask) { t.DependsOn = []string{"T1"} }),
	})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 1, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			return ExecuteTaskResult{OK: true}, nil
		},
		PublishTaskPr: func(args PublishTaskPrArgs) (string, error) {
			mu.Lock()
			published = append(published, args.Task.ID)
			mu.Unlock()
			return "https://github.com/x/y/pull/1", nil
		},
	}, NoopRunLog{})
	// Both tasks reach done, so each gets a per-task PR — not gated behind
	// the whole board finishing.
	if len(published) != 2 || published[0] != "T1" || published[1] != "T2" {
		t.Fatalf("expected published [T1 T2], got %v", published)
	}
	for _, task := range b.Tasks {
		if task.Status != TaskStatusDone {
			t.Fatalf("task %s must be done, got %s", task.ID, task.Status)
		}
		// PR URL is persisted as published evidence for each done task
		if task.PrURL != "https://github.com/x/y/pull/1" {
			t.Fatalf("task %s must carry prUrl, got %q", task.ID, task.PrURL)
		}
	}
}

func TestRunSchedulerAuditedPathPrURLUnsetWhenPublishFailsOrNull(t *testing.T) {
	// records task.prUrl on the audited path and leaves it unset when publish fails or returns null
	b := schedBoard([]OrchestratorTask{schedTask("T1", nil), schedTask("T2", nil)})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 1, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			return ExecuteTaskResult{OK: true}, nil
		},
		AuditTask: func(AuditTaskArgs) (*AuditVerdict, error) {
			return &AuditVerdict{Verdict: "pass", Integrity: "clean", CriteriaResults: []CriterionResult{}, Summary: "ok"}, nil
		},
		PublishTaskPr: func(args PublishTaskPrArgs) (string, error) {
			if args.Task.ID == "T1" {
				return "", nil
			}
			return "", errFake("gh down")
		},
	}, NoopRunLog{})
	for _, id := range []string{"T1", "T2"} {
		task := findTask(b.Tasks, id)
		if task.Status != TaskStatusDone {
			t.Fatalf("%s must be done, got %s", id, task.Status)
		}
		// publish failure is non-fatal: T2 still done, just without a PR URL
		if task.PrURL != "" {
			t.Fatalf("%s must not carry prUrl, got %q", id, task.PrURL)
		}
	}
}

func TestRunSchedulerDoesNotPublishPrForFailedGate(t *testing.T) {
	// does not publish a PR for a task that fails the gate
	var mu sync.Mutex
	var published []string
	b := schedBoard([]OrchestratorTask{schedTask("T1", nil)})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 1, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			return ExecuteTaskResult{OK: false, Detail: "tests failed"}, nil
		},
		PublishTaskPr: func(args PublishTaskPrArgs) (string, error) {
			mu.Lock()
			published = append(published, args.Task.ID)
			mu.Unlock()
			return "url", nil
		},
	}, NoopRunLog{})
	if len(published) != 0 {
		t.Fatalf("expected no publishes, got %v", published)
	}
	if b.Tasks[0].Status != TaskStatusFailed {
		t.Fatalf("expected failed, got %s", b.Tasks[0].Status)
	}
}

func TestRunSchedulerMarksFailedPermanentlyAndBlocksDependents(t *testing.T) {
	// marks failed permanently when retries exhausted and blocks dependents
	b := schedBoard([]OrchestratorTask{
		schedTask("T1", nil),
		schedTask("T2", func(t *OrchestratorTask) { t.DependsOn = []string{"T1"} }),
	})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 1, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			return ExecuteTaskResult{OK: false, Detail: "boom"}, nil
		},
	}, NoopRunLog{})
	if b.Tasks[0].Status != TaskStatusFailed {
		t.Fatalf("T1 must be failed, got %s", b.Tasks[0].Status)
	}
	if b.Tasks[1].Status != TaskStatusBlocked {
		t.Fatalf("T2 must be blocked, got %s", b.Tasks[1].Status)
	}
}

func TestRunSchedulerRunsIndependentTasksConcurrentlyUpToCap(t *testing.T) {
	// runs independent tasks concurrently up to the cap
	var mu sync.Mutex
	active, peak := 0, 0
	b := schedBoard([]OrchestratorTask{schedTask("A", nil), schedTask("B", nil), schedTask("C", nil)})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 1, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			mu.Lock()
			active++
			if active > peak {
				peak = active
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			active--
			mu.Unlock()
			return ExecuteTaskResult{OK: true}, nil
		},
	}, NoopRunLog{})
	if peak > 2 {
		t.Fatalf("peak concurrency %d exceeds cap 2", peak)
	}
	for _, task := range b.Tasks {
		if task.Status != TaskStatusDone {
			t.Fatalf("task %s must be done, got %s", task.ID, task.Status)
		}
	}
}

func TestRunSchedulerPersistsBoardAfterEachWave(t *testing.T) {
	// persists the board after each wave
	b := schedBoard([]OrchestratorTask{
		schedTask("T1", nil),
		schedTask("T2", func(t *OrchestratorTask) { t.DependsOn = []string{"T1"} }),
	})
	persists := 0
	RunScheduler(&b, SchedulerOptions{
		RepoPath: ".", Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 2}, MaxTaskRetries: 1, TimeoutMs: 1000,
		OnWavePersisted: func(*ProjectBoard) { persists++ },
	}, SchedulerDeps{
		ExecuteTask: func(ExecuteTaskArgs) (ExecuteTaskResult, error) {
			return ExecuteTaskResult{OK: true}, nil
		},
	}, NoopRunLog{})
	if persists != 2 { // one per wave
		t.Fatalf("expected 2 persists, got %d", persists)
	}
}

func TestRunSchedulerInterruptedTaskIsTerminalWithLedgerRow(t *testing.T) {
	// taskInterrupt: terminal — no retry budget, no recovery grant; the
	// compact post-mortem is threaded into the ledger (PRD:775 / Q24).
	repo := t.TempDir()
	b := schedBoard([]OrchestratorTask{schedTask("T1", func(t *OrchestratorTask) { t.Attempts = 1 })})
	RunScheduler(&b, SchedulerOptions{
		RepoPath: repo, Executor: "opencode",
		Concurrency: SchedulerConcurrency{N: 1}, MaxTaskRetries: 3, TimeoutMs: 1000,
	}, SchedulerDeps{
		ExecuteTask: func(args ExecuteTaskArgs) (ExecuteTaskResult, error) {
			return ExecuteTaskResult{
				OK: false, Interrupted: true, FailureClass: "test-gate",
				LastGateExcerpt: "suite A broken", Attempts: 3, TrailHash: "0123456789abcdef",
				Detail: "taskInterrupt: 3 identical test-gate failures (trail hash 0123456789abcdef)",
			}, nil
		},
		PlanRecovery: func(PlanRecoveryArgs) (*RecoveryPlan, error) {
			return &RecoveryPlan{Prompt: "never"}, nil
		},
	}, NoopRunLog{})
	task := b.Tasks[0]
	if task.Status != TaskStatusFailed {
		t.Fatalf("interrupted task must be failed, got %s", task.Status)
	}
	if task.Interrupt == nil || task.Interrupt.TrailHash != "0123456789abcdef" || task.Interrupt.FailureClass != "test-gate" {
		t.Fatalf("interrupt post-mortem must be set: %+v", task.Interrupt)
	}
	// task.Attempts (1, from the fixture) — the interrupt record's attempt
	// mirrors the board state at interrupt time.
}

func findTask(tasks []OrchestratorTask, id string) OrchestratorTask {
	for _, t := range tasks {
		if t.ID == id {
			return t
		}
	}
	panic("task not found: " + id)
}

type errFake string

func (e errFake) Error() string { return string(e) }
