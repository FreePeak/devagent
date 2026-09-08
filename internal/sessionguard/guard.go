// Package sessionguard is the Go port of FR-GO-07's cc-guard supervisor
// (src/sessionguard/*): backoff, stream-json event classification, the
// resume-on-terminal-API-failure retry loop, the child-process runner and
// Claude transcript inspection.
//
// Claude Code never retries a response that died mid-stream ("Connection
// lost mid-response") because partial output is already committed to the
// transcript. The supported recovery is starting a new turn in the same
// persisted session (`claude --resume <id> -p ...`). RunGuard automates
// exactly that: run the command, watch the structured stream-json output,
// and on terminal API failure re-launch against the same session id with
// exponential backoff until the turn completes or attempts are exhausted.
package sessionguard

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// AttemptResult is the outcome of one child launch.
type AttemptResult struct {
	// ExitCode is nil when the process was killed by a signal (TS null).
	ExitCode *int
	TimedOut bool
	// SessionID is "" when the stream never revealed a session id.
	SessionID     string
	ResultIsError bool
	SawResult     bool
	// SyntheticErrorText is "" when no synthetic error was observed.
	SyntheticErrorText string
}

// Stream identifiers passed to LineHandler.OnLine.
const (
	StreamStdout = "stdout"
	StreamStderr = "stderr"
)

// LineHandler receives every child output line untouched.
type LineHandler interface {
	OnLine(line string, stream string)
}

// LineHandlerFunc adapts a function to LineHandler.
type LineHandlerFunc func(line string, stream string)

// OnLine implements LineHandler.
func (f LineHandlerFunc) OnLine(line string, stream string) { f(line, stream) }

// SpawnOpts carries per-attempt spawn configuration.
type SpawnOpts struct {
	// NoProgressTimeoutMs kills the child when it emits nothing for this
	// long. 0 disables the watchdog.
	NoProgressTimeoutMs int
	// Env is the child environment; nil inherits the parent's.
	Env []string
}

// AttemptRunner runs one launch of the claude CLI to completion. An error
// return rejects the attempt (TS promise rejection), aborting RunGuard.
type AttemptRunner func(argv []string, handler LineHandler, opts SpawnOpts) (AttemptResult, error)

// Guard failure reasons (Reason field of GuardResult; "" = n/a).
const (
	ReasonAttemptsExhausted  = "attempts_exhausted"
	ReasonNonRetryableError  = "non_retryable_error"
	ReasonNoSessionID        = "no_session_id"
	maxAttemptsDefaultUnset  = math.MaxInt32
	defaultResumePrompt      = "Continue"
	noProgressWatchdogReason = "no-progress watchdog timeout"
)

// GuardOptions configures RunGuard.
type GuardOptions struct {
	// Argv is the full invocation, e.g. ['claude', '-p', 'do the thing'].
	Argv []string
	// ResumePrompt is the prompt used when resuming an interrupted
	// session; defaults to "Continue".
	ResumePrompt string
	// MaxAttempts is the total launches allowed, including the first.
	// nil falls back to DEVAGENT_API_MAX_ATTEMPTS (when set to a positive
	// integer) and otherwise means unbounded.
	MaxAttempts *int
	// Backoff mirrors the TS spread {...DEFAULT_BACKOFF, ...options.backoff}:
	// BaseDelayMs/MaxDelayMs apply verbatim when set (an explicit 0 is a
	// real value — the CLI accepts --base-delay-ms 0 / --max-delay-ms 0),
	// while a zero Factor keeps DEFAULT_BACKOFF.Factor (the CLI never
	// exposes --factor). nil uses DEFAULT_BACKOFF unchanged.
	Backoff *BackoffOptions
	// NoProgressTimeoutMs kills + resumes when the child emits nothing
	// for this long. 0 disables.
	NoProgressTimeoutMs int
	// Env is forwarded to each attempt; nil inherits the parent's.
	Env []string
	// Log receives progress lines; nil discards them.
	Log func(message string)
	// OnLine, when set, observes every child output line.
	OnLine func(line string, stream string)
	// Runner is the per-attempt launcher; RunGuard requires one (see
	// SpawnClaude).
	Runner AttemptRunner
	// Sleep and Random are injectable seams for deterministic tests;
	// nil uses time.Sleep / math/rand/v2.
	Sleep  func(ms int)
	Random func() float64
}

// GuardResult reports the outcome of the supervised run.
type GuardResult struct {
	OK        bool
	Attempts  int
	Resumed   int
	SessionID string // "" when unknown
	// Reason is one of the Reason* constants or "" on success.
	Reason    string
	LastError string // "" when none
}

// BuildResumeArgv drops the original prompt (-p/--print pair), removes any
// prior --resume pair so re-resuming never duplicates the flag, and appends
// `--resume <id> -p <prompt>`.
func BuildResumeArgv(argv []string, sessionID string, resumePrompt string) []string {
	out := append([]string(nil), argv...)
	for i := 0; i < len(out); i++ {
		token := out[i]
		if (token == "-p" || token == "--print") && i+1 < len(out) {
			next := out[i+1]
			if !strings.HasPrefix(next, "-") {
				out = append(out[:i], out[i+2:]...)
				break
			}
		}
	}
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "--resume" {
			out = append(out[:i], out[i+2:]...)
			break
		}
	}
	return append(out, "--resume", sessionID, "-p", resumePrompt)
}

