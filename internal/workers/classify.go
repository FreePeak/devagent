// Minimal local port of the retry-classification helpers the adapters need
// (src/resilience/classify.ts isTransientProviderError / transientErrorClass /
// isRetryableWithoutSession, src/sessionguard/events.ts isNonRetryableApiError,
// src/sessionguard/backoff.ts backoffDelay).
//
// TODO(FR-GO-06 #187 / FR-GO-07 #188): when the resilience/orchestrator ports
// land, replace these copies with the shared packages — the adapters must
// consume one classification source of truth.

package workers

import (
	"math"
	"math/rand"
	"regexp"
	"strings"
)

var transientPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)endpoint is unavailable`),
	regexp.MustCompile(`(?i)upstream request failed`),
	regexp.MustCompile(`(?i)error from provider`),
	regexp.MustCompile(`(?i)provider.*unavailable`),
	regexp.MustCompile(`(?i)connection lost`),
	regexp.MustCompile(`(?i)connection refused`),
	regexp.MustCompile(`ETIMEDOUT`),
	regexp.MustCompile(`ECONNRESET`),
	regexp.MustCompile(`ECONNREFUSED`),
	regexp.MustCompile(`ENOTFOUND`),
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

// IsTransientProviderError: transient provider errors that are safe to
// retry forever. Covers: Console Go endpoint, upstream failures,
// rate/overload, network, timeouts. Non-retryable auth/billing is
// excluded via IsNonRetryableApiError (the 401-fixture wording carries
// transient-sounding prose — "temporarily unavailable" — that must not
// flip an auth failure into an infinite infra retry).
func IsTransientProviderError(text string) bool {
	if text == "" {
		return false
	}
	if IsNonRetryableApiError(text) {
		return false
	}
	for _, p := range transientPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// transientClassRules: ordered coarse class labels for operator-visible
// transient errors. Most specific first so generic catch-alls (unavailable,
// timeout) never mask the actionable class.
//
// xAI 429 split (FR-GROK-06): TPM and RPS are ordered before the generic
// rate-limit label because they demand different remedies — a TPM hit needs
// the within-xAI model step-down (shorter context), an RPS hit just needs
// the same-model cooldown. Generic 429s keep the coarse 'rate-limit' label.
var transientClassRules = []struct {
	pattern *regexp.Regexp
	label   string
}{
	{regexp.MustCompile(`\[claude-code:unrecognized_model\]`), "unrecognized-model"},
	{regexp.MustCompile(`(?i)unrecognized_model`), "unrecognized-model"},
	{regexp.MustCompile(`(?i)empty stream|empty response`), "empty-stream"},
	{regexp.MustCompile(`(?i)\btpm\b|tokens?[ _-]?per[ _-]?minute`), "rate-limit-tpm"},
	{regexp.MustCompile(`(?i)\brps\b|requests?[ _-]?per[ _-]?second`), "rate-limit-rps"},
	{regexp.MustCompile(`(?i)rate limit|too many requests|429|529`), "rate-limit"},
	{regexp.MustCompile(`(?i)overloaded`), "overloaded"},
	{regexp.MustCompile(`(?i)upstream request failed|error from provider`), "upstream"},
	{regexp.MustCompile(`(?i)bad gateway|gateway timeout`), "bad-gateway"},
	{regexp.MustCompile(`ETIMEDOUT|(?i)timeout|(?i)timed out`), "timeout"},
	{regexp.MustCompile(`(?i)connection lost|connection refused|ECONNRESET|ECONNREFUSED|ENOTFOUND|fetch failed|network error|socket hang up`), "network"},
	{regexp.MustCompile(`\b5\d{2}\b`), "server-error"},
	{regexp.MustCompile(`(?i)endpoint is unavailable|provider.*unavailable|service unavailable|unavailable`), "unavailable"},
}

// TransientErrorClass is the coarse class for a transient provider error,
// or "" when not transient (TS null).
func TransientErrorClass(text string) string {
	if !IsTransientProviderError(text) {
		return ""
	}
	for _, rule := range transientClassRules {
		if rule.pattern.MatchString(text) {
			return rule.label
		}
	}
	return "transient"
}

// nonRetryablePatterns: errors that will never succeed on retry; abort
// instead of burning attempts (src/sessionguard/events.ts).
var nonRetryablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)invalid api key`),
	regexp.MustCompile(`(?i)authentication`),
	regexp.MustCompile(`(?i)unauthorized`),
	regexp.MustCompile(`(?i)credit balance`),
	regexp.MustCompile(`(?i)billing`),
	regexp.MustCompile(`(?i)not found.*model|model.*not found`),
}

// IsNonRetryableApiError matches the TS helper.
func IsNonRetryableApiError(text string) bool {
	for _, p := range nonRetryablePatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// sessionlessTransient: patterns that justify a retry even when no session
// id was emitted.
var sessionlessTransient = []*regexp.Regexp{
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

// IsRetryableWithoutSession: true when the failure should be retried from
// scratch (no session).
func IsRetryableWithoutSession(timedOut bool, exitCode int, errorText, stderr string) bool {
	if timedOut {
		return true // watchdog/timeout is transient by default
	}
	_ = exitCode
	text := errorText
	if text == "" {
		text = stderr
	}
	if strings.TrimSpace(text) == "" {
		return false
	}
	if IsNonRetryableApiError(text) {
		return false
	}
	for _, p := range sessionlessTransient {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// BackoffOptions mirrors TS BackoffOptions.
type BackoffOptions struct {
	BaseDelayMs int
	MaxDelayMs  int
	Factor      float64
}

// DEFAULT_BACKOFF mirrors TS DEFAULT_BACKOFF.
var DEFAULT_BACKOFF = BackoffOptions{BaseDelayMs: 2_000, MaxDelayMs: 60_000, Factor: 2}

// BackoffDelay: exponential backoff with +/-25% jitter so parallel guards
// do not sync up. Attempt is 1-based; attempt 1 waits roughly BaseDelayMs.
func BackoffDelay(attempt int, options BackoffOptions, random func() float64) int {
	capped := attempt
	if capped < 1 {
		capped = 1
	}
	raw := math.Min(
		float64(options.MaxDelayMs),
		float64(options.BaseDelayMs)*math.Pow(options.Factor, float64(capped-1)),
	)
	jitter := 1 + (random()*0.5 - 0.25)
	return int(math.Max(0, math.Round(raw*jitter)))
}

// backoffDelay attempt helper matching the TS call shape (default options,
// Math.random).
func backoffDelay(attempt int) int {
	return BackoffDelay(attempt, DEFAULT_BACKOFF, rand.Float64)
}
