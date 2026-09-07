// Go port of src/workers/claude-code.ts (FR-GO-05).
//
// Adapter over the Claude Code headless CLI:
//
//	claude -p <prompt> --output-format json [--max-turns N]
//
// stdout is a single JSON object with fields like `result` and
// `session_id` (newer CLIs emit the full event stream as a JSON array; the
// terminal entry carries type:'result').
//
// Claude Code never retries a response dropped mid-stream; when that
// happens the turn dies but the session transcript persists. This adapter
// resumes the same session (`--resume <id> -p "Continue"`) with exponential
// backoff until the turn completes, attempts are exhausted, or the error is
// auth/billing.

package workers

import (
	"regexp"
	"strings"
)

const claudeResumePrompt = "Continue"

// TS Infinity proxy: effectively unbounded attempt budget.
const claudeDefaultAPIMaxAttempts = 1 << 30

// ClaudeCodeAdapter mirrors TS ClaudeCodeAdapter. The embedded
// adapterBase carries the test seams (Sleep/Run/Prepare/NowMs); nil =
// production behavior.
type ClaudeCodeAdapter struct {
	adapterBase
}

// Name mirrors the TS readonly name.
func (a *ClaudeCodeAdapter) Name() string { return "claude-code" }

// Capabilities: Q30 — declares no watchdog budget (0 = watchdog off;
// callers like the executor arm one explicitly from config when they want
// it).
func (a *ClaudeCodeAdapter) Capabilities() WorkerCapabilities {
	return WorkerCapabilities{DefaultNoProgressTimeoutMs: 0}
}

// IsProgress: claude-code emits single-JSON envelopes rather than an
// NDJSON event stream; TS leaves isProgress undefined for this adapter, so
// the runtime fallback keeps the shared NDJSON core semantics.
func (a *ClaudeCodeAdapter) IsProgress(line string) bool { return IsNdjsonProgressLine(line) }

// Spawn runs the adapter retry loop.
func (a *ClaudeCodeAdapter) Spawn(opts WorkerSpawnOptions) WorkerResult {
	start := a.nowMs()
	maxAttempts := claudeDefaultAPIMaxAttempts
	if opts.APIMaxAttempts != nil {
		maxAttempts = *opts.APIMaxAttempts
	}
	caps := a.Capabilities()
	noProgressTimeoutMs := ResolveNoProgressTimeoutMs(opts.NoProgressTimeoutMs, &caps)
	wallDeadline := int64(1 << 62)
	if opts.TimeoutMs > 0 {
		wallDeadline = start + int64(opts.TimeoutMs)
	}

	args := claudeBaseArgs(opts)
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
		spawnOpts := SpawnCliOptions{
			Dir:                 opts.Cwd,
			TimeoutMs:           opts.TimeoutMs,
			Env:                 opts.Env,
			NoProgressTimeoutMs: intPtrIf(noProgressTimeoutMs != 0, noProgressTimeoutMs),
			ColdStartTimeoutMs:  opts.ColdStartTimeoutMs,
			WatchdogLedger:      opts.WatchdogLedger,
		}
		prepared, err := a.prepare("claude", args, spawnOpts)
		if err != nil {
			// prepareWorkerSpawn failures are loud by contract; surface as
			// a spawn failure result instead of panicking the dispatcher.
			res := SpawnCliResult{ExitCode: -1, Stderr: err.Error()}
			last = &res
			break
		}
		runOpts := RunWorkerCliOptions{SpawnCliOptions: prepared.Opts, Herdr: opts.Herdr}
		raw := a.run(prepared.Cmd, prepared.Args, runOpts)
		last = &raw

		outcome := claudeInterpret(raw)
		if outcome.sessionId != "" {
			sessionId = outcome.sessionId
		}

		ok := !raw.TimedOut && outcome.exitCode == 0 && !outcome.isError
		if ok {
			break
		}

		if attempt == maxAttempts {
			break
		}
		if raw.ExitCode == -1 && !raw.TimedOut {
			break // spawn failure (ENOENT) — never retry forever
		}
		if outcome.errorText != "" && IsNonRetryableApiError(outcome.errorText) {
			break
		}

		// Transient detection: watchdog timeouts and provider errors
		// (Console Go, upstream, etc.) are retried forever. Without a
		// session we retry from scratch only for provider/timeout signals
		// (not generic ECONNREFUSED).
		errorText := outcome.errorText
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
		if sessionId != "" {
			resume := []string{
				"--resume",
				sessionId,
				"-p",
				claudeResumePrompt,
				"--output-format",
				"json",
			}
			if opts.MaxSteps != nil {
				resume = append(resume, "--max-turns", itoa(*opts.MaxSteps))
			}
			args = resume
		} else {
			args = claudeBaseArgs(opts)
		}
	}

	return claudeFinalize(derefSpawn(last), a.nowMs()-start)
}

