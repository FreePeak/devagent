package cli

// Go-only tests for the guard CLI wiring (actions_guard.go): the frozen
// flag surface, the [cc-guard] output literals, and the exit codes the TS
// original assigns (issue #251). The claude child is a throwaway shell
// script so SpawnClaude exercises the real spawn/parse path hermetically.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/sessionguard"
)

// runGuardCmdCapture runs the full CLI with the given argv and captures
// os.Stdout and os.Stderr (the guard bodies print through fmt directly,
// bypassing cobra's writers). Fails the test on a dispatch error.
func runGuardCmdCapture(t *testing.T, args ...string) (string, string) {
	t.Helper()
	root := NewRoot()
	root.SetArgs(args)
	root.SilenceUsage = true

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outDone := make(chan string, 1)
	errDone := make(chan string, 1)
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, outR)
		outDone <- b.String()
	}()
	go func() {
		var b strings.Builder
		_, _ = io.Copy(&b, errR)
		errDone <- b.String()
	}()

	execErr := root.Execute()

	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, errOut := <-outDone, <-errDone
	if execErr != nil {
		t.Fatalf("execute %v: %v", args, execErr)
	}
	return out, errOut
}

// guardWriteScript writes an executable sh script (a stand-in claude).
func guardWriteScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guard-fake-claude.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertExitCode compares commandExitCode against want (nil = unset).
func assertExitCode(t *testing.T, want *int) {
	t.Helper()
	if want == nil {
		if commandExitCode != nil {
			t.Fatalf("exit code = %d, want none", *commandExitCode)
		}
		return
	}
	if commandExitCode == nil || *commandExitCode != *want {
		got := "<nil>"
		if commandExitCode != nil {
			got = fmt.Sprintf("%d", *commandExitCode)
		}
		t.Fatalf("exit code = %s, want %d", got, *want)
	}
}

func intPtr(v int) *int { return &v }

// interruptedTranscript is the golden interrupted-session fixture: a user
// turn followed by a synthetic API-error assistant turn (isApiErrorMessage
// + <synthetic> model) that never recovers.
func interruptedTranscript(sessionID string) string {
	return `{"type":"user","sessionId":"` + sessionID + `","timestamp":"2026-09-08T00:00:00.000Z"}` + "\n" +
		`{"type":"assistant","sessionId":"` + sessionID + `","timestamp":"2026-09-08T00:00:05.000Z","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Connection lost mid-response."}]}}` + "\n"
}

// guardStatusFixture wires a CLAUDE_CONFIG_DIR with a transcript for the
// slug of a fresh project dir and returns the project dir.
func guardStatusFixture(t *testing.T, transcript string) string {
	t.Helper()
	project := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	slugDir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", sessionguard.ProjectSlug(project))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slugDir, "session-1.jsonl"), []byte(transcript), 0o644); err != nil {
		t.Fatal(err)
	}
	return project
}

// latestFixtureFile returns the transcript path guard-status should pick
// (the fixture writes exactly one).
func latestFixtureFile(project string) string {
	return filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects",
		sessionguard.ProjectSlug(project), "session-1.jsonl")
}

// TestGuardWiring: the frozen surface lands with exact TS types/defaults,
// the commands leave the notPorted stub map, and both resolve to real
// commands in the built tree.
func TestGuardWiring(t *testing.T) {
	root := NewRoot()
	for _, dotted := range []string{"guard", "guard-status"} {
		cmd, _, err := root.Find(strings.Split(dotted, " "))
		if err != nil {
			t.Fatalf("%s not registered: %v", dotted, err)
		}
		if strings.Contains(cmd.Short, "Not yet ported") {
			t.Fatalf("%s still resolves to the exit-3 stub: %q", dotted, cmd.Short)
		}
		if _, ok := notPortedIssue[dotted]; ok {
			t.Errorf("%s still listed in notPortedIssue", dotted)
		}
		if !handled(dotted) {
			t.Errorf("%s missing from handled() — registerStubs would stack a stub on top", dotted)
		}
	}

	guard, _, err := root.Find([]string{"guard"})
	if err != nil {
		t.Fatal(err)
	}
	for name, wantType := range map[string]string{
		"resume-prompt": "string", "max-attempts": "int",
		"base-delay-ms": "int", "max-delay-ms": "int", "no-progress-timeout-ms": "int",
	} {
		f := guard.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("guard --%s not registered", name)
		}
		if f.Value.Type() != wantType {
			t.Errorf("guard --%s type = %q, want %q", name, f.Value.Type(), wantType)
		}
	}
	for name, want := range map[string]string{
		"resume-prompt": "Continue", "base-delay-ms": "2000", "max-delay-ms": "60000",
	} {
		f := guard.Flags().Lookup(name)
		if f != nil && f.DefValue != want {
			t.Errorf("guard --%s default = %q, want %q", name, f.DefValue, want)
		}
	}
	// --max-attempts has no TS default (undefined -> env/Infinity): the
	// registered flag stays at the zero value and Changed() distinguishes
	// an explicit --max-attempts 0 (empty loop) from the unset case.
	if f := guard.Flags().Lookup("max-attempts"); f.DefValue != "0" {
		t.Errorf("guard --max-attempts default = %q, want unset 0", f.DefValue)
	}

	status, _, err := root.Find([]string{"guard-status"})
	if err != nil {
		t.Fatal(err)
	}
	for name, wantType := range map[string]string{
		"project-dir": "string", "resume": "bool", "resume-prompt": "string",
	} {
		f := status.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("guard-status --%s not registered", name)
		}
		if f.Value.Type() != wantType {
			t.Errorf("guard-status --%s type = %q, want %q", name, f.Value.Type(), wantType)
		}
	}
	if f := status.Flags().Lookup("resume-prompt"); f.DefValue != "Continue" {
		t.Errorf("guard-status --resume-prompt default = %q, want Continue", f.DefValue)
	}
}

