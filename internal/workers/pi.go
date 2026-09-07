// Go port of src/workers/pi.ts (FR-GO-05).
//
// Adapter over the pi headless CLI:
//
//	pi --mode json -p <prompt> [--model <m>] [--thinking <v>]
//
// stdout is NDJSON: one JSON object per line.
//
// pi never retries a response dropped mid-stream; when that happens the
// turn dies but the session transcript persists. This adapter resumes the
// same session (`--continue` / `-c`) with exponential backoff until the
// turn completes, attempts are exhausted, or the error is auth/billing.
//
// pi uses provider-qualified model ids (`provider/model`, fuzzy matched).
// Driver tier aliases like "coding" (devagent.json model) are NOT pi ids:
// pi will error or fall back to its configured default. Drop any value
// without a `/` so pi falls back to ~/.pi/agent/config.yml model.
package workers

import "strings"

const piResumePrompt = "Continue"

// piDefaultNoProgressTimeoutMs: if set on opts, override the per-attempt
// no-progress watchdog for pi. Defaulting a nonzero watchdog is also
// load-bearing for stdin semantics: spawnCli's execFile path leaves stdin
// an open pipe (a claude-code requirement), but pi waits for stdin EOF and
// hangs forever on it (2026-09-01 live smoke: direct CLI 10-24s; execFile
// 0 bytes until wall-clock kill). A nonzero value routes the launch through
// the streaming spawn, which ends stdin after spawn. 10 minutes mirrors the
// omp adapter's default so retries fire instead of the wall clock being the
// only safety net.
//
// Q30: declared as the adapter's capability so the shared spawn-path
// resolver owns the precedence instead of a per-adapter copy.
const piDefaultNoProgressTimeoutMs = 10 * 60 * 1000

// PiAdapter mirrors TS PiAdapter.
type PiAdapter struct {
	adapterBase
}

// Name mirrors the TS readonly name.
func (a *PiAdapter) Name() string { return "pi" }

// Capabilities: Q30 — arms a 10-minute silence clock by default — a
// nonzero declaration is a floor: a caller-passed 0 falls back to it, since
// pi hangs on an open stdin without an armed watchdog.
func (a *PiAdapter) Capabilities() WorkerCapabilities {
	return WorkerCapabilities{DefaultNoProgressTimeoutMs: piDefaultNoProgressTimeoutMs}
}

// IsProgress: PRD Q33 — pi-specific progress classification. pi streams
// message_update/thinking_delta lines during deliberation and
// tool_execution_* lines when working; only the latter (plus answer text)
// reset the no-progress watchdog.
func (a *PiAdapter) IsProgress(line string) bool { return IsNdjsonProgressLine(line) }

// BuildPiArgs builds the exact argv passed to `pi` for a given spawn. Pure
// function — exercised at the test seam without spawning the CLI.
//
//	pi --mode json -p <prompt> [--model <provider/model>] [--thinking <level>]
//	pi --mode json --continue <resumePrompt>  (resume)
func BuildPiArgs(opts WorkerSpawnOptions, resume bool) []string {
	rawThinking := strings.TrimSpace(opts.Variant)
	// pi requires provider-qualified model ids (`provider/model`, fuzzy
	// matched). Driver tier aliases like "coding" (devagent.json model) are
	// NOT pi ids: `--model coding` exits 1 in ~12s with no output. Drop any
	// value without a `/` so pi falls back to its configured default model
	// (~/.pi/agent/config.yml).
	rawModel := strings.TrimSpace(opts.Model)
	var piModel string
	if rawModel != "" && strings.Contains(rawModel, "/") {
		piModel = rawModel
	}
	base := []string{"--mode", "json"}
	if resume {
		args := append([]string{}, base...)
		args = append(args, "--continue", piResumePrompt)
		if piModel != "" {
			args = append(args, "--model", piModel)
		}
		if rawThinking != "" {
			args = append(args, "--thinking", rawThinking)
		}
		return args
	}
	args := append([]string{}, base...)
	args = append(args, "-p", opts.Prompt)
	if piModel != "" {
		args = append(args, "--model", piModel)
	}
	if rawThinking != "" {
		args = append(args, "--thinking", rawThinking)
	}
	return args
}

