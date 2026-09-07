// Contract tests for the Orca workspace helpers, ported from
// test/cleanup.test.ts (describe('orca workspace integration')). The orca
// CLI is seamed — no real binary runs.
package integrations

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/FreePeak/devagent/internal/spawn"
)

// orcaPsPayload is the recorded `orca worktree ps --json` output from the
// vitest fixture.
var orcaPsPayload = map[string]any{
	"ok": true,
	"result": map[string]any{
		"worktrees": []any{
			map[string]any{"id": "repo-1::/Users/me/orca/other", "path": "/Users/me/orca/other"},
			map[string]any{"id": "repo-2::/Users/me/orca/hackathon-c3", "path": "/Users/me/orca/hackathon-c3/"},
		},
	},
}

func orcaPsJSON(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(orcaPsPayload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestMatchOrcaWorktreeMatchesSlashNormalized(t *testing.T) {
	// Ported: matches a repoPath against orca worktree ps output
	// (slash-normalized).
	if got := MatchOrcaWorktree(orcaPsPayload, "/Users/me/orca/hackathon-c3"); got != "repo-2::/Users/me/orca/hackathon-c3" {
		t.Errorf("got %q", got)
	}
	if got := MatchOrcaWorktree(orcaPsPayload, "/Users/me/orca/nope"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestMatchOrcaWorktreeMalformedOutput(t *testing.T) {
	// Ported: returns null on malformed ps output.
	if got := MatchOrcaWorktree(nil, "/x"); got != "" {
		t.Errorf("nil: got %q", got)
	}
	if got := MatchOrcaWorktree(map[string]any{"result": map[string]any{}}, "/x"); got != "" {
		t.Errorf("empty result: got %q", got)
	}
	if got := MatchOrcaWorktree("garbage", "/x"); got != "" {
		t.Errorf("garbage: got %q", got)
	}
}

func TestFindOrcaWorktreeByPathResolvesThroughPS(t *testing.T) {
	// Ported: findOrcaWorktreeByPath resolves through a successful ps call.
	runner := func(cmd string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: orcaPsJSON(t)}
	}
	if got := FindOrcaWorktreeByPath("/Users/me/orca/hackathon-c3", runner); got != "repo-2::/Users/me/orca/hackathon-c3" {
		t.Errorf("got %q", got)
	}
}

func TestFindOrcaWorktreeByPathDegradesToEmpty(t *testing.T) {
	// Ported: degrades to null when orca is missing, failing, timing out,
	// or emits garbage.
	fail := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 127}
	}
	if got := FindOrcaWorktreeByPath("/x", fail); got != "" {
		t.Errorf("exit 127: got %q", got)
	}

	slow := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: -1, TimedOut: true}
	}
	if got := FindOrcaWorktreeByPath("/x", slow); got != "" {
		t.Errorf("timed out: got %q", got)
	}

	// Noise prefix + invalid JSON (the TS extractJson would slice from the
	// first `{` and fail to parse "…{not json").
	noise := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: "SecCodeCheckValidity blah\n{not json"}
	}
	if got := FindOrcaWorktreeByPath("/x", noise); got != "" {
		t.Errorf("garbage: got %q", got)
	}

	boom := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: -1}
	}
	if got := FindOrcaWorktreeByPath("/x", boom); got != "" {
		t.Errorf("crash: got %q", got)
	}
}

func TestDropOrcaWorkspaceReportsWithoutThrowing(t *testing.T) {
	// Ported: dropOrcaWorkspace reports success/failure without throwing.
	ok := func(string, []string, spawn.Options) spawn.Result { return spawn.Result{ExitCode: 0} }
	if !DropOrcaWorkspace("repo-2::/p", "/p", ok) {
		t.Error("expected success")
	}

	fail := func(string, []string, spawn.Options) spawn.Result { return spawn.Result{ExitCode: 1} }
	if DropOrcaWorkspace("repo-2::/p", "/p", fail) {
		t.Error("expected failure")
	}
}

func TestCreateOrcaWorktreeParsesResultPaths(t *testing.T) {
	ok := func(cmd string, args []string, opts spawn.Options) spawn.Result {
		if !reflect.DeepEqual(args, []string{"worktree", "create", "--name", "slot-1", "--repo", "path:/repo", "--json"}) {
			t.Errorf("args = %#v", args)
		}
		return spawn.Result{ExitCode: 0, Stdout: `{"result":{"path":"/tmp/wt/slot-1"}}`}
	}
	if got := CreateOrcaWorktree("/repo", "slot-1", ok); got != "/tmp/wt/slot-1" {
		t.Errorf("got %q", got)
	}

	nested := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: `{"result":{"worktree":{"path":"/tmp/wt/nested"}}}`}
	}
	if got := CreateOrcaWorktree("/repo", "slot-2", nested); got != "/tmp/wt/nested" {
		t.Errorf("got %q", got)
	}

	bad := func(string, []string, spawn.Options) spawn.Result { return spawn.Result{ExitCode: 1} }
	if got := CreateOrcaWorktree("/repo", "slot-3", bad); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestListOrcaWorktreesPrefixMatches(t *testing.T) {
	ps := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: orcaPsJSON(t)}
	}
	// No path in the fixture starts with /repo — expect an empty list.
	got := ListOrcaWorktrees("/repo", ps)
	if len(got) != 0 {
		t.Errorf("got %#v", got)
	}

	mine := func(string, []string, spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: `{"result":{"worktrees":[{"path":"/repo/.devagent-worktrees/a"},{"path":"/elsewhere/b"}]}}`}
	}
	got = ListOrcaWorktrees("/repo", mine)
	if !reflect.DeepEqual(got, []string{"/repo/.devagent-worktrees/a"}) {
		t.Errorf("got %#v", got)
	}
}

func TestEnsureOrcaRepoBestEffort(t *testing.T) {
	ok := func(string, []string, spawn.Options) spawn.Result { return spawn.Result{ExitCode: 0} }
	if !EnsureOrcaRepo("/repo", ok) {
		t.Error("expected true on success")
	}
	fail := func(string, []string, spawn.Options) spawn.Result { return spawn.Result{ExitCode: 3} }
	if EnsureOrcaRepo("/repo", fail) {
		t.Error("expected false on failure")
	}
}
