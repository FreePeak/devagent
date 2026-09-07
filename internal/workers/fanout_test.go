// Go port of test/fanout.test.ts: winner ranking policy — one flaky
// rerun, clean pass outranks a flaky rescue, null when every leg fails.

package workers

import "testing"

func boolPtr(b bool) *bool { return &b }

// fakeWorkerResult builds a success-shaped WorkerResult with the given
// exit code (the TS workerMock(exitCode)).
func fakeWorkerResult(exitCode int) WorkerResult {
	return WorkerResult{ExitCode: exitCode, DurationMs: 1000}
}

// runFanoutLegs mirrors the per-leg portion of the TS runFanout: spawn
// (via injected runner), score with the flaky guard, derive the failure
// class, then rank across usable legs. Worktree creation failure falls
// back to the repo path (the TS catch branch).
type fanoutLegFixture struct {
	worker   string
	exitCode int
	scores   []*bool // per scoreLeg call for this leg
	timeouts []bool
}

func runFanoutLegs(fixtures []fanoutLegFixture) ([]FanoutLeg, []int) {
	legs := make([]FanoutLeg, 0, len(fixtures))
	callCounts := make([]int, len(fixtures))
	for i, f := range fixtures {
		worktree := "/tmp/leg-" + f.worker
		result := fakeWorkerResult(f.exitCode)
		ok := !result.TimedOut && result.ExitCode == 0
		timedOut := len(f.timeouts) > 0 && f.timeouts[0]
		var score FanoutScoreLeg
		if f.scores != nil {
			score = func(string, int) *bool {
				callCounts[i]++
				return f.scores[callCounts[i]-1]
			}
		}
		testsPassed, flaky := ScoreFanoutLeg(ok, worktree, score)
		legs = append(legs, FanoutLeg{
			Worker:       f.worker,
			WorktreePath: worktree,
			Ok:           ok,
			TestsPassed:  testsPassed,
			Flaky:        flaky,
			DurationMs:   1000,
			FailureClass: fanoutLegFailureClass(ok, timedOut, testsPassed),
		})
	}
	return legs, callCounts
}

func TestFanout_PrefersTheLegWhoseTestsPass(t *testing.T) {
	// claude leg passes; opencode leg fails once then passes on the rerun.
	legs, counts := runFanoutLegs([]fanoutLegFixture{
		{worker: "claude-code", exitCode: 0, scores: []*bool{boolPtr(true)}},
		{worker: "opencode", exitCode: 0, scores: []*bool{boolPtr(false), boolPtr(true)}},
	})
	usable := FilterUsableLegs(legs)
	if len(usable) != 2 {
		t.Fatalf("usable = %+v", usable)
	}
	winner := SelectFanoutWinner(usable)
	if winner.Worker != "claude-code" {
		t.Fatalf("winner = %+v", winner)
	}
	// opencode leg gets one flaky rerun (2 calls) + claude's 1.
	if counts[1] != 2 {
		t.Fatalf("opencode score calls = %d, want 2", counts[1])
	}
}

func TestFanout_ReturnsNilWhenEveryLegFails(t *testing.T) {
	// Worktree failure forces the cwd fallback; exit 1 keeps ok=false.
	legs, _ := runFanoutLegs([]fanoutLegFixture{
		{worker: "claude-code", exitCode: 1},
	})
	if usable := FilterUsableLegs(legs); len(usable) != 0 {
		t.Fatalf("expected no usable legs, got %+v", usable)
	}
	// The TS runFanout returns null — no winner selectable.
	if legs[0].FailureClass != "worker-error" {
		t.Fatalf("failureClass = %q, want worker-error", legs[0].FailureClass)
	}
}

func TestFanout_FallsBackToAnyUsableLegWhenScoringUnavailable(t *testing.T) {
	// No scoreLeg: claude fails (exit 1), opencode passes.
	legs, counts := runFanoutLegs([]fanoutLegFixture{
		{worker: "claude-code", exitCode: 1},
		{worker: "opencode", exitCode: 0},
	})
	winner := SelectFanoutWinner(FilterUsableLegs(legs))
	if winner.Worker != "opencode" {
		t.Fatalf("winner = %+v", winner)
	}
	if counts[1] != 0 {
		t.Fatalf("unscored leg must not call scoreLeg: %d", counts[1])
	}
	if legs[0].TestsPassed != nil {
		t.Fatalf("unscored leg testsPassed must be nil, got %v", *legs[0].TestsPassed)
	}
}

func TestFanout_RescuesFlakyLeg(t *testing.T) {
	// Tests fail once then pass on rerun — marked flaky, still wins.
	legs, counts := runFanoutLegs([]fanoutLegFixture{
		{worker: "opencode", exitCode: 0, scores: []*bool{boolPtr(false), boolPtr(true)}},
	})
	winner := SelectFanoutWinner(FilterUsableLegs(legs))
	if winner.Worker != "opencode" {
		t.Fatalf("winner = %+v", winner)
	}
	if !winner.Flaky {
		t.Fatalf("winner must be flagged flaky: %+v", winner)
	}
	if counts[0] != 2 {
		t.Fatalf("score calls = %d, want 2", counts[0])
	}
}

func TestFanout_PrefersCleanPassOverFlakyPass(t *testing.T) {
	// Claude passes cleanly; opencode fails then passes on the rerun.
	// The clean pass must outrank the flaky rescue.
	legs, counts := runFanoutLegs([]fanoutLegFixture{
		{worker: "claude-code", exitCode: 0, scores: []*bool{boolPtr(true)}},
		{worker: "opencode", exitCode: 0, scores: []*bool{boolPtr(false), boolPtr(true)}},
	})
	winner := SelectFanoutWinner(FilterUsableLegs(legs))
	if winner.Worker != "claude-code" {
		t.Fatalf("clean pass must outrank flaky: %+v", winner)
	}
	if winner.Flaky {
		t.Fatalf("winner must not be flaky: %+v", winner)
	}
	if counts[1] != 2 {
		t.Fatalf("opencode score calls = %d, want 2", counts[1])
	}
}

func TestFanout_KeepsPersistentlyFailingLegFailed(t *testing.T) {
	// A sole usable leg still wins (existing semantics) but stays unflagged
	// and ranked as failed — the guard only rescues pass-after-rerun legs.
	legs, counts := runFanoutLegs([]fanoutLegFixture{
		{worker: "opencode", exitCode: 0, scores: []*bool{boolPtr(false), boolPtr(false)}},
	})
	winner := SelectFanoutWinner(FilterUsableLegs(legs))
	if winner.Worker != "opencode" {
		t.Fatalf("sole usable leg still wins: %+v", winner)
	}
	if winner.TestsPassed == nil || *winner.TestsPassed {
		t.Fatalf("testsPassed must stay false: %+v", winner)
	}
	if winner.Flaky {
		t.Fatalf("flaky must stay false: %+v", winner)
	}
	if counts[0] != 2 {
		t.Fatalf("exactly one rerun allowed: %d calls", counts[0])
	}
}

func TestFanoutWorkerSuffix(t *testing.T) {
	if got := WorkerSuffix("claude-code", 0); got != "claude-code" {
		t.Fatalf("suffix = %q", got)
	}
	// non-alnum characters stripped (TS replace semantics)
	if got := WorkerSuffix("we!rd@name", 3); got != "werdname" {
		t.Fatalf("suffix = %q, want werdname", got)
	}
	if got := WorkerSuffix("!!!", 7); got != "leg7" {
		t.Fatalf("empty suffix must fall back to leg7, got %q", got)
	}
}
