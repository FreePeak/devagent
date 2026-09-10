package loopdriver

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// installFakes puts a fake devagent/gh/go/npm on PATH via t.Setenv. The
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
  task) [ "${DEVAGENT_FAKE_TASK_NO_PR:-0}" = "1" ] || echo "PR opened: https://example.fake/pr/1"; exit ${DEVAGENT_FAKE_TASK_RC:-0} ;;
esac
exit 0
`,
		"gh-fake": `#!/bin/sh
echo "gh $*" >> "${DEVAGENT_LOG:?}"
case "$1 $2" in
  "issue list")
    if [ -n "${GH_ROTATE_STATE:-}" ]; then
      c=$(cat "$GH_ROTATE_STATE" 2>/dev/null || echo 0)
      c=$((c + 1))
      echo "$c" > "$GH_ROTATE_STATE"
      n=$(( (c - 1) % 3 + 1 ))
      printf '[{"number":%d,"title":"Soak goal %d","labels":[{"name":"priority:P0"}]}]' "$((200 + n))" "$n"
    else
      printf '%s' "$GH_ISSUES_JSON"
    fi ;;
  "issue close") exit 0 ;;
  "pr view") printf '{"state":"%s"}' "${GH_PR_STATE:-OPEN}" ;;
esac
exit 0
`,
		"npm": "#!/bin/sh\nexit 0\n",
		"go":  "#!/bin/sh\nexit 0\n",
		"omp-fake": `#!/bin/sh
if [ -n "${RESEARCH_PICK_TEXT:-}" ]; then
  printf '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}\n' "$RESEARCH_PICK_TEXT"
fi
exit 0
`,
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

// Issue #238 regression: task rc 0 with no PR behind it must NOT close the
// tracker issue (soak-169 closed #230 with zero publish events). The
// no-pr row is non-productive, so the goal stays re-pickable.
func TestRunLoopNoPRLeavesIssueOpen(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("DEVAGENT_FAKE_TASK_NO_PR", "1")
	t.Setenv("GH_ISSUES_JSON", `[{"number":238,"title":"Publish silently skipped","labels":[{"name":"priority:P1"}]}]`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxIterations = 2
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "no-pr" || !strings.HasPrefix(rows[0]["goal"].(string), "Goal: Implement GitHub issue #238") {
		t.Fatalf("rows: %v", rows)
	}
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[publish] task succeeded without opening a PR") {
		t.Fatalf("no-pr skip not logged:\n%s", logData)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "gh issue close") {
		t.Fatalf("issue must stay open, calls:\n%s", calls)
	}
}

// Issue #301 AC3 regression: a research pick that says "merge PR #N, not a
// rewrite" must NOT emit the prompts.go implement template. The worker lands
// the already-open PR (which already carries its PRD update), so it opens no
// new PR of its own — the driver proves shipment via `gh pr view ... state ==
// MERGED`, not the #238 "PR opened" line.
func TestRunLoopMergePickVerifyAndMergeShips(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"Cleanup orphan worktrees","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("RESEARCH_PICK_TEXT", `THE single pick: #290 — merge PR #298, not a rewrite`)
	// Production reality: the merge dispatch opens no new PR.
	t.Setenv("DEVAGENT_FAKE_TASK_NO_PR", "1")
	// The tracker reports the PR merged.
	t.Setenv("GH_PR_STATE", "MERGED")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
		t.Fatalf("rc = %d, want 0\nlog:\n%s", rc, logData)
	}
	goalData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "goals", "loop-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	goal := string(goalData)
	if strings.Contains(goal, "in full and verifiably") {
		t.Fatalf("merge pick emitted the implement template:\n%s", goal)
	}
	if !strings.Contains(goal, "merge PR #298") || !strings.Contains(goal, "Pick rationale:") {
		t.Fatalf("merge goal lost the verify-and-merge directive or rationale:\n%s", goal)
	}
	// AC1: rationale survives; AC2: verify-and-merge dispatch, not a rewrite.
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "devagent task --prompt Goal: Close GitHub issue #290 by verifying and merging")
	// Productive "merged" row and the issue closes WITHOUT any new-PR line.
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "merged" {
		t.Fatalf("rows: %v", rows)
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh pr view 298")
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh issue close")
}

