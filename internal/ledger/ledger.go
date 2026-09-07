// Package ledger is the Go port of DevAgent's run ledger and run
// observability core (FR-GO-04): the append-only orchestration ledger from
// src/orchestrator/ledger.ts (JSONL schema byte-compatible with the Node
// writer in both directions), the structured run logger from src/logger.ts,
// and the run lock registry from src/runregistry.ts, plus the
// `devagent ledger --clusters` analytics/renderer.
package ledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
)

// LedgerDir is the repo-relative orchestration ledger directory (TS: LEDGER_DIR).
const LedgerDir = ".devagent/runs/orchestration"

func ledgerPath(repoPath string) string {
	return filepath.Join(repoPath, LedgerDir, "events.jsonl")
}

// marshalLine encodes v as one JSON object followed by '\n', matching the TS
// appendFileSync(file, `${JSON.stringify(record)}\n`) shape. HTML escaping is
// disabled: JSON.stringify does not escape <, > or &, so neither do we. (One
// documented divergence: V8 emits \b / \f for those control characters, Go's
// encoder emits \u0008 / \u000c — unreachable for prose-shaped ledger rows.)
func marshalLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// appendRecord is the shared append*Record body: create the ledger dir,
// append one JSON line, swallow every error. A ledger write failure must not
// fail an otherwise valid audit — best-effort observability by design.
func appendRecord(repoPath string, v any) {
	dir := filepath.Join(repoPath, LedgerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	line, err := marshalLine(v)
	if err != nil {
		return
	}
	f, err := os.OpenFile(ledgerPath(repoPath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(line)
}

// ---------------------------------------------------------------------------
// Record shapes. Field order = TS interface declaration order = JSON key
// order for Go-written rows. Optional TS fields (`field?:`) are pointers:
// nil omits the key (undefined), non-nil writes it even when zero-valued.
// ---------------------------------------------------------------------------

// AuditRecord is the Go AuditLedgerRecord. Every field is always written,
// matching the TS shape (a pass audit serializes "unmetCriteria":[]).
type AuditRecord struct {
	TS            string   `json:"ts"`
	Kind          string   `json:"kind"`
	TaskID        string   `json:"taskId"`
	Attempt       int      `json:"attempt"`
	Verdict       string   `json:"verdict"`
	Integrity     string   `json:"integrity"`
	UnmetCriteria []string `json:"unmetCriteria"`
	Summary       string   `json:"summary"`
}

// CriterionResult is the audit per-criterion row (src/orchestrator/types.ts).
type CriterionResult struct {
	Criterion string `json:"criterion"`
	Met       bool   `json:"met"`
	Evidence  string `json:"evidence"`
}

// Verdict is the Go AuditVerdict (src/orchestrator/types.ts).
type Verdict struct {
	Verdict         string // pass | fail | ask
	Integrity       string // clean | suspect | violation
	CriteriaResults []CriterionResult
	Summary         string
}

// MakeAuditRecord mirrors auditLedgerRecord: unmet criteria are extracted
// from the failed criteria and summary is truncated to 500 UTF-16 code units
// (TS .slice(0, 500); see truncateUTF16). ts == "" means now.
func MakeAuditRecord(taskID string, attempt int, verdict Verdict, ts string) AuditRecord {
	if ts == "" {
		ts = NowISO()
	}
	unmet := make([]string, 0)
	for _, c := range verdict.CriteriaResults {
		if !c.Met {
			unmet = append(unmet, c.Criterion)
		}
	}
	return AuditRecord{
		TS:            ts,
		Kind:          "audit",
		TaskID:        taskID,
		Attempt:       attempt,
		Verdict:       verdict.Verdict,
		Integrity:     verdict.Integrity,
		UnmetCriteria: unmet,
		Summary:       truncateUTF16(verdict.Summary, 500),
	}
}

// FixerRecord is the Go FixerLedgerRecord (CI-Fixer lifecycle, Q35).
type FixerRecord struct {
	TS           string   `json:"ts"`
	Kind         string   `json:"kind"`
	TaskID       string   `json:"taskId"`
	Attempt      int      `json:"attempt"`
	Event        string   `json:"event"` // ci-fix-dispatched | ci-fix-outcome
	PR           int      `json:"pr"`
	FailedChecks []string `json:"failedChecks"`
	// Outcome is set only on ci-fix-outcome rows; nil omits the key exactly
	// like the TS `outcome?: ...` field.
	Outcome *string `json:"outcome,omitempty"`
	Detail  *string `json:"detail,omitempty"`
}

// PrHygieneRecord is the Go PrHygieneLedgerRecord (zombie-PR hygiene).
type PrHygieneRecord struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	Event   string `json:"event"` // pr-hygiene
	PR      int    `json:"pr"`
	Action  string `json:"action"` // closed | flagged
	Reason  string `json:"reason"`
	// GraceAgeHours is required-but-nullable in TS: JSON.stringify writes
	// "graceAgeHours":null, so a nil Go pointer must serialize too (no
	// omitempty).
	GraceAgeHours *float64 `json:"graceAgeHours"`
	Detail        *string  `json:"detail,omitempty"`
}

// TaskInterruptRecord is the Go TaskInterruptLedgerRecord (executor interrupt
// post-mortem, PRD:775 / Q24 taxonomy mirror, PR #100).
type TaskInterruptRecord struct {
	TS              string  `json:"ts"`
	Kind            string  `json:"kind"`
	TaskID          string  `json:"taskId"`
	Attempt         int     `json:"attempt"`
	Event           string  `json:"event"` // taskInterrupt
	Goal            string  `json:"goal"`
	FailureClass    string  `json:"failureClass"`
	LastGateExcerpt string  `json:"lastGateExcerpt"`
	Attempts        int     `json:"attempts"`
	TrailHash       string  `json:"trailHash"`
	Detail          *string `json:"detail,omitempty"`
}

// OperatorDegradedRecord is the Go OperatorDegradedLedgerRecord (Q40
// operator-role provider preflight).
type OperatorDegradedRecord struct {
	TS       string  `json:"ts"`
	Kind     string  `json:"kind"`
	TaskID   string  `json:"taskId"`
	Attempt  int     `json:"attempt"`
	Event    string  `json:"event"` // operator-degraded
	Role     string  `json:"role"`
	Worker   string  `json:"worker"`
	Model    string  `json:"model"`
	OK       bool    `json:"ok"`
	Attempts int     `json:"attempts"`
	Detail   *string `json:"detail,omitempty"`
}

// ReleaseRecord is the Go ReleaseLedgerRecord (release/tag outcome, Q24).
type ReleaseRecord struct {
	TS      string  `json:"ts"`
	Kind    string  `json:"kind"`
	TaskID  string  `json:"taskId"`
	Attempt int     `json:"attempt"`
	Event   string  `json:"event"` // release-created
	Tag     string  `json:"tag"`
	SHA     string  `json:"sha"`
	Version string  `json:"version"`
	Source  string  `json:"source"`
	Detail  *string `json:"detail,omitempty"`
}

// StashRecord is the Go StashLedgerRecord (merge-back auto-stash, Q26).
type StashRecord struct {
	TS       string  `json:"ts"`
	Kind     string  `json:"kind"`
	TaskID   string  `json:"taskId"`
	Attempt  int     `json:"attempt"`
	Event    string  `json:"event"` // merge-back-stash
	StashSHA string  `json:"stashSha"`
	Outcome  string  `json:"outcome"` // restored | retained
	Detail   *string `json:"detail,omitempty"`
}

// OperatorAttachRecord is the Go OperatorAttachLedgerRecord (FR-VIS-03). The
// TS interface redeclares taskId from the base; the serialized object carries
// it once, in base position.
type OperatorAttachRecord struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	Event   string `json:"event"` // operator-attached
	PaneID  string `json:"paneId"`
	Session string `json:"session"`
}

// WatchdogHealthRecord is the Go WatchdogHealthLedgerRecord (Q34).
type WatchdogHealthRecord struct {
	TS                  string  `json:"ts"`
	Kind                string  `json:"kind"`
	TaskID              string  `json:"taskId"`
	Attempt             int     `json:"attempt"`
	Event               string  `json:"event"` // watchdog-health
	Site                string  `json:"site"`  // spawn-cli | herdr-pane
	Worker              string  `json:"worker"`
	NoProgressTimeoutMs int64   `json:"noProgressTimeoutMs"`
	WatchdogFired       bool    `json:"watchdogFired"`
	ColdStartFired      bool    `json:"coldStartFired"`
	WallClockMs         int64   `json:"wallClockMs"`
	ClockResets         int     `json:"clockResets"`
	MeaningfulBytes     int64   `json:"meaningfulBytes"`
	IdleMs              int64   `json:"idleMs"`
	Runtime             *string `json:"runtime,omitempty"` // herdr-pane | direct
	Visible             *bool   `json:"visible,omitempty"`
	Visibility          *string `json:"visibility,omitempty"` // herdr-pane | fallback | headless
}

// WorkerCostRecord is the Go WorkerCostLedgerRecord (FR-GROK-03): exact
// per-run cost in xAI USD ticks. A run the provider did not price writes no
// row — cost is never recorded as 0.
type WorkerCostRecord struct {
	TS           string `json:"ts"`
	Kind         string `json:"kind"`
	TaskID       string `json:"taskId"`
	Attempt      int    `json:"attempt"`
	Event        string `json:"event"` // worker-cost
	Worker       string `json:"worker"`
	CostUsdTicks int64  `json:"costUsdTicks"`
}

// AppendAuditRecord appends one audit record (best-effort, never throws).
func AppendAuditRecord(repoPath string, record AuditRecord) {
	appendRecord(repoPath, record)
}

// AppendFixerRecord appends a fixer lifecycle record (best-effort).
func AppendFixerRecord(repoPath string, record FixerRecord) {
	appendRecord(repoPath, record)
}

// AppendPrHygieneRecord appends a PR-hygiene record (best-effort).
func AppendPrHygieneRecord(repoPath string, record PrHygieneRecord) {
	appendRecord(repoPath, record)
}

// AppendTaskInterruptRecord appends an executor-interrupt post-mortem
// (best-effort).
func AppendTaskInterruptRecord(repoPath string, record TaskInterruptRecord) {
	appendRecord(repoPath, record)
}

// AppendOperatorDegradedRecord appends an operator-degraded record
// (best-effort).
func AppendOperatorDegradedRecord(repoPath string, record OperatorDegradedRecord) {
	appendRecord(repoPath, record)
}

// AppendReleaseRecord appends a release-created record (best-effort).
func AppendReleaseRecord(repoPath string, record ReleaseRecord) {
	appendRecord(repoPath, record)
}

// AppendStashRecord appends a merge-back-stash record (best-effort).
func AppendStashRecord(repoPath string, record StashRecord) {
	appendRecord(repoPath, record)
}

// AppendOperatorAttachRecord appends an operator-attached record
// (best-effort).
func AppendOperatorAttachRecord(repoPath string, record OperatorAttachRecord) {
	appendRecord(repoPath, record)
}

// AppendWatchdogHealthRecord appends a watchdog-health record (best-effort).
func AppendWatchdogHealthRecord(repoPath string, record WatchdogHealthRecord) {
	appendRecord(repoPath, record)
}

// AppendWorkerCostRecord appends a worker-cost record (best-effort).
func AppendWorkerCostRecord(repoPath string, record WorkerCostRecord) {
	appendRecord(repoPath, record)
}
