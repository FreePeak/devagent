package loopdriver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/queue"
)

// frozenClock returns a Now func advancing one second per call, starting at
// 2026-09-08T00:00:00Z (second precision matches the row format).
func frozenClock() (func() time.Time, *int) {
	ticks := 0
	base := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		ticks++
		return base.Add(time.Duration(ticks) * time.Second)
	}, &ticks
}

// installFakes puts a fake devagent/gh/npm on PATH via t.Setenv. The
// devagent fake logs every invocation to $DEVAGENT_LOG.
func installFakes(t *testing.T, repo string) {
	t.Helper()
	dir := fakeBinDir(t, map[string]string{
		"devagent-fake": `#!/bin/sh
echo "devagent $*" >> "${DEVAGENT_LOG:?}"
case "$1" in
  scan-text) echo GRADIENT-SCAN-TEXT ;;
  ledger) exit 0 ;;
  herdr-sweep) exit 0 ;;
  preflight) exit ${DEVAGENT_FAKE_PREFLIGHT_RC:-0} ;;
  sync-docs) echo "already at origin"; exit ${DEVAGENT_FAKE_SYNC_RC:-0} ;;
  page-degrade-breach) exit 0 ;;
  extract-text) exit 0 ;;
  pane-run) exit 0 ;;
  task) exit ${DEVAGENT_FAKE_TASK_RC:-0} ;;
esac
exit 0
`,
		"gh-fake": `#!/bin/sh
echo "gh $*" >> "${DEVAGENT_LOG:?}"
case "$1 $2" in
  "issue list") printf '%s' "$GH_ISSUES_JSON" ;;
  "issue close") exit 0 ;;
  "pr list") printf '%s' "${GH_PR_LIST_JSON:-}" ;;
esac
exit 0
`,
		"npm":      "#!/bin/sh\nexit 0\n",
		"omp-fake": "#!/bin/sh\nexit 0\n",
	})
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("DEVAGENT_LOG", filepath.Join(repo, "devagent-calls.log"))
}

func readLedger(t *testing.T, repo string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad ledger row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func readEvents(t *testing.T, repo string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("bad event row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

func TestRunLoopDryRunGoldenRows(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	// Seed a queued goal: dry-run skips the tracker pick and the PO phase,
	// so without a queue task the goal file never exists (bash dry-run would
	// record invalid — that path is covered by the issue-first test).
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID:    "TASK-1",
		Title: "Fix the loop ledger numbering",
		Goal:  "Goal: Fix the loop ledger numbering",
	}); err != nil {
		t.Fatal(err)
	}
	now, _ := frozenClock()
	var stdout bytes.Buffer
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.Stdout = &stdout
	})
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row["loop"].(float64) != 1 || row["status"] != "ok" {
		t.Fatalf("unexpected row: %v", row)
	}
	if !strings.HasPrefix(row["goal"].(string), "(dry-run) Goal:") {
		t.Fatalf("goal text: %v", row["goal"])
	}
	events := readEvents(t, repo)
	var phases []string
	resultCount := 0
	for _, e := range events {
		if e["event"] == "loop-result" {
			resultCount++
			if e["status"] != "ok" || e["loop"].(float64) != 1 {
				t.Fatalf("bad loop-result event: %v", e)
			}
		}
		if e["event"] == "loop-phase" {
			phases = append(phases, e["phase"].(string))
		}
	}
	// Bash parity: dry-run emits NO loop-phase events (preflight/research
	// phase rows live inside the non-dry-run branches).
	if resultCount != 1 || len(phases) != 0 {
		t.Fatalf("events: result=%d phases=%v", resultCount, phases)
	}
	for _, e := range events {
		ts := e["ts"].(string)
		if len(ts) != 20 || !strings.HasSuffix(ts, "Z") {
			t.Fatalf("bad ts format: %q", ts)
		}
	}
	// dry-run stub research file present.
	assertFileContains(t, filepath.Join(repo, ".selfbuild", "research", "loop-1.md"), "# dry-run stub")
	// The tail of the log is echoed to stdout (bash `tail -5`).
	if !strings.Contains(stdout.String(), "[ok] loop 1 complete") {
		t.Fatalf("stdout missing log tail: %q", stdout.String())
	}
}

