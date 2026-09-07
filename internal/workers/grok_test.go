// Go port of test/workers/grok.test.ts: argument building, streaming-json
// NDJSON parsing over the captured 2026-09-06/07 fixtures, exact cost
// ticks (FR-GROK-03), meaningful-line filter (Q33), chain model override
// (FR-GROK-06), and the per-task prompt-cache key (FR-GROK-04).
package workers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func grokBaseOpts() WorkerSpawnOptions {
	return WorkerSpawnOptions{Prompt: "do the thing", Cwd: "/tmp/work", TimeoutMs: 60_000}
}

func grokFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBuildGrokArgs(t *testing.T) {
	cases := []struct {
		name string
		opts WorkerSpawnOptions
		o    GrokArgsOptions
		want []string
	}{
		{"minimum headless argv", grokBaseOpts(), GrokArgsOptions{}, []string{"-p", "do the thing", "--output-format", "streaming-json"}},
		{
			"exact xAI slug forwarded",
			func() WorkerSpawnOptions { o := grokBaseOpts(); o.Model = "grok-4.6"; return o }(),
			GrokArgsOptions{},
			[]string{"-p", "do the thing", "--output-format", "streaming-json", "--model", "grok-4.6"},
		},
		{
			"resume argv",
			grokBaseOpts(),
			GrokArgsOptions{Resume: true},
			[]string{"-c", "-p", "Continue", "--output-format", "streaming-json"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertArgv(t, BuildGrokArgs(tc.opts, tc.o), tc.want)
		})
	}

	// xai/-qualified ids pass through.
	got := BuildGrokArgs(WorkerSpawnOptions{Prompt: "do the thing", Model: "xai/grok-build-0.1"}, GrokArgsOptions{})
	if !contains(strings.Join(got, " "), "--model xai/grok-build-0.1") {
		t.Fatalf("xai/-qualified id dropped: %v", got)
	}
	// Tier aliases and bare config aliases are dropped (loop-58 precedent);
	// the CLI default applies.
	for _, alias := range []string{"coding", "free", "  "} {
		opts := grokBaseOpts()
		opts.Model = alias
		if strings.Join(BuildGrokArgs(opts, GrokArgsOptions{}), " ") != "-p do the thing --output-format streaming-json" {
			t.Fatalf("alias %q must be dropped: %v", alias, BuildGrokArgs(opts, GrokArgsOptions{}))
		}
	}
	// No --api-key flag ever (OAuth/XAI_API_KEY env are the channels).
	joined := strings.Join(BuildGrokArgs(grokBaseOpts(), GrokArgsOptions{}), " ")
	if strings.Contains(joined, "--api-key") {
		t.Fatalf("argv leaked credential channel: %q", joined)
	}
}

// Chain model override on argv (FR-GROK-06 Seam A).
func TestBuildGrokArgs_ChainOverride(t *testing.T) {
	// Forwards the chain override even when opts.model is unset.
	got := BuildGrokArgs(WorkerSpawnOptions{Prompt: "do the thing"}, GrokArgsOptions{Model: "grok-4.3"})
	if !contains(strings.Join(got, " "), "--model grok-4.3") {
		t.Fatalf("override dropped: %v", got)
	}
	// The override still passes the FR-GROK-02 predicate (aliases dropped).
	got = BuildGrokArgs(WorkerSpawnOptions{Prompt: "do the thing"}, GrokArgsOptions{Model: "coding"})
	if strings.Contains(gotJoin(got), "--model") {
		t.Fatalf("alias override leaked: %v", got)
	}
}

func gotJoin(args []string) string { return strings.Join(args, " ") }

