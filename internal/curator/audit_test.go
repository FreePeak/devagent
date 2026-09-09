package curator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/queue"
)

// The fixed clock the tests scan with; every mtime age below is expressed
// against it (the TS tests use the same injectable-now determinism).
var testNow = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func testNowMs() int64 { return testNow.UnixMilli() }

const day = 24 * 60 * 60 * 1000

// seedRepo builds a temp repo with docs/prds files (name -> mtime age in ms)
// and .devagent/queue task files written through the real queue writer, so
// the fixtures exercise the same bytes the loop writes.
func seedRepo(t *testing.T, prds map[string]int64, tasks []*queue.QueuedTask) string {
	t.Helper()
	repo := t.TempDir()
	prdsDir := filepath.Join(repo, "docs", "prds")
	if err := os.MkdirAll(prdsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, ageMs := range prds {
		p := filepath.Join(prdsDir, name)
		if err := os.WriteFile(p, []byte("# prd\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		mtime := time.UnixMilli(testNowMs() - ageMs)
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(queue.QueueDir(repo), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		blob, err := json.MarshalIndent(task, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(queue.QueueDir(repo), task.ID+".json"), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func scan(t *testing.T, repo string) AuditReport {
	t.Helper()
	r := AuditPrdCoverage(repo, AuditOptions{Now: testNowMs})
	if r.Enqueued != 0 {
		t.Fatalf("Enqueued = %d, want 0 (Q15 advisory invariant)", r.Enqueued)
	}
	return r
}

// TestAuditClean: every PRD covered by a done task, recent mtimes — no
// findings, the advisory pass stays silent.
func TestAuditClean(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"alpha.md": 0},
		[]*queue.QueuedTask{{ID: "alpha", Status: queue.StatusDone}},
	)
	r := scan(t, repo)
	if r.Scanned != 1 || r.Tasks != 1 || len(r.Findings) != 0 {
		t.Fatalf("scanned=%d tasks=%d findings=%d, want 1/1/0", r.Scanned, r.Tasks, len(r.Findings))
	}
	if len(r.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", r.Warnings)
	}
}

// TestAuditUnqueued: a PRD no task covers. Warning text is the golden
// contract prd-curator.sh greps.
func TestAuditUnqueued(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"orphan.md": 3 * day},
		nil,
	)
	r := scan(t, repo)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(r.Findings), r.Findings)
	}
	f := r.Findings[0]
	if f.Kind != KindUnqueued || f.File != filepath.Join("docs", "prds", "orphan.md") || f.Stem != "orphan" || f.AgeMs != 3*day || len(f.TaskIDs) != 0 {
		t.Fatalf("finding shape = %+v", f)
	}
	want := "docs/prds/orphan.md (3d old) has no queue task covering it — the next scout cycle should enqueue it or the curator should retire the file"
	if f.Warning != want {
		t.Fatalf("warning =\n%q\nwant\n%q", f.Warning, want)
	}
}

// TestAuditStale: covered, still pending, mtime past the 14d threshold.
func TestAuditStale(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"stuck.md": 20 * day},
		[]*queue.QueuedTask{{ID: "stuck", Status: queue.StatusPending}},
	)
	r := scan(t, repo)
	if len(r.Findings) != 1 {
		t.Fatalf("findings = %d, want 1: %+v", len(r.Findings), r.Findings)
	}
	f := r.Findings[0]
	if f.Kind != KindStale || f.Stem != "stuck" || f.AgeMs != 20*day {
		t.Fatalf("finding shape = %+v", f)
	}
	if len(f.TaskIDs) != 1 || f.TaskIDs[0] != "stuck" {
		t.Fatalf("taskIds = %v, want [stuck]", f.TaskIDs)
	}
	want := "docs/prds/stuck.md is queued as stuck (pending) but has sat unchanged for 20d >= 14d threshold — check why the board has not picked it up"
	if f.Warning != want {
		t.Fatalf("warning =\n%q\nwant\n%q", f.Warning, want)
	}
}

// TestAuditTerminalCoverStaysSilent: all covering tasks done/failed —
// shipped or retired state, not a stale backlog, so no finding even at a
// very old mtime.
func TestAuditTerminalCoverStaysSilent(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"shipped.md": 40 * day, "retired.md": 40 * day},
		[]*queue.QueuedTask{
			{ID: "shipped", Status: queue.StatusDone},
			{ID: "retired", Status: queue.StatusFailed},
		},
	)
	r := scan(t, repo)
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
}