// resolveMaxAttempts mirrors guard.ts envMax logic: options win; otherwise
// DEVAGENT_API_MAX_ATTEMPTS applies when set to a positive integer;
// everything else is unbounded (TS Infinity).
func resolveMaxAttempts(maxAttempts *int) int {
	if maxAttempts != nil {
		return *maxAttempts
	}
	if envMax := os.Getenv("DEVAGENT_API_MAX_ATTEMPTS"); envMax != "" {
		if n, err := strconv.Atoi(envMax); err == nil && n > 0 {
			return n
		}
	}
	return maxAttemptsDefaultUnset
}

// RunGuard runs the supervised retry loop until success, a non-retryable
// error, or attempts are exhausted.
func RunGuard(options GuardOptions) (GuardResult, error) {
	maxAttempts := resolveMaxAttempts(options.MaxAttempts)
	resumePrompt := options.ResumePrompt
	if resumePrompt == "" {
		resumePrompt = defaultResumePrompt
	}
	// The TS merge `{ ...DEFAULT_BACKOFF, ...options.backoff }` applies
	// provided keys verbatim, so an explicit 0 is a real value (the guard
	// CLI can pass --base-delay-ms 0 / --max-delay-ms 0). Factor has no CLI
	// flag and 0 is not a meaningful curve, so a zero falls back to the
	// package default.
	backoff := DEFAULT_BACKOFF
	if options.Backoff != nil {
		backoff.BaseDelayMs = options.Backoff.BaseDelayMs
		backoff.MaxDelayMs = options.Backoff.MaxDelayMs
		if options.Backoff.Factor != 0 {
			backoff.Factor = options.Backoff.Factor
		}
	}
	if options.Runner == nil {
		return GuardResult{}, errors.New("runGuard requires a runner (see spawnClaude)")
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	}
	random := options.Random
	if random == nil {
		random = rand.Float64
	}
	log := options.Log
	if log == nil {
		log = func(string) {}
	}

	var sessionID string
	var lastError string

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		isFirst := attempt == 1 || sessionID == ""
		if !isFirst && sessionID == "" {
			return GuardResult{
				OK:        false,
				Attempts:  attempt - 1,
				Resumed:   attempt - 2,
				SessionID: sessionID,
				Reason:    ReasonNoSessionID,
				LastError: lastError,
			}, nil
		}
		var argv []string
		if isFirst || sessionID == "" {
			argv = options.Argv
		} else {
			argv = BuildResumeArgv(options.Argv, sessionID, resumePrompt)
		}

		forwarded := LineHandlerFunc(func(line string, stream string) {
			if options.OnLine != nil {
				options.OnLine(line, stream)
			}
		})
		outcome, err := options.Runner(argv, forwarded, SpawnOpts{
			NoProgressTimeoutMs: options.NoProgressTimeoutMs,
			Env:                 options.Env,
		})
		if err != nil {
			return GuardResult{}, err
		}

		if sessionID == "" && outcome.SessionID != "" {
			sessionID = outcome.SessionID
		}
		if outcome.SyntheticErrorText != "" {
			lastError = outcome.SyntheticErrorText
		} else if outcome.TimedOut {
			lastError = noProgressWatchdogReason
		}

		ok := outcome.ExitCode != nil && *outcome.ExitCode == 0 && !outcome.ResultIsError
		if ok {
			return GuardResult{
				OK:        true,
				Attempts:  attempt,
				Resumed:   attempt - 1,
				SessionID: sessionID,
			}, nil
		}

		errorText := outcome.SyntheticErrorText
		if errorText != "" && IsNonRetryableApiError(errorText) {
			return GuardResult{
				OK:        false,
				Attempts:  attempt,
				Resumed:   attempt - 1,
				SessionID: sessionID,
				Reason:    ReasonNonRetryableError,
				LastError: errorText,
			}, nil
		}
		if attempt == maxAttempts {
			break
		}

		delay := BackoffDelay(attempt, backoff, random)
		failure := fmt.Sprintf("exit %s", formatExitCode(outcome.ExitCode))
		if outcome.TimedOut {
			failure = "watchdog"
		} else if outcome.ResultIsError {
			failure = "api error"
		}
		shownSession := sessionID
		if shownSession == "" {
			shownSession = "?"
		}
		log(fmt.Sprintf(
			"[cc-guard] attempt %d/%d failed (%s); resuming session %s in %dms",
			attempt, maxAttempts, failure, shownSession, delay,
		))
		sleep(delay)
	}

	return GuardResult{
		OK:        false,
		Attempts:  maxAttempts,
		Resumed:   maxAttempts - 1,
		SessionID: sessionID,
		Reason:    ReasonAttemptsExhausted,
		LastError: lastError,
	}, nil
}

// formatExitCode renders a nullable exit code the way the TS template
// literal does (null stays "null").
func formatExitCode(exitCode *int) string {
	if exitCode == nil {
		return "null"
	}
	return strconv.Itoa(*exitCode)
}