// Golden fixtures (issue #190 acceptance: stream-error case + model-alias
// argv hardening pinned above).
func TestInterpretGrok_Fixtures(t *testing.T) {
	t.Run("clean answer run (grok-smoke-2026-09-06.jsonl)", func(t *testing.T) {
		o := InterpretGrokForTest(SpawnCliResult{ExitCode: 0, Stdout: grokFixture(t, "grok-smoke-2026-09-06.jsonl")})
		if o.IsError || o.ErrorText != "" {
			t.Fatalf("outcome = %+v", o)
		}
		if o.SessionId != "01a0779c-edce-7d70-91a5-214626e30396" {
			t.Fatalf("sessionId = %q", o.SessionId)
		}
		if o.ResultText != "OK" {
			t.Fatalf("resultText = %q, want OK", o.ResultText)
		}
		// The terminal `end` event carries usage/cost metadata (FR-GROK-03).
		if o.Parsed == nil || o.Parsed["type"] != "end" || o.Parsed["stopReason"] != "end_turn" {
			t.Fatalf("parsed = %+v", o.Parsed)
		}
	})

	t.Run("chunked text across tool calls (grok-toolrun-2026-09-06.jsonl)", func(t *testing.T) {
		o := InterpretGrokForTest(SpawnCliResult{ExitCode: 0, Stdout: grokFixture(t, "grok-toolrun-2026-09-06.jsonl")})
		if o.IsError {
			t.Fatalf("outcome = %+v", o)
		}
		if o.SessionId != "01a0779c-edce-7d70-91a5-214626e30396" {
			t.Fatalf("sessionId = %q", o.SessionId)
		}
		want := "I'll run `echo hi` and reply with only its output.hi"
		if o.ResultText != want {
			t.Fatalf("resultText = %q, want %q", o.ResultText, want)
		}
		numTurns, _ := o.Parsed["num_turns"].(float64)
		if numTurns != 2 {
			t.Fatalf("num_turns = %v, want 2", o.Parsed["num_turns"])
		}
	})

	t.Run("in-stream error at exit-0 shape (grok-error-2026-09-06.jsonl)", func(t *testing.T) {
		// Live capture: the provider 401 surfaced as {"type":"error"}; Grok
		// Build is documented to exit 0 on provider failures (same trap as
		// omp 2026-08-30), so the walk must not rely on the exit code alone.
		o := InterpretGrokForTest(SpawnCliResult{ExitCode: 0, Stdout: grokFixture(t, "grok-error-2026-09-06.jsonl")})
		if !o.IsError {
			t.Fatalf("expected isError, got %+v", o)
		}
		if o.ResultText != "" {
			t.Fatalf("resultText = %q, want empty", o.ResultText)
		}
		if !contains(o.ErrorText, "Unauthorized (401)") {
			t.Fatalf("errorText = %q", o.ErrorText)
		}
		if o.SessionId != "" {
			t.Fatalf("sessionId = %q, want empty", o.SessionId)
		}
		if o.Parsed != nil {
			t.Fatalf("parsed = %+v, want nil", o.Parsed)
		}
		// The captured 401 is non-retryable end-to-end (FR-GROK-06): the
		// message contains "temporarily unavailable"/"retry in a few
		// seconds" transient wording that must NOT flip an auth failure
		// into an infinite infra retry.
		if IsTransientProviderError(o.ErrorText) {
			t.Fatalf("401 must not classify transient: %q", o.ErrorText)
		}
		if TransientErrorClass(o.ErrorText) != "" {
			t.Fatalf("class = %q, want empty", TransientErrorClass(o.ErrorText))
		}
	})

	t.Run("stderr on garbage stdout with no end event", func(t *testing.T) {
		o := InterpretGrokForTest(SpawnCliResult{ExitCode: 1, Stdout: "not json at all\n{\"broken\n", Stderr: "grok boom"})
		if !contains(o.ErrorText, "grok boom") {
			t.Fatalf("errorText = %q", o.ErrorText)
		}
	})

	t.Run("stderr noise not promoted while tool activity present (FR-GROK-06)", func(t *testing.T) {
		o := InterpretGrokForTest(SpawnCliResult{
			ExitCode: 0,
			Stdout:   grokFixture(t, "grok-toolcall-whole-2026-09-07.jsonl"),
			Stderr:   "warning: telemetry flush failed",
		})
		if o.IsError || o.ErrorText != "" {
			t.Fatalf("outcome = %+v", o)
		}
	})
}

