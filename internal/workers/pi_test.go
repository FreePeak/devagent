// Go port of test/workers/pi.test.ts (argument building + output parsing).
package workers

import (
	"strings"
	"testing"
)

func piBaseOpts() WorkerSpawnOptions {
	return WorkerSpawnOptions{Prompt: "do the thing", Cwd: "/tmp/work", TimeoutMs: 60_000}
}

const piSessionHeader = `{"type":"session","version":3,"id":"abc-123","timestamp":"2026-09-01T00:00:00.000Z","cwd":"/tmp/work"}`
const piAssistantEnd = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"RESULT_TEXT"}],"stopReason":"stop"}}`

func TestBuildPiArgs(t *testing.T) {
	cases := []struct {
		name   string
		opts   WorkerSpawnOptions
		resume bool
		want   []string
	}{
		{"minimum argv", piBaseOpts(), false, []string{"--mode", "json", "-p", "do the thing"}},
		{
			"provider-qualified model forwarded",
			func() WorkerSpawnOptions { o := piBaseOpts(); o.Model = "omniroute/bai/glm-5.3-flash"; return o }(),
			false,
			[]string{"--mode", "json", "-p", "do the thing", "--model", "omniroute/bai/glm-5.3-flash"},
		},
		{
			"driver-tier alias dropped",
			func() WorkerSpawnOptions { o := piBaseOpts(); o.Model = "coding"; return o }(),
			false,
			[]string{"--mode", "json", "-p", "do the thing"},
		},
		{
			"thinking from variant",
			func() WorkerSpawnOptions { o := piBaseOpts(); o.Variant = "high"; return o }(),
			false,
			[]string{"--mode", "json", "-p", "do the thing", "--thinking", "high"},
		},
		{"resume argv", piBaseOpts(), true, []string{"--mode", "json", "--continue", "Continue"}},
		{
			"resume keeps model and thinking",
			func() WorkerSpawnOptions { o := piBaseOpts(); o.Model = "omniroute/x"; o.Variant = "max"; return o }(),
			true,
			[]string{"--mode", "json", "--continue", "Continue", "--model", "omniroute/x", "--thinking", "max"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertArgv(t, BuildPiArgs(tc.opts, tc.resume), tc.want)
		})
	}
}

func TestInterpretPi(t *testing.T) {
	cases := []struct {
		name       string
		run        SpawnCliResult
		sessionId  string
		resultText string
		isError    bool
	}{
		{
			name:      "session header id",
			run:       SpawnCliResult{Stdout: piSessionHeader + "\n" + piAssistantEnd + "\n"},
			sessionId: "abc-123", resultText: "RESULT_TEXT",
		},
		{
			name: "keeps LAST textual assistant message (answer after tool turns)",
			run: SpawnCliResult{Stdout: piSessionHeader + "\n" +
				`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","toolName":"bash"}],"stopReason":"toolUse"}}` + "\n" +
				`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"LATER"}],"stopReason":"stop"}}` + "\n"},
			sessionId: "abc-123", resultText: "LATER",
		},
		{
			name: "falls back to earlier textual turn when later turns are textless",
			run: SpawnCliResult{Stdout: piSessionHeader + "\n" +
				`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ANSWER"}],"stopReason":"stop"}}` + "\n" +
				`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","toolName":"bash"}],"stopReason":"toolUse"}}` + "\n"},
			sessionId: "abc-123", resultText: "ANSWER",
		},
		{
			name: "ignores user message_end",
			run: SpawnCliResult{Stdout: piSessionHeader + "\n" +
				`{"type":"message_end","message":{"role":"user","content":[{"type":"text","text":"PROMPT ECHO"}]}}` + "\n" +
				piAssistantEnd + "\n"},
			sessionId: "abc-123", resultText: "RESULT_TEXT",
		},
		{
			name: "concatenates multiple text parts",
			run: SpawnCliResult{Stdout: piSessionHeader + "\n" +
				`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"A"},{"type":"text","text":"B"}],"stopReason":"stop"}}` + "\n"},
			sessionId: "abc-123", resultText: "AB",
		},
		{
			name:      "survives garbage lines interleaved with valid NDJSON",
			run:       SpawnCliResult{Stdout: piSessionHeader + "\nnot json at all\n" + piAssistantEnd + "\n"},
			sessionId: "abc-123", resultText: "RESULT_TEXT",
		},
		{
			name:      "legacy single-JSON envelope",
			run:       SpawnCliResult{Stdout: `{"result":"legacy","session_id":"s-1"}`},
			sessionId: "s-1", resultText: "legacy",
		},
		{
			name:      "no assistant message yields empty result",
			run:       SpawnCliResult{Stdout: piSessionHeader + "\n"},
			sessionId: "abc-123", resultText: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := InterpretPiForTest(tc.run)
			if o.SessionId != tc.sessionId {
				t.Fatalf("sessionId = %q, want %q", o.SessionId, tc.sessionId)
			}
			if o.ResultText != tc.resultText {
				t.Fatalf("resultText = %q, want %q", o.ResultText, tc.resultText)
			}
			if o.IsError != tc.isError {
				t.Fatalf("isError = %v, want %v", o.IsError, tc.isError)
			}
		})
	}
}

// Stream-error case: provider failure at exit 0 via assistant errorMessage.
func TestInterpretPi_ErrorMessageAtExit0(t *testing.T) {
	stdout := piSessionHeader + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[],"errorMessage":"Provider 401: unauthorized","stopReason":"error"}}` + "\n"
	o := InterpretPiForTest(SpawnCliResult{Stdout: stdout})
	if !o.IsError {
		t.Fatalf("expected isError, got %+v", o)
	}
	if !contains(o.ErrorText, "401") {
		t.Fatalf("errorText = %q", o.ErrorText)
	}
	if o.ResultText != "" {
		t.Fatalf("resultText must be empty: %q", o.ResultText)
	}
}

func TestInterpretPi_NonZeroExitNoOutput(t *testing.T) {
	o := InterpretPiForTest(SpawnCliResult{ExitCode: 1, Stderr: "boom"})
	if !o.IsError || o.ErrorText != "boom" {
		t.Fatalf("outcome = %+v", o)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