// The mirror failure: the task finished but the open PR is NOT merged yet.
// The iteration must stay non-productive (no-pr) and leave the issue open —
// never re-implement from scratch and never falsely ship.
func TestRunLoopMergePickUnmergedStaysOpen(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"Cleanup orphan worktrees","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("RESEARCH_PICK_TEXT", `THE single pick: #290 — merge PR #298, not a rewrite`)
	t.Setenv("DEVAGENT_FAKE_TASK_NO_PR", "1")
	t.Setenv("GH_PR_STATE", "OPEN")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0 (skip, not breaker)", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "no-pr" {
		t.Fatalf("rows: %v, want one no-pr", rows)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "gh issue close") {
		t.Fatalf("unmerged PR must leave the issue open, calls:\n%s", calls)
	}
}

// AC1 for the ordinary path: an implement pick still carries the research
// rationale into the goal text (selection reasoning is never dropped again).
func TestRunLoopIssuePickCarriesRationale(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"Cleanup orphan worktrees","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("RESEARCH_PICK_TEXT", `THE single pick: #290 — smallest change, one file, unblocks the queue`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
		t.Fatalf("rc = %d, want 0\nlog:\n%s", rc, logData)
	}
	goalData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "goals", "loop-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	goal := string(goalData)
	if !strings.Contains(goal, "in full and verifiably") || !strings.Contains(goal, "Pick rationale: #290 — smallest change") {
		t.Fatalf("implement goal missing template or carried rationale:\n%s", goal)
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

func TestRunLoopRepoTestGateFailure(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	// The gate is the TestCmd override — no npm fake involved here: a
	// failing command (the `go test ...` shape at FR-GO-16) records the
	// failed-tests row and feeds the breaker, exactly like the bash
	// driver's hardcoded `npm test` failure.
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
		c.TestCmd = "false"
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 0 {
		logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
		t.Fatalf("rc = %d, want 0 (skip, not breaker at fails=1)\nlog:\n%s", rc, logData)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "failed-tests" || rows[0]["goal"] != "Goal: Fix the loop ledger numbering" {
		t.Fatalf("rows: %v", rows)
	}
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[testing] repo tests failed after merge-back") {
		t.Fatalf("gate failure not logged:\n%s", logData)
	}
	// Bash parity: the failing gate skips the push phase and leaves the
	// queue task claimed (queue-done is only written on the task-failure
	// path); loop 2 halts at the cap.
	assertFileContains(t, filepath.Join(repo, ".devagent", "queue", "TASK-1.json"), `"status": "claimed"`)
}

// TestGoldenSoak is the FR-VAL-01 golden soak (issue #289): the full happy
// path the fragment tests cover only in pieces — pick → research → task
// dispatch → repo test gate → ledger ok row — across three consecutive
// green iterations, the already-shipped guard rejecting a 4th re-pick, and
// the failed-tests classification pin. Hermetic (fixture repo + fake
// worker CLIs + fake gh), so `go test ./...` runs it in CI unchanged.
func TestGoldenSoak(t *testing.T) {
	// The rotating fake serves pick N as issue #(200+N), so the pick order
	// is observable in the ledger goal texts. record() caps ledger goal
	// text at 160 chars via rowText.
	goalFor := func(n int) string {
		return rowText(issueGoalTemplate(200+n, fmt.Sprintf("Soak goal %d", n)), 160)
	}

	t.Run("three green iterations then the re-pick guard", func(t *testing.T) {
		repo := initFixtureRepo(t)
		installFakes(t, repo)
		t.Setenv("GH_ROTATE_STATE", filepath.Join(t.TempDir(), "pick-count"))
		now, _ := frozenClock()
		cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
			c.Now = now
			c.MaxIterations = 4 // three green iterations fit before the head cap
			c.NoSyncDocs = false
		})
		cfg.DryRun = false
		if rc := RunLoop(cfg); rc != 0 {
			logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
			t.Fatalf("rc = %d, want 0\nlog:\n%s", rc, logData)
		}
		rows := readLedger(t, repo)
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want 3 consecutive ok", len(rows))
		}
		for i, row := range rows {
			if row["status"] != "ok" || row["loop"] != float64(i+1) || row["goal"] != goalFor(i+1) {
				t.Fatalf("row %d = %v, want ok / loop %d / %q", i, row, i+1, goalFor(i+1))
			}
			// Research + goal artifacts created under .selfbuild/.
			assertFileContains(t, filepath.Join(repo, ".selfbuild", "goals", fmt.Sprintf("loop-%d.md", i+1)), "Goal: Implement GitHub issue #")
			if _, err := os.Stat(filepath.Join(repo, ".selfbuild", "research", fmt.Sprintf("loop-%d.md", i+1))); err != nil {
				t.Fatalf("research artifact missing: %v", err)
			}
		}

		// 4th pick: the rotating tracker re-serves issue #201 — the
		// already-shipped guard must reject the re-pick without another
		// task dispatch.
		cfg2 := loopConfigFor(t, repo, func(c *LoopConfig) {
			c.Now = now
			c.MaxIterations = 5
			c.NoSyncDocs = false
		})
		cfg2.DryRun = false
		if rc := RunLoop(cfg2); rc != 0 {
			t.Fatalf("re-pick rc = %d, want 0", rc)
		}
		rows = readLedger(t, repo)
		if len(rows) != 4 {
			t.Fatalf("rows = %d, want 3 ok + 1 skipped", len(rows))
		}
		last := rows[3]
		if last["status"] != "skipped" || last["loop"] != float64(4) || last["goal"] != goalFor(1) {
			t.Fatalf("4th row = %v, want skipped / loop 4 / %q", last, goalFor(1))
		}
		calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(string(calls), "devagent task"); got != 3 {
			t.Fatalf("task dispatches = %d, want 3 (guard must reject the 4th pick)\n%s", got, calls)
		}
		if got := strings.Count(string(calls), "gh issue close"); got != 4 {
			t.Fatalf("issue closes = %d, want 4 (3 ships + 1 guard close)\n%s", got, calls)
		}
	})

	// Failure-path pin: flip one fake to fail tests — the post-merge-back
	// repo gate goes red after a successful dispatch — and the ledger row
	// must classify failed-tests, leaving the tracker issue open.
	t.Run("failed-tests classification pin", func(t *testing.T) {
		repo := initFixtureRepo(t)
		installFakes(t, repo)
		t.Setenv("GH_ISSUES_JSON", `[{"number":201,"title":"Soak goal 1","labels":[{"name":"priority:P0"}]}]`)
		now, _ := frozenClock()
		cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
			c.Now = now
			c.TestCmd = "false"
		})
		cfg.DryRun = false
		if rc := RunLoop(cfg); rc != 0 {
			logData, _ := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
			t.Fatalf("rc = %d, want 0 (skip, not breaker at fails=1)\nlog:\n%s", rc, logData)
		}
		rows := readLedger(t, repo)
		if len(rows) != 1 || rows[0]["status"] != "failed-tests" || rows[0]["goal"] != goalFor(1) {
			t.Fatalf("rows: %v", rows)
		}
		calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(calls), "gh issue close") {
			t.Fatalf("gate failure must leave the issue open, calls:\n%s", calls)
		}
	})
}

