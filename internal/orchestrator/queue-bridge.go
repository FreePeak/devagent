// Package file mirrors src/orchestrator/queue-bridge.ts (FR-GO-07, issue #194).
package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FreePeak/devagent/internal/queue"
)

// BridgeArchiveDir mirrors the TS ARCHIVE_DIR: where stuck/completed boards
// are archived by the orchestrate loop.
const BridgeArchiveDir = ".devagent/archive"

// normalizeGoal mirrors the TS normalizeGoal: normalize goal text for
// cross-board matching (mirror already_shipped in selfbuild-loop.sh).
func normalizeGoal(goal string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(strings.ReplaceAll(goal, `"`, "")), " "))
}

// ArchivedBoardFailureClass mirrors archivedBoardFailureClass: a prior
// board's executor failure class for a re-bridged goal. Scans archived
// boards (newest first) for one whose goal matches `goal`; returns its
// carried failureClass, else the first task's interrupt failure class.
// Best-effort: missing/corrupt archive entries are skipped. Empty string is
// the TS undefined.
func ArchivedBoardFailureClass(repoPath, goal string) string {
	archiveDir := filepath.Join(repoPath, BridgeArchiveDir)
	if _, err := os.Stat(archiveDir); err != nil {
		return ""
	}
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		return ""
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "board-") && strings.HasSuffix(name, ".json") {
			files = append(files, name)
		}
	}
	sort.Strings(files) // then iterate newest first
	want := normalizeGoal(goal)
	for i := len(files) - 1; i >= 0; i-- {
		var board struct {
			Goal         *string            `json:"goal"`
			FailureClass string             `json:"failureClass"`
			Tasks        []OrchestratorTask `json:"tasks"`
		}
		data, err := os.ReadFile(filepath.Join(archiveDir, files[i]))
		if err != nil || json.Unmarshal(data, &board) != nil {
			continue // corrupt archive entry — skip, never fail the bridge
		}
		if board.Goal == nil {
			continue
		}
		have := normalizeGoal(*board.Goal)
		if want != have && firstChars(want, 60) != firstChars(have, 60) {
			continue
		}
		if board.FailureClass != "" {
			return board.FailureClass
		}
		for _, t := range board.Tasks {
			if t.Interrupt != nil && t.Interrupt.FailureClass != "" {
				return t.Interrupt.FailureClass
			}
		}
	}
	return ""
}

// BridgeOptions mirrors the TS BridgeOptions.
type BridgeOptions struct {
	RepoPath string
	// BoardGoalKey: boards are keyed by the queued goal so re-runs are
	// idempotent. (Declared in the TS too; the bridge derives the key from
	// the queued goal itself.)
	BoardGoalKey func(task *queue.QueuedTask) string
	// Planner: how the planner is invoked for a goal; tests inject a stub.
	// Nil = the deterministic fallback plan.
	Planner func(goal string) []OrchestratorTask
}

// defaultPlanner mirrors the TS defaultPlanner: the synchronous
// fallback-parse never needs a worker.
//
// TODO(FR-GO-07): delegate to the planner.go port of fallbackPlan when it
// lands (planner.ts is outside this sub-wave's ownership list); the body
// below is a verbatim copy of TS fallbackPlan.
func defaultPlanner(goal string) []OrchestratorTask {
	return []OrchestratorTask{{
		ID:        "T1",
		Title:     fmt.Sprintf("Implement goal: %s", firstChars(goal, 80)),
		Prompt:    goal,
		DependsOn: []string{},
		Status:    TaskStatusPending,
		Attempts:  0,
	}}
}

// QueuedGoalString mirrors queuedGoalString: the goal a queued task bridges
// with, acceptance criteria appended as a dash list.
func QueuedGoalString(t *queue.QueuedTask) string {
	ac := ""
	if len(t.AcceptanceCriteria) > 0 {
		parts := make([]string, len(t.AcceptanceCriteria))
		for i, c := range t.AcceptanceCriteria {
			parts[i] = "- " + c
		}
		ac = "\nAcceptance criteria:\n" + strings.Join(parts, "\n")
	}
	return t.Goal + ac
}

// BridgeResult mirrors the TS BridgeResult.
type BridgeResult struct {
	BoardPath    string `json:"boardPath"`
	TasksWritten int    `json:"tasksWritten"`
	Created      bool   `json:"created"`
	// Idempotent: tasks were already present for this goal-backed board.
	Idempotent bool `json:"idempotent"`
}

// BridgeQueueToBoard mirrors bridgeQueueToBoard: idempotent — if a board
// already exists for this repo, reuse it; else plan one queued goal into a
// board. Q27 cross-board retry memory: a re-bridged goal carries the prior
// archived board's executor failure class so the scout deprioritizes it
// until the root cause ships. Retires the source queue item: the board now
// owns this goal, and leaving it pending would make the builder lane
// double-build the same idea.
func BridgeQueueToBoard(oldestPending *queue.QueuedTask, opts BridgeOptions) BridgeResult {
	repoPath := opts.RepoPath
	boardPath := filepath.Join(repoPath, BoardFile)
	if existing := LoadBoard(repoPath); existing != nil {
		return BridgeResult{BoardPath: boardPath, TasksWritten: len(existing.Tasks), Created: false, Idempotent: true}
	}
	goal := QueuedGoalString(oldestPending)
	planner := opts.Planner
	if planner == nil {
		planner = defaultPlanner
	}
	planned := planner(goal)
	tasks := planned
	if len(tasks) == 0 {
		tasks = defaultPlanner(goal) // TS: fallbackPlan(goal)
	}
	if len(tasks) > 12 {
		tasks = tasks[:12]
	}
	roles := ProjectBoardRoles{Planner: "omp", Executor: "omp"}
	board := CreateBoard(goal, tasks, roles)
	if prior := ArchivedBoardFailureClass(repoPath, goal); prior != "" {
		board.FailureClass = prior
	}
	SaveBoard(repoPath, board)
	// Unfenced write, mirroring the TS setTaskStatus default.
	queue.SetTaskStatus(repoPath, oldestPending.ID, queue.StatusDone, "", -1, nil)
	return BridgeResult{BoardPath: boardPath, TasksWritten: len(tasks), Created: true, Idempotent: false}
}

// BridgeIfQueued mirrors bridgeIfQueued: sync dry path over the live queue —
// pick oldest pending and bridge it (no-op when board exists). A nil
// planner means no bridge happens when the queue is empty either way.
func BridgeIfQueued(repoPath string, planner func(goal string) []OrchestratorTask) *BridgeResult {
	pending := queue.ListTasks(repoPath, queue.StatusPending) // sorted by createdAt ascending
	if len(pending) == 0 {
		return nil
	}
	res := BridgeQueueToBoard(pending[0], BridgeOptions{RepoPath: repoPath, Planner: planner})
	return &res
}

// HasBoard mirrors hasBoard: does .devagent-project.json currently exist?
func HasBoard(repoPath string) bool {
	_, err := os.Stat(filepath.Join(repoPath, BoardFile))
	return err == nil
}