func TestRunLoopIssueFirstShip(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":202,"title":"Port loop driver","labels":[{"name":"priority:P0"}]},{"number":15,"title":"Older P2","labels":[]}]`)
	t.Setenv("GH_PR_LIST_JSON", `[{"headRefName":"devagent/TASK-loop-1","url":"https://github.com/o/r/pull/9"}]`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[issue] claimed #202 from tracker (issue-first outranks LLM selection): Port loop driver") {
		t.Fatalf("issue claim missing from log: %s", logData)
	}
	goalData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "goals", "loop-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "Goal: Implement GitHub issue #202 (Port loop driver) in full and verifiably. Read the issue body for scope, acceptance criteria, and source links before planning. The PR must close the issue on merge.\n"
	if string(goalData) != want {
		t.Fatalf("goal file mismatch:\n%q", goalData)
	}
	// The issue goal carries the "Goal:" prefix, so the gate passes and the
	// faked task+npm succeed → the iteration ships and closes the issue.
	rows := readLedger(t, repo)
	if rows[0]["status"] != "ok" {
		t.Fatalf("expected ok row: %v", rows[0])
	}
	if !strings.HasPrefix(rows[0]["goal"].(string), "Goal: Implement GitHub issue #202") {
		t.Fatalf("goal text: %v", rows[0]["goal"])
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh issue close")
}

// TestRunLoopIssueNotClosedWithoutPR is the soak-169 regression (issue
// #238, BUG 2): the driver closed the tracker issue right after the task
// dispatch returned rc 0, although no PR existed (BUG 1 had silently
// skipped publishing). The close must fire only when a PR for the run
// branch actually exists; without one the row is recorded non-productive
// and the issue stays open.
func TestRunLoopIssueNotClosedWithoutPR(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	// The fake devagent task exits 0 but ships no PR, and gh knows of no
	// open PR for the run branch.
	t.Setenv("GH_ISSUES_JSON", `[{"number":238,"title":"Go task pipeline soak bugs","labels":[]}]`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "issue close") {
		t.Fatalf("issue must NOT be closed without a PR; gh calls:\n%s", calls)
	}
	rows := readLedger(t, repo)
	if len(rows) == 0 {
		t.Fatal("no ledger rows")
	}
	if rows[len(rows)-1]["status"] != "failed" {
		t.Fatalf("status = %v, want non-productive 'failed' (issue stays open)", rows[len(rows)-1]["status"])
	}
}

// TestRunLoopIssueClosedWhenPrexists proves the close path still fires
// when a PR for the run branch exists: rc 0 + gh pr list hit -> issue
// closed with the shipped comment and the ok row.
func TestRunLoopIssueClosedWhenPrexists(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":202,"title":"Port loop driver","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("GH_PR_LIST_JSON", `[{"headRefName":"devagent/TASK-loop-1","url":"https://github.com/o/r/pull/9"}]`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "issue close") {
		t.Fatalf("issue must be closed when the PR exists; gh calls:\n%s", calls)
	}
	if !strings.Contains(string(calls), "pr list --repo") || !strings.Contains(string(calls), "--head devagent/") {
		t.Fatalf("expected a gh pr list --head probe before closing; gh calls:\n%s", calls)
	}
	rows := readLedger(t, repo)
	if rows[len(rows)-1]["status"] != "ok" {
		t.Fatalf("status = %v, want ok", rows[len(rows)-1]["status"])
	}
}

func TestRunLoopPreflightBreaker(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("DEVAGENT_FAKE_PREFLIGHT_RC", "1")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxConsecutiveFailures = 3
		c.MaxIterations = 4 // three failing iterations fit before the cap
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 provider-degraded", len(rows))
	}
	for _, row := range rows {
		if row["status"] != "provider-degraded" {
			t.Fatalf("row status: %v", row["status"])
		}
	}
}

func TestRunLoopStarvationHaltsZero(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	writeRepoFile(t, repo, ".selfbuild/ledger.jsonl",
		`{"loop":1,"ts":"2026-09-07T00:00:00Z","status":"failed","goal":"a"}
{"loop":2,"ts":"2026-09-07T00:00:01Z","status":"failed","goal":"b"}
{"loop":3,"ts":"2026-09-07T00:00:02Z","status":"failed","goal":"c"}
{"loop":4,"ts":"2026-09-07T00:00:03Z","status":"failed","goal":"d"}
{"loop":5,"ts":"2026-09-07T00:00:04Z","status":"failed","goal":"e"}
`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (intentional stop)", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 5 {
		t.Fatalf("ledger must be unchanged, got %d rows", len(rows))
	}
}

