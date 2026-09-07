package tui

import (
	"strings"
)

// Human-readable status card (PRD §21 FR-SIMPLE-03/04): `devagent status`
// renders the current phase + the one next action in the §20.8 card/chip
// visual language (port of src/commands/status.ts). The renderer composes
// only existing state — the orchestrator board (.devagent-project.json), the
// queue, and the herdr pane roster — no second status system. Machine
// consumers read the same view as JSON via StatusJSON.
//
// Sibling-isolation seam (FR-GO-07 #194 orchestrator/queue, FR-GO-10 #201
// herdr): the board, queue counts and pane roster arrive as plain data — the
// CLI wiring (parent) supplies them once those packages land.

// StatusTask is the slice of an orchestrator task the status view uses.
type StatusTask struct {
	ID            string
	Title         string
	Status        string
	FailureDetail string
}

// StatusBoard is the minimal board view (goal + tasks).
type StatusBoard struct {
	Goal  string
	Tasks []StatusTask
}

// StatusPane is the pane-roster slice the attach hint checks.
type StatusPane struct {
	TaskID string
	State  string
}

// StatusView is the aggregate view backing both the human card and the
// --json payload.
type StatusView struct {
	// Phase is the chip phase label (plain language, ≤18 chars).
	Phase string
	// ChipState drives the dot color (running/ok/idle/failed).
	ChipState string
	// NextAction is the one next action (§20.8 progressive disclosure).
	NextAction string
	// AttachHint is the literal `devagent attach <taskId>` when a live pane
	// exists; "" otherwise.
	AttachHint string
	// Detail is one-line context: current task title, counts, or goal.
	Detail string
	// BoardExists reports whether a board file was found.
	BoardExists bool
	// Goal is the board goal ("" when no board).
	Goal string
	// CurrentTask is the task the operator should look at; nil when none.
	CurrentTask *StatusTaskRef
	// TaskCounts maps board task status → count.
	TaskCounts map[string]int
	// Queue is the queue snapshot.
	Queue QueueCounts
}

// StatusTaskRef identifies the current task in the status view.
type StatusTaskRef struct {
	ID     string
	Title  string
	Status string
}

// firstTask finds the first task in scheduler wave order for a status.
func firstStatusTask(board *StatusBoard, statuses []string) *StatusTask {
	if board == nil {
		return nil
	}
	for _, s := range statuses {
		for i := range board.Tasks {
			if board.Tasks[i].Status == s {
				return &board.Tasks[i]
			}
		}
	}
	return nil
}

// ComposeStatusView composes the status view from existing state
// (FR-SIMPLE-03/04): current phase + one next action. Never fails — missing
// state degrades to the not-started view.
func ComposeStatusView(board *StatusBoard, queue QueueCounts, panes []StatusPane, hasConfig bool) StatusView {
	if board == nil {
		if queue.Pending > 0 {
			return StatusView{
				Phase:      "queued",
				ChipState:  "idle",
				NextAction: "nothing — workers claim queued tasks automatically",
				Detail:     itoa(queue.Pending) + " task(s) waiting in the queue",
				Queue:      queue,
			}
		}
		if hasConfig {
			return StatusView{
				Phase:      "not started",
				ChipState:  "idle",
				NextAction: "state your goal in one sentence: devagent orchestrate --goal \"...\"",
				Detail:     "config found — no goal dispatched yet",
				Queue:      queue,
			}
		}
		return StatusView{
			Phase:      "not started",
			ChipState:  "idle",
			NextAction: "run devagent init to set up this repository",
			Detail:     "no setup yet",
			Queue:      queue,
		}
	}

	counts := map[string]int{}
	for _, t := range board.Tasks {
		counts[t.Status]++
	}
	running := firstStatusTask(board, []string{"dispatched", "untrusted"})
	failed := firstStatusTask(board, []string{"failed"})
	ask := firstStatusTask(board, []string{"ask"})
	allDone := len(board.Tasks) > 0
	for _, t := range board.Tasks {
		if t.Status != "done" {
			allDone = false
			break
		}
	}
	var pane *StatusPane
	if running != nil {
		for i := range panes {
			if panes[i].TaskID == running.ID && panes[i].State == "running" {
				pane = &panes[i]
				break
			}
		}
	}

	if allDone {
		return StatusView{
			Phase:       "all done",
			ChipState:   "ok",
			NextAction:  "state a new goal: devagent orchestrate --goal \"...\"",
			Detail:      "goal: " + Truncate(board.Goal, 60),
			BoardExists: true,
			Goal:        board.Goal,
			TaskCounts:  counts,
			Queue:       queue,
		}
	}
	if ask != nil {
		return StatusView{
			Phase:       "paused for you",
			ChipState:   "idle",
			NextAction:  "answer the paused task: devagent orchestrate --goal \"\" --resume --answer " + ask.ID + "=\"...\"",
			Detail:      "task " + ask.ID + " needs your answer: " + Truncate(ask.Title, 50),
			BoardExists: true,
			Goal:        board.Goal,
			CurrentTask: &StatusTaskRef{ID: ask.ID, Title: ask.Title, Status: ask.Status},
			TaskCounts:  counts,
			Queue:       queue,
		}
	}
	if failed != nil {
		return StatusView{
			Phase:       "failed",
			ChipState:   "failed",
			NextAction:  "inspect the failure: devagent project (task " + failed.ID + ")",
			Detail:      "task " + failed.ID + ": " + Truncate(orDefault(failed.FailureDetail, failed.Title), 60),
			BoardExists: true,
			Goal:        board.Goal,
			CurrentTask: &StatusTaskRef{ID: failed.ID, Title: failed.Title, Status: failed.Status},
			TaskCounts:  counts,
			Queue:       queue,
		}
	}
	if running != nil {
		phase := "implementing"
		if running.Status == "untrusted" {
			phase = "awaiting audit"
		}
		// boardNextAction (TS): a live pane yields the literal attach hint;
		// dispatched/untrusted work without a pane falls back to the board.
		nextAction := "watch the board: devagent project"
		attachHint := ""
		if pane != nil {
			nextAction = "watch the worker"
			attachHint = "devagent attach " + running.ID
		}
		return StatusView{
			Phase:       phase,
			ChipState:   "running",
			NextAction:  nextAction,
			AttachHint:  attachHint,
			Detail:      running.ID + ": " + Truncate(running.Title, 50),
			BoardExists: true,
			Goal:        board.Goal,
			CurrentTask: &StatusTaskRef{ID: running.ID, Title: running.Title, Status: running.Status},
			TaskCounts:  counts,
			Queue:       queue,
		}
	}
	return StatusView{
		Phase:       "implementing",
		ChipState:   "running",
		NextAction:  "nothing — executors pick up the remaining tasks automatically",
		Detail:      itoa(len(board.Tasks)) + " task(s) on the board",
		BoardExists: true,
		Goal:        board.Goal,
		TaskCounts:  counts,
		Queue:       queue,
	}
}

