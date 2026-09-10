package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The FR-VAL-05 (issue #293) CLI surface: `eval score` writes the rows, `eval
// drift` reads the ratchet back, and `ledger --clusters` carries the same
// warning the self-build driver captures into its research prompts.

// evalLedgerFixture stages a repo whose orchestration ledger holds two feat
// scores — a strong one, then a shallow one whose test evidence collapsed. The
// rows are hand-written JSONL (not the Go writer) so the wire schema itself is
// what the test pins.
func evalLedgerFixture(t *testing.T, rows ...string) string {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, ".devagent", "runs", "orchestration")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

const (
	evalRowStrong  = `{"ts":"2026-09-10T00:00:00.000Z","kind":"event","taskId":"TASK-good-1","attempt":1,"event":"eval-score","pr":501,"goalClass":"feat","rubricVersion":"1","rubricDigest":"aaaaaaaaaaaa","judge":"omp@onegw/free","criteria":[{"criterion":"requirement-coverage","score":28,"max":30},{"criterion":"test-evidence","score":24,"max":25},{"criterion":"prd-currency","score":15,"max":15}],"total":67,"max":70,"notes":"strong"}`
	evalRowShallow = `{"ts":"2026-09-11T00:00:00.000Z","kind":"event","taskId":"TASK-shal-2","attempt":1,"event":"eval-score","pr":502,"goalClass":"feat","rubricVersion":"1","rubricDigest":"aaaaaaaaaaaa","judge":"omp@onegw/free","criteria":[{"criterion":"requirement-coverage","score":26,"max":30},{"criterion":"test-evidence","score":2,"max":25},{"criterion":"prd-currency","score":0,"max":15}],"total":28,"max":70,"notes":"no test evidence"}`
)

// TestEvalCommandWiring guards the surface itself: both subcommands resolve and
// every flag the gate arithmetic depends on is typed. An untyped numeric flag
// lands as a string, the action reads 0, and the nightly stays alert-only
// forever without ever saying so.
func TestEvalCommandWiring(t *testing.T) {
	root := NewRoot()
	if got := findImplemented(root, "eval score").Name(); got != "score" {
		t.Fatalf("eval score not wired (resolved %q)", got)
	}
	if got := findImplemented(root, "eval drift").Name(); got != "drift" {
		t.Fatalf("eval drift not wired (resolved %q)", got)
	}
	if _, stubbed := notPortedIssue["eval score"]; stubbed {
		t.Error("eval score is still an exit-3 stub")
	}
	for _, want := range []struct {
		cmd, flag, kind string
	}{
		{"eval score", "pr", "int"}, {"eval score", "last", "int"}, {"eval score", "timeout", "int"},
		{"eval score", "json", "bool"}, {"eval score", "rubric", "string"},
		{"eval drift", "window", "int"}, {"eval drift", "max-drop", "int"}, {"eval drift", "top", "int"},
	} {
		f := findImplemented(root, want.cmd).Flags().Lookup(want.flag)
		if f == nil {
			t.Errorf("devagent %s has no --%s", want.cmd, want.flag)
			continue
		}
		if got := f.Value.Type(); got != want.kind {
			t.Errorf("devagent %s --%s is %s, want %s", want.cmd, want.flag, got, want.kind)
		}
	}
}

// TestEvalScoreRefusesEmptyRun: a run that was asked to score nothing must not
// read as success, or the nightly passes by measuring nothing.
func TestEvalScoreRefusesEmptyRun(t *testing.T) {
	root := NewRoot()
	root.SetArgs([]string{"eval", "score", "--repo", t.TempDir()})
	root.SetOut(os.Stderr)
	err := root.Execute()
	if err == nil {
		t.Fatal("`eval score` with no --pr/--last exited 0")
	}
	if !strings.Contains(err.Error(), "nothing to score") {
		t.Fatalf("wrong refusal: %v", err)
	}
}

func TestEvalDriftReportsAndGates(t *testing.T) {
	repo := evalLedgerFixture(t, evalRowStrong, evalRowShallow)
	t.Cleanup(func() { commandExitCode = nil })

	commandExitCode = nil
	out := runCmdCapture(t, "eval", "drift", "--repo", repo)
	// The offending criterion is the biggest point loss, i.e. the share of the
	// reported drop it explains: test evidence fell 24 → 2 (22 points), which
	// beats prd currency's 15 → 0 (15). A rate-based rule would have named
	// prd-currency instead, and the drop is reported in points, not rates.
	if !strings.Contains(out, "quality drift (top 1 of 1)") {
		t.Fatalf("drift view missing the violation section:\n%s", out)
	}
	if !strings.Contains(out, "PR #502") || !strings.Contains(out, "test-evidence 2/25") {
		t.Fatalf("drift line does not name the regressed artifact and its criterion:\n%s", out)
	}
	if commandExitCode != nil {
		t.Fatalf("the default run is alert-only, got exit %d", *commandExitCode)
	}

	commandExitCode = nil
	_ = runCmdCapture(t, "eval", "drift", "--repo", repo, "--max-drop", "5")
	if commandExitCode == nil || *commandExitCode != 1 {
		t.Fatalf("--max-drop 5 against a 39-point drop must fail the run, got %v", commandExitCode)
	}

	// One predecessor is a baseline: window 1 still compares against PR 501.
	commandExitCode = nil
	if out := runCmdCapture(t, "eval", "drift", "--repo", repo, "--window", "1"); !strings.Contains(out, "quality drift") {
		t.Fatalf("window 1 lost the nearest-predecessor baseline:\n%s", out)
	}

	// A lone score has nothing to be below: honest note, never a violation.
	solo := evalLedgerFixture(t, evalRowShallow)
	if out := runCmdCapture(t, "eval", "drift", "--repo", solo); !strings.Contains(out, "insufficient baseline") {
		t.Fatalf("single-row ledger should report an insufficient baseline:\n%s", out)
	}
}

// TestLedgerClustersCarriesDrift is the loop-side half of the ratchet:
// loopdriver.failureClusters() shells out to exactly this command and echoes the
// output into the research and PO prompts, so a drift warning that renders here
// reaches the next pick through the plumbing that already exists.
func TestLedgerClustersCarriesDrift(t *testing.T) {
	repo := evalLedgerFixture(t, evalRowStrong, evalRowShallow)
	out := runCmdCapture(t, "ledger", "--clusters", "--repo", repo)
	if !strings.Contains(out, "quality drift") || !strings.Contains(out, "PR #502") {
		t.Fatalf("ledger --clusters does not carry the ratchet verdict:\n%s", out)
	}
	jsonOut := runCmdCapture(t, "ledger", "--clusters", "--json", "--repo", repo)
	if !strings.Contains(jsonOut, `"qualityDrift"`) || !strings.Contains(jsonOut, `"drop": 39`) {
		t.Fatalf("--json payload missing the drift block:\n%s", jsonOut)
	}
}
