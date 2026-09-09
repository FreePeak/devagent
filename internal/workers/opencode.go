// Go port of src/workers/opencode.ts (FR-GO-05).
//
// Adapter over the OpenCode headless CLI:
//
//	opencode run --format json <prompt>
//
// stdout is NDJSON: one JSON object per line, possibly interleaved with
// garbage.
//
// On transient API failures (e.g. `Error from provider (Console Go):
// Upstream request failed: Endpoint is unavailable.`) the turn dies but
// the session persists. This adapter resumes the same session
// (`--session <id> Continue`) with exponential backoff forever (default
// Infinity) until it succeeds, matching the claude-code adapter's
// resume-retry semantics. The loop only stops on success, wall-clock
// timeout, spawn failure, or non-retryable auth/billing errors; any
// provider error keeps looping with backoff. Binary fallback: tries
// `opencode` then `opencode2` if the first binary is missing (ENOENT ->
// exitCode -1).

package workers

import (
	"encoding/json"
	"strings"
)

const opencodeResumePrompt = "Continue"

// opencodeProbeTimeoutMs/ProbePrompt: cheap liveness probe for the
// zero-event/empty-output signature (loop 59 run-11: a hung opencode
// burned all retry attempts on full-timeout runs while a trivial probe
// returned empty output). Short wall-clock deadline: an alive endpoint
// answers in seconds; anything else (empty output, error, timeout, spawn
// failure) means the endpoint is dead and retrying the real prompt would
// only burn the remaining budget.
const opencodeProbeTimeoutMs = 30_000
const opencodeProbePrompt = "Reply with the single word: ok"

// TS Infinity proxy.
const opencodeDefaultAPIMaxAttempts = 1 << 30

// OpenCodeAdapter mirrors TS OpenCodeAdapter.
type OpenCodeAdapter struct {
	adapterBase
}

// Name mirrors the TS readonly name.
func (a *OpenCodeAdapter) Name() string { return "opencode" }

// Capabilities: Q30 — watchdog off by default (callers pass an armed
// budget from config in prod); the spawn path reads this declaration
// instead of a local resolver.
func (a *OpenCodeAdapter) Capabilities() WorkerCapabilities {
	return WorkerCapabilities{DefaultNoProgressTimeoutMs: 0}
}

// IsProgress: opencode streams NDJSON; the shared core applies.
func (a *OpenCodeAdapter) IsProgress(line string) bool { return IsNdjsonProgressLine(line) }

