// Go port of src/workers/omp.ts (FR-GO-05).
//
// Adapter over the `omp` headless CLI. Mirrors the structure of the
// claude-code and opencode adapters so the executor can treat it as just
// another WorkerAdapter. omp-specific behavior:
//   - Uses `--mode json` (not `--output-format json`).
//   - Resume via `-c` (continue) + RESUME_PROMPT (no session-id flag).
//   - No `--api-key` CLI flag — env is the supported credential channel.
//   - stdout is NDJSON event stream; the parser walks lines (see
//     interpretOmp).

package workers

import (
	"strings"
)

// ompResumePrompt is the resume prompt (TS RESUME_PROMPT).
const ompResumePrompt = "Continue"

// DEFAULT_NO_PROGRESS_TIMEOUT_MS: if set on opts, override the per-attempt
// no-progress watchdog for omp. omp is observed to hang silently on slow
// providers (2026-08-30 lesson: `omp -p` on omniroute/bai/glm-5.3-flash
// produced no stdout for 240s). The 10-minute default mirrors the
// resilience default elsewhere in devagent so retries fire instead of
// letting the wall clock be the only safety net.
//
// Q30: declared as the adapter's capability so the shared spawn-path
// resolver owns the precedence instead of a per-adapter copy.
const ompDefaultNoProgressTimeoutMs = 10 * 60 * 1000

// OmpArgsOptions mirrors TS OmpArgsOptions.
type OmpArgsOptions struct {
	// Resume builds a resume argv (uses -c + RESUME_PROMPT, no -p).
	Resume bool
}

// BuildOmpArgs builds the exact argv passed to `omp` for a given spawn.
// Pure function — exercised at the test seam without spawning the CLI.
//
//	omp -p <prompt> --mode json [--model <m>] [--thinking <v>]
//	omp --mode json -c <resumePrompt>   (resume)
func BuildOmpArgs(opts WorkerSpawnOptions, o OmpArgsOptions) []string {
	rawThinking := strings.TrimSpace(opts.Variant)
	// omp requires provider-qualified model ids (`provider/model`, fuzzy
	// matched). Driver tier aliases like "coding" (devagent.json model) are
	// claude-code proxy selectors, not omp ids: `--model coding` exits 1 in
	// ~12s with no output (2026-08-31 live loop 58 attempts 1-3). Drop any
	// value without a `/` so omp falls back to its configured
	// modelRoles.default (~/.omp/agent/config.yml), which is the intended
	// worker model anyway. Pass provider/model values through untouched.
	rawModel := strings.TrimSpace(opts.Model)
	var ompModel string
	if rawModel != "" && strings.Contains(rawModel, "/") {
		ompModel = rawModel
	}
	// --no-prewalk: the interactive config's prewalk (second planning turn)
	// loops forever on some models (2026-08-30 A/B: glm-5.3-flash prewalk
	// turn streamed 986+ thinking events and never terminated; with
	// --no-prewalk the same prompt completed in 12 events / 20s). Headless
	// runs must not inherit prewalk.enabled from ~/.omp/agent/config.yml.
	// --no-lsp --no-extensions: LSP/MCP discovery in devagent worktrees
	// stalls omp startup for 60-487s (observed: "Still starting after 487s
	// — phase: discoverAndLoadMCPTools"); with them stripped, identical runs
	// complete in ~17-21s. Coding workers exercise tools via the provider,
	// not LSP.
	base := []string{"--mode", "json", "--no-prewalk", "--no-lsp", "--no-extensions"}
	if o.Resume {
		args := append([]string{}, base...)
		if ompModel != "" {
			args = append(args, "--model", ompModel)
		}
		if rawThinking != "" {
			args = append(args, "--thinking", rawThinking)
		}
		return append(args, "-c", ompResumePrompt)
	}
	args := []string{"-p", opts.Prompt}
	args = append(args, base...)
	if ompModel != "" {
		args = append(args, "--model", ompModel)
	}
	if rawThinking != "" {
		args = append(args, "--thinking", rawThinking)
	}
	return args
}

// OmpOutcome mirrors TS OmpOutcome.
type OmpOutcome struct {
	IsError    bool
	SessionId  string
	ErrorText  string
	ResultText string
	Parsed     map[string]any
	TimedOut   bool
}

// InterpretOmpForTest is the test seam re-export of the parser.
func InterpretOmpForTest(run SpawnCliResult) OmpOutcome {
	return interpretOmp(run)
}

