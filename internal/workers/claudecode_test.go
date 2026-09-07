// Go port of test/claude-code-adapter.test.ts (envelope parsing) plus the
// model-family argv normalization and the sandboxed spawn env-scrub
// integration (test/sandbox.test.ts adapter block) via a fake claude bin.

package workers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInterpretClaude_ArrayEnvelope(t *testing.T) {
	stdout, _ := json.Marshal([]map[string]any{
		{"type": "system", "subtype": "init", "session_id": "s-1"},
		{"type": "assistant", "message": map[string]any{}},
		{
			"type": "result", "subtype": "success", "is_error": false,
			"session_id": "s-1",
			"result":     `[{"id":"T1","title":"t","prompt":"p","dependsOn":[]}]`,
		},
	})
	o := InterpretClaudeForTest(SpawnCliResult{ExitCode: 0, Stdout: string(stdout)})
	if o.isError {
		t.Fatalf("isError = %v", o.isError)
	}
	if o.sessionId != "s-1" {
		t.Fatalf("sessionId = %q", o.sessionId)
	}
	// errorText carries the raw result string; the fixture parses as a
	// 1-element JSON array.
	var parsed []any
	if err := json.Unmarshal([]byte(o.errorText), &parsed); err != nil {
		t.Fatalf("errorText = %q (not JSON): %v", o.errorText, err)
	}
	if len(parsed) != 1 {
		t.Fatalf("errorText array length = %d, want 1", len(parsed))
	}
}

func TestInterpretClaude_LegacyObjectEnvelope(t *testing.T) {
	stdout := `{"type":"result","is_error":false,"session_id":"s-2","result":"done"}`
	o := InterpretClaudeForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
	if o.sessionId != "s-2" || o.errorText != "done" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestInterpretClaude_Garbage(t *testing.T) {
	o := InterpretClaudeForTest(SpawnCliResult{ExitCode: 0, Stdout: "not json at all"})
	if o.parsed != nil || o.sessionId != "" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestClaudeBaseArgs_ModelFamilyNormalization(t *testing.T) {
	cases := []struct {
		model string
		want  []string
	}{
		// Known claude families pass through (minus #variant suffix).
		{"claude-opus-4-6", []string{"-p", "x", "--output-format", "json", "--model", "claude-opus-4-6"}},
		{"sonnet", []string{"-p", "x", "--output-format", "json", "--model", "sonnet"}},
		{"haiku-4", []string{"-p", "x", "--output-format", "json", "--model", "haiku-4"}},
		{"opus#fast", []string{"-p", "x", "--output-format", "json"}},
		// Driver tier aliases are claude-proxy selectors, not API-key model
		// ids (2026-09-01 loop 58 "403 Combo" burn): dropped.
		{"coding", []string{"-p", "x", "--output-format", "json"}},
		{"", []string{"-p", "x", "--output-format", "json"}},
	}
	for _, tc := range cases {
		opts := WorkerSpawnOptions{Prompt: "x", Model: tc.model}
		assertArgv(t, claudeBaseArgs(opts), tc.want)
	}
}

func TestClaudeBaseArgs_MaxTurns(t *testing.T) {
	maxSteps := 7
	opts := WorkerSpawnOptions{Prompt: "p", MaxSteps: &maxSteps}
	assertArgv(t, claudeBaseArgs(opts), []string{"-p", "p", "--output-format", "json", "--max-turns", "7"})
}

// Adapter spawn integration: the claude-code spawn reaches the child with
// the scrubbed sandbox env (NPM_TOKEN stripped, PATH ensured) and parses
// the canned JSON (test/sandbox.test.ts "claude-code spawn reaches
// execFile scrubbed").
func TestClaudeCodeAdapter_SpawnScrubbedEnv(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	// The fake claude prints a canned result AND dumps $NPM_TOKEN to a
	// side file so the test can observe the env it received.
	script := "#!/bin/sh\necho \"$NPM_TOKEN\" > \"" + dir + "/envdump\"\necho '{\"type\":\"result\",\"is_error\":false,\"session_id\":\"cc-1\",\"result\":\"ok\"}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("NPM_TOKEN", "leak")

	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	adapter := &ClaudeCodeAdapter{}
	res := adapter.Spawn(WorkerSpawnOptions{Prompt: "do thing", Cwd: dir, TimeoutMs: 15_000})
	if res.ExitCode != 0 || res.TimedOut {
		t.Fatalf("result = %+v", res)
	}
	if res.ResultText != "ok" || res.SessionId != "cc-1" {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Events) != 1 || res.Events[0]["type"] != "result" {
		t.Fatalf("events = %+v", res.Events)
	}
	dump, err := os.ReadFile(filepath.Join(dir, "envdump"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(dump)) != "" {
		t.Fatalf("NPM_TOKEN leaked into the worker env: %q", string(dump))
	}
}