// TestGuardCompletesAfterFirstAttempt: a claude stand-in that emits a
// stream-json init + success result and exits 0 completes on the first
// launch, forwards the child lines to stdout, and exits clean.
func TestGuardCompletesAfterFirstAttempt(t *testing.T) {
	claude := guardWriteScript(t, `
echo '{"type":"system","subtype":"init","session_id":"sess-ok"}'
echo '{"type":"result","is_error":false,"session_id":"sess-ok"}'
exit 0
`)
	commandExitCode = nil
	out, errOut := runGuardCmdCapture(t, "guard", claude)
	assertExitCode(t, nil)
	if !strings.Contains(errOut, "[cc-guard] completed after 1 attempt(s), resumed 0 time(s)") {
		t.Fatalf("stderr %q", errOut)
	}
	if !strings.Contains(out, `"session_id":"sess-ok"`) {
		t.Fatalf("child stdout not forwarded: %q", out)
	}
}

// TestGuardGivesUpAfterAttempts: an always-failing claude exhausts
// --max-attempts (with zero-cost backoff) and reports attempts_exhausted
// with exit 1 — pinning that the Changed()-based MaxAttempts plumbing
// passes the flag through (a String-typed regression would report
// "after 0 attempt(s)" or loop forever).
func TestGuardGivesUpAfterAttempts(t *testing.T) {
	claude := guardWriteScript(t, `exit 1
`)
	commandExitCode = nil
	_, errOut := runGuardCmdCapture(t, "guard",
		"--max-attempts", "2", "--base-delay-ms", "1", "--max-delay-ms", "1", claude)
	assertExitCode(t, intPtr(1))
	if !strings.Contains(errOut, "[cc-guard] gave up after 2 attempt(s): attempts_exhausted") {
		t.Fatalf("stderr %q", errOut)
	}
}

// TestGuardExplicitZeroBackoffRetriesImmediately: the TS spread
// `{...DEFAULT_BACKOFF, ...options.backoff}` applies an explicit 0 verbatim,
// so a zero base delay must not fall back to the 2s default (RunGuard merge
// regression guard). The retry line reports the computed delay, so the
// assertion reads "in 0ms" off stderr — deterministic, unlike the former
// wall-clock bound, which the correct zero-delay path also breached (3.5s)
// under `go test ./...` parallel-package load.
func TestGuardExplicitZeroBackoffRetriesImmediately(t *testing.T) {
	claude := guardWriteScript(t, `exit 1
`)
	commandExitCode = nil
	_, errOut := runGuardCmdCapture(t, "guard",
		"--max-attempts", "2", "--base-delay-ms", "0", "--max-delay-ms", "60000", claude)
	assertExitCode(t, intPtr(1))
	if !strings.Contains(errOut, "[cc-guard] gave up after 2 attempt(s): attempts_exhausted") {
		t.Fatalf("stderr %q", errOut)
	}
	if !strings.Contains(errOut, "resuming session ? in 0ms") {
		t.Fatalf("zero base delay not delivered to RunGuard: stderr %q", errOut)
	}
}