func TestRunLoopStarvationExemptsDegraded(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	writeRepoFile(t, repo, ".selfbuild/ledger.jsonl",
		`{"loop":1,"ts":"2026-09-07T00:00:00Z","status":"provider-degraded","goal":"a"}
{"loop":2,"ts":"2026-09-07T00:00:01Z","status":"provider-degraded","goal":"b"}
{"loop":3,"ts":"2026-09-07T00:00:02Z","status":"provider-degraded","goal":"c"}
{"loop":4,"ts":"2026-09-07T00:00:03Z","status":"provider-degraded","goal":"d"}
{"loop":5,"ts":"2026-09-07T00:00:04Z","status":"provider-degraded","goal":"e"}
`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxIterations = 7 // allow loop 6 to run despite the seeded ledger
	})
	// Seed a queue goal so the exempt iteration lands an ok row, not invalid.
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID:    "TASK-1",
		Title: "Fix the loop ledger numbering",
		Goal:  "Goal: Fix the loop ledger numbering",
	}); err != nil {
		t.Fatal(err)
	}
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	// Degraded rows are exempt, so the loop proceeds and records the
	// dry-run ok row for loop 6.
	if rows[len(rows)-1]["status"] != "ok" {
		t.Fatalf("last row: %v", rows[len(rows)-1])
	}
}

func TestRunLoopSyncDocsOperatorRc2(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("DEVAGENT_FAKE_SYNC_RC", "2")
	now, _ := frozenClock()
	sleeps := 0
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxConsecutiveFailures = 1 // prove rc2 does NOT increment the breaker
		c.Sleep = func(time.Duration) { sleeps++ }
	})
	cfg.DryRun = false
	cfg.NoSyncDocs = false
	// Seed a queue goal so the skipped iteration's successor lands a real
	// row instead of the empty-goal invalid row.
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID:    "TASK-1",
		Title: "Fix the loop ledger numbering",
		Goal:  "Goal: Fix the loop ledger numbering",
	}); err != nil {
		t.Fatal(err)
	}
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0 (rc2 must not trip breaker)", rc)
	}
	if sleeps == 0 {
		t.Fatalf("sync retry sleep must fire")
	}
	rows := readLedger(t, repo)
	t.Logf("all rows: %v", rows)
	if rows[0]["status"] != "operator-degraded" {
		t.Fatalf("row: %v", rows)
	}
	// MaxConsecutiveFailures=1: any breaker increment would have exited 1
	// before this point — rc==0 is itself the no-increment proof.
}

func TestRunLoopSyncDocsProviderRc1Breaker(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("DEVAGENT_FAKE_SYNC_RC", "1")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxConsecutiveFailures = 1 // one provider failure trips the breaker
	})
	cfg.DryRun = false
	cfg.NoSyncDocs = false
	if rc := RunLoop(cfg); rc != 1 {
		t.Fatalf("rc = %d, want 1", rc)
	}
	rows := readLedger(t, repo)
	if rows[0]["status"] != "provider-degraded" {
		t.Fatalf("row: %v", rows[0])
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "page-degrade-breach")
}

func TestRunLoopTaskFailureRecordsQueueFailed(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("DEVAGENT_FAKE_TASK_RC", "1")
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID:    "TASK-1",
		Title: "Fix the loop ledger numbering",
		Goal:  "Goal: Fix the loop ledger numbering",
	}); err != nil {
		t.Fatal(err)
	}
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxConsecutiveFailures = 1
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 1 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
		t.Fatalf("rc = %d, want 1\nlog:\n%s", rc, logData)
	}
	rows := readLedger(t, repo)
	if rows[0]["status"] != "failed" || rows[0]["goal"] != "Goal: Fix the loop ledger numbering" {
		t.Fatalf("row: %v", rows[0])
	}
	// The fenced queue-done write must have failed the task with the detail.
	taskData, err := os.ReadFile(filepath.Join(repo, ".devagent", "queue", "TASK-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(taskData), `"status": "failed"`) ||
		!strings.Contains(string(taskData), "implement failed at loop 1") {
		t.Fatalf("queue task not failed: %s", taskData)
	}
}

func TestRunLoopQueueFirstDone(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	// Seed a pending queue task with a real goal.
	if err := os.MkdirAll(filepath.Join(repo, ".devagent", "queue"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Enqueue via the sibling package to guarantee schema compatibility.
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
		ID:    "TASK-1",
		Title: "Fix the loop ledger numbering",
		Goal:  "Goal: Fix the loop ledger numbering",
	}); err != nil {
		t.Fatal(err)
	}
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	if rows[0]["status"] != "ok" || rows[0]["goal"] != "(dry-run) Goal: Fix the loop ledger numbering" {
		t.Fatalf("row: %v", rows[0])
	}
	// Bash parity: the dry-run ok path does NOT call queue-done — the task
	// stays claimed until its lease lapses.
	assertFileContains(t, filepath.Join(repo, ".devagent", "queue", "TASK-1.json"), `"status": "claimed"`)
}
