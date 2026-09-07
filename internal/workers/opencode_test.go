// Go port of the opencode adapter tests (test/workers.test.ts
// "opencode adapter" + "opencode zero-event no-progress detection",
// parser/argv seams).
package workers

import "testing"

func ocBaseOpts() WorkerSpawnOptions {
	return WorkerSpawnOptions{Prompt: "do the thing", Cwd: "/tmp/work", TimeoutMs: 60_000}
}

func TestOpencodeModelArgs(t *testing.T) {
	cases := []struct {
		name   string
		opts   WorkerSpawnOptions
		binary string
		want   []string
	}{
		{"no model", ocBaseOpts(), "opencode", nil},
		{
			"plain model",
			func() WorkerSpawnOptions { o := ocBaseOpts(); o.Model = "openai/gpt-5"; return o }(),
			"opencode",
			[]string{"--model", "openai/gpt-5"},
		},
		{
			"opencode variant flag",
			func() WorkerSpawnOptions { o := ocBaseOpts(); o.Model = "openai/gpt-5"; o.Variant = "max"; return o }(),
			"opencode",
			[]string{"--model", "openai/gpt-5", "--variant", "max"},
		},
		{
			"opencode2 hash-variant encoding",
			func() WorkerSpawnOptions { o := ocBaseOpts(); o.Model = "openai/gpt-5"; o.Variant = "max"; return o }(),
			"opencode2",
			[]string{"--model", "openai/gpt-5#max"},
		},
		{
			"model already carries #variant — not duplicated",
			func() WorkerSpawnOptions {
				o := ocBaseOpts()
				o.Model = "openai/gpt-5#max"
				o.Variant = "max"
				return o
			}(),
			"opencode2",
			[]string{"--model", "openai/gpt-5#max"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := opencodeModelArgs(tc.opts, tc.binary)
			if len(got) != len(tc.want) {
				t.Fatalf("modelArgs = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("modelArgs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestOpencodeBaseAndResumeArgs(t *testing.T) {
	assertArgv(t, opencodeBaseArgs(ocBaseOpts(), "opencode"), []string{"run", "--format", "json", "do the thing"})
	assertArgv(t, opencodeResumeArgs("sess-1", ocBaseOpts(), "opencode"),
		[]string{"run", "--format", "json", "--session", "sess-1", "Continue"})
}

func TestInterpretOpencode(t *testing.T) {
	t.Run("parses NDJSON events with garbage interleaved", func(t *testing.T) {
		stdout := "garbage line\n" +
			`{"type":"message.part","sessionID":"oc-1","part":{"type":"text","text":"partial"}}` + "\n" +
			`{"type":"text","sessionID":"oc-1","text":"final"}` + "\n"
		o := InterpretOpencodeForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
		if len(o.Events) != 2 {
			t.Fatalf("events = %+v", o.Events)
		}
		if o.SessionId != "oc-1" {
			t.Fatalf("sessionId = %q", o.SessionId)
		}
		// resultText: last text found
		if o.ResultText != "final" {
			t.Fatalf("resultText = %q", o.ResultText)
		}
		if o.IsError {
			t.Fatalf("isError = %v", o.IsError)
		}
	})

	t.Run("string error event flags isError and surfaces message", func(t *testing.T) {
		stdout := `{"type":"error","error":"Error from provider (Console Go): Upstream request failed"}` + "\n"
		o := InterpretOpencodeForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
		if !o.IsError {
			t.Fatalf("isError = %v", o.IsError)
		}
		if !contains(o.ErrorText, "Upstream request failed") {
			t.Fatalf("errorText = %q", o.ErrorText)
		}
	})

	t.Run("nested error object message surfaces", func(t *testing.T) {
		stdout := `{"type":"error","error":{"message":"nested boom"}}` + "\n"
		o := InterpretOpencodeForTest(SpawnCliResult{ExitCode: 0, Stdout: stdout})
		if !o.IsError || o.ErrorText != "nested boom" {
			t.Fatalf("outcome = %+v", o)
		}
	})

	t.Run("non-zero exit with no events prefers stderr tail", func(t *testing.T) {
		o := InterpretOpencodeForTest(SpawnCliResult{ExitCode: 1, Stderr: "l1\nl2\nl3\nl4"})
		if !o.IsError {
			t.Fatal("expected isError")
		}
		// TS: stderr.trim().split('\n').slice(-3).join('\n')
		if o.ErrorText != "l2\nl3\nl4" {
			t.Fatalf("errorText = %q, want last 3 lines", o.ErrorText)
		}
	})

	t.Run("session_id snake_case and nested part.sessionID both bind", func(t *testing.T) {
		o := InterpretOpencodeForTest(SpawnCliResult{Stdout: `{"type":"x","session_id":"sid-1"}` + "\n"})
		if o.SessionId != "sid-1" {
			t.Fatalf("sessionId = %q", o.SessionId)
		}
		o = InterpretOpencodeForTest(SpawnCliResult{Stdout: `{"type":"x","part":{"sessionID":"sid-2"}}` + "\n"})
		if o.SessionId != "sid-2" {
			t.Fatalf("sessionId = %q", o.SessionId)
		}
	})
}

// Zero-event/empty-output signature: exit 0, no events, no text.
func TestOpencodeZeroEventNoProgress(t *testing.T) {
	t.Run("empty stdout at exit 0 is zero-event", func(t *testing.T) {
		run := SpawnCliResult{ExitCode: 0}
		o := InterpretOpencodeForTest(run)
		if !isOpencodeZeroEventNoProgress(o, run) {
			t.Fatalf("expected zero-event signature: %+v", o)
		}
	})
	t.Run("pure garbage at exit 0 is zero-event", func(t *testing.T) {
		run := SpawnCliResult{ExitCode: 0, Stdout: "not json\nalso not json"}
		o := InterpretOpencodeForTest(run)
		if !isOpencodeZeroEventNoProgress(o, run) {
			t.Fatalf("expected zero-event signature: %+v", o)
		}
	})
	t.Run("an event or text defeats the signature", func(t *testing.T) {
		run := SpawnCliResult{ExitCode: 0, Stdout: `{"type":"text","text":"ok"}`}
		o := InterpretOpencodeForTest(run)
		if isOpencodeZeroEventNoProgress(o, run) {
			t.Fatal("text-bearing run is not zero-event")
		}
	})
	t.Run("non-zero exit or timeout is not the signature", func(t *testing.T) {
		run := SpawnCliResult{ExitCode: 0, TimedOut: true}
		o := InterpretOpencodeForTest(run)
		if isOpencodeZeroEventNoProgress(o, run) {
			t.Fatal("timeout is not zero-event")
		}
	})
}

func TestOpencodeSpawnFailureShape(t *testing.T) {
	// ENOENT-ish: exit -1, empty streams, no timeout.
	if !isOpencodeSpawnFailure(SpawnCliResult{ExitCode: -1}) {
		t.Fatal("expected spawn-failure shape")
	}
	if isOpencodeSpawnFailure(SpawnCliResult{ExitCode: -1, Stderr: "something"}) {
		t.Fatal("stderr present is not the spawn-failure shape")
	}
	if isOpencodeSpawnFailure(SpawnCliResult{ExitCode: -1, TimedOut: true}) {
		t.Fatal("timeout is not the spawn-failure shape")
	}
}