// interpretOmp parses omp's stdout into the worker outcome shape. omp
// --mode json emits an NDJSON event stream (one JSON object per line) per
// the live smoke captured 2026-08-30 in
// test/workers/__fixtures__/omp-smoke-2026-08-30.jsonl (175KB, 1591 lines).
// The terminal turn carries the assistant text.
//
// For backward compat with hand-rolled object/array envelopes, a single
// JSON document is also accepted when NDJSON parsing finds no session/turn
// events.
func interpretOmp(run SpawnCliResult) OmpOutcome {
	var parsed map[string]any
	sessionId := ""
	var terminalMessage map[string]any
	streamError := ""

	// Try single-JSON first (legacy callers). If it parses as object/array,
	// skip the NDJSON walk.
	if s := strings.TrimSpace(run.Stdout); s != "" {
		if candidate, ok := parseJSONAny(s); ok {
			switch v := candidate.(type) {
			case []any:
				for i := len(v) - 1; i >= 0; i-- {
					if e, ok := v[i].(map[string]any); ok && e["type"] == "result" {
						parsed = e
						break
					}
				}
			case map[string]any:
				// NDJSON guard: real omp's first line {"type":"session",...}
				// parses as a single object but is just a stream header. Only
				// treat the single-JSON fast path as valid when the object
				// has result metadata; otherwise fall through to the NDJSON
				// walk.
				_, hasResult := v["result"]
				_, hasIsError := v["is_error"]
				_, hasSessionId := v["session_id"]
				if hasResult || hasIsError || v["type"] == "result" || hasSessionId {
					parsed = v
				}
			}
		}
	}

	rawResult := ""
	haveRawResult := false
	if parsed != nil {
		if raw, ok := parsed["result"].(string); ok {
			rawResult = raw
			haveRawResult = true
		}
		if sid, ok := parsed["session_id"].(string); ok {
			sessionId = sid
		}
	} else {
		// NDJSON walk: each non-empty line is one event. We keep the FIRST
		// turn_end/message_end (the user-answer turn), not the last — real
		// omp emits a prewalk turn after the answer when --prewalk is
		// configured, and we want the original assistant text, not the
		// prewalk thought.
		for _, line := range strings.Split(run.Stdout, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			v, ok := parseJSONAny(trimmed)
			if !ok {
				continue
			}
			event, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if event["type"] == "session" {
				if id, ok := event["id"].(string); ok {
					sessionId = id
				}
			}
			if event["type"] == "turn_end" || event["type"] == "message_end" {
				msg, ok := event["message"].(map[string]any)
				if !ok {
					continue
				}
				// Both user and assistant turns emit message_end; only the
				// assistant's text is the worker result.
				if msg["role"] == "assistant" && terminalMessage == nil {
					terminalMessage = msg
				}
				// omp exits 0 even when the model call fails (observed
				// 2026-08-30: GitLab Duo 401 surfaces as assistant
				// message_end with errorMessage and NO result event).
				// Capture the first errorMessage so the failure is not
				// misread as an empty-but-successful run.
				if streamError == "" {
					if em, ok := msg["errorMessage"].(string); ok {
						streamError = em
					}
				}
			}
		}
		if terminalMessage != nil {
			if content, ok := terminalMessage["content"].([]any); ok {
				for _, part := range content {
					p, ok := part.(map[string]any)
					if !ok {
						continue
					}
					if p["type"] == "text" {
						if text, ok := p["text"].(string); ok {
							rawResult = text
							haveRawResult = true
							break
						}
					}
				}
			}
		}
	}

	isError := asBool(parsed["is_error"]) || streamError != ""
	resultText := ""
	if run.ExitCode == 0 && !isError && haveRawResult {
		resultText = rawResult
	}
	errorText := streamError
	if errorText == "" {
		// TS precedence: streamError ?? (rawResult && isError ? rawResult :
		// (parsed === null && !rawResult && stderr.trim() ? stderr.trim() :
		// undefined))
		if rawResult != "" && isError {
			errorText = rawResult
		} else if parsed == nil && rawResult == "" && strings.TrimSpace(run.Stderr) != "" {
			errorText = strings.TrimSpace(run.Stderr)
		}
	}
	return OmpOutcome{
		IsError:    isError,
		SessionId:  sessionId,
		ErrorText:  errorText,
		ResultText: resultText,
		Parsed:     parsed,
		TimedOut:   run.TimedOut,
	}
}

// ompFallbackEmpty mirrors TS fallbackEmpty().
func ompFallbackEmpty() SpawnCliResult {
	return SpawnCliResult{
		ExitCode: -1,
		Stdout:   "",
		Stderr:   "omp adapter produced no spawn result",
		TimedOut: false,
	}
}

