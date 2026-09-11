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
  task) [ "${DEVAGENT_FAKE_TASK_NO_PR:-0}" = "1" ] || echo "PR opened: https://github.com/FreePeak/devagent/pull/1"; exit ${DEVAGENT_FAKE_TASK_RC:-0} ;;
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
  api\ *) printf '%s' "$GH_TIMELINE_JSON" ;;
  # gh 2.98 pipes --json output (compact); prState decodes, so the shape is
  # the real one and the driver must not care. GH_PR_STATE answers every call;
  # GH_PR_STATE_FIRST + GH_PR_STATE_FILE sequence the first one differently
  # (a merge route reads the PR twice: OPEN at pick time, MERGED at ship).
  # GH_PR_STATE_OPEN_CALLS + GH_PR_STATE_OPEN_CALLS_FILE go finer: the first
  # K pr view calls read OPEN, every later one reads GH_PR_STATE — the
  # driver-side verify-and-merge rescues read the PR several times before
  # the final ship evidence.
  "pr view")
    s="${GH_PR_STATE:-OPEN}"
    if [ -n "${GH_PR_STATE_FIRST:-}" ] && [ ! -f "${GH_PR_STATE_FILE:-/nonexistent-marker}" ]; then
      : > "${GH_PR_STATE_FILE:?}"; s="$GH_PR_STATE_FIRST"
    fi
    if [ -n "${GH_PR_STATE_OPEN_CALLS:-}" ]; then
      c=$(cat "${GH_PR_STATE_OPEN_CALLS_FILE:?}" 2>/dev/null || echo 0)
      c=$((c + 1)); echo "$c" > "$GH_PR_STATE_OPEN_CALLS_FILE"
      [ "$c" -le "$GH_PR_STATE_OPEN_CALLS" ] && s="OPEN" || s="${GH_PR_STATE:-OPEN}"
    fi
    printf '{"state":"%s"}\n' "$s" ;;
esac
exit 0
`,
		"npm": "#!/bin/sh\nexit 0\n",
		"go":  "#!/bin/sh\nexit 0\n",
		// The headless research/PO bin: RESEARCH_OUT names the artifact a
		// test wants phase 1 to have produced (stdout → raw → the extractor's
		// plain-text passthrough).
		"omp-fake": "#!/bin/sh\nif [ -n \"${RESEARCH_OUT:-}\" ]; then cat \"$RESEARCH_OUT\"; fi\nexit 0\n",
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
	// merged = shipped: the `ok` row and the issue close below depend on the
	// fake tracker reporting the opened PR as MERGED (the merely-open variant
	// is pinned by TestRunLoopPROpenLeavesIssueOpenAndRepickable below).
	t.Setenv("GH_PR_STATE", "MERGED")
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

// merged = shipped (loopdriver DECISION.md): a PR that is opened but not
// merged records the productive `pr-open` row and leaves the tracker issue
// open — and since `pr-open` rows do not satisfy AlreadyShipped, the very
// next iteration re-picks the issue (the verify-and-merge re-pick path that
// landed #286/#320 and #315/#324). The old close-at-PR-open stranded the
// work on branches (#323 Case B).
func TestRunLoopPROpenLeavesIssueOpenAndRepickable(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":323,"title":"Shipped PR unrecorded","labels":[{"name":"priority:P2"}]}]`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxIterations = 3 // two iterations fit before the head cap (n >= cap)
	})
	cfg.DryRun = false
	rc := RunLoop(cfg)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (the issue must stay re-pickable)", len(rows))
	}
	for i, row := range rows {
		if row["status"] != "pr-open" {
			t.Fatalf("row %d status = %v, want pr-open", i, row["status"])
		}
		if !strings.HasPrefix(row["goal"].(string), "Goal: Implement GitHub issue #323") {
			t.Fatalf("row %d goal: %v", i, row["goal"])
		}
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "gh issue close") {
		t.Fatalf("issue must stay open until the PR merges:\n%s", calls)
	}
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

// researchMergePick is a phase-1 research artifact in the shape the live
// loop writes (loop-219 verbatim in spirit): a ranked list, then a "## Pick"
// section that names the tracker issue AND the green, already-open PR that
// implements it.
const researchMergePick = `## Ranked Top-3

**1. #290 (FR-VAL-02: devagent doctor) — merge PR #298, not a rewrite.** PR #298 is OPEN, MERGEABLE, +1114 lines, full CI matrix green.

**2. #291 (FR-VAL-03: driver observability parity).** Strong local evidence.

## Pick

**#290 — land via open PR #298, not a rewrite.** Evidence says the work is done and green; re-implementing from scratch would duplicate a passing 1114-line PR.
`

