package tracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/scout"
)

// trkPtr is a small pointer helper for optional heartbeat fields.
func trkPtr[T any](v T) *T { return &v }

// trkTmpRepo seeds a temp repo: .selfbuild/ with a one-line ledger, exactly
// like the TS tmpRepo() helper.
func trkTmpRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".selfbuild"), 0o755); err != nil {
		t.Fatal(err)
	}
	ledgerLine := `{"loop":1,"ts":"2026-08-25T00:00:00Z","status":"ok","goal":"seed"}` + "\n"
	if err := os.WriteFile(filepath.Join(repo, ".selfbuild", "ledger.jsonl"), []byte(ledgerLine), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// trkOkRunner answers git and gh with canned success output like okRunner.
func trkOkRunner(cmd string, _ []string, _ CliRunOptions) CliRunResult {
	switch cmd {
	case "git":
		return CliRunResult{ExitCode: 0, Stdout: "abc1234 feat: x\ndef5678 fix: y\n"}
	case "gh":
		return CliRunResult{ExitCode: 0, Stdout: `[{"number":41,"title":"Add thing","url":"https://example/pr/41","state":"OPEN"}]`}
	}
	return CliRunResult{ExitCode: 1}
}

// trkFailRunner always fails like the TS failRunner.
func trkFailRunner(string, []string, CliRunOptions) CliRunResult {
	return CliRunResult{ExitCode: 127, Stderr: "not found"}
}

// TestCollectProgressAsyncGathersEvidence mirrors "collects queue + scout +
// ledger + git + gh into a snapshot".
func TestCollectProgressAsyncGathersEvidence(t *testing.T) {
	repo := trkTmpRepo(t)
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "T-1", Title: "one", Goal: "Goal: one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "T-2", Title: "two", Goal: "Goal: two"}); err != nil {
		t.Fatal(err)
	}
	if queue.ClaimNextPending(repo, "w1", nil) == nil {
		t.Fatal("claim T-1 failed")
	}
	if queue.SetTaskStatus(repo, "T-2", queue.StatusFailed, "boom", 0, nil) == nil {
		t.Fatal("fail T-2 refused")
	}
	if _, err := scout.WriteHeartbeat(repo, scout.HeartbeatPatch{
		LastStatus:      trkPtr("ok"),
		LastDetail:      trkPtr("enqueued T-1"),
		Worker:          trkPtr("opencode"),
		IntervalMinutes: trkPtr(30.0),
		LastTaskID:      trkPtr("T-1"),
	}); err != nil {
		t.Fatal(err)
	}

	snap := CollectProgressAsync(TrackerOptions{RepoPath: repo}, trkOkRunner)

	if snap.Queue.Total != 2 || snap.Queue.Claimed != 1 || snap.Queue.Failed != 1 {
		t.Fatalf("queue counts = %+v, want total 2 claimed 1 failed 1", snap.Queue)
	}
	if snap.Scout == nil || !snap.Scout.Alive {
		t.Fatalf("scout = %+v, want alive", snap.Scout)
	}
	seedFound := false
	for _, l := range snap.LedgerTail {
		if strings.Contains(l, "seed") {
			seedFound = true
		}
	}
	if !seedFound {
		t.Fatalf("ledgerTail %v missing seed line", snap.LedgerTail)
	}
	commitFound := false
	for _, c := range snap.RecentCommits {
		if c == "abc1234 feat: x" {
			commitFound = true
		}
	}
	if !commitFound {
		t.Fatalf("recentCommits %v missing git line", snap.RecentCommits)
	}
	if len(snap.OpenPrs) == 0 || !strings.Contains(snap.OpenPrs[0], "#41") {
		t.Fatalf("openPrs %v missing #41", snap.OpenPrs)
	}
}

// TestTrackOnceWritesProgressArtifacts mirrors "trackOnce writes
// progress.md + progress.json + heartbeat".
func TestTrackOnceWritesProgressArtifacts(t *testing.T) {
	repo := trkTmpRepo(t)
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "W-1", Title: "work", Goal: "Goal: work"}); err != nil {
		t.Fatal(err)
	}

	r := TrackOnce(TrackerOptions{RepoPath: repo}, trkOkRunner)

	if !r.OK {
		t.Fatalf("TrackOnce not ok: %s", r.Detail)
	}
	mdPath := filepath.Join(repo, ".selfbuild", "progress.md")
	jsonPath := filepath.Join(repo, ".selfbuild", "progress.json")
	mdRaw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatal(err)
	}
	md := string(mdRaw)
	if !strings.Contains(md, "# DevAgent Self-Build Progress") {
		t.Fatal("progress.md missing title")
	}
	if !strings.Contains(md, "[pending] W-1") {
		t.Fatal("progress.md missing [pending] W-1")
	}
	hb := ReadTrackerHeartbeat(repo)
	if hb == nil || hb.LastStatus != "ok" {
		t.Fatalf("tracker heartbeat = %+v, want lastStatus ok", hb)
	}
}