// Exact cost ticks (FR-GROK-03 Seam B).
func TestInterpretGrok_CostTicks(t *testing.T) {
	run := func(stdout string) GrokOutcome {
		return InterpretGrokForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
	}
	cases := []struct {
		name   string
		stdout string
		want   *float64
	}{
		{
			"total_cost_usd_ticks off the end event verbatim",
			`{"type":"end","sessionId":"s1","stopReason":"end_turn","usage":{"input_tokens":10},"total_cost_usd_ticks":527119000}` + "\n",
			f64(527119000),
		},
		{
			"cost_in_usd_ticks off a usage event when end omits it",
			`{"type":"usage","usage":{"input_tokens":5,"cost_in_usd_ticks":12345}}` + "\n" +
				`{"type":"end","sessionId":"s1","stopReason":"end_turn"}` + "\n",
			f64(12345),
		},
		{
			"terminal end total wins over an earlier usage row",
			`{"type":"usage","usage":{"cost_in_usd_ticks":111}}` + "\n" +
				`{"type":"end","total_cost_usd_ticks":999}` + "\n",
			f64(999),
		},
		{
			"no event carries it — never coerced to 0",
			`{"type":"end","sessionId":"s1","stopReason":"end_turn","usage":{"input_tokens":1}}` + "\n",
			nil,
		},
		{"genuine 0-tick run stays 0", `{"type":"end","total_cost_usd_ticks":0}` + "\n", f64(0)},
		{"non-numeric cost ignored", `{"type":"end","total_cost_usd_ticks":"527119000"}` + "\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := run(tc.stdout).CostUsdTicks
			if !sameF64(got, tc.want) {
				t.Fatalf("costUsdTicks = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("recorded smoke run cost verbatim", func(t *testing.T) {
		o := InterpretGrokForTest(SpawnCliResult{ExitCode: 0, Stdout: grokFixture(t, "grok-smoke-2026-09-06.jsonl")})
		if !sameF64(o.CostUsdTicks, f64(527119000)) {
			t.Fatalf("costUsdTicks = %v", o.CostUsdTicks)
		}
	})
}

// Meaningful-line filter (Q33 Seam C).
func TestIsGrokProgressLine(t *testing.T) {
	adapter := &GrokAdapter{}
	if IsGrokProgressLine(`{"type":"thought","data":"The"}`) {
		t.Fatal("thought chunks are never progress")
	}
	if adapter.IsProgress(`{"type":"thought","data":" user"}`) {
		t.Fatal("thought chunks are never progress (adapter seam)")
	}
	for _, line := range []string{
		`{"type":"tool_call","toolCallId":"c1","title":"run_terminal_command"}`,
		`{"type":"tool_call_update","toolCallId":"c1","status":"completed"}`,
		`{"type":"text","data":"OK"}`,
	} {
		if !IsGrokProgressLine(line) || !adapter.IsProgress(line) {
			t.Fatalf("expected progress: %s", line)
		}
	}
	for _, line := range []string{
		`{"type":"available_commands","tools":["read_file"]}`,
		`{"type":"usage","usage":{"input_tokens":1}}`,
		"",
		"   ",
		"not json",
	} {
		if IsGrokProgressLine(line) {
			t.Fatalf("expected non-progress: %q", line)
		}
	}
}

// Within-xAI fallback chain (FR-GROK-06 Seam D).
func TestGrokChainNext(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"grok-4.6", "grok-4.3"},
		{"grok-4.3", "grok-build-0.1"},
		{"grok-build-0.1", ""},
		{"grok-4.6-2026-08-14", "grok-4.3"},
		{"xai/grok-4.3", "grok-build-0.1"},
		{"", "grok-4.6"},
		{"grok-4.5", ""},
	}
	for _, tc := range cases {
		if got := GrokChainNext(tc.in); got != tc.want {
			t.Fatalf("GrokChainNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGrokRetryModel(t *testing.T) {
	tpm := "429 rate_limit_error: exceeded tokens per minute (TPM) limit"
	if got := GrokRetryModel("grok-4.6", tpm); got != "grok-4.3" {
		t.Fatalf("TPM class must advance the chain: %q", got)
	}
	// RPS is a cooldown problem, not a context problem: same model.
	if got := GrokRetryModel("grok-4.6", "429 requests per second (RPS) limit exceeded"); got != "grok-4.6" {
		t.Fatalf("RPS must keep the model: %q", got)
	}
	// 5xx, generic rate-limit, and non-transient text never step the chain.
	if got := GrokRetryModel("grok-4.6", "Server error (503) from https://api.x.ai/v1"); got != "grok-4.6" {
		t.Fatalf("5xx must keep the model: %q", got)
	}
	if got := GrokRetryModel("grok-4.6", "429 too many requests"); got != "grok-4.6" {
		t.Fatalf("generic rate-limit must keep the model: %q", got)
	}
	if got := GrokRetryModel("grok-4.6", "Unauthorized (401)"); got != "grok-4.6" {
		t.Fatalf("non-transient must keep the model: %q", got)
	}
}

// Per-task prompt-cache key (FR-GROK-04 Seam A).
func TestGrokPromptCacheKey(t *testing.T) {
	ledger := func(taskId string) *WatchdogLedgerContext {
		return &WatchdogLedgerContext{RepoPath: "/r", TaskId: taskId, Attempt: 1, Worker: "grok"}
	}
	if got := GrokPromptCacheKey(WorkerSpawnOptions{WatchdogLedger: ledger("TASK-abc")}); got != "devagent-TASK-abc" {
		t.Fatalf("key = %q", got)
	}
	env := GrokPromptCacheEnv(WorkerSpawnOptions{WatchdogLedger: ledger("TASK-abc")})
	if env[GrokPromptCacheKeyEnv] != "devagent-TASK-abc" || len(env) != 1 {
		t.Fatalf("env = %+v", env)
	}
	// No ledger context (probe/one-off spawns): no key.
	if got := GrokPromptCacheKey(WorkerSpawnOptions{}); got != "" {
		t.Fatalf("keyless spawn produced %q", got)
	}
	if env := GrokPromptCacheEnv(WorkerSpawnOptions{}); len(env) != 0 {
		t.Fatalf("keyless env = %+v", env)
	}
	// Blank taskId: no key.
	if got := GrokPromptCacheKey(WorkerSpawnOptions{WatchdogLedger: ledger("   ")}); got != "" {
		t.Fatalf("blank taskId produced %q", got)
	}
	// The key never rides argv (grok 1.0.13 has no cache-key flag).
	opts := WorkerSpawnOptions{Prompt: "x", WatchdogLedger: ledger("TASK-abc")}
	for _, argv := range [][]string{
		BuildGrokArgs(opts, GrokArgsOptions{}),
		BuildGrokArgs(opts, GrokArgsOptions{Resume: true}),
	} {
		if contains(strings.Join(argv, " "), "devagent-TASK-abc") {
			t.Fatalf("cache key leaked into argv: %v", argv)
		}
	}
}

func f64(n float64) *float64 { return &n }

func sameF64(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