// PiOutcome mirrors TS PiOutcome.
type PiOutcome struct {
	IsError    bool
	SessionId  string
	ErrorText  string
	ResultText string
	Parsed     map[string]any
	TimedOut   bool
}

// InterpretPiForTest is the test seam re-export of the parser.
func InterpretPiForTest(run SpawnCliResult) PiOutcome {
	return interpretPi(run)
}

// extractTextParts: concatenated text parts of an assistant message; empty
// string when textless.
func extractTextParts(m map[string]any) string {
	content, ok := m["content"].([]any)
	if !ok {
		return ""
	}
	var out strings.Builder
	for _, part := range content {
		p, ok := part.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := p["text"].(string); ok && text != "" {
			out.WriteString(text)
		}
	}
	return out.String()
}

// interpretPi parses pi's stdout into the worker outcome shape. pi --mode
// json emits an NDJSON event stream (one JSON object per line). The
// terminal assistant message carries the assistant text.
//
// We keep the LAST assistant message that carries text content — pi's
// agentic turns emit one assistant message_end per turn (tool-call turns
// may have no text), and the final answer is the last textual turn
// (2026-09-01 live smoke: first assistant turn was a toolCall turn, answer
// text in the second). errorMessage capture stays first-wins.
func interpretPi(run SpawnCliResult) PiOutcome {
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
				// NDJSON guard: real pi's first line {"type":"session",...}
				// parses as a single object but is just a stream header.
				// Only treat the single-JSON fast path as valid when the
				// object has result metadata; otherwise fall through to the
				// NDJSON walk.
				_, hasResult := v["result"]
				_, hasIsError := v["is_error"]
				_, hasSessionId := v["session_id"]
				_, hasId := v["id"]
				if hasResult || hasIsError || v["type"] == "result" || hasSessionId || hasId {
					parsed = v
				}
			}
		}
	}

	rawResult := ""
	if parsed != nil {
		if raw, ok := parsed["result"].(string); ok {
			rawResult = raw
		}
		// pi uses "id" for session id in the session header event
		if sid, ok := parsed["id"].(string); ok {
			sessionId = sid
		}
		if sid, ok := parsed["session_id"].(string); ok {
			sessionId = sid
		}
	} else {
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
			// pi session header: {"type":"session","version":3,"id":"uuid",...}
			if event["type"] == "session" {
				if id, ok := event["id"].(string); ok {
					sessionId = id
				}
			}
			// pi emits message_end for both user and assistant turns; only
			// the assistant's text is the worker result.
			if event["type"] == "turn_end" || event["type"] == "message_end" {
				msg, ok := event["message"].(map[string]any)
				if !ok {
					continue
				}
				if msg["role"] == "assistant" {
					text := extractTextParts(msg)
					if text != "" {
						terminalMessage = msg
					}
					if streamError == "" {
						if em, ok := msg["errorMessage"].(string); ok {
							streamError = em
						}
					}
				}
			}
		}
	}

	if terminalMessage != nil {
		rawResult = extractTextParts(terminalMessage)
	}

	// Determine error status: explicit isError, or non-zero exit with no result
	isError := streamError != "" ||
		(run.ExitCode != 0 && rawResult == "" && parsed == nil)
	errorText := streamError
	if errorText == "" {
		if em, ok := parsed["errorMessage"].(string); ok {
			errorText = em
		}
	}
	if errorText == "" && strings.TrimSpace(run.Stderr) != "" {
		errorText = lastNLines(run.Stderr, 3)
	}

	return PiOutcome{
		IsError:    isError,
		SessionId:  sessionId,
		ErrorText:  errorText,
		ResultText: rawResult,
		Parsed:     parsed,
		TimedOut:   run.TimedOut,
	}
}