// TestGuardStatusOK: a healthy transcript reports the last session id,
// file, and activity timestamp with no exit code.
func TestGuardStatusOK(t *testing.T) {
	dir := guardStatusFixture(t, `{"type":"user","sessionId":"sess-1","timestamp":"2026-09-08T00:00:00.000Z"}
{"type":"assistant","sessionId":"sess-1","timestamp":"2026-09-08T00:00:05.000Z","message":{"model":"claude-x","content":[{"type":"text","text":"done"}]}}
`)
	commandExitCode = nil
	out, errOut := runGuardCmdCapture(t, "guard-status", "--project-dir", dir)
	assertExitCode(t, nil)
	if errOut != "" {
		t.Fatalf("unexpected stderr %q", errOut)
	}
	want := "OK session sess-1 (" + latestFixtureFile(dir) + ") last activity 2026-09-08T00:00:05.000Z"
	if !strings.Contains(out, want) {
		t.Fatalf("stdout %q, want %q", out, want)
	}
}

// TestGuardStatusInterruptedWithoutResume: a transcript ending on a
// synthetic API error prints the INTERRUPTED block plus the resume hint and
// exits 1 when --resume is absent.
func TestGuardStatusInterruptedWithoutResume(t *testing.T) {
	dir := guardStatusFixture(t, interruptedTranscript("sess-7"))
	commandExitCode = nil
	out, _ := runGuardCmdCapture(t, "guard-status", "--project-dir", dir)
	assertExitCode(t, intPtr(1))
	want := "INTERRUPTED session sess-7 (" + latestFixtureFile(dir) + ")\n" +
		"last error: API Error: Connection lost mid-response.\n" +
		"resume with: claude --resume sess-7"
	if !strings.Contains(out, want) {
		t.Fatalf("stdout %q, want %q", out, want)
	}
}

// TestGuardStatusResumesHeadlessly: --resume relaunches the interrupted
// session via `claude --resume <id> -p <prompt>`; a claude stand-in records
// its argv so the resume wiring is observable end to end.
func TestGuardStatusResumesHeadlessly(t *testing.T) {
	dir := guardStatusFixture(t, interruptedTranscript("sess-7"))
	binDir := t.TempDir()
	record := filepath.Join(t.TempDir(), "argv.txt")
	claude := guardWriteScript(t, `
printf '%s\n' "$@" > "$GUARD_TEST_RECORD"
echo '{"type":"system","subtype":"init","session_id":"sess-7"}'
echo '{"type":"result","is_error":false,"session_id":"sess-7"}'
`)
	// The script must live at a path named "claude" on PATH for the resume
	// argv's bare binary to resolve to it.
	if err := os.Rename(claude, filepath.Join(binDir, "claude")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GUARD_TEST_RECORD", record)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	commandExitCode = nil
	_, errOut := runGuardCmdCapture(t, "guard-status", "--project-dir", dir, "--resume")
	assertExitCode(t, nil)
	if !strings.Contains(errOut, "[cc-guard] session sess-7 resumed and completed") {
		t.Fatalf("stderr %q", errOut)
	}
	blob, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("resume argv record missing: %v", err)
	}
	if got, want := strings.TrimSpace(string(blob)), "--resume\nsess-7\n-p\nContinue"; got != want {
		t.Fatalf("resume argv %q, want %q", got, want)
	}
}

// TestGuardStatusNoTranscriptDir: a slug dir that does not exist reports
// "No transcript directory at" (the TS latestTranscript throw) with exit 1.
func TestGuardStatusNoTranscriptDir(t *testing.T) {
	dir := t.TempDir() // no <CLAUDE_CONFIG_DIR>/projects/<slug> anywhere
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	commandExitCode = nil
	_, errOut := runGuardCmdCapture(t, "guard-status", "--project-dir", dir)
	assertExitCode(t, intPtr(1))
	slugDir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", sessionguard.ProjectSlug(dir))
	if !strings.Contains(errOut, "No transcript directory at "+slugDir) {
		t.Fatalf("stderr %q, want transcript-directory report at %s", errOut, slugDir)
	}
}

// TestGuardStatusNoTranscripts: a present-but-empty slug dir reports
// "No transcripts found in" with exit 1.
func TestGuardStatusNoTranscripts(t *testing.T) {
	project := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	slugDir := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", sessionguard.ProjectSlug(project))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatal(err)
	}
	commandExitCode = nil
	_, errOut := runGuardCmdCapture(t, "guard-status", "--project-dir", project)
	assertExitCode(t, intPtr(1))
	if !strings.Contains(errOut, "No transcripts found in "+slugDir) {
		t.Fatalf("stderr %q, want transcripts report at %s", errOut, slugDir)
	}
}

// TestWiredCommandsAreHandled: every wired command that also appears in the
// frozen surface must be listed in handled() — otherwise registerStubs
// stacks an exit-3 stub with the same name on top of the real command
// (cobra resolves the first match, so the bug lurks silently).
func TestWiredCommandsAreHandled(t *testing.T) {
	for dotted := range wiredCommands() {
		if _, frozen := frozenSurface[dotted]; frozen && !handled(dotted) {
			t.Errorf("wired command %q is missing from handled()", dotted)
		}
	}
}
