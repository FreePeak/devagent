package tui

import (
	"encoding/json"
	"strings"
)

// Shared §20.8 card/chip human defaults for queue / validate / ledger
// (FR-SIMPLE-03 remainder, issue #144 → FR-GO-11 port of
// src/commands/human-card.ts). Reuses ChipFor / BoxLines / DimText /
// CyanText from this package — same language as RenderStatusCard.
//
// The TS functions take the queue/ledger/gate state as typed inputs; this
// port takes the already-extracted rows (the sibling ports for queue/ledger
// are FR-GO-07) so the renderer stays testable against fixture state.

// CardWidth is the §20.8 card width: clamp the terminal to [46, 100].
func CardWidth(columns int) int {
	return maxInt(46, minInt(columns, 100))
}

// QueueCardTask is the slice of a queued task the card renders.
type QueueCardTask struct {
	ID        string
	Title     string
	Status    string // 'pending' | 'claimed' | 'done' | 'failed'
	LastError string
}

// QueueCounts mirrors Record<QueuedTaskStatus | 'total', number>.
type QueueCounts struct {
	Total   int
	Pending int
	Claimed int
	Done    int
	Failed  int
}

// chipStateForQueue maps a queue status onto a chip state.
func chipStateForQueue(status string) string {
	switch status {
	case "done":
		return "ok"
	case "failed":
		return "failed"
	case "claimed":
		return "running"
	default:
		return "idle"
	}
}

// queueNextAction is the one next action for the queue card.
func queueNextAction(c QueueCounts) string {
	if c.Pending > 0 {
		return "devagent consume --auto-pr"
	}
	if c.Claimed > 0 {
		return "devagent status"
	}
	if c.Failed > 0 {
		return "inspect failed tasks: devagent queue list --status failed"
	}
	return "devagent status"
}

// queueRowCap is the row cap before the "… and N more" line.
const queueRowCap = 8

// RenderQueueCard renders the human card for `devagent queue list` (default,
// non-`--json`). columns is the terminal width (cardWidth()).
func RenderQueueCard(tasks []QueueCardTask, counts QueueCounts, columns int) string {
	width := CardWidth(columns)
	countLine := " " + ChipFor("idle", "queue") + "  " +
		DimText("pending "+itoa(counts.Pending)+" · claimed "+itoa(counts.Claimed)+
			" · done "+itoa(counts.Done)+" · failed "+itoa(counts.Failed))
	var rows []string
	capped := tasks
	if len(capped) > queueRowCap {
		capped = capped[:queueRowCap]
	}
	for _, t := range capped {
		err := ""
		if t.LastError != "" {
			err = " — " + Truncate(t.LastError, 40)
		}
		rows = append(rows, " "+ChipFor(chipStateForQueue(t.Status), t.Status)+"  "+t.ID+"  "+
			Truncate(t.Title, 40)+err)
	}
	if len(tasks) == 0 {
		rows = append(rows, " "+DimText("No tasks in the queue."))
	} else if len(tasks) > queueRowCap {
		rows = append(rows, " "+DimText("… and "+itoa(len(tasks)-queueRowCap)+" more"))
	}
	next := " next: " + CyanText(queueNextAction(counts))
	return BoxLines("Queue", append([]string{countLine}, append(rows, next)...), width)
}

// ValidateGateRow is one gate row for the validate cards.
type ValidateGateRow struct {
	// Label is the short label shown on the chip (G1 / G3).
	Label string
	// Gate is the machine gate name ('G1-tests' etc).
	Gate string
	// Passed reports the gate outcome.
	Passed bool
	// Skipped is true when the gate did not run (missing prerequisites).
	Skipped bool
	// Detail is the human-readable evidence (multi-line allowed).
	Detail string
	// FindingsJSON is the machine findings array (rendered verbatim by
	// ValidateJSON; the renderer does not interpret it).
	FindingsJSON string
}

func gateChipState(row ValidateGateRow) string {
	if row.Skipped {
		return "idle"
	}
	if row.Passed {
		return "ok"
	}
	return "failed"
}

func gateChipLabel(row ValidateGateRow) string {
	if row.Skipped {
		return row.Label + " SKIP"
	}
	if row.Passed {
		return row.Label + " PASS"
	}
	return row.Label + " FAIL"
}

// oneLineDetail takes the first non-blank line of a multi-line detail.
func oneLineDetail(detail string) string {
	if detail == "" {
		return ""
	}
	for _, l := range strings.Split(detail, "\n") {
		if strings.TrimSpace(l) != "" {
			return Truncate(strings.TrimSpace(l), 70)
		}
	}
	return ""
}

func validateNextAction(rows []ValidateGateRow) string {
	for _, r := range rows {
		if !r.Passed && !r.Skipped {
			return "fix tests / inspect gate detail above"
		}
	}
	return "devagent status"
}

