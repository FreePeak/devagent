package loopdriver

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// soakTrackerFakes installs a stateful gh fake on top of installFakes.
// `issue list` serves the open-issue set tracked by the counter file
// ($GH_PICK_FILE) — a list that actually contains multiple issues so the
// driver's priority-rank pick is exercised for real — and `issue close`
// advances the counter. When the tracker is exhausted the fake re-serves
// the first list: a lagging tracker, the exact situation the Q27
// already-shipped guard exists for — the 4th pick re-proposes a shipped
// goal and must be rejected.
func soakTrackerFakes(t *testing.T, repo string) {
	t.Helper()
	pick := filepath.Join(repo, ".gh-pick-counter")
	if err := os.WriteFile(pick, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_PICK_FILE", pick)
	// #301 carries priority:P1 and outranks the unlabeled #302/#303 even
	// though all three sit in one list — the soak pins that ranking.
	t.Setenv("GH_ISSUE_1", `[{"number":301,"title":"Fix the loop starvation gate","labels":[{"name":"selfbuild"},{"name":"priority:P1"}]},{"number":302,"title":"Port the ledger cluster report","labels":[{"name":"selfbuild"}]},{"number":303,"title":"Harden the task dispatch wall","labels":[{"name":"selfbuild"}]}]`)
	t.Setenv("GH_ISSUE_2", `[{"number":302,"title":"Port the ledger cluster report","labels":[{"name":"selfbuild"}]},{"number":303,"title":"Harden the task dispatch wall","labels":[{"name":"selfbuild"}]}]`)
	t.Setenv("GH_ISSUE_3", `[{"number":303,"title":"Harden the task dispatch wall","labels":[{"name":"selfbuild"}]}]`)
	dir := fakeBinDir(t, map[string]string{
		"gh-fake": `#!/bin/sh
echo "gh $*" >> "${DEVAGENT_LOG:?}"
case "$1 $2" in
  "issue list")
    idx=$(cat "${GH_PICK_FILE:?}" 2>/dev/null || echo 1)
    case "$idx" in
      2) printf '%s' "$GH_ISSUE_2" ;;
      3) printf '%s' "$GH_ISSUE_3" ;;
      *) printf '%s' "$GH_ISSUE_1" ;;
    esac
    ;;
  "issue close")
    idx=$(cat "${GH_PICK_FILE:?}" 2>/dev/null || echo 1)
    echo $((idx + 1)) > "${GH_PICK_FILE:?}"
    ;;
esac
exit 0
`,
	})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// TestGoldenSoak (FR-VAL-01, issue #289): the golden soak self-test — the
// full loop driver end-to-end (pick → research → issue-first goal → task →
// repo test gate → ledger) over a fixture repo with fake worker CLIs.
// Asserts three consecutive "ok" ledger rows, artifact creation under
// .selfbuild/, the already-shipped guard rejecting the 4th re-pick, and
// exit 0.
func TestGoldenSoak(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	soakTrackerFakes(t, repo)
	now, _ := frozenClock()
	var stdout bytes.Buffer
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.Stdout = &stdout
	})
	cfg.DryRun = false
	// Loops 1-3 ship the three tracker issues; loop 4 re-picks the first
	// (wrapped) issue and must be guard-skipped; loop 5 stops at the cap.
	cfg.MaxIterations = 5

	if rc := RunLoop(cfg); rc != 0 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-4.log"))
		t.Fatalf("rc = %d, want 0\nloop-4 log:\n%s", rc, logData)
	}

	// Three consecutive ok rows, then the guard's skipped row.
	rows := readLedger(t, repo)
	if len(rows) != 4 {
		t.Fatalf("ledger rows = %d, want 4 (ok ok ok skipped): %v", len(rows), rows)
	}
	for i, want := range []string{"ok", "ok", "ok", "skipped"} {
		if rows[i]["status"] != want {
			t.Fatalf("row %d status = %v, want %q", i+1, rows[i]["status"], want)
		}
		if rows[i]["loop"].(float64) != float64(i+1) {
			t.Fatalf("row %d loop = %v", i+1, rows[i]["loop"])
		}
	}
	// Pick order pinned: the P1-labeled #301 outranks the unlabeled #302/
	// #303 in the same list, then age order advances the tracker; the
	// wrapped 4th pick re-proposes the byte-identical shipped goal.
	for i, want := range []string{"#301", "#302", "#303", "#301"} {
		if !strings.Contains(rows[i]["goal"].(string), "issue "+want) {
			t.Fatalf("row %d goal = %q, want pick of %s", i+1, rows[i]["goal"], want)
		}
	}
	if rows[3]["goal"] != rows[0]["goal"] {
		t.Fatalf("4th pick goal = %q, want the already-shipped %q", rows[3]["goal"], rows[0]["goal"])
	}
	// Artifacts under .selfbuild/: the issue-goal file (PO slot) and the
	// phase-1 research extraction, one pair per iteration — including the
	// guard-skipped loop, which researches before the guard rejects.
	for n := 1; n <= 4; n++ {
		data, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "goals", fmt.Sprintf("loop-%d.md", n)))
		if err != nil || !strings.HasPrefix(string(data), "Goal:") {
			t.Fatalf("goal artifact loop-%d: %v (%q)", n, err, data)
		}
		if _, err := os.Stat(filepath.Join(repo, ".selfbuild", "research", fmt.Sprintf("loop-%d.md", n))); err != nil {
			t.Fatalf("research artifact missing for loop %d: %v", n, err)
		}
	}

	// loop-result events mirror the ledger sequence.
	events := readEvents(t, repo)
	var results []string
	for _, e := range events {
		if e["event"] == "loop-result" {
			results = append(results, e["status"].(string))
		}
	}
	if len(results) != 4 || results[0] != "ok" || results[1] != "ok" || results[2] != "ok" || results[3] != "skipped" {
		t.Fatalf("loop-result events = %v, want [ok ok ok skipped]", results)
	}
	// Phase breadcrumbs: shipped iterations chain preflight → issue →
	// task; the guard-skipped loop stops at the pick (no task dispatch).
	phases := map[float64][]string{}
	for _, e := range events {
		if e["event"] == "loop-phase" {
			phases[e["loop"].(float64)] = append(phases[e["loop"].(float64)], e["phase"].(string))
		}
	}
	for n, want := range map[int][]string{1: {"preflight", "issue", "task"}, 4: {"preflight", "issue"}} {
		got := phases[float64(n)]
		if len(got) != len(want) {
			t.Fatalf("loop %d phases = %v, want %v", n, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("loop %d phases = %v, want %v", n, got, want)
			}
		}
	}

	// The guard closed the re-picked issue too: 3 ship closes + 1 guard close.
	callLog, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(callLog), "gh issue close"); got != 4 {
		t.Fatalf("gh issue close calls = %d, want 4\n%s", got, callLog)
	}
	if !strings.Contains(stdout.String(), "[ok] loop 3 complete") {
		t.Fatalf("stdout missing log tail: %q", stdout.String())
	}
}

