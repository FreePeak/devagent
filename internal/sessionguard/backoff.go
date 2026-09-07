// Port of src/sessionguard/backoff.ts: exponential backoff with +/-25%
// jitter so parallel guards do not sync up.

package sessionguard

import (
	"math"
	"math/rand/v2"
)

// BackoffOptions configures BackoffDelay.
type BackoffOptions struct {
	BaseDelayMs int
	MaxDelayMs  int
	Factor      float64
}

// DEFAULT_BACKOFF is the default backoff curve: attempt 1 waits roughly
// 2s, doubling up to a 60s ceiling.
var DEFAULT_BACKOFF = BackoffOptions{
	BaseDelayMs: 2_000,
	MaxDelayMs:  60_000,
	Factor:      2,
}

// BackoffDelay returns the wait before the given (1-based) attempt: base
// delay scaled by the factor, capped at maxDelayMs, then jittered by +/-25%.
// random is injectable for deterministic tests (nil uses math/rand/v2).
// Rounding follows JS Math.round semantics (half toward +Infinity); the
// jittered value is never negative in practice, matching Math.max(0, ...).
func BackoffDelay(attempt int, options BackoffOptions, random func() float64) int {
	capped := attempt
	if capped < 1 {
		capped = 1
	}
	if random == nil {
		random = rand.Float64
	}
	raw := math.Min(
		float64(options.MaxDelayMs),
		float64(options.BaseDelayMs)*math.Pow(options.Factor, float64(capped-1)),
	)
	jitter := 1 + (random()*0.5 - 0.25)
	delay := math.Max(0, math.Round(raw*jitter))
	return int(delay)
}