// RenderValidateCards renders the human cards for `devagent validate`
// (default, non-`--json`).
func RenderValidateCards(rows []ValidateGateRow, columns int) string {
	width := CardWidth(columns)
	var out []string
	for _, row := range rows {
		detail := oneLineDetail(row.Detail)
		if detail == "" {
			switch {
			case row.Skipped:
				detail = "skipped"
			case row.Passed:
				detail = "passed"
			default:
				detail = "failed"
			}
		}
		body := []string{" " + ChipFor(gateChipState(row), gateChipLabel(row)) + "  " + DimText(detail)}
		out = append(out, boxLinesLines("Gate "+row.Label, body, width)...)
	}
	out = append(out, " next: "+CyanText(validateNextAction(rows)))
	return strings.Join(out, "\n")
}

// LedgerSummary mirrors the ledger summarizer output (FR-GO-07 owns the
// ledger port; this is the render-side view).
type LedgerSummary struct {
	Tasks              int      `json:"tasks"`
	Audits             int      `json:"audits"`
	Resolved           int      `json:"resolved"`
	MeanAttemptsToPass *float64 `json:"meanAttemptsToPass"`
	Unresolved         int      `json:"unresolved"`
}

func ledgerNextAction(s LedgerSummary) string {
	if s.Unresolved > 0 {
		return "inspect open work: devagent ledger"
	}
	return "devagent status"
}

// RenderLedgerSummaryCard renders the summary card for
// `devagent ledger --summary`.
func RenderLedgerSummaryCard(s LedgerSummary, columns int) string {
	width := CardWidth(columns)
	mean := ""
	if s.MeanAttemptsToPass != nil {
		mean = " · mean attempts-to-pass " + jsNum(*s.MeanAttemptsToPass)
	}
	unresolvedState := "ok"
	if s.Unresolved > 0 {
		unresolvedState = "failed"
	}
	body := []string{
		" " + ChipFor("idle", "audits") + "  " + DimText(itoa(s.Audits)) + "  " +
			ChipFor("ok", "resolved") + "  " + DimText(itoa(s.Resolved)) + "  " +
			ChipFor(unresolvedState, "unresolved") + "  " + DimText(itoa(s.Unresolved)),
		" " + DimText("tasks "+itoa(s.Tasks)+mean),
		" next: " + CyanText(ledgerNextAction(s)),
	}
	return BoxLines("Ledger summary", body, width)
}

// LedgerListRecord is the audit-row slice the list lines render.
type LedgerListRecord struct {
	Kind       string // 'audit' | 'event'
	Ts         string
	TaskID     string
	Attempt    int
	Verdict    string // 'pass' | 'fail' | 'ask' | ...
	Integrity  string
	UnmetCount int
	Summary    string
}

// RenderLedgerListLines renders the chip lines for default `devagent ledger`
// list (● state chips; replaces +/x/? ascii). The CloddsBot ✓/✗ glyphs stay
// on the checklist surfaces (init/smoke) where the outcome is the point.
func RenderLedgerListLines(records []LedgerListRecord) []string {
	if len(records) == 0 {
		return []string{"No ledger records. Audits append to .devagent/runs/orchestration/events.jsonl."}
	}
	var lines []string
	for _, r := range records {
		if r.Kind != "audit" {
			continue
		}
		state := "failed"
		label := "fail"
		switch r.Verdict {
		case "pass":
			state, label = "ok", "pass"
		case "ask":
			state, label = "idle", "ask"
		}
		unmet := ""
		if r.UnmetCount > 0 {
			unmet = " unmet:" + itoa(r.UnmetCount)
		}
		detail := r.Verdict + "/" + r.Integrity + unmet + " — " + Truncate(r.Summary, 90)
		lines = append(lines, ChipFor(state, label)+"  "+r.Ts+" ["+r.Kind+"] "+r.TaskID+
			" (attempt "+itoa(r.Attempt)+") "+detail)
	}
	if len(lines) == 0 {
		return []string{"No audit records in the ledger."}
	}
	return lines
}

// ValidateJSON returns the machine payload for `devagent validate --json`
// (2-space indent like JSON.stringify(_, null, 2)).
func ValidateJSON(rows []ValidateGateRow) string {
	type gateOut struct {
		Gate     string          `json:"gate"`
		Label    string          `json:"label"`
		Passed   bool            `json:"passed"`
		Skipped  bool            `json:"skipped"`
		Detail   *string         `json:"detail"`
		Findings json.RawMessage `json:"findings"`
	}
	gates := make([]gateOut, 0, len(rows))
	ok := true
	for _, r := range rows {
		var detail *string
		if r.Detail != "" {
			d := r.Detail
			detail = &d
		}
		findings := json.RawMessage(r.FindingsJSON)
		if len(findings) == 0 || !json.Valid(findings) {
			findings = json.RawMessage("[]")
		}
		gates = append(gates, gateOut{
			Gate: r.Gate, Label: r.Label, Passed: r.Passed,
			Skipped: r.Skipped, Detail: detail, Findings: findings,
		})
		if !r.Passed && !r.Skipped {
			ok = false
		}
	}
	out := struct {
		Gates []gateOut `json:"gates"`
		OK    bool      `json:"ok"`
	}{Gates: gates, OK: ok}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// LedgerJSON returns the `--json` payload for the ledger summary or list.
func LedgerJSON(summary *LedgerSummary, records []LedgerListRecord) string {
	var payload any
	if summary != nil {
		payload = summary
	} else {
		payload = records
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "null"
	}
	return string(b)
}
