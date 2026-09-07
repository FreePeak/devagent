package tui

import (
	"encoding/json"
	"sync"
	"time"
)

// FetchSnapshot polls /status + /agents + /history + /sessions concurrently
// and normalizes the envelopes. Never fails: every field tolerates a partial
// outage (the header degrades to DAEMON UNREACHABLE instead of throwing).
// Port of src/tui/tui.ts fetchSnapshot.
func FetchSnapshot(opts TuiOptions) *Snapshot {
	var (
		stStatus int
		stValue  any
		agValue  any
		hiValue  any
		seValue  any
		wg       sync.WaitGroup
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		stStatus, stValue = GetJSON(opts, "/status")
	}()
	go func() {
		defer wg.Done()
		_, agValue = GetJSON(opts, "/agents")
	}()
	go func() {
		defer wg.Done()
		_, hiValue = GetJSON(opts, "/history?limit="+itoa(HistoryRows))
	}()
	go func() {
		defer wg.Done()
		_, seValue = GetJSON(opts, "/sessions")
	}()
	wg.Wait()

	snap := &Snapshot{
		Status:     decodeStatus(stValue),
		Agents:     NormalizeAgents(agValue),
		History:    normalizeHistory(hiValue),
		Sessions:   normalizeSessions(seValue),
		Reachable:  stStatus != 0,
		AuthFailed: stStatus == 401,
		FetchedAt:  time.Now(),
	}
	return snap
}

// decodeStatus maps a /status payload into StatusPayload; anything
// non-object degrades to nil (the renderer's UNREACHABLE path).
func decodeStatus(value any) *StatusPayload {
	if value == nil {
		return nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var s StatusPayload
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return &s
}

// normalizeHistory accepts either the bare-array shape or the
// {records:[...]} envelope the daemon answers today, unwrapping the envelope
// so the history panel renders (the bare-array expectation silently yielded
// [] forever).
func normalizeHistory(value any) []HistoryRow {
	switch v := value.(type) {
	case []any:
		return rowsFromAny(v)
	case map[string]any:
		if rec, ok := v["records"].([]any); ok {
			return rowsFromAny(rec)
		}
	}
	return []HistoryRow{}
}

func rowsFromAny(raw []any) []HistoryRow {
	rows := make([]HistoryRow, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}