// RenderStatusCard renders the §20.8 card lines: chip + next action
// (+ attach hint) in a rounded box. columns is the terminal width.
func RenderStatusCard(view StatusView, columns int) string {
	width := CardWidth(columns)
	next := "next: " + view.NextAction
	if view.AttachHint != "" {
		next = "next: " + view.NextAction + " — " + CyanText(view.AttachHint)
	}
	return BoxLines("Project status", []string{
		" " + ChipFor(view.ChipState, view.Phase) + "  " + DimText(view.Detail),
		" " + next,
	}, width)
}

// StatusJSON returns the --json payload for scripts (FR-SIMPLE-03 machine
// format), 2-space indented like JSON.stringify(_, null, 2).
func StatusJSON(view StatusView) string {
	var b strings.Builder
	b.WriteString("{\n")
	writeKV(&b, "phase", quote(view.Phase), true)
	writeKV(&b, "chipState", quote(view.ChipState), true)
	if view.BoardExists {
		writeKV(&b, "goal", quote(view.Goal), true)
	} else {
		writeKV(&b, "goal", "null", true)
	}
	writeKV(&b, "boardExists", boolJSON(view.BoardExists), true)
	if view.CurrentTask != nil {
		b.WriteString("  \"currentTask\": {\n")
		writeKVIndent(&b, 4, "id", quote(view.CurrentTask.ID), true)
		writeKVIndent(&b, 4, "title", quote(view.CurrentTask.Title), true)
		writeKVIndent(&b, 4, "status", quote(view.CurrentTask.Status), false)
		b.WriteString("  },\n")
	} else {
		writeKV(&b, "currentTask", "null", true)
	}
	// taskCounts in first-seen board order (TS object insertion order).
	b.WriteString("  \"taskCounts\": {")
	first := true
	for _, t := range orderedStatuses(view) {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString("\n    " + quote(t) + ": " + itoa(view.TaskCounts[t]))
	}
	if first {
		b.WriteString("}")
	} else {
		b.WriteString("\n  }")
	}
	b.WriteString(",\n")
	b.WriteString("  \"queue\": {\n")
	writeKVIndent(&b, 4, "total", itoa(view.Queue.Total), true)
	writeKVIndent(&b, 4, "pending", itoa(view.Queue.Pending), true)
	writeKVIndent(&b, 4, "claimed", itoa(view.Queue.Claimed), true)
	writeKVIndent(&b, 4, "done", itoa(view.Queue.Done), true)
	writeKVIndent(&b, 4, "failed", itoa(view.Queue.Failed), false)
	b.WriteString("  },\n")
	writeKV(&b, "nextAction", quote(view.NextAction), true)
	if view.AttachHint != "" {
		writeKV(&b, "attachHint", quote(view.AttachHint), false)
	} else {
		writeKV(&b, "attachHint", "null", false)
	}
	b.WriteString("}")
	return b.String()
}

func orderedStatuses(view StatusView) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range []string{"pending", "ready", "blocked", "ask", "dispatched", "untrusted", "done", "failed"} {
		if _, ok := view.TaskCounts[s]; ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for s := range view.TaskCounts {
		if !seen[s] {
			out = append(out, s)
		}
	}
	return out
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\t':
			b.WriteString("\\t")
		case '\r':
			b.WriteString("\\r")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func writeKV(b *strings.Builder, key, val string, comma bool) {
	writeKVIndent(b, 2, key, val, comma)
}

func writeKVIndent(b *strings.Builder, indent int, key, val string, comma bool) {
	pad := strings.Repeat(" ", indent)
	b.WriteString(pad + quote(key) + ": " + val)
	if comma {
		b.WriteString(",")
	}
	b.WriteString("\n")
}
