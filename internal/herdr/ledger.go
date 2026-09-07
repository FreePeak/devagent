// Minimal ledger seam for the herdr package: the two JSONL rows a herdr
// launch/attach writes. The full orchestrator ledger is a separate migration
// item; these appends replicate its byte-exact row shapes so nothing observes
// a different ledger.
//
// TODO(FR-GO-10 #201): replace with the sibling orchestrator/ledger port
// (src/orchestrator/ledger.ts) once it lands.
package herdr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// LEDGER_DIR mirrors LEDGER_DIR (src/orchestrator/ledger.ts).
const ledgerDir = ".devagent/runs/orchestration"

func ledgerPath(repoPath string) string {
	return filepath.Join(repoPath, ledgerDir, "events.jsonl")
}

// ledgerNow mirrors `new Date().toISOString()`.
func ledgerNow() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// appendRow mirrors the ledger appenders: best-effort observability, never
// throws into the caller's path. One JSON object per line, key order exactly
// the TS record's property order (JSON.stringify contract).
func appendRow(repoPath string, row any) {
	data, err := json.Marshal(row)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(repoPath, ledgerDir), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(ledgerPath(repoPath), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// WatchdogHealthRow mirrors WatchdogHealthLedgerRecord as written by the
// herdr-pane site. Field order is the JSONL contract.
type WatchdogHealthRow struct {
	Ts                  string `json:"ts"`
	Kind                string `json:"kind"`
	Event               string `json:"event"`
	TaskID              string `json:"taskId"`
	Attempt             int    `json:"attempt"`
	Worker              string `json:"worker"`
	Site                string `json:"site"`
	Runtime             string `json:"runtime"`
	Visible             bool   `json:"visible"`
	Visibility          string `json:"visibility"`
	NoProgressTimeoutMs int    `json:"noProgressTimeoutMs"`
	WatchdogFired       bool   `json:"watchdogFired"`
	ColdStartFired      bool   `json:"coldStartFired"`
	WallClockMs         int    `json:"wallClockMs"`
	ClockResets         int    `json:"clockResets"`
	MeaningfulBytes     int    `json:"meaningfulBytes"`
	IdleMs              int    `json:"idleMs"`
}

// AppendWatchdogHealthRecord mirrors appendWatchdogHealthRecord: one
// watchdog-health event per worker-CLI launch with a no-progress clock armed.
func AppendWatchdogHealthRecord(repoPath string, row WatchdogHealthRow) {
	appendRow(repoPath, row)
}

// OperatorAttachRecord mirrors OperatorAttachLedgerRecord (FR-VIS-03): an
// operator jumped into a task's pane.
type OperatorAttachRecord struct {
	Ts      string `json:"ts"`
	Kind    string `json:"kind"`
	Event   string `json:"event"`
	TaskID  string `json:"taskId"`
	Attempt int    `json:"attempt"`
	PaneID  string `json:"paneId"`
	Session string `json:"session"`
}

// AppendOperatorAttachRecord mirrors appendOperatorAttachRecord.
func AppendOperatorAttachRecord(repoPath string, row OperatorAttachRecord) {
	appendRow(repoPath, row)
}
