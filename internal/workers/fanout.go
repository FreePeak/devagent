// Go port of src/workers/fanout.ts (FR-GO-05): fan-out mode (FR-IMPL-03) —
// run the same plan through multiple workers in parallel isolated
// worktrees; score each leg with the repo's own test suite; exactly one
// leg wins (tests decide, claude-code breaks ties).
//
// The leg-spawning orchestration (prompt building, worktree creation) is
// owned by the orchestrator/planner/git ports (FR-GO-07/08); the ranking
// policy — the piece this issue pins — lives here as a pure function over
// leg results so the winner selection cannot drift between runtimes.
package workers

import (
	"regexp"
)

// FanoutLeg mirrors TS FanoutLeg.
type FanoutLeg struct {
	Worker       string
	WorktreePath string
	Branch       string
	Ok           bool
	// TestsPassed: nil = leg could not be scored (no worktree/scorer).
	TestsPassed *bool
	// Flaky: true when tests failed once and passed on the single flaky
	// rerun.
	Flaky      bool
	DurationMs int64
	// FailureClass: the leg's terminal failure mode when it did not
	// produce usable work (PRD:775 / Q24 taxonomy mirror).
	FailureClass string // "" = none; "test-gate" | "timeout" | "worker-error"
}

// FanoutScoreLeg mirrors the TS FanoutOptions.scoreLeg hook: score one
// leg's worktree with the repo's test suite. nil = could not score.
type FanoutScoreLeg func(worktreePath string, timeoutMs int) *bool

// RankFanoutLeg returns the winner ranking score for one leg (TS `rank`
// closure): clean pass outranks a flaky rescue — a pass that needed a
// rerun earns no more trust than a leg we could not score at all.
//
//	score: pass(clean)=2, unscored/flaky=1, fail(clean)=0; ×10
//	tie-break: claude-code +1
func RankFanoutLeg(l FanoutLeg) int {
	class := 1
	if l.TestsPassed != nil && *l.TestsPassed && !l.Flaky {
		class = 2
	} else if l.TestsPassed != nil && !*l.TestsPassed && !l.Flaky {
		class = 0
	}
	tie := 0
	if l.Worker == "claude-code" {
		tie = 1
	}
	return class*10 + tie
}

// SelectFanoutWinner returns the winning usable leg (TS
// usable.reduce((best, leg) => rank(leg) > rank(best) ? leg : best)).
// usable must be non-empty (the TS caller filters legs by ok first).
func SelectFanoutWinner(usable []FanoutLeg) FanoutLeg {
	best := usable[0]
	bestRank := RankFanoutLeg(best)
	for _, leg := range usable[1:] {
		if r := RankFanoutLeg(leg); r > bestRank {
			best = leg
			bestRank = r
		}
	}
	return best
}

// FilterUsableLegs mirrors TS `legs.filter((l) => l.ok)`.
func FilterUsableLegs(legs []FanoutLeg) []FanoutLeg {
	var usable []FanoutLeg
	for _, l := range legs {
		if l.Ok {
			usable = append(usable, l)
		}
	}
	return usable
}

// fanoutLegFailureClass mirrors the TS failure-class derivation: the leg's
// terminal failure mode when it did not produce usable work.
//
//	failureClass = ok
//	  ? (testsPassed === false ? 'test-gate' : undefined)
//	  : (timedOut ? 'timeout' : 'worker-error')
func fanoutLegFailureClass(ok bool, timedOut bool, testsPassed *bool) string {
	if ok {
		if testsPassed != nil && !*testsPassed {
			return "test-gate"
		}
		return ""
	}
	if timedOut {
		return "timeout"
	}
	return "worker-error"
}

// ScoreFanoutLeg applies the flaky guard (PRD section 17 Phase 4): one
// rerun before condemning a leg — nondeterministic suites must not discard
// otherwise good work. Returns testsPassed plus the flaky flag.
//
//	testsPassed = ok && worktree && scorer ? score() : nil
//	if testsPassed === false && worktree && scorer:
//	  testsPassed = score(); flaky = testsPassed === true
func ScoreFanoutLeg(ok bool, worktreePath string, score FanoutScoreLeg) (*bool, bool) {
	if !ok || worktreePath == "" || score == nil {
		return nil, false
	}
	testsPassed := score(worktreePath, 0)
	flaky := false
	if testsPassed != nil && !*testsPassed {
		testsPassed = score(worktreePath, 0)
		if testsPassed != nil && *testsPassed {
			flaky = true
		}
	}
	return testsPassed, flaky
}

// workerSuffixRe mirrors TS name.replace(/[^A-Za-z0-9-_]/g, ”).
var workerSuffixRe = regexp.MustCompile(`[^A-Za-z0-9\-_]`)

// WorkerSuffix mirrors TS workerSuffix: filesystem-safe leg id suffix.
func WorkerSuffix(name string, index int) string {
	s := workerSuffixRe.ReplaceAllString(name, "")
	if s == "" {
		return "leg" + itoa(index)
	}
	return s
}