// ompFinalize mirrors TS finalize.
func ompFinalize(run SpawnCliResult, sessionId string, durationMs int64) WorkerResult {
	outcome := interpretOmp(run)
	if run.TimedOut {
		result := WorkerResult{
			ExitCode:   run.ExitCode,
			Events:     []WorkerEvent{},
			ResultText: "",
			SessionId:  sessionId,
			DurationMs: durationMs,
			TimedOut:   true,
			ErrorText:  strings.TrimSpace(run.Stderr),
		}
		if run.ColdStart {
			result.ColdStart = true
		}
		return result
	}
	var events []WorkerEvent
	if outcome.Parsed != nil {
		events = []WorkerEvent{eventFromResult(outcome.Parsed)}
	} else {
		events = []WorkerEvent{}
	}
	return WorkerResult{
		ExitCode:   run.ExitCode,
		Events:     events,
		ResultText: outcome.ResultText,
		SessionId:  sessionId,
		DurationMs: durationMs,
		TimedOut:   false,
		ErrorText:  outcome.ErrorText,
	}
}

// OmpAdapter is the adapter over the `omp` headless CLI.
type OmpAdapter struct {
	adapterBase
}

// Name mirrors the TS readonly name.
func (a *OmpAdapter) Name() string { return "omp" }

// Capabilities: Q30 — arms a 10-minute silence clock by default; the
// declaration is a floor, so a caller-passed 0 falls back to it rather than
// disarming the watchdog this adapter's retry loop depends on.
func (a *OmpAdapter) Capabilities() WorkerCapabilities {
	return WorkerCapabilities{DefaultNoProgressTimeoutMs: ompDefaultNoProgressTimeoutMs}
}

// IsProgress: omp streams the shared NDJSON event shapes (Q33).
func (a *OmpAdapter) IsProgress(line string) bool { return IsNdjsonProgressLine(line) }

// Spawn runs the omp retry loop.
func (a *OmpAdapter) Spawn(opts WorkerSpawnOptions) WorkerResult {
	start := a.nowMs()
	caps := a.Capabilities()
	noProgressTimeoutMs := ResolveNoProgressTimeoutMs(opts.NoProgressTimeoutMs, &caps)
	wallDeadline := int64(1 << 62)
	if opts.TimeoutMs > 0 {
		wallDeadline = start + int64(opts.TimeoutMs)
	}

	args := BuildOmpArgs(opts, OmpArgsOptions{})
	sessionId := ""
	var last *SpawnCliResult
	// omp exposes -r/--resume <id>, but using it requires carrying the
	// session_id between attempts. The adapter parses sessionId from the
	// prior attempt's output but does not feed it to -r because that would
	// cross-talk between concurrent devagent runs sharing the same cwd.
	// Instead, retries use -c (continue most-recent session in cwd) and we
	// cap at maxAttempts so a hang cannot loop forever.
	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if a.nowMs() >= wallDeadline {
			if last != nil {
				last.TimedOut = true
			}
			break
		}
		prepared, err := a.prepare("omp", args, SpawnCliOptions{
			Dir:                 opts.Cwd,
			TimeoutMs:           opts.TimeoutMs,
			Env:                 opts.Env,
			NoProgressTimeoutMs: &noProgressTimeoutMs,
			ColdStartTimeoutMs:  opts.ColdStartTimeoutMs,
			WatchdogLedger:      opts.WatchdogLedger,
		})
		if err != nil {
			res := SpawnCliResult{ExitCode: -1, Stderr: err.Error()}
			last = &res
			break
		}
		raw := a.run(prepared.Cmd, prepared.Args, RunWorkerCliOptions{
			SpawnCliOptions: prepared.Opts,
			Herdr:           opts.Herdr,
		})
		last = &raw

		outcome := interpretOmp(raw)
		if outcome.SessionId != "" {
			sessionId = outcome.SessionId
		}
		ok := !raw.TimedOut && raw.ExitCode == 0 && !outcome.IsError
		if ok {
			break
		}
		if raw.ExitCode == -1 && !raw.TimedOut {
			break // ENOENT
		}
		if attempt == maxAttempts {
			break
		}
		if a.nowMs() >= wallDeadline {
			break
		}
		a.sleepMs(2000 * attempt)
		args = BuildOmpArgs(opts, OmpArgsOptions{Resume: true})
	}

	if last == nil {
		last = &SpawnCliResult{}
		*last = ompFallbackEmpty()
	}
	return ompFinalize(*last, sessionId, a.nowMs()-start)
}