// Spawn runs the opencode retry loop.
func (a *OpenCodeAdapter) Spawn(opts WorkerSpawnOptions) WorkerResult {
	start := a.nowMs()
	maxAttempts := opencodeDefaultAPIMaxAttempts
	if opts.APIMaxAttempts != nil {
		maxAttempts = *opts.APIMaxAttempts
	}
	caps := a.Capabilities()
	noProgressTimeoutMs := ResolveNoProgressTimeoutMs(opts.NoProgressTimeoutMs, &caps)
	wallDeadline := int64(1 << 62)
	if opts.TimeoutMs > 0 {
		wallDeadline = start + int64(opts.TimeoutMs)
	}

	sessionId := ""
	var last *SpawnCliResult
	var lastEvents []WorkerEvent
	lastResultText := ""
	binary := "opencode"
	noProgress := false
	args := opencodeBaseArgs(opts, binary)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if a.nowMs() >= wallDeadline {
			if last != nil {
				last.TimedOut = true
			}
			break
		}
		prepared, err := a.prepare(binary, args, SpawnCliOptions{
			Dir:                 opts.Cwd,
			TimeoutMs:           launchBudgetMs(wallDeadline, a.nowMs(), int64(opts.TimeoutMs)),
			Env:                 opts.Env,
			NoProgressTimeoutMs: intPtrIf(noProgressTimeoutMs != 0, noProgressTimeoutMs),
			WatchdogLedger:      opts.WatchdogLedger,
			ColdStartTimeoutMs:  opts.ColdStartTimeoutMs,
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
		// Binary fallback: if `opencode` is not installed, try `opencode2`
		// once.
		if isOpencodeSpawnFailure(raw) && binary == "opencode" {
			binary = "opencode2"
			args = opencodeBaseArgs(opts, binary)
			fallbackPrepared, ferr := a.prepare(binary, args, SpawnCliOptions{
				Dir:                 opts.Cwd,
				TimeoutMs:           launchBudgetMs(wallDeadline, a.nowMs(), int64(opts.TimeoutMs)),
				Env:                 opts.Env,
				NoProgressTimeoutMs: intPtrIf(noProgressTimeoutMs != 0, noProgressTimeoutMs),
				WatchdogLedger:      opts.WatchdogLedger,
				ColdStartTimeoutMs:  opts.ColdStartTimeoutMs,
			})
			if ferr != nil {
				res := SpawnCliResult{ExitCode: -1, Stderr: ferr.Error()}
				raw = res
			} else {
				raw = a.run(fallbackPrepared.Cmd, fallbackPrepared.Args, RunWorkerCliOptions{
					SpawnCliOptions: fallbackPrepared.Opts,
					Herdr:           opts.Herdr,
				})
			}
			if isOpencodeSpawnFailure(raw) {
				last = &raw
				lastEvents = []WorkerEvent{}
				lastResultText = ""
				break
			}
		}

		last = &raw
		outcome := interpretOpencode(raw)
		if outcome.SessionId != "" {
			sessionId = outcome.SessionId
		}
		lastEvents = outcome.Events
		lastResultText = outcome.ResultText

		zeroEvent := isOpencodeZeroEventNoProgress(outcome, raw)
		ok := raw.ExitCode == 0 && !outcome.IsError && !raw.TimedOut && !zeroEvent
		if ok {
			break
		}

		// Zero-event empty-output signature: probe cheaply before burning
		// the next full attempt (loop 59 run-11). A dead endpoint bails out
		// immediately with noProgress so callers can fall back; a live one
		// gets the normal backoff retry (fresh prompt, since a silent run
		// typically emitted no session id to resume).
		if zeroEvent {
			if attempt < maxAttempts && a.nowMs() < wallDeadline {
				probe := a.probeEndpointAlive(opts, binary)
				if !probe {
					noProgress = true
					break
				}
				a.sleepMs(backoffDelay(attempt))
				if sessionId != "" {
					args = opencodeResumeArgs(sessionId, opts, binary)
				} else {
					args = opencodeBaseArgs(opts, binary)
				}
				continue
			}
			// No budget for another attempt: surface the no-progress
			// outcome instead of a false success.
			noProgress = true
			break
		}

		if attempt == maxAttempts {
			break
		}
		if raw.ExitCode == -1 && !raw.TimedOut {
			break // spawn failure (ENOENT etc) — never loops forever
		}
		if outcome.ErrorText != "" && IsNonRetryableApiError(outcome.ErrorText) {
			break
		}
		// Wall-clock overall budget already checked at loop top
		if a.nowMs() >= wallDeadline {
			break
		}

		errorText := outcome.ErrorText
		if errorText == "" {
			errorText = raw.Stderr
		}
		if sessionId == "" {
			if !IsRetryableWithoutSession(raw.TimedOut, raw.ExitCode, errorText, raw.Stderr) {
				break
			}
		}

		a.sleepMs(backoffDelay(attempt))
		if sessionId != "" {
			args = opencodeResumeArgs(sessionId, opts, binary)
		} else {
			args = opencodeBaseArgs(opts, binary)
		}
	}

	final := derefSpawn(last)
	if final.TimedOut {
		result := WorkerResult{
			ExitCode:   final.ExitCode,
			Events:     []WorkerEvent{},
			ResultText: "",
			SessionId:  "",
			DurationMs: a.nowMs() - start,
			TimedOut:   true,
		}
		if final.ColdStart {
			result.ColdStart = true
		}
		return stampStreamMetrics(result, final)
	}

	return stampStreamMetrics(WorkerResult{
		ExitCode:   final.ExitCode,
		Events:     lastEvents,
		ResultText: lastResultText,
		SessionId:  sessionId,
		DurationMs: a.nowMs() - start,
		TimedOut:   false,
		// Zero-event attempts (probe-dead bail or exhausted budget) surface
		// as noProgress, never as a false success.
		NoProgress: noProgress,
	}, final)
}

// probeEndpointAlive: short-deadline trivial probe run before spending
// another full retry attempt on the zero-event signature. Alive =
// non-empty output; empty/error/timed-out responses all mean the endpoint
// is dead and further attempts are waste.
func (a *OpenCodeAdapter) probeEndpointAlive(opts WorkerSpawnOptions, binary string) bool {
	probeOpts := opts
	probeOpts.Prompt = opencodeProbePrompt
	probeArgs := opencodeBaseArgs(probeOpts, binary)
	prepared, err := a.prepare(binary, probeArgs, SpawnCliOptions{
		Dir:       opts.Cwd,
		TimeoutMs: opencodeProbeTimeoutMs,
		Env:       opts.Env,
	})
	if err != nil {
		return false
	}
	raw := a.run(prepared.Cmd, prepared.Args, RunWorkerCliOptions{
		SpawnCliOptions: prepared.Opts,
		Herdr:           opts.Herdr,
	})
	return raw.ExitCode == 0 && !raw.TimedOut && strings.TrimSpace(raw.Stdout) != ""
}

// opencodeModelArgs mirrors TS modelArgs. opencode2 encodes variant as
// provider/model#variant; opencode uses a separate --variant flag. If the
// model already carries a #variant suffix, keep it as-is and don't
// duplicate.
func opencodeModelArgs(opts WorkerSpawnOptions, binary string) []string {
	if opts.Model == "" {
		return nil
	}
	rawModel := strings.TrimSpace(opts.Model)
	variant := strings.TrimSpace(opts.Variant)
	hasHashVariant := strings.Contains(rawModel, "#")
	if variant == "" || hasHashVariant {
		return []string{"--model", rawModel}
	}
	if binary == "opencode2" {
		return []string{"--model", rawModel + "#" + variant}
	}
	return []string{"--model", rawModel, "--variant", variant}
}

func opencodeBaseArgs(opts WorkerSpawnOptions, binary string) []string {
	args := []string{"run", "--format", "json"}
	args = append(args, opencodeModelArgs(opts, binary)...)
	return append(args, opts.Prompt)
}

func opencodeResumeArgs(sessionId string, opts WorkerSpawnOptions, binary string) []string {
	args := []string{"run", "--format", "json"}
	args = append(args, opencodeModelArgs(opts, binary)...)
	return append(args, "--session", sessionId, opencodeResumePrompt)
}

// isOpencodeSpawnFailure mirrors TS isSpawnFailure.
func isOpencodeSpawnFailure(run SpawnCliResult) bool {
	return run.ExitCode == -1 && strings.TrimSpace(run.Stdout) == "" &&
		strings.TrimSpace(run.Stderr) == "" && !run.TimedOut
}

// isOpencodeZeroEventNoProgress: the zero-event/empty-output signature —
// exit 0 but no parsed events and no result text (pure garbage or empty
// stdout parses the same). Not a success — the worker turned nothing.
// Classified as noProgress so callers can distinguish it from success and
// fall back cheaply.
func isOpencodeZeroEventNoProgress(outcome OpencodeOutcome, run SpawnCliResult) bool {
	return run.ExitCode == 0 && !run.TimedOut && len(outcome.Events) == 0 && outcome.ResultText == ""
}

// OpencodeOutcome mirrors TS OpencodeOutcome.
type OpencodeOutcome struct {
	IsError    bool
	SessionId  string
	ErrorText  string
	Events     []WorkerEvent
	ResultText string
}

// InterpretOpencodeForTest is the test seam re-export of the parser.
func InterpretOpencodeForTest(run SpawnCliResult) OpencodeOutcome {
	return interpretOpencode(run)
}

// interpretOpencode parses opencode's NDJSON stdout.
func interpretOpencode(run SpawnCliResult) OpencodeOutcome {
	events := []WorkerEvent{}
	sessionId := ""
	errorText := ""

	for _, line := range strings.Split(run.Stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		v, ok := parseJSONAny(trimmed)
		if !ok {
			continue
		}
		record, ok := v.(map[string]any)
		if !ok {
			continue
		}
		ev := WorkerEvent{}
		if t, ok := record["type"].(string); ok {
			ev["type"] = t
		} else {
			ev["type"] = "event"
		}
		for k, val := range record {
			ev[k] = val
		}
		events = append(events, ev)
		if sid, ok := record["sessionID"].(string); ok {
			sessionId = sid
		}
		if sessionId == "" {
			if sid, ok := record["session_id"].(string); ok {
				sessionId = sid
			}
		}
		// also check nested part.sessionID
		if sessionId == "" {
			if part, ok := record["part"].(map[string]any); ok {
				if sid, ok := part["sessionID"].(string); ok {
					sessionId = sid
				}
			}
		}
	}

	// Detect error event
	isError := false
	for _, e := range events {
		if e["type"] == "error" || e["error"] != nil {
			isError = true
			errorText = extractOpencodeErrorText(e)
			break
		}
	}
	if !isError && run.ExitCode != 0 {
		isError = true
		// Prefer stderr tail, else stdout-embedded message
		if strings.TrimSpace(run.Stderr) != "" {
			errorText = lastNLines(run.Stderr, 3)
		} else if len(events) == 0 && strings.TrimSpace(run.Stdout) != "" {
			errorText = truncateRunes(strings.TrimSpace(run.Stdout), 500)
		}
	}
	if errorText == "" && isError && strings.TrimSpace(run.Stderr) != "" {
		errorText = lastNLines(run.Stderr, 3)
	}

	// resultText: last text found
	resultText := ""
	for _, event := range events {
		if t := extractOpencodeText(event); t != "" {
			resultText = t
		}
	}

	return OpencodeOutcome{IsError: isError, SessionId: sessionId, ErrorText: errorText, Events: events, ResultText: resultText}
}

// extractOpencodeErrorText mirrors TS extractErrorText.
func extractOpencodeErrorText(event WorkerEvent) string {
	err := event["error"]
	if err == nil {
		return ""
	}
	switch e := err.(type) {
	case string:
		return e
	case map[string]any:
		if m, ok := e["message"].(string); ok && m != "" {
			return m
		}
		if data, ok := e["data"].(map[string]any); ok {
			if m, ok := data["message"].(string); ok && m != "" {
				return m
			}
			if s, ok := data["error"].(string); ok && s != "" {
				return s
			}
		}
		if s, ok := e["error"].(string); ok && s != "" {
			return s
		}
		// TS: JSON.stringify(e).slice(0, 500)
		if b, mErr := json.Marshal(e); mErr == nil {
			return truncateRunes(string(b), 500)
		}
		return ""
	}
	return ""
}

// extractOpencodeText mirrors TS extractText.
func extractOpencodeText(event WorkerEvent) string {
	if s, ok := event["text"].(string); ok && s != "" {
		return s
	}
	if s, ok := event["part"].(string); ok && s != "" {
		return s
	}
	if part, ok := event["part"].(map[string]any); ok {
		if s, ok := part["text"].(string); ok && s != "" {
			return s
		}
	}
	// opencode2 nested: part.content[].text etc not needed for now
	return ""
}

// truncateRunes mirrors JS String.slice(0, n) (UTF-16 code units; for the
// ASCII-ish error payloads these carry, rune truncation is equivalent).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
