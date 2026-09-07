// Package file mirrors src/resilience/classify.ts (FR-GO-07, issue #194):
// transient provider-error classification — the patterns safe to retry
// forever, the ordered coarse class labels reported by `devagent status
// --providers`, and the sessionless-retry decision. isNonRetryableApiError
// mirrors the re-export from src/sessionguard/events.ts.
package resilience

import (
	"regexp"
	"strings"
)

// nonRetryablePatterns are the errors that will never succeed on retry;
// abort instead of burning attempts (src/sessionguard/events.ts).
var nonRetryablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)invalid api key`),
	regexp.MustCompile(`(?i)authentication`),
	regexp.MustCompile(`(?i)unauthorized`),
	regexp.MustCompile(`(?i)credit balance`),
	regexp.MustCompile(`(?i)billing`),
	regexp.MustCompile(`(?i)not found.*model|model.*not found`),
}

// IsNonRetryableApiError mirrors the re-export from src/sessionguard/events.ts.
func IsNonRetryableApiError(text string) bool {
	for _, p := range nonRetryablePatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// transientPatterns are the transient provider errors that are safe to
// retry forever. Covers: Console Go endpoint, upstream failures,
// rate/overload, network, timeouts. Non-retryable auth/billing is excluded
// via IsNonRetryableApiError.
var transientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)endpoint is unavailable`),
	regexp.MustCompile(`(?i)upstream request failed`),
	regexp.MustCompile(`(?i)error from provider`),
	regexp.MustCompile(`(?i)provider.*unavailable`),
	regexp.MustCompile(`(?i)connection lost`),
	regexp.MustCompile(`(?i)connection refused`),
	regexp.MustCompile(`(?i)ETIMEDOUT`),
	regexp.MustCompile(`(?i)ECONNRESET`),
	regexp.MustCompile(`(?i)ECONNREFUSED`),
	regexp.MustCompile(`(?i)ENOTFOUND`),
	regexp.MustCompile(`(?i)fetch failed`),
	regexp.MustCompile(`(?i)network error`),
	regexp.MustCompile(`(?i)overloaded`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
	// xAI 429 sub-classes (FR-GROK-06, PRD:1116): the API distinguishes RPS
	// from TPM (tokens-per-minute) limits — a 500k-ctx prompt can exhaust TPM
	// in one shot while staying under RPS. Match the wording directly so the
	// class survives bodies that omit "429"/"rate limit" verbatim.
	regexp.MustCompile(`(?i)\btpm\b|tokens?[ _-]?per[ _-]?minute`),
	regexp.MustCompile(`(?i)\brps\b|requests?[ _-]?per[ _-]?second`),
	// Numeric 5xx: xAI surfaces them as "Server error (503) from ..."; the
	// named gateway/timeout prose already matched above, bare codes did not.
	regexp.MustCompile(`\b5\d{2}\b`),
	regexp.MustCompile(`429|529`),
	regexp.MustCompile(`(?i)timeout`),
	regexp.MustCompile(`(?i)timed out`),
	regexp.MustCompile(`(?i)unavailable`),
	regexp.MustCompile(`(?i)service unavailable`),
	regexp.MustCompile(`(?i)bad gateway`),
	regexp.MustCompile(`(?i)gateway timeout`),
	regexp.MustCompile(`(?i)socket hang up`),
	// omniroute proxy surfaces rate-limited empty upstream streams as
	// "[claude-code:unrecognized_model]" on stderr and a JSON array with no
	// .result field. The proxy's own log shows "all 1 active accounts rate
	// limited" — the only fix is to retry once the upstream cooldowns. Loop
	// stalls without this (loop-66 incident: 1h+ of false-failed workers).
	regexp.MustCompile(`(?i)unrecognized_model`),
	regexp.MustCompile(`\[claude-code:unrecognized_model\]`),
	regexp.MustCompile(`(?i)empty stream`),
	regexp.MustCompile(`(?i)empty response`),
}

// IsTransientProviderError reports whether text is a transient provider
// error (safe to retry forever). A nil pointer (TS undefined/null) or an
// empty string is not transient.
func IsTransientProviderError(text *string) bool {
	if text == nil || *text == "" {
		return false
	}
	if IsNonRetryableApiError(*text) {
		return false
	}
	for _, p := range transientPatterns {
		if p.MatchString(*text) {
			return true
		}
	}
	return false
}

