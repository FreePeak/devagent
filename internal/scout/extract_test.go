package scout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runExtract drives RunExtractText the way the selfbuild driver invokes the
// Node helper: `selfbuild-extract-text.mjs <raw> <out> [aborted]`.
func runExtract(t *testing.T, rawContent string, abortedArg string) (out string, code int) {
	t.Helper()
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.ndjson")
	outPath := filepath.Join(dir, "out.txt")
	if err := os.WriteFile(rawPath, []byte(rawContent), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{rawPath, outPath}
	if abortedArg != "" {
		args = append(args, abortedArg)
	}
	stderr := &strings.Builder{}
	code = RunExtractText(args, stderr)
	if code != 0 {
		return "", code
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("out file not written: %v", err)
	}
	return string(b), 0
}

const ompAssistantText = "Goal: Ship the flux capacitor\n\n## Acceptance criteria\n- fluxes\n"

// ompStream: a real-shaped omp/pi --mode json event stream (message_end
// carries the assistant text; other events must not).
func ompStream(text string) string {
	return `{"type":"session_start","session_id":"s1"}
{"type":"thinking_delta","delta":"hmm"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":` + jsonString(text) + `}]}}
{"type":"step_finish"}`
}

func TestExtractOmpMessageEnd(t *testing.T) {
	out, code := runExtract(t, ompStream(ompAssistantText), "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out != ompAssistantText {
		t.Fatalf("out = %q, want assistant text %q", out, ompAssistantText)
	}
}

// The claude -p --output-format json shape is a live possibility
// (SELFBUILD_*_BIN is overridable): its `result` line must win.
func TestExtractClaudeResultLine(t *testing.T) {
	stream := "{\"type\":\"system\",\"subtype\":\"init\"}\n{\"type\":\"result\",\"result\":" + jsonString("plain answer text") + "}\n"
	out, code := runExtract(t, stream, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out != "plain answer text" {
		t.Fatalf("out = %q", out)
	}
}

// Non-array message content must not crash the line parse (the JS try/catch
// skips the line); a later good line still extracts.
func TestExtractSkipsMalformedContent(t *testing.T) {
	stream := `{"type":"message_end","message":{"role":"assistant","content":"scalar-not-array"}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"good"}]}}
`
	out, code := runExtract(t, stream, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out != "good" {
		t.Fatalf("out = %q", out)
	}
}

// Empty input: both dispatch paths died before writing. Diagnostic, never a
// blank file.
func TestExtractEmptyInput(t *testing.T) {
	out, code := runExtract(t, "", "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out != "[extract-aborted] empty worker output" {
		t.Fatalf("out = %q", out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("blank output is a zero-signal invalid ledger row")
	}
}

// Aborted NDJSON stream (timeout/died mid-turn): diagnostic WITHOUT the
// "Goal:" prefix (the driver's ^Goal: gate must reject it), event count
// named, raw preserved only at an explicit non-dash path.
func TestExtractAbortedNdjsonPreservesRawAtExplicitPath(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.ndjson")
	outPath := filepath.Join(dir, "out.txt")
	abPath := filepath.Join(dir, "last.aborted.ndjson")
	raw := `{"type":"session_start"}
{"type":"thinking_delta","delta":"thinking forever"}
`
	if err := os.WriteFile(rawPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	code := RunExtractText([]string{rawPath, outPath, abPath}, &strings.Builder{})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	want := "[extract-aborted] no assistant text in 2 NDJSON events (raw kept at " + abPath + ")"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
	if strings.HasPrefix(out, "Goal:") {
		t.Fatal("abort diagnostic must not pass the driver's ^Goal: gate")
	}
	ab, err := os.ReadFile(abPath)
	if err != nil {
		t.Fatalf("raw not preserved for triage: %v", err)
	}
	if string(ab) != raw {
		t.Fatal("preserved raw differs from the stream")
	}
}

// No aborted path given: diagnostic names no location, nothing strays.
func TestExtractAbortedWithoutPath(t *testing.T) {
	out, code := runExtract(t, "{\"type\":\"session_start\"}\n", "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	want := "[extract-aborted] no assistant text in 1 NDJSON events"
	if out != want {
		t.Fatalf("out = %q, want %q", out, want)
	}
}

// Dash-guard: the hot-load sentinel (and any -prefixed arg) is NOT a path —
// no stray file, no "(raw kept at ...)" suffix.
func TestExtractDashGuardIgnoresSentinel(t *testing.T) {
	dir := t.TempDir()
	rawPath := filepath.Join(dir, "raw.ndjson")
	outPath := filepath.Join(dir, "out.txt")
	sentinel := "--sentinel"
	if err := os.WriteFile(rawPath, []byte("{\"type\":\"session_start\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := RunExtractText([]string{rawPath, outPath, sentinel}, &strings.Builder{})
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	b, _ := os.ReadFile(outPath)
	if string(b) != "[extract-aborted] no assistant text in 1 NDJSON events" {
		t.Fatalf("out = %q", string(b))
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "sentinel" || strings.Contains(e.Name(), "sentinel") {
			t.Fatalf("dash-guard leaked a stray file: %s", e.Name())
		}
	}
}

// Plain-text worker (claude -p without --output-format json): the raw
// output IS the result, passed through unchanged.
func TestExtractPlainTextPassthrough(t *testing.T) {
	raw := "Build the thing.\nStep two.\n"
	out, code := runExtract(t, raw, "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out != raw {
		t.Fatalf("out = %q, want %q", out, raw)
	}
}

// JSON whose first line is an array/object without .type is plain text for
// the discriminating branch only if the first non-blank line lacks a string
// .type — mirror the TS decision precisely.
func TestExtractFirstLineDecidesShape(t *testing.T) {
	// First line is a JSON string: parses, has no .type -> plain text.
	out, code := runExtract(t, "\"just a string\"\n", "")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if out != "\"just a string\"\n" {
		t.Fatalf("out = %q, want raw passthrough", out)
	}
}

// Usage error exits 2 with the exact usage line.
func TestExtractUsageError(t *testing.T) {
	stderr := &strings.Builder{}
	code := RunExtractText([]string{"only-one-arg"}, stderr)
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if got := stderr.String(); got != "usage: selfbuild-extract-text.mjs <raw-file> <out-file> [aborted-file]\n" {
		t.Fatalf("stderr = %q", got)
	}
}

// Missing raw file: the Node script throws (non-zero exit); the Go port
// returns 1 with the error on stderr.
func TestExtractMissingRawFile(t *testing.T) {
	dir := t.TempDir()
	stderr := &strings.Builder{}
	code := RunExtractText([]string{filepath.Join(dir, "nope.ndjson"), filepath.Join(dir, "out.txt")}, stderr)
	if code == 0 {
		t.Fatal("missing raw must not exit 0")
	}
	if stderr.String() == "" {
		t.Fatal("error must land on stderr")
	}
}
