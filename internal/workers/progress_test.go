// Go port of test/progress.test.ts (PRD Q33): the no-progress watchdog
// must not treat model deliberation as progress.
package workers

import "testing"

func TestIsPureThinkingLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"thinking_delta", `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" = \""}}"`, true},
		{"thinking_start", `{"type":"message_update","assistantMessageEvent":{"type":"thinking_start"}}`, true},
		{"tool line not flagged", `{"type":"tool_execution_start","toolName":"bash"}`, false},
		{"text line not flagged", `{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"hi"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPureThinkingLine(tc.line); got != tc.want {
				t.Fatalf("IsPureThinkingLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestIsNdjsonProgressLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"tool_execution_start", `{"type":"tool_execution_start","toolName":"read","args":{}}`, true},
		{"tool_execution_end", `{"type":"tool_execution_end","toolName":"bash","result":{}}`, true},
		{"text_end (answer turn completed)", `{"type":"message_update","assistantMessageEvent":{"type":"text_end","content":"hi"}}`, true},
		{"toolcall_start (tool arg assembled)", `{"type":"message_update","assistantMessageEvent":{"type":"toolcall_start","id":"c1","toolName":"bash"}}`, true},
		{"thinking_delta not progress", `{"type":"message_update","assistantMessageEvent":{"type":"thinking_delta","delta":" x"}}`, false},
		{"empty line", "", false},
		{"blank line", "   ", false},
		{"session header", `{"type":"session","id":"abc"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNdjsonProgressLine(tc.line); got != tc.want {
				t.Fatalf("IsNdjsonProgressLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
