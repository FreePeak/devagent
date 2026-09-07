// Go port of test/workers/omp.test.ts (Seams A + B): argument building and
// stdout envelope parsing, including the real NDJSON fixture (2026-08-30).
package workers

import (
	"os"
	"path/filepath"
	"testing"
)

func ompBaseOpts() WorkerSpawnOptions {
	return WorkerSpawnOptions{Prompt: "do the thing", Cwd: "/tmp/work", TimeoutMs: 60_000}
}

func TestBuildOmpArgs_MinimumArgv(t *testing.T) {
	got := BuildOmpArgs(ompBaseOpts(), OmpArgsOptions{})
	want := []string{"-p", "do the thing", "--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions"}
	assertArgv(t, got, want)
}

func TestBuildOmpArgs_ForwardsModel(t *testing.T) {
	opts := ompBaseOpts()
	opts.Model = "omniroute/bai/glm-5.3-flash"
	got := BuildOmpArgs(opts, OmpArgsOptions{})
	want := []string{"-p", "do the thing", "--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions", "--model", "omniroute/bai/glm-5.3-flash"}
	assertArgv(t, got, want)
}

func TestBuildOmpArgs_ForwardsThinking(t *testing.T) {
	opts := ompBaseOpts()
	opts.Variant = "high"
	got := BuildOmpArgs(opts, OmpArgsOptions{})
	want := []string{"-p", "do the thing", "--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions", "--thinking", "high"}
	assertArgv(t, got, want)
}

func TestBuildOmpArgs_ResumeArgv(t *testing.T) {
	got := BuildOmpArgs(ompBaseOpts(), OmpArgsOptions{Resume: true})
	want := []string{"--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions", "-c", "Continue"}
	assertArgv(t, got, want)
}

// Model-alias rejection case (issue #190 acceptance): driver tier aliases
// are dropped; provider/model passes through.
func TestBuildOmpArgs_ModelAliasRejection(t *testing.T) {
	opts := ompBaseOpts()
	opts.Model = "coding"
	got := BuildOmpArgs(opts, OmpArgsOptions{})
	want := []string{"-p", "do the thing", "--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions"}
	assertArgv(t, got, want)

	opts.Model = "omniroute/bai/glm-5.3-flash"
	got = BuildOmpArgs(opts, OmpArgsOptions{})
	found := false
	for _, a := range got {
		if a == "--model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider/model must be forwarded: %v", got)
	}
}

func TestBuildOmpArgs_NoApiKeyFlag(t *testing.T) {
	for _, a := range BuildOmpArgs(ompBaseOpts(), OmpArgsOptions{}) {
		if len(a) >= 9 && a[:9] == "--api-key" {
			t.Fatalf("env is the supported credential channel; argv leaked %q", a)
		}
	}
}

func TestInterpretOmp_ObjectEnvelope(t *testing.T) {
	stdout := `{"type":"result","is_error":false,"session_id":"omp-s-1","result":"hello world"}`
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
	if o.IsError || o.SessionId != "omp-s-1" || o.ResultText != "hello world" || o.ErrorText != "" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestInterpretOmp_ArrayEnvelope(t *testing.T) {
	stdout := `[{"type":"system","session_id":"omp-s-2"},{"type":"assistant","text":"thinking"},{"type":"result","is_error":false,"session_id":"omp-s-2","result":"final"}]`
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
	if o.IsError || o.SessionId != "omp-s-2" || o.ResultText != "final" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestInterpretOmp_IsErrorFlag(t *testing.T) {
	stdout := `{"type":"result","is_error":true,"session_id":"omp-s-3","result":"upstream rejected the prompt"}`
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
	if !o.IsError || o.ErrorText != "upstream rejected the prompt" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestInterpretOmp_GarbageStdout(t *testing.T) {
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 0, Stdout: "not json", Stderr: "parser boom"})
	if o.Parsed != nil || o.SessionId != "" || o.ErrorText != "parser boom" {
		t.Fatalf("outcome = %+v", o)
	}
}

func TestInterpretOmp_TimeoutShape(t *testing.T) {
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 124, Stderr: "killed by watchdog", TimedOut: true})
	if o.Parsed != nil || !o.TimedOut {
		t.Fatalf("outcome = %+v", o)
	}
}

// Stream-error case (issue #190 acceptance): omp exits 0 even when the
// model call fails — the failure surfaces as assistant message_end with
// errorMessage and NO result event; the parser must catch it.
func TestInterpretOmp_ErrorMessageCaptureAtExit0(t *testing.T) {
	stdout := "" +
		`{"type":"session","id":"omp-err-1"}` + "\n" +
		`{"type":"message_end","message":{"role":"assistant","content":[],"errorMessage":"401 Unauthorized from provider"}}` + "\n"
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
	if !o.IsError {
		t.Fatalf("exit-0 stream error must flag IsError: %+v", o)
	}
	if o.ErrorText != "401 Unauthorized from provider" {
		t.Fatalf("errorText = %q", o.ErrorText)
	}
	if o.ResultText != "" {
		t.Fatalf("resultText must stay empty on a failed run: %q", o.ResultText)
	}
	if o.SessionId != "omp-err-1" {
		t.Fatalf("sessionId = %q", o.SessionId)
	}
}

// Golden fixture: the real omp NDJSON event stream captured 2026-08-30.
func TestInterpretOmp_RealNdjsonFixture(t *testing.T) {
	stdout, err := os.ReadFile(filepath.Join("testdata", "omp-smoke-2026-08-30.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	o := InterpretOmpForTest(SpawnCliResult{ExitCode: 0, Stdout: string(stdout)})
	if o.IsError {
		t.Fatalf("fixture must parse clean: %+v", o)
	}
	if o.SessionId != "01a05127-5cc0-7680-9853-7dc3c80a1477" {
		t.Fatalf("sessionId = %q", o.SessionId)
	}
	if o.ResultText != "OK" {
		t.Fatalf("resultText = %q, want OK", o.ResultText)
	}
}

func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}
