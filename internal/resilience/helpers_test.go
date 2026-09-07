package resilience

import (
	"encoding/json"
	"time"

	"github.com/FreePeak/devagent/internal/lessons"
)

// EventsFilePath is the orchestration events.jsonl path the TS tests write
// fixtures to (test/degradation.test.ts imports EVENTS_FILE from
// src/lessons/guard.js).
const EventsFilePath = lessons.EventsFile

// jsonCompact marshals a fixture row the way the vitest fixtures do
// (JSON.stringify, single line).
func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// jsonUnmarshal is json.Unmarshal (kept as a named indirection for the
// proxy-state tests).
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// parseUnixMs builds a time.Time from a fixed Unix-millisecond value
// (deterministic test clocks).
func parseUnixMs(ms int64) time.Time {
	return time.Unix(ms/1000, (ms%1000)*int64(time.Millisecond)).UTC()
}

// formatIsoMs formats a time the way `new Date().toISOString()` does.
func formatIsoMs(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// unixMsFunc builds an injectable Now func from a fixed Unix-millisecond
// value.
func unixMsFunc(ms int64) func() time.Time {
	return func() time.Time { return parseUnixMs(ms) }
}