// seedResearchOutput points the fake research bin's stdout at body, so the
// driver's own extraction lands it as this iteration's research artifact.
func seedResearchOutput(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "research.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESEARCH_OUT", path)
}

// Issue #301 regression: a research pick that says "land via open PR #N"
// must dispatch a verify-and-merge iteration, never the implement template.
func TestRunLoopMergePickDispatchesVerifyAndMerge(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	seedResearchOutput(t, researchMergePick)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"FR-VAL-02: devagent doctor","labels":[{"name":"priority:P0"}]}]`)
	// A merge dispatch lands the EXISTING pull request, so it opens none:
	// DEVAGENT_FAKE_TASK_NO_PR proves the ship rests on the merged state,
	// not on a "PR opened:" line. The PR must read OPEN when the pick is
	// taken (that is what makes it work to land) and MERGED by the ship gate.
	t.Setenv("DEVAGENT_FAKE_TASK_NO_PR", "1")
	t.Setenv("GH_PR_STATE_FILE", filepath.Join(t.TempDir(), "pr-view-count"))
	t.Setenv("GH_PR_STATE_FIRST", "OPEN")
	t.Setenv("GH_PR_STATE", "MERGED")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		// A resolved tracker repo, like production's origin-derived
		// SELFBUILD_GH_REPO fallback, so the gh evidence call is assertable
		// whole.
		c.GHRepo = "FreePeak/devagent"
	})
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
	if strings.Contains(goal, "Implement GitHub issue") {
		t.Fatalf("merge pick must not emit the implement template:\n%s", goal)
	}
	if !strings.Contains(goal, "merging the existing open PR #298") {
		t.Fatalf("verify-and-merge goal missing:\n%s", goal)
	}
	if !strings.Contains(goal, "gh pr checks 298") {
		t.Fatalf("verify-and-merge goal must name the CI check:\n%s", goal)
	}
	if !strings.Contains(goal, "land via open PR #298") {
		t.Fatalf("pick rationale dropped from the goal:\n%s", goal)
	}
	// The ship rests on gh's merged state for the existing PR, read from a
	// decoded `--json state` (real gh output shape), with the tracker repo
	// resolved.
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"),
		"gh pr view 298 --repo FreePeak/devagent --json state")
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[issue] research pick lands existing PR #298") {
		t.Fatalf("merge dispatch not logged:\n%s", logData)
	}
	// The landed merge moved origin/main ahead of the driver's checkout, so
	// the repo gate must try to sync before verdicting it.
	if !strings.Contains(string(logData), "origin/main is not fast-forwardable") {
		t.Fatalf("merge route must sync before the repo gate:\n%s", logData)
	}
	rows := readLedger(t, repo)
	// The row stays `ok` (lessons tallies any other status as a failure); the
	// land is recorded in the row's goal text and the phase detail.
	if len(rows) != 1 || rows[0]["status"] != "ok" ||
		!strings.HasPrefix(rows[0]["goal"].(string), "Goal: Land GitHub issue #290") {
		t.Fatalf("rows: %v", rows)
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh issue close")
}

// merged = shipped, detection half (end to end): research names no PR, but
// the issue's timeline cross-references an OPEN pull request — the pick must
// route to the verify-and-merge template (a rewrite cannot open a second PR)
// and the iteration ships on that PR's merged state. #323 Case B: three
// no-pr rows re-ran #316 while its PR sat open.
func TestRunLoopOpenPRDetectedRoutesToVerifyAndMerge(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	t.Setenv("GH_ISSUES_JSON", `[{"number":316,"title":"run locks","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("GH_TIMELINE_JSON", `[{"event":"cross-referenced","source":{"issue":{"number":325,"pull_request":{}}}}]`)
	// The merge dispatch lands the EXISTING PR and opens none; the PR reads
	// OPEN at detection time and MERGED by the ship gate (same sequencing
	// contract as the merge-pick route).
	t.Setenv("DEVAGENT_FAKE_TASK_NO_PR", "1")
	t.Setenv("GH_PR_STATE_FILE", filepath.Join(t.TempDir(), "pr-view-count"))
	t.Setenv("GH_PR_STATE_FIRST", "OPEN")
	t.Setenv("GH_PR_STATE", "MERGED")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.GHRepo = "FreePeak/devagent"
	})
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
	if !strings.Contains(goal, "Land GitHub issue #316 by verifying and merging the existing open PR #325") {
		t.Fatalf("detection must route to verify-and-merge:\n%s", goal)
	}
	if strings.Contains(goal, "Implement GitHub issue") {
		t.Fatalf("implement template emitted for an issue with an open PR:\n%s", goal)
	}
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[issue] open PR #325 found for issue #316 — verify-and-merge dispatch (merged = shipped)") {
		t.Fatalf("detection not logged:\n%s", logData)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "ok" {
		t.Fatalf("rows: %v", rows)
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh issue close")
}