// piFinalize mirrors TS finalize.
func piFinalize(run SpawnCliResult, durationMs int64) WorkerResult {
	if run.TimedOut {
		result := WorkerResult{
			ExitCode:   run.ExitCode,
			Events:     []WorkerEvent{},
			ResultText: "",
			SessionId:  "",
			DurationMs: durationMs,
			TimedOut:   true,
			ErrorText:  strings.TrimSpace(run.Stderr),
		}
		if run.ColdStart {
			result.ColdStart = true
		}
		return result
	}

	outcome := interpretPi(run)
	if strings.TrimSpace(run.Stdout) == "" || outcome.ResultText == "" {
		return WorkerResult{
			ExitCode:   run.ExitCode,
			Events:     []WorkerEvent{},
			ResultText: "",
			SessionId:  outcome.SessionId,
			DurationMs: durationMs,
			TimedOut:   false,
			// Stderr often carries the real failure reason when the
			// upstream returned an empty stream. Surface it so the
			// executor can classify properly.
			ErrorText: outcome.ErrorText,
		}
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
		SessionId:  outcome.SessionId,
		DurationMs: durationMs,
		TimedOut:   false,
		ErrorText:  outcome.ErrorText,
	}
}

// Spawn runs the pi retry loop.
func (a *PiAdapter) Spawn(opts WorkerSpawnOptions) WorkerResult {
	start := a.nowMs()
	maxAttempts := piDefaultAPIMaxAttempts
	if opts.APIMaxAttempts != nil {
		maxAttempts = *opts.APIMaxAttempts
	}
	caps := a.Capabilities()
	noProgressTimeoutMs := ResolveNoProgressTimeoutMs(opts.NoProgressTimeoutMs, &caps)
	wallDeadline := int64(1 << 62)
	if opts.TimeoutMs > 0 {
		wallDeadline = start + int64(opts.TimeoutMs)
	}

	args := BuildPiArgs(opts, false)
	sessionId := ""
	var last *SpawnCliResult

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Wall-clock budget: stop retrying when the run's overall timeout
		// is spent.
		if a.nowMs() >= wallDeadline {
			if last != nil {
				last.TimedOut = true
			}
			break
		}

		prepared, err := a.prepare("pi", args, SpawnCliOptions{
			Dir:                 opts.Cwd,
			TimeoutMs:           opts.TimeoutMs,
			Env:                 opts.Env,
			NoProgressTimeoutMs: intPtrIf(noProgressTimeoutMs != 0, noProgressTimeoutMs),
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

		outcome := interpretPi(raw)
		if outcome.SessionId != "" {
			sessionId = outcome.SessionId
		}

		ok := !raw.TimedOut && raw.ExitCode == 0 && !outcome.IsError
		if ok {
			break
		}

		if attempt == maxAttempts {
			break
		}
		if raw.ExitCode == -1 && !raw.TimedOut {
			break // spawn failure (ENOENT) — never retry forever
		}
		if outcome.ErrorText != "" && IsNonRetryableApiError(outcome.ErrorText) {
			break
		}

		// Transient detection: watchdog timeouts and provider errors are
		// retried forever. Without a session we retry from scratch only for
		// provider/timeout signals.
		errorText := outcome.ErrorText
		if errorText == "" {
			errorText = raw.Stderr
		}
		if sessionId == "" {
			if !IsRetryableWithoutSession(raw.TimedOut, raw.ExitCode, errorText, raw.Stderr) {
				break
			}
		}

		// Wall-clock budget already checked above; also don't retry if
		// we'd exceed it after sleep.
		if a.nowMs() >= wallDeadline {
			break
		}

		a.sleepMs(backoffDelay(attempt))
		// pi supports --continue / -c to resume the most recent session in cwd
		args = BuildPiArgs(opts, true)
	}

	return piFinalize(derefSpawn(last), a.nowMs()-start)
}

// TS Infinity proxy.
const piDefaultAPIMaxAttempts = 1 << 30
