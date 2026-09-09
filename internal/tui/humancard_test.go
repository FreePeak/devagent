package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// Port of test/queue-human.test.ts (queue list human card, #144 R3) plus the
// validate/ledger card contracts from human-card usage.

func TestRenderQueueCardEmpty(t *testing.T) {
	card := plain(RenderQueueCard(nil, QueueCounts{}, 100))
	for _, want := range []string{"Queue ─", "pending 0", "next:", "devagent status", "╰"} {
		if !strings.Contains(card, want) {
			t.Fatalf("empty queue card missing %q:\n%s", want, card)
		}
	}
	if got := strings.Count(card, "next:"); got != 1 {
		t.Fatalf("next: appears %d times, want 1", got)
	}
	if !strings.Contains(card, "No tasks in the queue.") {
		t.Fatal("empty queue must say so")
	}
}

func TestRenderQueueCardPending(t *testing.T) {
	tasks := []QueueCardTask{
		{ID: "T1", Title: "research", Status: "pending"},
		{ID: "T2", Title: "implement", Status: "pending"},
	}
	card := plain(RenderQueueCard(tasks, QueueCounts{Total: 2, Pending: 2}, 100))
	for _, want := range []string{"pending 2", "T1", "●", "devagent consume --auto-pr"} {
		if !strings.Contains(card, want) {
			t.Fatalf("pending card missing %q:\n%s", want, card)
		}
	}
	if got := strings.Count(card, "next:"); got != 1 {
		t.Fatalf("next: appears %d times, want 1", got)
	}
}

func TestRenderQueueCardRowCapAndErrors(t *testing.T) {
	var tasks []QueueCardTask
	for i := 0; i < 12; i++ {
		tasks = append(tasks, QueueCardTask{ID: itoa(i), Title: "task", Status: "failed", LastError: "boom " + itoa(i)})
	}
	card := plain(RenderQueueCard(tasks, QueueCounts{Total: 12, Failed: 12}, 100))
	if !strings.Contains(card, "… and 4 more") {
		t.Fatal("row cap must append the overflow line")
	}
	if !strings.Contains(card, "— boom") {
		t.Fatal("lastError must render after an em-dash")
	}
	if !strings.Contains(card, "devagent queue list --status failed") {
		t.Fatal("failed counts must suggest the failed-status next action")
	}
}

func TestRenderQueueCardChipStates(t *testing.T) {
	tasks := []QueueCardTask{
		{ID: "A", Status: "done"},
		{ID: "B", Status: "claimed"},
	}
	card := plain(RenderQueueCard(tasks, QueueCounts{Total: 2, Done: 1, Claimed: 1}, 100))
	if !strings.Contains(card, "● done") || !strings.Contains(card, "● claimed") {
		t.Fatalf("chip labels must render:\n%s", card)
	}
}

func TestRenderValidateCards(t *testing.T) {
	rows := []ValidateGateRow{
		{Label: "G1", Gate: "G1-tests", Passed: true, Detail: "first line\nsecond line"},
		{Label: "G3", Gate: "G3-migration-static", Passed: false, Detail: "static failed"},
	}
	out := plain(RenderValidateCards(rows, 100))
	for _, want := range []string{"Gate G1 ─", "G1 PASS", "first line", "Gate G3 ─", "G3 FAIL", "next: fix tests / inspect gate detail above"} {
		if !strings.Contains(out, want) {
			t.Fatalf("validate cards missing %q:\n%s", want, out)
		}
	}
	allPass := []ValidateGateRow{{Label: "G1", Passed: true, Detail: "ok"}}
	out2 := plain(RenderValidateCards(allPass, 100))
	if !strings.Contains(out2, "next: devagent status") {
		t.Fatal("all-pass next action")
	}
	skip := []ValidateGateRow{{Label: "G2", Skipped: true}}
	out3 := plain(RenderValidateCards(skip, 100))
	if !strings.Contains(out3, "G2 SKIP") || !strings.Contains(out3, "skipped") {
		t.Fatalf("skip chip:\n%s", out3)
	}
}

func TestValidateCardsWidthClamp(t *testing.T) {
	// width clamps to [46,100]; a 40-col terminal still gets a 46-wide card
	out := RenderValidateCards([]ValidateGateRow{{Label: "G1", Passed: true}}, 40)
	maxLen := 0
	for _, l := range strings.Split(out, "\n") {
		if n := VisibleLen(l); n > maxLen {
			maxLen = n
		}
	}
	if maxLen < 44 || maxLen > 102 {
		t.Fatalf("clamped width out of range: %d", maxLen)
	}
}

func TestRenderLedgerSummaryCard(t *testing.T) {
	mean := 1.5
	card := plain(RenderLedgerSummaryCard(LedgerSummary{
		Tasks: 10, Audits: 14, Resolved: 8, MeanAttemptsToPass: &mean, Unresolved: 2,
	}, 100))
	for _, want := range []string{"Ledger summary ─", "audits", "resolved", "unresolved", "tasks 10 · mean attempts-to-pass 1.5", "next: inspect open work: devagent ledger"} {
		if !strings.Contains(card, want) {
			t.Fatalf("ledger summary missing %q:\n%s", want, card)
		}
	}
	clean := plain(RenderLedgerSummaryCard(LedgerSummary{Tasks: 3, Audits: 3, Resolved: 3}, 100))
	if !strings.Contains(clean, "next: devagent status") {
		t.Fatal("clean ledger next action")
	}
}