// The destructive false-positive class: research mentions a pull request that
// already landed (it is ASKED to weigh "does a merged PR already cover it?").
// Such a mention must never route the iteration to merge — shipping evidence
// for an already-merged PR is always true, so the iteration would record a
// productive row and close an issue nobody worked (issue #301).
func TestRunLoopMergePickIgnoresAlreadyLandedPR(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	seedResearchOutput(t, researchMergePick)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"FR-VAL-02: devagent doctor","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("GH_PR_STATE", "MERGED") // merged before this iteration started
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.GHRepo = "FreePeak/devagent"
	})
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	goalData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "goals", "loop-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	goal := string(goalData)
	if !strings.HasPrefix(goal, "Goal: Implement GitHub issue #290") {
		t.Fatalf("a landed PR is not work to merge — implement route expected:\n%s", goal)
	}
	if strings.Contains(goal, "merging the existing open PR") {
		t.Fatalf("verify-and-merge goal emitted for a merged PR:\n%s", goal)
	}
	if !strings.Contains(goal, "land via open PR #298") {
		t.Fatalf("rationale must still ride along:\n%s", goal)
	}
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[issue] research pick names PR #298 but it is MERGED, not open") {
		t.Fatalf("pick-time state guard not logged:\n%s", logData)
	}
	// It ships as an ordinary implement run.
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "ok" {
		t.Fatalf("rows: %v", rows)
	}
}

// A verify-and-merge dispatch whose worker finished green but never merged:
// the driver closes the loop itself — it lands the still-open PR through the
// bounded AutoReviewAndMergeOne path, records the productive row, closes the
// issue, and applies the pr-hygiene sweep (loop 272's failure class).
func TestRunLoopMergePickRescuedByDriverSideMerge(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	seedResearchOutput(t, researchMergePick)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"FR-VAL-02: devagent doctor","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("DEVAGENT_FAKE_TASK_NO_PR", "1")
	// The PR reads OPEN through the pick, the post-run evidence check, the
	// rescue's OPEN guard, and the auto-merge status poll — then MERGED for
	// the final ship evidence (4 pr view calls precede it).
	t.Setenv("GH_PR_STATE_OPEN_CALLS", "4")
	t.Setenv("GH_PR_STATE_OPEN_CALLS_FILE", filepath.Join(t.TempDir(), "pr-view-calls"))
	t.Setenv("GH_PR_STATE", "MERGED")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.GHRepo = "FreePeak/devagent"
		c.GhRun = execFakeGh
	})
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "ok" {
		t.Fatalf("rows: %v, want one productive ok row", rows)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh pr view 298")
	if !strings.Contains(string(calls), "gh pr merge 298") {
		t.Fatalf("driver-side merge not attempted, calls:\n%s", calls)
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh issue close")
	// The shipped pr-hygiene triage applied after the ship (sweep's PR list).
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh pr list")
	logData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "logs", "loop-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logData), "[verify] run ended without shipping evidence — driver-side verify-and-merge of open PR #298") {
		t.Fatalf("rescue not logged:\n%s", logData)
	}
}