// TestTrackOnceDegradesCleanly mirrors "degrades cleanly when git/gh fail
// and scout never ran".
func TestTrackOnceDegradesCleanly(t *testing.T) {
	repo := trkTmpRepo(t)

	r := TrackOnce(TrackerOptions{RepoPath: repo}, trkFailRunner)

	if !r.OK {
		t.Fatalf("TrackOnce not ok: %s", r.Detail)
	}
	if r.Snapshot == nil {
		t.Fatal("TrackOnce result missing snapshot")
	}
	if len(r.Snapshot.RecentCommits) != 0 {
		t.Fatalf("recentCommits = %v, want empty", r.Snapshot.RecentCommits)
	}
	if len(r.Snapshot.OpenPrs) != 0 {
		t.Fatalf("openPrs = %v, want empty", r.Snapshot.OpenPrs)
	}
	if r.Snapshot.Scout != nil {
		t.Fatalf("scout = %+v, want nil", r.Snapshot.Scout)
	}
	if hb := ReadTrackerHeartbeat(repo); hb == nil || hb.LastStatus != "ok" {
		t.Fatalf("tracker heartbeat = %+v, want lastStatus ok", hb)
	}
}

// TestCollectProgressAsyncMarksStaleScoutDead mirrors "marks scout dead
// when heartbeat is stale (>6h)".
func TestCollectProgressAsyncMarksStaleScoutDead(t *testing.T) {
	repo := trkTmpRepo(t)
	stale := time.Now().Add(-7 * time.Hour).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	if _, err := scout.WriteHeartbeat(repo, scout.HeartbeatPatch{
		LastRunAt:       &stale,
		LastStatus:      trkPtr("ok"),
		LastDetail:      trkPtr("old"),
		Worker:          trkPtr("opencode"),
		IntervalMinutes: trkPtr(30.0),
	}); err != nil {
		t.Fatal(err)
	}

	snap := CollectProgressAsync(TrackerOptions{RepoPath: repo}, trkOkRunner)

	if snap.Scout == nil || snap.Scout.Alive {
		t.Fatalf("scout = %+v, want alive=false", snap.Scout)
	}
	if !strings.Contains(RenderProgressMarkdown(snap), "worker=opencode") {
		t.Fatal("markdown missing worker=opencode")
	}
}

// TestCollectProgressAsyncReadsOrchestratorBoard mirrors "includes
// orchestrator board when .devagent-project.json exists" and pins the
// insertion-order rendering of the counts line.
func TestCollectProgressAsyncReadsOrchestratorBoard(t *testing.T) {
	repo := trkTmpRepo(t)
	board := `{"goal":"Example goal","tasks":[` +
		`{"id":"T1","title":"First","status":"done","attempts":1},` +
		`{"id":"T2","title":"Second","dependsOn":["T1"],"status":"blocked","attempts":0,"failureDetail":"upstream blocked"}]}`
	if err := os.WriteFile(filepath.Join(repo, ".devagent-project.json"), []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := CollectProgressAsync(TrackerOptions{RepoPath: repo}, trkOkRunner)

	if snap.Board == nil {
		t.Fatal("board missing from snapshot")
	}
	if snap.Board.Counts.Get("done") != 1 || snap.Board.Counts.Get("blocked") != 1 {
		t.Fatalf("board counts = %v, want done 1 blocked 1", snap.Board.Counts.Entries())
	}
	md := RenderProgressMarkdown(snap)
	if !strings.Contains(md, "## Orchestrator board") {
		t.Fatal("markdown missing Orchestrator board section")
	}
	if !strings.Contains(md, "goal: Example goal") {
		t.Fatal("markdown missing goal line")
	}
	if !strings.Contains(md, "tasks: done:1 blocked:1") {
		t.Fatalf("markdown counts line not in insertion order: %s", md)
	}
	if !strings.Contains(md, "blocked: upstream blocked") {
		t.Fatal("markdown missing blocked reason")
	}
}