// TestAuditUnqueuedWins: both a done cover (via a differently-named prdPath
// stem) and an open cover exist, but unqueued still fires only for the file
// with no cover at all; the mixed stale check exercises the open cover path
// through the prdPath join key.
func TestAuditUnqueuedWins(t *testing.T) {
	prdCopy := "/repo/.devagent/prds/alias-target.md"
	repo := seedRepo(t,
		map[string]int64{"plain.md": 0, "covered.md": 20 * day, "alias-target.md": 20 * day},
		[]*queue.QueuedTask{
			{ID: "done-task", Status: queue.StatusDone},
			{ID: "covered", Status: queue.StatusClaimed},
			{ID: "alias-task", Status: queue.StatusPending, PrdPath: &prdCopy},
		},
	)
	r := scan(t, repo)
	byStem := map[string]PrdAuditFinding{}
	for _, f := range r.Findings {
		byStem[f.Stem] = f
	}
	if f, ok := byStem["covered"]; !ok || f.Kind != KindStale {
		t.Fatalf("covered.md must be stale via id join, got %+v", byStem)
	}
	// covered.md's mtime is 20d old, and its covering task is claimed (open)
	// — stale. But wait: it IS covered, so unqueued must not fire.
	if _, ok := byStem["covered"]; ok && byStem["covered"].Kind == KindUnqueued {
		t.Fatal("unqueued must not fire for a covered file (unqueued wins only when nothing covers)")
	}
	// alias-target.md is covered only through alias-task's prdPath basename.
	if f, ok := byStem["alias-target"]; !ok || f.Kind != KindStale {
		t.Fatalf("alias-target.md must be stale via prdPath join, got %+v", byStem)
	}
	if len(byStem["alias-target"].TaskIDs) != 1 || byStem["alias-target"].TaskIDs[0] != "alias-task" {
		t.Fatalf("taskIds = %v, want [alias-task]", byStem["alias-target"].TaskIDs)
	}
	if f, ok := byStem["plain"]; !ok || f.Kind != KindUnqueued {
		t.Fatalf("plain.md must be unqueued, got %+v", byStem)
	}
}

// TestAuditBelowThresholdSilent: covered + open but mtime under 14d — not
// stale yet.
func TestAuditBelowThresholdSilent(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"fresh.md": day},
		[]*queue.QueuedTask{{ID: "fresh", Status: queue.StatusPending}},
	)
	r := scan(t, repo)
	if len(r.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", r.Findings)
	}
}

// TestAuditThresholdBoundary: exactly staleAfterMs is stale (>=).
func TestAuditThresholdBoundary(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"edge.md": 14 * day},
		[]*queue.QueuedTask{{ID: "edge", Status: queue.StatusPending}},
	)
	r := scan(t, repo)
	if len(r.Findings) != 1 || r.Findings[0].Kind != KindStale {
		t.Fatalf("findings = %+v, want exactly one stale", r.Findings)
	}
}

// TestAuditMissingDir: a repo with no docs/prds scans nothing and never
// errors.
func TestAuditMissingDir(t *testing.T) {
	repo := t.TempDir()
	r := scan(t, repo)
	if r.Scanned != 0 || r.Tasks != 0 || len(r.Findings) != 0 {
		t.Fatalf("report = %+v, want empty scan", r)
	}
	if r.PrdsDir != filepath.Join(repo, "docs", "prds") {
		t.Fatalf("prdsDir = %q", r.PrdsDir)
	}
}

// TestAuditStaleAfterOverride: the threshold is injectable.
func TestAuditStaleAfterOverride(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"two.md": 2 * day},
		[]*queue.QueuedTask{{ID: "two", Status: queue.StatusPending}},
	)
	r := AuditPrdCoverage(repo, AuditOptions{Now: testNowMs, StaleAfterMs: day})
	if len(r.Findings) != 1 || r.Findings[0].Kind != KindStale || r.StaleAfterMs != day {
		t.Fatalf("report = %+v, want one stale at 1d threshold", r)
	}
}