// Same rescue on the failure path: a task dispatch that exits nonzero must
// not record `failed` when the picked issue's PR landed anyway — the run
// ships (loops 270-272 mis-recorded green work as failures).
func TestRunLoopFailedTaskRescuedByLandedPR(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	seedResearchOutput(t, researchMergePick)
	t.Setenv("GH_ISSUES_JSON", `[{"number":290,"title":"FR-VAL-02: devagent doctor","labels":[{"name":"priority:P0"}]}]`)
	t.Setenv("DEVAGENT_FAKE_TASK_RC", "1")
	// OPEN through pick, rescue guard, and status poll; MERGED for the
	// final ship evidence (3 pr view calls precede it).
	t.Setenv("GH_PR_STATE_OPEN_CALLS", "3")
	t.Setenv("GH_PR_STATE_OPEN_CALLS_FILE", filepath.Join(t.TempDir(), "pr-view-calls"))
	t.Setenv("GH_PR_STATE", "MERGED")
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.GHRepo = "FreePeak/devagent"
		c.GhRun = execFakeGh
	})
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	rows := readLedger(t, repo)
	if len(rows) != 1 || rows[0]["status"] != "ok" {
		t.Fatalf("rows: %v, want the landed run recorded ok, not failed", rows)
	}
	calls, err := os.ReadFile(filepath.Join(repo, "devagent-calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "gh issue close") == false {
		t.Fatalf("shipped issue must close, calls:\n%s", calls)
	}
	assertFileContains(t, filepath.Join(repo, "devagent-calls.log"), "gh pr merge 298")
}

// Issue #301, first acceptance criterion: an ordinary implement pick still
// carries the research rationale into the goal the worker is dispatched with.
func TestRunLoopImplementPickCarriesRationale(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	seedResearchOutput(t, "## Ranked Top-3\n\n**1. #202 (Port loop driver).** Tractable.\n\n## Pick\n\n**#202 — implement.** Nothing in the ledger covers it; the failure-cluster report points at queue numbering, so it is the highest-impact tractable item.\n")
	t.Setenv("GH_ISSUES_JSON", `[{"number":202,"title":"Port loop driver","labels":[{"name":"priority:P0"}]}]`)
	now, _ := frozenClock()
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) { c.Now = now })
	cfg.DryRun = false
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	goalData, err := os.ReadFile(filepath.Join(repo, ".selfbuild", "goals", "loop-1.md"))
	if err != nil {
		t.Fatal(err)
	}
	goal := string(goalData)
	if !strings.HasPrefix(goal, "Goal: Implement GitHub issue #202 (Port loop driver) in full and verifiably.") {
		t.Fatalf("implement template changed shape:\n%s", goal)
	}
	if !strings.Contains(goal, "highest-impact tractable item") {
		t.Fatalf("pick rationale dropped from the goal:\n%s", goal)
	}
}

// TestResearchPickReadsTheArtifact pins the seam #301 removed: what phase 1
// said about THIS issue is what the iteration acts on — and anything it said
// about another issue (or no parsable text at all) steers nothing.
func TestResearchPickReadsTheArtifact(t *testing.T) {
	dir := t.TempDir()
	d := &driver{stateDir: dir}
	cases := []struct {
		name      string
		artifact  string // "" = no artifact written
		issueNum  int
		wantPR    int
		wantRatio string // substring the rationale must carry; "" = none
	}{
		{
			name:      "merge verb beside the PR ref",
			artifact:  "## Pick\n\n**#290 — merge PR #298.** Green and complete.\n",
			issueNum:  290,
			wantPR:    298,
			wantRatio: "Green and complete.",
		},
		{
			name:      "land-via phrasing",
			artifact:  "## Pick\n\n**#290 — land via open PR #298, not a rewrite.**\n",
			issueNum:  290,
			wantPR:    298,
			wantRatio: "not a rewrite",
		},
		{
			name:      "implement pick keeps its rationale, names no PR",
			artifact:  "## Pick\n\n**#202 — implement.** Nothing in the ledger covers it.\n",
			issueNum:  202,
			wantPR:    0,
			wantRatio: "Nothing in the ledger covers it.",
		},
		{
			name:      "no Pick heading falls back to the last line",
			artifact:  "## Ranked Top-3\n\n**1. #202 ...**\n\n**#202 — merge PR #211.**\n",
			issueNum:  202,
			wantPR:    211,
			wantRatio: "merge PR #211",
		},
		{
			name:      "past-tense history is not a directive",
			artifact:  "## Pick\n\n**#291 — implement.** The same pattern shipped via PR #260 last loop; extend it.\n",
			issueNum:  291,
			wantPR:    0,
			wantRatio: "extend it",
		},
		{
			name:      "a PR reference equal to the issue number is not a PR",
			artifact:  "## Pick\n\n**#290 — merge PR #290.**\n",
			issueNum:  290,
			wantPR:    0,
			wantRatio: "merge PR #290",
		},
		{
			name:     "a pick about another issue is ignored",
			artifact: "## Pick\n\n**#291 — merge PR #298.**\n",
			issueNum: 290,
			wantPR:   0,
		},
		{
			name:     "a longer issue number is not this issue",
			artifact: "## Pick\n\n**#2900 — merge PR #298.**\n",
			issueNum: 290,
			wantPR:   0,
		},
		{
			// The live loop-219 paragraph read by the NEXT iteration's pick:
			// "#291" there is a fallback mention, not that pick's subject.
			name:     "a passing mention is not the pick's subject",
			artifact: "## Pick\n\n**#290 — merge PR #298.** Fallback if the merge hits a surprise: fall through to #291.\n",
			issueNum: 291,
			wantPR:   0,
		},
		{
			name:     "extract failure carries nothing",
			artifact: "[extract-failed] research produced no parsable output\n",
			issueNum: 290,
			wantPR:   0,
		},
		{name: "missing artifact reads as no pick", issueNum: 290, wantPR: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "research", "loop-1.md")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if tc.artifact != "" {
				writeRepoFile(t, dir, filepath.Join("research", "loop-1.md"), tc.artifact)
			}
			got := d.researchPick(1, tc.issueNum)
			if got.mergePR != tc.wantPR {
				t.Fatalf("mergePR = %d, want %d (artifact %q)", got.mergePR, tc.wantPR, tc.artifact)
			}
			if tc.wantRatio == "" {
				if got.rationale != "" {
					t.Fatalf("rationale = %q, want none", got.rationale)
				}
				return
			}
			if !strings.Contains(got.rationale, tc.wantRatio) {
				t.Fatalf("rationale = %q, want it to carry %q", got.rationale, tc.wantRatio)
			}
		})
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

