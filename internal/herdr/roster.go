// Operator visibility: pane roster + attach (FR-VIS-02/03), ported from
// src/integrations/herdr.ts.
package herdr

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// PaneEnvOpAttach mirrors PANE_ENV_OP_ATTACH: env var a pane may set to flag
// that an operator is attached (herdr-side attach detection). Detection stays
// herdr-side; devagent only records/traces it.
const PaneEnvOpAttach = "DEVAGENT_OPERATOR_ATTACHED"

// SessionPaneInfo mirrors SessionPaneInfo: one operator-observable worker pane
// (FR-VIS-02). Field order is the `sessions --json` output contract.
type SessionPaneInfo struct {
	TaskID      string `json:"taskId"`
	Role        string `json:"role"`
	Worker      string `json:"worker"`
	PaneID      string `json:"paneId"`
	WorkspaceID string `json:"workspaceId"`
	Label       string `json:"label"`
	Cwd         string `json:"cwd"`
	AgentStatus string `json:"agentStatus"`
	State       string `json:"state"` // "running" | "idle" | "stale"
	// StartedAt: pane creation timestamp as reported by herdr; "" when
	// unavailable. Never fabricated: only what herdr reports.
	StartedAt string `json:"startedAt"`
}

// mapPaneState mirrors mapPaneState: stale (sweepable) = idle/unknown agent
// sitting in a .devagent-worktrees checkout — the exact shape SweepStalePanes
// closes.
func mapPaneState(agentStatus, cwd string) string {
	if idleStatuses[agentStatus] && strings.Contains(cwd, ".devagent-worktrees") {
		return "stale"
	}
	if agentStatus == "working" {
		return "running"
	}
	return "idle"
}

// agentRow mirrors HerdrAgentRow; pointers so "field absent" is observable the
// way TS `??` and `||` treat undefined.
type agentRow struct {
	Name        *string `json:"name"`
	Label       *string `json:"label"`
	Agent       *string `json:"agent"` // agent kind herdr detected: omp | claude | codex | …
	PaneID      *string `json:"pane_id"`
	WorkspaceID *string `json:"workspace_id"`
	AgentStatus *string `json:"agent_status"`
	Cwd         *string `json:"cwd"`
	CreatedAt   *string `json:"created_at"`
}

// attemptSuffixRe: worktree attempt suffix (`-a1`, `-a1r2` —
// src/orchestrator/types.ts attemptSuffix shape).
var attemptSuffixRe = regexp.MustCompile(`-a\d+(?:r\d+)?$`)

// taskIdFromWorktreeBase mirrors taskIdFromWorktreeBase: strip the worktree
// attempt suffix from a worktree basename to recover the task id.
func taskIdFromWorktreeBase(base string) string {
	return attemptSuffixRe.ReplaceAllString(base, "")
}

// paneTaskIdFromCwd mirrors paneTaskIdFromCwd: task id from a pane cwd — only
// worktree checkouts (.devagent-worktrees/<taskId>-a<attempt>) carry one;
// scratch panes have no task semantics.
func paneTaskIdFromCwd(cwd string) string {
	if !strings.Contains(cwd, ".devagent-worktrees") {
		return ""
	}
	base := strings.TrimSpace(filepath.Base(cwd))
	if base == "" {
		return ""
	}
	return taskIdFromWorktreeBase(base)
}

// workerFromLabel mirrors workerFromLabel: the pane label is the worktree
// basename (<taskId>-a<attempt>); the prefix before the first attempt suffix is
// the best available worker name. 'unknown' when the label carries no suffix.
func workerFromLabel(label string) string {
	idx := strings.Index(label, "-a")
	if idx > 0 {
		return label[:idx]
	}
	return "unknown"
}

// parseAgentRows mirrors parseAgentRows: `agent list` reply rows; tolerates
// junk stdout.
func parseAgentRows(stdout string) []agentRow {
	var envelope struct {
		Result *struct {
			Agents []agentRow `json:"agents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil || envelope.Result == nil {
		return nil
	}
	return envelope.Result.Agents
}

// ListSessionPanes mirrors listSessionPanes(): roster of worker panes in a
// herdr session (FR-VIS-02), one SessionPaneInfo per agent row, tolerating
// missing fields. Empty/failed list -> nil.
func ListSessionPanes(cli CliRunner, session string) []SessionPaneInfo {
	s := ResolveSession(session)
	res := cli.HerdrCli([]string{"--session", s, "agent", "list"}, 10_000)
	if res.Code != 0 {
		return nil
	}
	rows := parseAgentRows(res.Stdout)
	out := []SessionPaneInfo{}
	for _, a := range rows {
		cwd := derefOr(a.Cwd, "")
		// ?? chain: an empty-string label is still "set" (not nullish).
		label := ""
		if a.Label != nil {
			label = *a.Label
		} else if a.Name != nil {
			label = *a.Name
		}
		agentStatus := derefOr(a.AgentStatus, "unknown")
		// Worker identity: herdr's detected agent kind (omp/claude/…) is the
		// truthful answer; the label-derived prefix is the fallback.
		worker := workerFromLabel(label)
		if a.Agent != nil && *a.Agent != "" {
			worker = *a.Agent
		}
		out = append(out, SessionPaneInfo{
			Worker:      worker,
			TaskID:      paneTaskIdFromCwd(cwd),
			Role:        "worker",
			PaneID:      derefOr(a.PaneID, ""),
			WorkspaceID: derefOr(a.WorkspaceID, ""),
			Label:       label,
			Cwd:         cwd,
			AgentStatus: agentStatus,
			State:       mapPaneState(agentStatus, cwd),
			// Never fabricated: only what herdr reports.
			StartedAt: derefOr(a.CreatedAt, ""),
		})
	}
	return out
}

// AttachCommandFor mirrors attachCommandFor(): shell command an operator runs
// to jump into the task's pane (FR-VIS-03); "" when no pane is rostered for
// the task.
func AttachCommandFor(cli CliRunner, taskID, session string) string {
	s := ResolveSession(session)
	panes := ListSessionPanes(cli, s)
	for _, p := range panes {
		if p.TaskID == taskID && p.PaneID != "" {
			return "herdr --session " + s + " agent attach " + p.PaneID
		}
	}
	return ""
}

// OperatorAttachTrace mirrors operatorAttachTrace(): true when a live
// (running) pane for the task is rostered — the cheap one-call probe later
// used to suppress watchdog auto-kill while an operator is attached
// (FR-VIS-03).
func OperatorAttachTrace(cli CliRunner, taskID, session string) bool {
	for _, p := range ListSessionPanes(cli, session) {
		if p.TaskID == taskID && p.State == "running" {
			return true
		}
	}
	return false
}
