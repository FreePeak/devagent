// Shared progress-classifier helpers for worker adapters (PRD Q33).
//
// The no-progress watchdog must not treat model deliberation as progress:
// glm-style providers stream tens of MB of `thinking_delta` while making
// zero tool calls (2026-08-31 evidence: 60k+ deltas over a full hour), and
// pi does the same on hard goals (2026-09-01: 25MB thinking-only, 13+ min).
// A stream line counts as progress when it carries *new work* — a tool call
// starting/ending, or assistant text (the answer) — never bare thinking.
package workers

import "strings"

// IsPureThinkingLine: lines that are pure model deliberation — never
// progress, any adapter.
func IsPureThinkingLine(line string) bool {
	if strings.Contains(line, `"thinking_delta"`) {
		return true
	}
	// pi: message_update whose assistantMessageEvent is a thinking variant.
	if strings.Contains(line, `"type":"thinking_delta"`) || strings.Contains(line, `"type":"thinking_start"`) {
		return true
	}
	// grok: streaming-json thought chunks (observed 2026-09-06: 48 thought
	// chunks in a 10s echo run — pure deliberation, never progress).
	if strings.Contains(line, `"type":"thought"`) {
		return true
	}
	return false
}

// IsNdjsonProgressLine is the NDJSON-shape progress predicate shared by
// omp, pi, and grok (omp/pi emit `{"type":"tool_execution_start|end", ...}`
// and text-bearing message_update events; grok emits
// `tool_call`/`tool_call_update` and `{"type":"text","data":...}` chunks).
// Returns true when the line evidences new work.
func IsNdjsonProgressLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	if IsPureThinkingLine(line) {
		return false
	}
	if strings.Contains(line, `"tool_execution_start"`) {
		return true
	}
	if strings.Contains(line, `"tool_execution_end"`) {
		return true
	}
	// Assistant text completed (answer turn) counts; thinking text does not.
	if strings.Contains(line, `"type":"text_end"`) {
		return true
	}
	if strings.Contains(line, `"type":"toolcall_start"`) {
		return true
	}
	// grok streaming-json shapes (captured 2026-09-06): tool calls and
	// finalized text chunks evidence new work; headers/usage do not.
	if strings.Contains(line, `"type":"tool_call"`) {
		return true
	}
	if strings.Contains(line, `"type":"tool_call_update"`) {
		return true
	}
	if strings.Contains(line, `"type":"text","data"`) {
		return true
	}
	return false
}