func TestPhaseWritesHeartbeatFile(t *testing.T) {
	// FR-VAL-03 #291d: every phase boundary mirrors iteration · phase into
	// .selfbuild/heartbeat.json so GET /status (not the ledger) carries the
	// loop liveness surface.
	repo := initFixtureRepo(t)
	now, _ := frozenClock()
	// RunLoop mkdirs .selfbuild before the first phase; mirror it here.
	if err := os.MkdirAll(filepath.Join(repo, ".selfbuild"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &driver{cfg: LoopConfig{Repo: repo}.WithDefaults(), stateDir: filepath.Join(repo, ".selfbuild")}
	d.cfg.Now = now

	d.phase(7, "task", "goal text")

	data, err := os.ReadFile(filepath.Join(d.stateDir, "heartbeat.json"))
	if err != nil {
		t.Fatal(err)
	}
	var hb struct {
		Iteration int    `json:"iteration"`
		Phase     string `json:"phase"`
		Pid       int    `json:"pid"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &hb); err != nil {
		t.Fatalf("bad heartbeat %q: %v", data, err)
	}
	if hb.Iteration != 7 || hb.Phase != "task" {
		t.Fatalf("heartbeat = %+v", hb)
	}
	if hb.Pid != os.Getpid() {
		t.Fatalf("pid = %d, want %d", hb.Pid, os.Getpid())
	}
	if hb.UpdatedAt == "" {
		t.Fatalf("updatedAt empty: %+v", hb)
	}
}
