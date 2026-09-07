// Contract tests for the gh/git publisher, ported from
// test/integrations.test.ts (describe('github')). The CLI is seamed — no
// real gh/git runs.

package integrations

import (
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/spawn"
)

// fakeRunner replays scripted results in call order and records every
// invocation (the execFile mock port).
type fakeRunner struct {
	t      *testing.T
	script []spawn.Result
	calls  []struct {
		cmd  string
		args []string
		opts spawn.Options
	}
}

func (f *fakeRunner) run(cmd string, args []string, opts spawn.Options) spawn.Result {
	f.calls = append(f.calls, struct {
		cmd  string
		args []string
		opts spawn.Options
	}{cmd, args, opts})
	if len(f.script) == 0 {
		f.t.Fatalf("unexpected CLI call: %s %v", cmd, args)
	}
	next := f.script[0]
	f.script = f.script[1:]
	return next
}

func okResult(stdout string) spawn.Result {
	return spawn.Result{ExitCode: 0, Stdout: stdout}
}

func failResult(stderr string) spawn.Result {
	return spawn.Result{ExitCode: 1, Stderr: stderr}
}

func TestCreatePrShellsOutWithCorrectArgs(t *testing.T) {
	// Ported: createPr shells out to gh pr create with correct args and
	// returns URL.
	fr := &fakeRunner{t: t, script: []spawn.Result{okResult("Creating PR...\nhttps://github.com/o/r/pull/7\n")}}
	url, err := CreatePr(CreatePrOptions{RepoPath: "/repo", Branch: "devagent/ENG-9", Title: "PR title", Body: "PR body"},
		GitHubOptions{Runner: fr.run, Sleep: noopSleep})
	if err != nil {
		t.Fatalf("CreatePr: %v", err)
	}
	if url != "https://github.com/o/r/pull/7" {
		t.Errorf("url = %q", url)
	}
	if len(fr.calls) != 1 {
		t.Fatalf("calls = %d", len(fr.calls))
	}
	call := fr.calls[0]
	if call.cmd != "gh" {
		t.Errorf("cmd = %q", call.cmd)
	}
	if call.opts.Dir != "/repo" {
		t.Errorf("cwd = %q", call.opts.Dir)
	}
	args := call.args
	if args[0] != "pr" || args[1] != "create" {
		t.Errorf("args head = %#v", args)
	}
	want := map[string]string{"-t": "PR title", "-b": "PR body", "-H": "devagent/ENG-9"}
	for flag, val := range want {
		idx := indexOf(args, flag)
		if idx < 0 || idx+1 >= len(args) || args[idx+1] != val {
			t.Errorf("args[%s] missing or wrong: %#v", flag, args)
		}
	}
	if indexOf(args, "-B") >= 0 {
		t.Errorf("args must not contain -B without baseBranch: %#v", args)
	}
}

func TestCreatePrIncludesBaseBranch(t *testing.T) {
	// Ported: createPr includes -B baseBranch when provided.
	fr := &fakeRunner{t: t, script: []spawn.Result{okResult("https://github.com/o/r/pull/8\n")}}
	_, err := CreatePr(CreatePrOptions{RepoPath: "/repo", Branch: "feature", Title: "t", Body: "b", BaseBranch: "develop"},
		GitHubOptions{Runner: fr.run, Sleep: noopSleep})
	if err != nil {
		t.Fatal(err)
	}
	args := fr.calls[0].args
	if idx := indexOf(args, "-B"); idx < 0 || args[idx+1] != "develop" {
		t.Errorf("args = %#v", args)
	}
}

func TestCreatePrThrowsDescriptiveErrorWithStderr(t *testing.T) {
	// Ported: createPr throws descriptive error including stderr on
	// failure (simulated execFile non-zero exit with collected stderr).
	fr := &fakeRunner{t: t, script: []spawn.Result{failResult("no remote configured")}}
	_, err := CreatePr(CreatePrOptions{RepoPath: "/repo", Branch: "b", Title: "t", Body: "b"},
		GitHubOptions{Runner: fr.run, Sleep: noopSleep})
	// /gh pr create failed.*no remote configured/s — the stderr detail
	// rides the re-described error.
	if err == nil || !strings.Contains(err.Error(), "gh pr create failed") || !strings.Contains(err.Error(), "no remote configured") {
		t.Errorf("err = %v", err)
	}
}

func TestCreatePrNoURLError(t *testing.T) {
	// A successful run printing no URL yields the dedicated no-PR-URL
	// error (with stderr noted when present), NOT re-described.
	fr := &fakeRunner{t: t, script: []spawn.Result{okResult("\n\n")}}
	_, err := CreatePr(CreatePrOptions{RepoPath: "/repo", Branch: "b", Title: "t", Body: "b"},
		GitHubOptions{Runner: fr.run, Sleep: noopSleep})
	if err == nil || !strings.HasPrefix(err.Error(), "gh pr create produced no PR URL") {
		t.Errorf("err = %v", err)
	}

	fr2 := &fakeRunner{t: t, script: []spawn.Result{okResult(""), okResult("")}}
	// cover stderr-carrying no-URL branch
	fr2.script = []spawn.Result{{ExitCode: 0, Stdout: "", Stderr: "hint: nothing happened"}}
	_, err = CreatePr(CreatePrOptions{RepoPath: "/repo", Branch: "b", Title: "t", Body: "b"},
		GitHubOptions{Runner: fr2.run, Sleep: noopSleep})
	if err == nil || err.Error() != "gh pr create produced no PR URL (stderr: hint: nothing happened)" {
		t.Errorf("err = %v", err)
	}
}

