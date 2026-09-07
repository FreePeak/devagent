// Package file mirrors src/resilience/degradation.ts (FR-GO-07, issue
// #194): consecutive cross-role provider degradation (Q41) — the read-side
// aggregation that closes the starvation-gate observability gap for
// `devagent status --providers`.
//
// Why: the self-build starvation gate deliberately exempts degraded rows —
// one pause is expected, not a thrashing loop (scripts/selfbuild-loop.sh
// skips operator-degraded | operator-diverged | provider-degraded). The
// exemption makes a sustained outage invisible on every human surface.
//
// Contract: pure, read-only. Walk ledger rows newest-first, counting the
// trailing run of degradation rows and stopping at the first productive
// row. No new ledger status, no writes, no typed-union/TUI plumbing.
package resilience

import (
	"time"

	"github.com/FreePeak/devagent/internal/lessons"
)

// DegradeStreakThreshold is the trailing degraded rows that surface as a
// provider-outage breach line.
const DegradeStreakThreshold = 3

// degradedLoopResultStatuses are the loop-result statuses meaning "the loop
// paused without provider progress": provider-degraded (preflight found the
// provider down; no spend) and operator-diverged (doc-sync hit a history the
// operator must reconcile). Every other loop-result status (ok, failed,
// failed-tests, invalid, skipped, push-failed) means the provider answered —
// productive.
var degradedLoopResultStatuses = map[string]bool{
	"provider-degraded": true,
	"operator-diverged": true,
}

// DegradationStreak is the aggregated trailing-degradation view over the
// orchestration ledger.
type DegradationStreakResult struct {
	// Length of the trailing run of degradation rows (0 when the newest row
	// is productive).
	Count int `json:"count"`
	// Threshold the breach check used.
	Threshold int `json:"threshold"`
	// Count >= Threshold (never true at Count 0).
	Breach bool `json:"breach"`
	// ts of the newest degradation row (null when Count is 0).
	LatestTs *string `json:"latestTs"`
	// ts of the oldest degradation row in the run — the outage window start.
	OldestTs *string `json:"oldestTs"`
	// LatestTs - OldestTs in ms; null unless both streak edges have
	// parseable ts.
	WindowMs *float64 `json:"windowMs"`
	// Distinct roles seen on operator-degraded rows in the run, newest
	// first.
	Roles []string `json:"roles"`
}

// IsDegradationRow classifies one ledger row as degradation evidence:
//   - `operator-degraded` events (the Q40 preflight rows) with ok !== true.
//     Deliberately not filtered by role: the gate only ever writes the five
//     PREFLIGHT_ROLES, and the streak is meant to be cross-role — an
//     ok:true row is a passed probe (recovery), so it is productive.
//   - `loop-result` rows with status provider-degraded | operator-diverged.
//
// Everything else (audit rows, lessons-eval, loop-phase, healthy
// loop-results, ...) is productive: the first such row stops the walk.
func IsDegradationRow(row map[string]any) bool {
	if row == nil {
		return false
	}
	if ev, _ := row["event"].(string); ev == "operator-degraded" {
		ok, isBool := row["ok"].(bool)
		return !(isBool && ok)
	}
	if ev, _ := row["event"].(string); ev == "loop-result" {
		status, isStr := row["status"].(string)
		return isStr && degradedLoopResultStatuses[status]
	}
	return false
}

// parseTsMs parses an ISO ts the way Date.parse does for the shapes the
// ledger carries; ok=false mirrors NaN (unparseable).
func parseTsMs(ts string) (float64, bool) {
	if ts == "" {
		return 0, false
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02T15:04:05Z07:00",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, ts); err == nil {
			return float64(t.UnixMilli()), true
		}
	}
	// Date.parse also accepts non-ISO fallbacks; unparseable for the
	// ledger's ISO shapes here.
	return 0, false
}

// DegradationStreak is the pure aggregation over ledger rows in file
// order (oldest first, as appended). Walks newest-first counting the
// trailing degradation run; stops at the first productive row. O(rows)
// worst case, O(streak) typical.
func DegradationStreak(rows []map[string]any, threshold int) DegradationStreakResult {
	var latestTs, oldestTs *string
	var newestMs, oldestMs *float64 // parsed ts of the streak edges (nil = unparseable)
	var roles []string
	count := 0

	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if !IsDegradationRow(row) {
			break
		}
		count++
		if ts, isStr := row["ts"].(string); isStr {
			if latestTs == nil {
				v := ts
				latestTs = &v
			}
			v := ts
			oldestTs = &v
		}
		var parsed *float64
		if ts, isStr := row["ts"].(string); isStr {
			if ms, ok := parseTsMs(ts); ok {
				v := ms
				parsed = &v
			}
		}
		// The window is only meaningful when both streak edges parse: the
		// first degraded row fixes newestMs; every further row overwrites
		// oldestMs.
		if count == 1 {
			newestMs = parsed
		}
		oldestMs = parsed
		if ev, _ := row["event"].(string); ev == "operator-degraded" {
			if role, isStr := row["role"].(string); isStr && role != "" {
				seen := false
				for _, r := range roles {
					if r == role {
						seen = true
						break
					}
				}
				if !seen {
					roles = append(roles, role)
				}
			}
		}
	}

	var windowMs *float64
	if newestMs != nil && oldestMs != nil {
		w := *newestMs - *oldestMs
		windowMs = &w
	}

	breach := count > 0 && count >= threshold
	if roles == nil {
		roles = []string{}
	}
	return DegradationStreakResult{
		Count:     count,
		Threshold: threshold,
		Breach:    breach,
		LatestTs:  latestTs,
		OldestTs:  oldestTs,
		WindowMs:  windowMs,
		Roles:     roles,
	}
}

// ReadDegradationStreak is the read-side helper: parse the repo's
// orchestration events.jsonl (best-effort — corrupt lines skipped, absent
// file = no rows) and compute the trailing degradation streak for
// `devagent status --providers`. Real import per priority order: the events
// reader is internal/lessons.ReadEvents.
func ReadDegradationStreak(repoPath string, threshold int) DegradationStreakResult {
	return DegradationStreak(lessons.ReadEvents(repoPath), threshold)
}
