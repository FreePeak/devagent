package sessionguard

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// sgo helpers (sgo* prefix per batch contract; single test package).

func sgoIntPtr(v int) *int { return &v }

func sgoWriteScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

type sgoLineCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *sgoLineCollector) OnLine(line string, stream string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *sgoLineCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.lines)
}

func TestParseStreamLine(t *testing.T) {
	t.Run("classifies init events with session id", func(t *testing.T) {
		event := ParseStreamLine(`{"type":"system","subtype":"init","session_id":"abc-123"}`)
		if event.Kind != KindInit || event.SessionID != "abc-123" {
			t.Fatalf("got %+v", event)
		}
		if event.Attempt != 0 || event.Error != "" || event.IsError || event.Text != "" {
			t.Fatalf("unexpected extra fields: %+v", event)
		}
	})

	t.Run("classifies api_retry events", func(t *testing.T) {
		event := ParseStreamLine(`{"type":"system","subtype":"api_retry","attempt":2,"max_retries":10,"retry_delay_ms":1247,"error_status":null,"error":"unknown"}`)
		if event.Kind != KindAPIRetry || event.Attempt != 2 || event.DelayMs == nil || *event.DelayMs != 1247 {
			t.Fatalf("got %+v", event)
		}
		if event.MaxRetries == nil || *event.MaxRetries != 10 {
			t.Fatalf("maxRetries: %+v", event)
		}
		if event.ErrorStatus != nil || event.Error != "unknown" {
			t.Fatalf("error fields: %+v", event)
		}
	})

	t.Run("classifies synthetic assistant errors from mid-stream drops", func(t *testing.T) {
		line := `{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Connection lost mid-response."}]}}`
		event := ParseStreamLine(line)
		if event.Kind != KindSyntheticError || event.Text != "API Error: Connection lost mid-response." {
			t.Fatalf("got %+v", event)
		}
	})

	t.Run("classifies result events including is_error", func(t *testing.T) {
		event := ParseStreamLine(`{"type":"result","is_error":true,"session_id":"s1"}`)
		if event.Kind != KindResult || !event.IsError || event.SessionID != "s1" {
			t.Fatalf("got %+v", event)
		}
	})

	t.Run("returns other for malformed or unrelated lines", func(t *testing.T) {
		if got := ParseStreamLine("not json"); got.Kind != KindOther {
			t.Fatalf("got %+v", got)
		}
		if got := ParseStreamLine(`{"type":"assistant"}`); got.Kind != KindOther {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestIsNonRetryableApiError(t *testing.T) {
	if !IsNonRetryableApiError("Invalid API key provided") {
		t.Fatal("auth failure should be non-retryable")
	}
	if !IsNonRetryableApiError("your credit balance is too low") {
		t.Fatal("billing failure should be non-retryable")
	}
	if IsNonRetryableApiError("API Error: Connection lost mid-response") {
		t.Fatal("mid-stream drop should stay retryable")
	}
}

func TestBuildResumeArgv(t *testing.T) {
	t.Run("replaces the original prompt with a resume invocation", func(t *testing.T) {
		argv := BuildResumeArgv(
			[]string{"claude", "-p", "do the thing", "--permission-mode", "bypassPermissions"},
			"sess-9",
			"Continue",
		)
		want := []string{"claude", "--permission-mode", "bypassPermissions", "--resume", "sess-9", "-p", "Continue"}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("got %v", argv)
		}
	})

	t.Run("handles --print long form and preserves other flags", func(t *testing.T) {
		argv := BuildResumeArgv([]string{"claude", "--print", "task"}, "s", "go")
		want := []string{"claude", "--resume", "s", "-p", "go"}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("got %v", argv)
		}
	})

	t.Run("replaces a prior --resume id when re-resuming", func(t *testing.T) {
		argv := BuildResumeArgv([]string{"claude", "--resume", "old-s", "-p", "Continue"}, "new-s", "Continue")
		want := []string{"claude", "--resume", "new-s", "-p", "Continue"}
		if !reflect.DeepEqual(argv, want) {
			t.Fatalf("got %v", argv)
		}
	})
}

func TestBackoffDelay(t *testing.T) {
	t.Run("grows exponentially up to the ceiling", func(t *testing.T) {
		fixed := func() float64 { return 0.5 } // zero jitter
		if got := BackoffDelay(1, DEFAULT_BACKOFF, fixed); got != 2_000 {
			t.Fatalf("attempt 1: got %d", got)
		}
		if got := BackoffDelay(3, DEFAULT_BACKOFF, fixed); got != 8_000 {
			t.Fatalf("attempt 3: got %d", got)
		}
		if got := BackoffDelay(10, DEFAULT_BACKOFF, fixed); got != 60_000 {
			t.Fatalf("attempt 10: got %d", got)
		}
	})

	t.Run("applies bounded jitter", func(t *testing.T) {
		value := BackoffDelay(1, BackoffOptions{BaseDelayMs: 1000, MaxDelayMs: 8000, Factor: 2}, func() float64 { return 0 })
		if value < 750 || value > 1250 {
			t.Fatalf("jittered delay out of bounds: %d", value)
		}
	})
}

func sgoFakeRunner(script []AttemptResult) AttemptRunner {
	call := 0
	return func(argv []string, handler LineHandler, opts SpawnOpts) (AttemptResult, error) {
		outcome := script[call]
		if call < len(script)-1 {
			call++
		}
		return outcome, nil
	}
}

func TestRunGuardResumesUntilSuccess(t *testing.T) {
	var seenArgv [][]string
	var mu sync.Mutex
	var logLines []string
	var sleeps []int
	call := 0
	runner := AttemptRunner(func(argv []string, handler LineHandler, opts SpawnOpts) (AttemptResult, error) {
		mu.Lock()
		seenArgv = append(seenArgv, append([]string(nil), argv...))
		mu.Unlock()
		call++
		if call < 3 {
			return AttemptResult{
				ExitCode:           sgoIntPtr(1),
				SessionID:          "sess-7",
				SawResult:          true,
				SyntheticErrorText: "API Error: Connection lost mid-response.",
			}, nil
		}
		return AttemptResult{ExitCode: sgoIntPtr(0), SessionID: "sess-7", SawResult: true}, nil
	})
	result, err := RunGuard(GuardOptions{
		Argv:        []string{"claude", "-p", "ship it"},
		Runner:      runner,
		MaxAttempts: sgoIntPtr(5),
		Sleep:       func(ms int) { sleeps = append(sleeps, ms) },
		Random:      func() float64 { return 0.5 },
		Log: func(message string) {
			mu.Lock()
			defer mu.Unlock()
			logLines = append(logLines, message)
		},
	})
	if err != nil {
		t.Fatalf("RunGuard: %v", err)
	}
	if !result.OK || result.Attempts != 3 || result.Resumed != 2 || result.SessionID != "sess-7" {
		t.Fatalf("result: %+v", result)
	}
	wantSecond := []string{"claude", "--resume", "sess-7", "-p", "Continue"}
	if !reflect.DeepEqual(seenArgv[1], wantSecond) {
		t.Fatalf("second argv: got %v", seenArgv[1])
	}
	if !reflect.DeepEqual(sleeps, []int{2000, 4000}) {
		t.Fatalf("sleeps: %v", sleeps)
	}
	wantLog := []string{
		"[cc-guard] attempt 1/5 failed (exit 1); resuming session sess-7 in 2000ms",
		"[cc-guard] attempt 2/5 failed (exit 1); resuming session sess-7 in 4000ms",
	}
	if !reflect.DeepEqual(logLines, wantLog) {
		t.Fatalf("log lines: got %v", logLines)
	}
}

func TestRunGuardAbortsOnNonRetryableErrors(t *testing.T) {
	result, err := RunGuard(GuardOptions{
		Argv: []string{"claude", "-p", "x"},
		Runner: sgoFakeRunner([]AttemptResult{{
			ExitCode:           sgoIntPtr(1),
			SessionID:          "s",
			ResultIsError:      true,
			SawResult:          true,
			SyntheticErrorText: "Invalid API key provided",
		}}),
		MaxAttempts: sgoIntPtr(5),
		Sleep:       func(int) {},
	})
	if err != nil {
		t.Fatalf("RunGuard: %v", err)
	}
	if result.OK || result.Reason != ReasonNonRetryableError || result.Attempts != 1 {
		t.Fatalf("result: %+v", result)
	}
	if result.LastError != "Invalid API key provided" {
		t.Fatalf("lastError: %q", result.LastError)
	}
}

func TestRunGuardExhaustsAttempts(t *testing.T) {
	failing := AttemptResult{ExitCode: sgoIntPtr(1), SessionID: "s"}
	result, err := RunGuard(GuardOptions{
		Argv:        []string{"claude", "-p", "x"},
		Runner:      sgoFakeRunner([]AttemptResult{failing}),
		MaxAttempts: sgoIntPtr(3),
		Sleep:       func(int) {},
	})
	if err != nil {
		t.Fatalf("RunGuard: %v", err)
	}
	if result.OK || result.Attempts != 3 || result.Resumed != 2 || result.Reason != ReasonAttemptsExhausted {
		t.Fatalf("result: %+v", result)
	}
}

func TestRunGuardEnvMaxAttempts(t *testing.T) {
	t.Setenv("DEVAGENT_API_MAX_ATTEMPTS", "2")
	result, err := RunGuard(GuardOptions{
		Argv:   []string{"claude", "-p", "x"},
		Runner: sgoFakeRunner([]AttemptResult{{ExitCode: sgoIntPtr(1)}}),
		Sleep:  func(int) {},
	})
	if err != nil {
		t.Fatalf("RunGuard: %v", err)
	}
	if result.OK || result.Attempts != 2 || result.Resumed != 1 || result.Reason != ReasonAttemptsExhausted {
		t.Fatalf("result: %+v", result)
	}
}

func TestRunGuardRequiresRunner(t *testing.T) {
	_, err := RunGuard(GuardOptions{Argv: []string{"claude"}})
	if err == nil || err.Error() != "runGuard requires a runner (see spawnClaude)" {
		t.Fatalf("err: %v", err)
	}
}

func TestInspectTranscript(t *testing.T) {
	t.Run("flags a session whose last assistant turn is a synthetic API error", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "s1.jsonl")
		content := strings.Join([]string{
			`{"type":"user","sessionId":"s1","message":{"role":"user","content":"go"}}`,
			`{"type":"assistant","sessionId":"s1","timestamp":"2026-08-23T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"working..."}]}}`,
			`{"type":"assistant","sessionId":"s1","timestamp":"2026-08-23T10:00:05Z","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Connection lost mid-response."}]}}`,
		}, "\n")
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		status, err := InspectTranscript(file)
		if err != nil {
			t.Fatal(err)
		}
		if !status.Interrupted || status.SessionID != "s1" {
			t.Fatalf("status: %+v", status)
		}
		if !strings.Contains(status.LastErrorText, "Connection lost") {
			t.Fatalf("lastErrorText: %q", status.LastErrorText)
		}
		if status.LastTimestamp != "2026-08-23T10:00:05Z" {
			t.Fatalf("lastTimestamp: %q", status.LastTimestamp)
		}
	})

	t.Run("clears interruption once a real assistant message follows the error", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "s2.jsonl")
		content := strings.Join([]string{
			`{"type":"assistant","sessionId":"s2","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":"API Error"}}`,
			`{"type":"assistant","sessionId":"s2","message":{"role":"assistant","content":"recovered"}}`,
		}, "\n")
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		status, err := InspectTranscript(file)
		if err != nil {
			t.Fatal(err)
		}
		if status.Interrupted || status.LastErrorText != "" {
			t.Fatalf("status: %+v", status)
		}
	})

	t.Run("reports healthy sessions as not interrupted and skips malformed lines", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "s3.jsonl")
		content := "not json\n" + `{"type":"user","sessionId":"s3"}`
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		status, err := InspectTranscript(file)
		if err != nil {
			t.Fatal(err)
		}
		if status.Interrupted || status.SessionID != "s3" {
			t.Fatalf("status: %+v", status)
		}
	})
}