// claudeBaseArgs mirrors the TS baseArgs. claude-code ignores variant;
// model is the only knob. Driver tier aliases ("coding", devagent.json
// model) are claude-proxy selectors, not API-key model ids: `--model
// coding` fails with "403 Combo "coding" is not allowed for this API key"
// (2026-09-01 live loop 58: implement worker retried it forever because
// events=0 + null resultText reads as a logic failure, not transient).
// Drop any value without a "/" or known claude family prefix so claude
// falls back to ~/.claude/settings.json model. Pass explicit provider ids
// through.
func claudeBaseArgs(opts WorkerSpawnOptions) []string {
	rawModel := strings.TrimSpace(opts.Model)
	var model string
	if rawModel != "" && claudeModelRe.MatchString(rawModel) {
		// TS rawModel.split('#')[0]
		if i := strings.IndexByte(rawModel, '#'); i >= 0 {
			model = rawModel[:i]
		} else {
			model = rawModel
		}
	}
	args := []string{"-p", opts.Prompt, "--output-format", "json"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if opts.MaxSteps != nil {
		args = append(args, "--max-turns", itoa(*opts.MaxSteps))
	}
	return args
}

// claudeModelRe mirrors the TS regex pair
// /^claude-/.test(rawModel) || /^(opus|sonnet|haiku)(-|$)/.test(rawModel).
var claudeModelRe = regexp.MustCompile(`^claude-|^(?:opus|sonnet|haiku)(?:-|$)`)

// claudeRunOutcome mirrors TS RunOutcome.
type claudeRunOutcome struct {
	exitCode  int
	isError   bool
	sessionId string
	errorText string
	parsed    map[string]any
}

// InterpretClaudeForTest is the test seam re-export of the parser.
func InterpretClaudeForTest(run SpawnCliResult) claudeRunOutcome {
	return claudeInterpret(run)
}

// claudeInterpret parses claude's stdout into the worker outcome shape.
func claudeInterpret(run SpawnCliResult) claudeRunOutcome {
	var parsed map[string]any
	if s := strings.TrimSpace(run.Stdout); s != "" {
		if candidate, ok := parseJSONAny(s); ok {
			switch v := candidate.(type) {
			case []any:
				// Newer claude CLI: --output-format json emits the full
				// event stream as a JSON array; the terminal entry carries
				// type:'result' (live-smoke lesson: treating arrays as
				// unparsable made every planner call look empty).
				for i := len(v) - 1; i >= 0; i-- {
					if e, ok := v[i].(map[string]any); ok && e["type"] == "result" {
						parsed = e
						break
					}
				}
			case map[string]any:
				parsed = v
			}
		}
	}
	isError := asBool(parsed["is_error"])
	rawResult, _ := parsed["result"].(string)
	sessionId := ""
	if parsed != nil {
		sessionId, _ = parsed["session_id"].(string)
	}
	errorText := ""
	if rawResult != "" {
		errorText = rawResult
	} else if t := strings.TrimSpace(run.Stderr); t != "" {
		// TS: stderr.trim().split('\n').slice(-3).join('\n')
		errorText = lastNLines(t, 3)
	}
	isError = isError || (run.ExitCode != 0 && parsed == nil)
	return claudeRunOutcome{
		exitCode:  run.ExitCode,
		isError:   isError,
		sessionId: sessionId,
		errorText: errorText,
		parsed:    parsed,
	}
}

// claudeFinalize mirrors TS finalize.
func claudeFinalize(run SpawnCliResult, durationMs int64) WorkerResult {
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

	outcome := claudeInterpret(run)
	if strings.TrimSpace(run.Stdout) == "" || outcome.parsed == nil {
		// Stderr often carries the real failure reason (e.g.
		// [claude-code:unrecognized_model]) when the upstream returned an
		// empty stream. Surface it so the executor can classify properly.
		errorText := outcome.errorText
		if errorText == "" {
			errorText = strings.TrimSpace(run.Stderr)
		}
		return WorkerResult{
			ExitCode:   outcome.exitCode,
			Events:     []WorkerEvent{},
			ResultText: "",
			SessionId:  outcome.sessionId,
			DurationMs: durationMs,
			TimedOut:   false,
			ErrorText:  errorText,
		}
	}

	events := []WorkerEvent{eventFromResult(outcome.parsed)}
	resultText := ""
	if outcome.exitCode == 0 {
		if s, ok := outcome.parsed["result"].(string); ok {
			resultText = s
		}
	}
	return WorkerResult{
		ExitCode:   outcome.exitCode,
		Events:     events,
		ResultText: resultText,
		SessionId:  outcome.sessionId,
		DurationMs: durationMs,
		TimedOut:   false,
		ErrorText:  outcome.errorText,
	}
}