func TestValidateJSONPayload(t *testing.T) {
	rows := []ValidateGateRow{
		{Label: "G1", Gate: "G1-tests", Passed: true, Detail: "suite green", FindingsJSON: `[{"ruleId":"R","severity":"warn","message":"m"}]`},
		{Label: "G3", Gate: "G3-migration-static", Passed: false, FindingsJSON: "[]"},
	}
	raw := ValidateJSON(rows)
	var parsed struct {
		Gates []struct {
			Gate     string           `json:"gate"`
			Label    string           `json:"label"`
			Passed   bool             `json:"passed"`
			Skipped  bool             `json:"skipped"`
			Detail   *string          `json:"detail"`
			Findings []map[string]any `json:"findings"`
		} `json:"gates"`
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("invalid json: %v\n%s", err, raw)
	}
	if len(parsed.Gates) != 2 || parsed.OK {
		t.Fatalf("gates = %+v", parsed)
	}
	if parsed.Gates[0].Detail == nil || *parsed.Gates[0].Detail != "suite green" {
		t.Fatalf("detail = %v", parsed.Gates[0].Detail)
	}
	if parsed.Gates[1].Detail != nil {
		t.Fatal("absent detail must be null")
	}
	if len(parsed.Gates[0].Findings) != 1 || parsed.Gates[0].Findings[0]["ruleId"] != "R" {
		t.Fatalf("findings = %v", parsed.Gates[0].Findings)
	}
	allPass := ValidateJSON([]ValidateGateRow{{Label: "G1", Gate: "G1-tests", Passed: true, FindingsJSON: "[]"}})
	if !strings.Contains(allPass, `"ok": true`) {
		t.Fatalf("all-pass ok flag: %s", allPass)
	}
}

func TestLedgerJSONPayload(t *testing.T) {
	mean := 2.0
	summary := LedgerSummary{Tasks: 2, Audits: 3, Resolved: 1, MeanAttemptsToPass: &mean, Unresolved: 1}
	raw := LedgerJSON(&summary, nil)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if parsed["tasks"] != float64(2) || parsed["audits"] != float64(3) ||
		parsed["resolved"] != float64(1) || parsed["unresolved"] != float64(1) {
		t.Fatalf("summary json = %s", raw)
	}
	if parsed["meanAttemptsToPass"] != float64(2) {
		t.Fatalf("mean = %v", parsed["meanAttemptsToPass"])
	}
	records := []LedgerListRecord{{Kind: "audit", Ts: "t", TaskID: "T1", Attempt: 1, Verdict: "pass", Integrity: "ok", Summary: "s"}}
	listRaw := LedgerJSON(nil, records)
	var listParsed []map[string]any
	if err := json.Unmarshal([]byte(listRaw), &listParsed); err != nil || len(listParsed) != 1 {
		t.Fatalf("list json = %s", listRaw)
	}
}

func TestRenderLedgerListLines(t *testing.T) {
	if got := RenderLedgerListLines(nil); got[0] != "No ledger records. Audits append to .devagent/runs/orchestration/events.jsonl." {
		t.Fatalf("empty list = %v", got)
	}
	records := []LedgerListRecord{
		{Kind: "event", Ts: "t1", TaskID: "T1", Attempt: 1},
		{Kind: "audit", Ts: "2026-09-07T00:00:00Z", TaskID: "T1", Attempt: 2, Verdict: "pass", Integrity: "ok", Summary: "clean"},
		{Kind: "audit", Ts: "2026-09-07T01:00:00Z", TaskID: "T2", Attempt: 1, Verdict: "ask", Integrity: "partial", UnmetCount: 2, Summary: "needs input"},
		{Kind: "audit", Ts: "2026-09-07T02:00:00Z", TaskID: "T3", Attempt: 1, Verdict: "fail", Integrity: "none", Summary: "red"},
	}
	lines := RenderLedgerListLines(records)
	if len(lines) != 3 {
		t.Fatalf("event rows must be skipped: %v", lines)
	}
	if !strings.Contains(lines[0], "pass/ok") || !strings.Contains(lines[0], "T1 (attempt 2)") {
		t.Fatalf("pass line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "ask/partial unmet:2") {
		t.Fatalf("ask line = %q", lines[1])
	}
	if !strings.Contains(lines[2], "fail/none") {
		t.Fatalf("fail line = %q", lines[2])
	}
	auditOnly := []LedgerListRecord{{Kind: "event", Ts: "t", TaskID: "T", Attempt: 1}}
	if got := RenderLedgerListLines(auditOnly); got[0] != "No audit records in the ledger." {
		t.Fatalf("event-only list = %v", got)
	}
}