// TestRunLoopStarvationHaltArmsExitWatchdog is the issue #286 wiring test:
// once the driver has printed the starvation halt verdict, something must
// guarantee the process leaves — the 2026-09-10 hang kept a live driver
// pinned in an exec teardown after the banner, and restart=always turned
// it into a zombie that logged halts forever. With os.Exit swapped for a
// recorder and the grace shortened, the halt must return 0 AND leave a
// watchdog that fires with the verdict code.
func TestRunLoopStarvationHaltArmsExitWatchdog(t *testing.T) {
	repo := initFixtureRepo(t)
	installFakes(t, repo)
	writeRepoFile(t, repo, ".selfbuild/ledger.jsonl",
		`{"loop":1,"ts":"2026-09-07T00:00:00Z","status":"failed","goal":"a"}
{"loop":2,"ts":"2026-09-07T00:00:01Z","status":"failed","goal":"b"}
{"loop":3,"ts":"2026-09-07T00:00:02Z","status":"failed","goal":"c"}
{"loop":4,"ts":"2026-09-07T00:00:03Z","status":"failed","goal":"d"}
{"loop":5,"ts":"2026-09-07T00:00:04Z","status":"failed","goal":"e"}
`)
	fired := make(chan int, 1)
	now, _ := frozenClock()
	// MaxIterations 7 > the next loop number (6): the loop-head cap must
	// not fire first — this test is about the starvation halt path. The
	// watchdog seams ride the config: 50ms grace, a recorder instead of
	// the real os.Exit.
	cfg := loopConfigFor(t, repo, func(c *LoopConfig) {
		c.Now = now
		c.MaxIterations = 7
		c.ExitWatchdogDelay = 50 * time.Millisecond
		c.Exit = func(code int) { fired <- code }
	})
	if rc := RunLoop(cfg); rc != 0 {
		t.Fatalf("rc = %d, want 0 (intentional stop)", rc)
	}
	assertFileContains(t, filepath.Join(repo, ".selfbuild", "logs", "loop-6.log"),
		"[starvation] 5 consecutive non-productive iterations — halting loop")
	select {
	case code := <-fired:
		if code != 0 {
			t.Fatalf("watchdog exit = %d, want the verdict code 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exit watchdog never armed at the starvation halt — the #286 hang class is unguarded")
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
		// merged = shipped: this soak pins the green path, so the fake
		// tracker reports each opened PR as MERGED (the `pr-open` variant —
		// productive row, issue left open and re-pickable — is pinned by
		// TestRunLoopPROpenLeavesIssueOpenAndRepickable).
		t.Setenv("GH_PR_STATE", "MERGED")
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
