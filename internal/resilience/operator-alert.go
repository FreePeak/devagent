// Package file mirrors src/resilience/operator-alert.ts (FR-GO-07, issue
// #194): operator paging transport (PRD:913 Q16). An OperatorAlert is a
// discriminated union keyed on `event`, so a receiver routes the payload
// without parsing prose. PostOperatorAlert is the default transport; every
// caller injects its own notifier in tests and swallows the result — paging
// is observability, never a second failure surface on top of the condition
// it reports.
package resilience

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OperatorAlertTimeoutMs is the wall-clock cap for the paging POST: paging
// must never stall a loop cycle.
const OperatorAlertTimeoutMs = 5_000

// BoardArchivedAlert is a board moved into `.devagent/archive/` (Q16).
// Fired once per archive, for both gate verdicts: Prefix 'board' is the
// completed-board infinity-cycle archive, Prefix 'board-stuck' is the
// undispatchable archive.
type BoardArchivedAlert struct {
	// Discriminator so a receiver routes the payload without parsing prose.
	Event string `json:"event"` // board-archived
	// ISO ts of the paging POST.
	TS string `json:"ts"`
	// Repo whose board was archived.
	Repo string `json:"repo"`
	// Archive filename prefix — the gate verdict that caused the move.
	Prefix string `json:"prefix"` // board | board-stuck
	// Repo-relative path the board was moved to.
	Path string `json:"path"`
	// Human-readable why, carried off the recovery verdict.
	Reason string `json:"reason"`
}

func (BoardArchivedAlert) operatorAlertEvent() string { return "board-archived" }

// OperatorAlert is every payload the operator webhook accepts, keyed on
// `event` (TS union DegradeBreachAlert | BoardArchivedAlert).
type OperatorAlert interface {
	operatorAlertEvent() string
}

// OperatorNotifier is the injection seam for the outbound paging transport
// (tests swap it out).
type OperatorNotifier func(url string, alert OperatorAlert) error

// PostOperatorAlert is the default paging transport: JSON POST to the
// operator webhook. A non-2xx response returns an error; every caller
// swallows it so paging can never become a second failure surface on top of
// the condition it reports.
func PostOperatorAlert(url string, alert OperatorAlert) error {
	body, err := marshalJSONNoEscapeHTML(alert)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: OperatorAlertTimeoutMs * time.Millisecond}
	res, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("operator alert webhook responded %d", res.StatusCode)
	}
	return nil
}

// marshalJSONNoEscapeHTML mirrors JSON.stringify: <, > and & are not
// escaped.
func marshalJSONNoEscapeHTML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode appends '\n'; JSON.stringify has no trailing newline.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
