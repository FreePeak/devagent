package herdr

import "strings"

// IsPureThinkingLine mirrors isPureThinkingLine: lines that are pure model
// deliberation — never progress, any adapter.
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

// IsNdjsonProgressLine mirrors isNdjsonProgressLine: the NDJSON-shape progress
// predicate shared by omp, pi, and grok (omp/pi emit
// `{"type":"tool_execution_start|end", ...}` and text-bearing message_update
// events; grok emits `tool_call`/`tool_call_update` and
// `{"type":"text","data":...}` chunks). Returns true when the line evidences
// new work.
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