func TestBranchExists(t *testing.T) {
	// Ported: branchExists resolves false when rev-parse fails / true when
	// rev-parse succeeds.
	fr := &fakeRunner{t: t, script: []spawn.Result{failResult("fatal: Needed a single revision")}}
	if BranchExists("/repo", "missing", GitHubOptions{Runner: fr.run}) {
		t.Error("missing branch should not exist")
	}
	if got := fr.calls[0].args; got[0] != "rev-parse" {
		t.Errorf("args = %#v", got)
	}

	fr2 := &fakeRunner{t: t, script: []spawn.Result{okResult("")}}
	if !BranchExists("/repo", "main", GitHubOptions{Runner: fr2.run}) {
		t.Error("main should exist")
	}
}

func TestWithRateLimitRetryWaitsSixtySecondsOnRateLimitText(t *testing.T) {
	// Rate-limit semantics: a rate-limit-shaped failure retries ONCE after
	// a fixed 60s pause; the second failure propagates unchanged.
	var sleeps []int
	calls := 0
	_, err := WithRateLimitRetry(func() (string, error) {
		calls++
		return "", &CliError{Message: "gh: API rate limit exceeded"}
	}, func(ms int) { sleeps = append(sleeps, ms) })
	if err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (initial + 1 retry)", calls)
	}
	if len(sleeps) != 1 || sleeps[0] != 60000 {
		t.Errorf("sleeps = %#v, want [60000]", sleeps)
	}

	// Secondary-rate-limit phrasing also matches.
	calls = 0
	_, _ = WithRateLimitRetry(func() (string, error) {
		calls++
		return "", &CliError{Message: "secondary rate limit triggered"}
	}, func(ms int) { sleeps = append(sleeps, ms) })
	if calls != 2 {
		t.Errorf("secondary: calls = %d", calls)
	}
}

func TestWithRateLimitRetryPassesNonRateLimitErrorsThrough(t *testing.T) {
	// Non-rate-limit failures propagate immediately with no wait.
	calls := 0
	_, err := WithRateLimitRetry(func() (string, error) {
		calls++
		return "", &CliError{Message: "remote: permission denied"}
	}, func(ms int) { t.Errorf("unexpected sleep %d", ms) })
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d", calls)
	}
}

func TestPushBranchUsesExplicitRefspec(t *testing.T) {
	fr := &fakeRunner{t: t, script: []spawn.Result{okResult("")}}
	if err := PushBranch("/repo", "devagent/E-1", GitHubOptions{Runner: fr.run, Sleep: noopSleep}); err != nil {
		t.Fatalf("PushBranch: %v", err)
	}
	call := fr.calls[0]
	if call.cmd != "git" {
		t.Errorf("cmd = %q", call.cmd)
	}
	want := []string{"push", "-u", "origin", "devagent/E-1:devagent/E-1"}
	if len(call.args) != len(want) {
		t.Fatalf("args = %#v", call.args)
	}
	for i, w := range want {
		if call.args[i] != w {
			t.Errorf("args[%d] = %q, want %q", i, call.args[i], w)
		}
	}

	// Push failure surfaces as `git push <branch> failed: <stderr>`.
	fr2 := &fakeRunner{t: t, script: []spawn.Result{failResult("rejected non-fast-forward")}}
	err := PushBranch("/repo", "b", GitHubOptions{Runner: fr2.run, Sleep: noopSleep})
	if err == nil || !strings.Contains(err.Error(), "git push b failed: rejected non-fast-forward") {
		t.Errorf("err = %v", err)
	}
}

func TestAutoMergePrStrategies(t *testing.T) {
	fr := &fakeRunner{t: t, script: []spawn.Result{okResult("Squashing pull #7\n")}}
	out, err := AutoMergePr("/repo", "7", "squash", GitHubOptions{Runner: fr.run, Sleep: noopSleep})
	if err != nil || out != "Squashing pull #7" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	if got := fr.calls[0].args; !strings.HasPrefix(strings.Join(got, " "), "pr merge 7 --auto --squash") {
		t.Errorf("args = %#v", got)
	}

	fr2 := &fakeRunner{t: t, script: []spawn.Result{okResult("Merged\n")}}
	if _, err := AutoMergePr("/repo", "7", "merge", GitHubOptions{Runner: fr2.run, Sleep: noopSleep}); err != nil {
		t.Fatal(err)
	}
	if !contains(fr2.calls[0].args, "--merge") {
		t.Errorf("args = %#v", fr2.calls[0].args)
	}
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

func contains(haystack []string, needle string) bool {
	return indexOf(haystack, needle) >= 0
}