func TestLatestTranscript(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "a.jsonl")
	newer := filepath.Join(dir, "b.jsonl")
	note := filepath.Join(dir, "notes.txt")
	for _, f := range []string{old, newer, note} {
		if err := os.WriteFile(f, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if got := LatestTranscript(dir); got != newer {
		t.Fatalf("got %q, want %q", got, newer)
	}
	if got := LatestTranscript(filepath.Join(dir, "missing")); got != "" {
		t.Fatalf("missing dir: got %q", got)
	}
}

func TestProjectSlug(t *testing.T) {
	if got := ProjectSlug("/Users/linh.doan/work/repo"); got != "-Users-linh-doan-work-repo" {
		t.Fatalf("got %q", got)
	}
}

func TestSpawnClaudeStreamJSON(t *testing.T) {
	stub := sgoWriteScript(t, "stub.sh", `printf '%s\n' \
  '{"type":"system","subtype":"init","session_id":"stub-1"}' \
  '{"type":"system","subtype":"api_retry","attempt":1,"max_retries":2,"retry_delay_ms":10,"error_status":null,"error":"unknown"}' \
  '{"type":"assistant","isApiErrorMessage":true,"message":{"model":"<synthetic>","content":[{"type":"text","text":"API Error: Connection refused"}]}}'
exit 1
`)
	var lines sgoLineCollector
	outcome, err := SpawnClaude([]string{stub}, &lines, SpawnOpts{NoProgressTimeoutMs: 0})
	if err != nil {
		t.Fatalf("SpawnClaude: %v", err)
	}
	if outcome.SessionID != "stub-1" {
		t.Fatalf("sessionId: %q", outcome.SessionID)
	}
	if !strings.Contains(outcome.SyntheticErrorText, "Connection refused") {
		t.Fatalf("syntheticErrorText: %q", outcome.SyntheticErrorText)
	}
	if outcome.ExitCode == nil || *outcome.ExitCode != 1 {
		t.Fatalf("exitCode: %v", outcome.ExitCode)
	}
	if lines.count() != 3 {
		t.Fatalf("lines: %d", lines.count())
	}
}

func TestSpawnClaudeWatchdogKillsSilentChild(t *testing.T) {
	var lines sgoLineCollector
	outcome, err := SpawnClaude([]string{"/bin/sh", "-c", "sleep 60"}, &lines, SpawnOpts{NoProgressTimeoutMs: 500})
	if err != nil {
		t.Fatalf("SpawnClaude: %v", err)
	}
	if !outcome.TimedOut {
		t.Fatalf("expected watchdog timeout: %+v", outcome)
	}
	if outcome.ExitCode != nil && *outcome.ExitCode == 0 {
		t.Fatalf("expected non-zero/nil exit code: %+v", outcome.ExitCode)
	}
}