// transientClassRule is one ordered coarse-class rule: most specific first
// so generic catch-alls (unavailable, timeout) never mask the actionable
// class. Exhaustive over transientPatterns: every pattern above maps to
// exactly one label. Reported by `devagent status --providers` so the
// proxy-gate decision is operator-observable instead of log-grep-only.
type transientClassRule struct {
	pattern *regexp.Regexp
	label   string
}

// xAI 429 split (FR-GROK-06): TPM and RPS are ordered before the generic
// rate-limit label because they demand different remedies — a TPM hit needs
// the within-xAI model step-down (shorter context), an RPS hit just needs
// the same-model cooldown. Generic 429s keep the coarse 'rate-limit' label.
var transientClassRules = []transientClassRule{
	{regexp.MustCompile(`\[claude-code:unrecognized_model\]`), "unrecognized-model"},
	{regexp.MustCompile(`(?i)unrecognized_model`), "unrecognized-model"},
	{regexp.MustCompile(`(?i)empty stream|empty response`), "empty-stream"},
	{regexp.MustCompile(`(?i)\btpm\b|tokens?[ _-]?per[ _-]?minute`), "rate-limit-tpm"},
	{regexp.MustCompile(`(?i)\brps\b|requests?[ _-]?per[ _-]?second`), "rate-limit-rps"},
	{regexp.MustCompile(`(?i)rate limit|too many requests|429|529`), "rate-limit"},
	{regexp.MustCompile(`(?i)overloaded`), "overloaded"},
	{regexp.MustCompile(`(?i)upstream request failed|error from provider`), "upstream"},
	{regexp.MustCompile(`(?i)bad gateway|gateway timeout`), "bad-gateway"},
	{regexp.MustCompile(`(?i)ETIMEDOUT|timeout|timed out`), "timeout"},
	{regexp.MustCompile(`(?i)connection lost|connection refused|ECONNRESET|ECONNREFUSED|ENOTFOUND|fetch failed|network error|socket hang up`), "network"},
	{regexp.MustCompile(`\b5\d{2}\b`), "server-error"},
	{regexp.MustCompile(`(?i)endpoint is unavailable|provider.*unavailable|service unavailable|unavailable`), "unavailable"},
}

// TransientErrorClass is the coarse class for a transient provider error, or
// "" when not transient (TS returns null).
func TransientErrorClass(text *string) string {
	if !IsTransientProviderError(text) {
		return ""
	}
	for _, rule := range transientClassRules {
		if rule.pattern.MatchString(*text) {
			return rule.label
		}
	}
	return "transient"
}

// sessionlessTransientPatterns are the patterns that justify a retry even
// when no session id was emitted.
var sessionlessTransientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)endpoint is unavailable`),
	regexp.MustCompile(`(?i)upstream request failed`),
	regexp.MustCompile(`(?i)error from provider`),
	regexp.MustCompile(`(?i)provider.*unavailable`),
	regexp.MustCompile(`(?i)overloaded`),
	regexp.MustCompile(`(?i)rate limit`),
	regexp.MustCompile(`(?i)too many requests`),
	regexp.MustCompile(`429|529`),
	regexp.MustCompile(`(?i)unavailable`),
	regexp.MustCompile(`(?i)service unavailable`),
	regexp.MustCompile(`(?i)timeout`),
	regexp.MustCompile(`(?i)timed out`),
}

func isSessionlessTransient(text string) bool {
	if IsNonRetryableApiError(text) {
		return false
	}
	for _, p := range sessionlessTransientPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// RetryableWithoutSessionOpts mirrors the TS isRetryableWithoutSession opts
// object. ExitCode is part of the TS argument shape but unused by the
// decision (kept for call-site parity).
type RetryableWithoutSessionOpts struct {
	TimedOut  bool
	ExitCode  *int
	ErrorText *string
	Stderr    *string
}

// IsRetryableWithoutSession is true when the failure should be retried from
// scratch (no session).
func IsRetryableWithoutSession(opts RetryableWithoutSessionOpts) bool {
	if opts.TimedOut {
		return true // watchdog/timeout is transient by default
	}
	text := ""
	if opts.ErrorText != nil {
		text = *opts.ErrorText
	} else if opts.Stderr != nil {
		text = *opts.Stderr
	}
	if strings.TrimSpace(text) == "" {
		return false
	}
	return isSessionlessTransient(text)
}
