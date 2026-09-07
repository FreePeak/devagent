// Package file mirrors src/orchestrator/store.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/devagent/internal/queue"
)

// HumanAnswerResult mirrors the TS applyHumanAnswer result ({ok, note}).
type HumanAnswerResult struct {
	OK   bool
	Note string
}

// AnswerEndpointResult mirrors the TS AnswerEndpointResult: the HTTP-shaped
// result of applyAnswerToRepo.
type AnswerEndpointResult struct {
	Status int
	Body   map[string]any
}

// LoadBoard mirrors loadBoard: read <repoPath>/.devagent-project.json, or nil
// when missing/corrupted. A board without an array `tasks` or a string `goal`
// counts as corrupted (caller decides to re-plan).
//
// Deviation vs the TS: the TS returns the parsed object as-is (loose task
// field types, extra unknown fields ride along). Go returns a typed
// *ProjectBoard; boards with field-type mismatches or unknown extra fields
// are normalized to the struct shape (or nil on type mismatch).
func LoadBoard(repoPath string) *ProjectBoard {
	file := filepath.Join(repoPath, BoardFile)
	if _, err := os.Stat(file); err != nil {
		return nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	// TS: JSON.parse then `Array.isArray(raw.tasks) && typeof raw.goal === 'string'`.
	var probe struct {
		Goal  json.RawMessage `json:"goal"`
		Tasks json.RawMessage `json:"tasks"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return nil // corrupted board: caller decides to re-plan
	}
	var goal string
	if len(probe.Goal) == 0 || json.Unmarshal(probe.Goal, &goal) != nil {
		return nil
	}
	var tasksArr []json.RawMessage
	if len(probe.Tasks) == 0 || json.Unmarshal(probe.Tasks, &tasksArr) != nil {
		return nil
	}
	var board ProjectBoard
	if json.Unmarshal(data, &board) != nil {
		return nil
	}
	return &board
}

// SaveBoard mirrors saveBoard: bump updatedAt, recompute readiness, write the
// whole board atomically (tmp+rename) so a crash mid-write never corrupts
// state, as 2-space JSON with a trailing newline.
func SaveBoard(repoPath string, board *ProjectBoard) {
	next := *board
	next.UpdatedAt = queue.NowIso()
	next.Tasks = RecomputeReadiness(next.Tasks)
	file := filepath.Join(repoPath, BoardFile)
	writeJSONAtomic(file, &next)
}

// CreateBoard mirrors createBoard: a fresh board with timestamps and roles.
func CreateBoard(goal string, tasks []OrchestratorTask, roles ProjectBoardRoles) *ProjectBoard {
	now := queue.NowIso()
	return &ProjectBoard{
		Goal:      goal,
		CreatedAt: now,
		UpdatedAt: now,
		Roles:     roles,
		Tasks:     tasks,
	}
}

// writeJSONAtomic is the package-wide atomic JSON write: 2-space indent,
// no HTML escaping (JSON.stringify does not escape <>&), trailing newline,
// tmp+rename.
func writeJSONAtomic(path string, v any) {
	data := jsonToBytes(v)
	if data == nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.WriteFile(path, data, 0o644)
		_ = os.Remove(tmp)
	}
}

// jsonToBytes marshals v the way TS JSON.stringify does: no HTML escaping.
func jsonToBytes(v any) []byte {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return []byte(buf.String())
}

// FormatPlanOnly mirrors formatPlanOnly: a human-readable plan-only preview
// with no state notes.
func FormatPlanOnly(board *ProjectBoard) string {
	lines := []string{"Contracts:"}
	for _, t := range board.Tasks {
		lines = append(lines, "", fmt.Sprintf("[%s] %s (%s)", t.ID, t.Title, t.Status), t.Prompt)
		if len(t.AcceptanceCriteria) > 0 {
			lines = append(lines, "  criteria:\n    "+strings.Join(t.AcceptanceCriteria, "\n    "))
		}
		if len(t.BoundaryConstraints) > 0 {
			lines = append(lines, "  constraints:\n    "+strings.Join(t.BoundaryConstraints, "\n    "))
		}
	}
	return strings.Join(lines, "\n")
}

// ApplyHumanAnswer mirrors applyHumanAnswer: fold a human answer into a task
// paused for input and put it back in the pending queue.
func ApplyHumanAnswer(board *ProjectBoard, taskId, answer string) HumanAnswerResult {
	id := strings.TrimSpace(taskId)
	text := strings.TrimSpace(answer)
	for i := range board.Tasks {
		t := &board.Tasks[i]
		if t.ID != id || t.Status != TaskStatusAsk || text == "" {
			continue
		}
		detail := t.FailureDetail
		if detail == "" {
			detail = "prior question"
		}
		t.Prompt += fmt.Sprintf("\n\nHuman answer to %q: %s", detail, text)
		t.EvidenceGaps = nil
		t.Status = TaskStatusPending
		return HumanAnswerResult{OK: true, Note: fmt.Sprintf("Answered %s; task back in queue.", id)}
	}
	return HumanAnswerResult{OK: false, Note: fmt.Sprintf("no task '%s' paused for input (or empty answer)", id)}
}

// ApplyAnswerToRepo mirrors applyAnswerToRepo: the store-side answer endpoint.
// Status codes mirror the TS contract: 400 empty answer, 404 no board,
// 409 board did not accept, 200 folded + requeued.
func ApplyAnswerToRepo(repoPath, taskId, answer string) AnswerEndpointResult {
	if strings.TrimSpace(answer) == "" {
		return AnswerEndpointResult{Status: 400, Body: map[string]any{"ok": false, "note": "empty answer"}}
	}
	board := LoadBoard(repoPath)
	if board == nil {
		return AnswerEndpointResult{Status: 404, Body: map[string]any{"ok": false, "note": "no project board for this repo"}}
	}
	res := ApplyHumanAnswer(board, taskId, answer)
	if !res.OK {
		return AnswerEndpointResult{Status: 409, Body: map[string]any{"ok": false, "note": res.Note}}
	}
	SaveBoard(repoPath, board)
	return AnswerEndpointResult{Status: 200, Body: map[string]any{"ok": true, "note": res.Note}}
}