// TestGoldenSoakFailurePath is the soak's failure-path pin (issue #289):
// flipping the repo test-gate fake to fail must classify the iteration as
// failed-tests — the soak goes red if the driver ever misclassifies a red
// gate as a green ship.
func TestGoldenSoakFailurePath(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	soakTrackerFakes(t, repo)
	// Flip the test-gate fake (shadows installFakes' npm on PATH).
	dir := fakeBinDir(t, map[string]string{
		"npm": "#!/bin/sh\necho \"FAIL: soak fault injection\" >&2\nexit 1\n",
	})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	// Loops 1-2 fail the gate; the default breaker (3 consecutive
	// failures) must NOT trip at fails=2, so the run still exits 0.
	cfg.MaxIterations = 3

	if rc := RunLoop(cfg); rc != 0 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-2.log"))
		t.Fatalf("rc = %d, want 0\nloop-2 log:\n%s", rc, logData)
	}

	rows := readLedger(t, repo)
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2: %v", len(rows), rows)
	}
	for i, row := range rows {
		if row["status"] != "failed-tests" {
			t.Fatalf("row %d status = %v, want failed-tests", i+1, row["status"])
		}
	}
	// A red gate never closes the tracker issue: the failed goal must stay
	// re-pickable.
	callLog, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(callLog), "gh issue close") {
		t.Fatalf("failed-tests path must not close the tracker issue:\n%s", callLog)
	}
}