// TestAuditCustomNow vs default clock: nil Now uses the real clock (a fresh
// file reads as 0h, not negative).
func TestAuditDefaultClock(t *testing.T) {
	repo := t.TempDir()
	p := filepath.Join(repo, "docs", "prds", "now.md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# prd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Default clock = time.Now; a freshly written file reads as ~0 age and
	// never negative. (Exact 0 only holds when now lands in the file's
	// write millisecond — assert the invariant, not the coincidence.)
	r := AuditPrdCoverage(repo, AuditOptions{})
	if len(r.Findings) != 1 || r.Findings[0].Kind != KindUnqueued {
		t.Fatalf("findings = %+v", r.Findings)
	}
	if r.Findings[0].AgeMs < 0 || r.Findings[0].AgeMs >= 60_000 {
		t.Fatalf("ageMs = %d, want a fresh file: 0 <= age < 1min", r.Findings[0].AgeMs)
	}
}

// TestFormatPrdAge pins the day/hour rendering incl. the never-negative
// clamp.
func TestFormatPrdAge(t *testing.T) {
	cases := []struct {
		ageMs int64
		want  string
	}{
		{0, "0h"},
		{-5_000, "0h"},
		{59 * 60 * 1000, "0h"},
		{60 * 60 * 1000, "1h"},
		{23*60*60*1000 + 59*60*1000, "23h"},
		{day, "1d"},
		{20 * day, "20d"},
	}
	for _, c := range cases {
		if got := FormatPrdAge(c.ageMs); got != c.want {
			t.Errorf("FormatPrdAge(%d) = %q, want %q", c.ageMs, got, c.want)
		}
	}
}

// TestFormatAuditReport pins the exact [prd-audit] rendering (summary +
// warning lines) the CLI forwards.
func TestFormatAuditReport(t *testing.T) {
	report := AuditReport{
		RepoPath: "/repo", PrdsDir: "/repo/docs/prds",
		Scanned: 2, Tasks: 1, StaleAfterMs: DefaultStaleAfterMs,
		Findings: []PrdAuditFinding{
			{Kind: KindUnqueued, File: "docs/prds/a.md", Stem: "a", AgeMs: day, TaskIDs: []string{}, Warning: "w-one"},
			{Kind: KindStale, File: "docs/prds/b.md", Stem: "b", AgeMs: 20 * day, TaskIDs: []string{"t1"}, Warning: "w-two"},
		},
		Warnings: []string{"w-one", "w-two"},
	}
	want := "[prd-audit] scanned 2 PRD(s) in /repo/docs/prds against 1 queue task(s) — 2 warning(s), advisory only (Q15: no enqueue)\n" +
		"[prd-audit] warn unqueued w-one\n" +
		"[prd-audit] warn stale w-two"
	if got := FormatAuditReport(report); got != want {
		t.Fatalf("FormatAuditReport =\n%q\nwant\n%q", got, want)
	}
}

// TestFormatAuditReportClean: no findings -> just the summary line.
func TestFormatAuditReportClean(t *testing.T) {
	report := AuditReport{RepoPath: "/repo", PrdsDir: "/repo/docs/prds", StaleAfterMs: DefaultStaleAfterMs}
	want := "[prd-audit] scanned 0 PRD(s) in /repo/docs/prds against 0 queue task(s) — 0 warning(s), advisory only (Q15: no enqueue)"
	if got := FormatAuditReport(report); got != want {
		t.Fatalf("FormatAuditReport =\n%q\nwant\n%q", got, want)
	}
}

// TestAuditJSONRoundTrip: the report marshals to the TS --json shape
// (findings carry kind/file/stem/ageMs/taskIds/warning).
func TestAuditJSONRoundTrip(t *testing.T) {
	repo := seedRepo(t,
		map[string]int64{"orphan.md": day},
		nil,
	)
	r := scan(t, repo)
	blob, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Findings []struct {
			Kind    string   `json:"kind"`
			File    string   `json:"file"`
			Stem    string   `json:"stem"`
			AgeMs   int64    `json:"ageMs"`
			TaskIds []string `json:"taskIds"`
			Warning string   `json:"warning"`
		} `json:"findings"`
		Enqueued int `json:"enqueued"`
	}
	if err := json.Unmarshal(blob, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Findings) != 1 || probe.Findings[0].Kind != "unqueued" || probe.Findings[0].Stem != "orphan" || probe.Enqueued != 0 {
		t.Fatalf("json probe = %+v", probe)
	}
}
