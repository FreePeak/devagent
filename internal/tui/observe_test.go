package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Port of test/observe.test.ts: the dashboard HTML generator must render the
// identical board model from identical JSONL input.

func writeRuns(t *testing.T, files map[string]string) string {
	t.Helper()
	home := t.TempDir()
	dir := filepath.Join(home, "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func strPtr(s string) *string { return &s }

func testSummary(over RunSummary) RunSummary {
	base := RunSummary{
		RunID:       "aaaa1111",
		File:        "a.jsonl",
		StartedAt:   strPtr("2026-08-24T03:00:00Z"),
		LastAt:      strPtr("2026-08-24T03:05:00Z"),
		LastStage:   "implement",
		LastLevel:   "info",
		LastMessage: "ok",
		EventCount:  2,
		Ok:          true,
		Timeline: []RunEvent{{
			Ts: "2026-08-24T03:01:00Z", Stage: "implement", Level: "info", Message: "working",
		}},
	}
	if over.RunID != "" {
		base.RunID = over.RunID
	}
	if over.StartedAt != nil {
		base.StartedAt = over.StartedAt
	}
	if over.Title != nil {
		base.Title = over.Title
	}
	if over.PrURL != nil {
		base.PrURL = over.PrURL
	}
	if over.Ticket != nil {
		base.Ticket = over.Ticket
	}
	if over.Repo != nil {
		base.Repo = over.Repo
	}
	if over.DurationMs != nil {
		base.DurationMs = over.DurationMs
	}
	if over.ExitCode != nil {
		base.ExitCode = over.ExitCode
	}
	if over.LastAt != nil {
		base.LastAt = over.LastAt
	}
	if over.LastStage != "" {
		base.LastStage = over.LastStage
	}
	if over.LastLevel != "" {
		base.LastLevel = over.LastLevel
	}
	if over.LastMessage != "" {
		base.LastMessage = over.LastMessage
	}
	if over.EventCount != 0 {
		base.EventCount = over.EventCount
	}
	if over.TimedOut || over.LastLevel == "error" {
		base.Ok = false
		base.TimedOut = over.TimedOut
	}
	if over.Timeline != nil {
		base.Timeline = over.Timeline
	}
	return base
}

func TestCollectRunSummariesFirstLast(t *testing.T) {
	dir := writeRuns(t, map[string]string{
		"run1.jsonl": `{"runId":"aaaa1111","ts":"T1","stage":"fetch","level":"info","message":"start"}
{"runId":"aaaa1111","ts":"T2","stage":"publish","level":"info","message":"PR opened"}
`,
		"run2.jsonl": `{"runId":"bbbb2222","ts":"T3","stage":"validate","level":"error","message":"gate failed"}
`,
	})
	s := CollectRunSummaries(dir)
	if len(s) != 2 {
		t.Fatalf("summaries = %d, want 2", len(s))
	}
	var okRun, errRun *RunSummary
	for i := range s {
		switch s[i].RunID {
		case "aaaa1111":
			okRun = &s[i]
		case "bbbb2222":
			errRun = &s[i]
		}
	}
	if okRun == nil || errRun == nil {
		t.Fatal("run ids not found")
	}
	if okRun.EventCount != 2 || !okRun.Ok || okRun.LastStage != "publish" {
		t.Fatalf("ok run = %+v", *okRun)
	}
	if errRun.Ok {
		t.Fatal("error-level last event must mark the run failed")
	}
}

func TestCollectRunSummariesMissingDirs(t *testing.T) {
	if got := CollectRunSummaries("/nonexistent-da"); len(got) != 0 {
		t.Fatalf("missing dir = %v", got)
	}
	if got := CollectRunSummaries(t.TempDir()); len(got) != 0 {
		t.Fatalf("empty dir = %v", got)
	}
}

func TestCollectRunSummariesSkipsMalformed(t *testing.T) {
	dir := writeRuns(t, map[string]string{
		"r.jsonl": "{not json\n" +
			`{"runId":"cccc3333","ts":"T","stage":"s","level":"info","message":"m"}` + "\n",
	})
	s := CollectRunSummaries(dir)
	if len(s) != 1 {
		t.Fatalf("summaries = %d", len(s))
	}
	if s[0].EventCount != 2 {
		t.Fatalf("eventCount counts raw lines: %d", s[0].EventCount)
	}
}

func TestCollectRunSummariesEnrichment(t *testing.T) {
	var events []string
	for i := 0; i < 80; i++ {
		events = append(events, fmt.Sprintf(
			`{"runId":"ffff6666","ts":"2026-08-24T00:%02d:00Z","stage":"implement","level":"info","message":"step %d"}`, i%60, i))
	}
	events = append(events, `{"runId":"ffff6666","ts":"2026-08-24T01:00:00Z","stage":"publish","level":"info","message":"PR opened: https://github.com/acme/api/pull/42.","data":{"title":"Fix gate G2","repo":"/repos/api","ticket":"LIN-9","durationMs":95000,"exitCode":0}}`)
	dir := writeRuns(t, map[string]string{"r.jsonl": strings.Join(events, "\n")})
	s := CollectRunSummaries(dir)
	if len(s) != 1 {
		t.Fatalf("summaries = %d", len(s))
	}
	run := s[0]
	if derefStr(run.Title) != "Fix gate G2" || derefStr(run.Repo) != "/repos/api" ||
		derefStr(run.Ticket) != "LIN-9" {
		t.Fatalf("enrichment = %+v", run)
	}
	if run.DurationMs == nil || *run.DurationMs != 95000 {
		t.Fatalf("durationMs = %v", run.DurationMs)
	}
	if run.ExitCode == nil || *run.ExitCode != 0 {
		t.Fatalf("exitCode = %v", run.ExitCode)
	}
	if derefStr(run.PrURL) != "https://github.com/acme/api/pull/42" {
		t.Fatalf("prUrl = %v (trailing punctuation must be stripped)", derefStr(run.PrURL))
	}
	if len(run.Timeline) != 50 {
		t.Fatalf("timeline cap = %d", len(run.Timeline))
	}
	if !strings.Contains(run.Timeline[len(run.Timeline)-1].Message, "PR opened") {
		t.Fatal("timeline must keep the newest events")
	}
}

func TestDeriveRunStatus(t *testing.T) {
	// ok:false and timedOut both mean failed.
	failed := testSummary(RunSummary{})
	failed.Ok = false
	timedOut := testSummary(RunSummary{})
	timedOut.TimedOut = true
	done := testSummary(RunSummary{})
	done.PrURL = strPtr("https://x/pull/1")
	todo := testSummary(RunSummary{Timeline: []RunEvent{{Ts: "t", Stage: "plan", Level: "info", Message: "planning"}}})
	cases := []struct {
		s    RunSummary
		want CardStatus
	}{
		{failed, CardFailed},
		{timedOut, CardFailed},
		{done, CardDone},
		{testSummary(RunSummary{}), CardInProgress}, // implement-stage activity
		{todo, CardTodo},
	}
	for _, c := range cases {
		if got := DeriveRunStatus(c.s); got != c.want {
			t.Fatalf("deriveRunStatus(%+v) = %s, want %s", c.s, got, c.want)
		}
	}
}

func TestMapBoardStatus(t *testing.T) {
	for status, want := range map[string]CardStatus{
		"pending": CardTodo, "ready": CardTodo, "blocked": CardTodo, "ask": CardTodo,
		"dispatched": CardInProgress, "untrusted": CardInProgress,
		"done": CardDone, "failed": CardFailed, "mystery": CardTodo,
	} {
		if got := MapBoardStatus(status); got != want {
			t.Fatalf("mapBoardStatus(%s) = %s, want %s", status, got, want)
		}
	}
}

func TestDeriveLabel(t *testing.T) {
	base := testSummary(RunSummary{})
	if got := DeriveLabel(base); got != "working" {
		t.Fatalf("informative message = %q", got)
	}
	ticketed := testSummary(RunSummary{Ticket: strPtr("LIN-204"), Timeline: []RunEvent{}})
	if got := DeriveLabel(ticketed); got != "LIN-204" {
		t.Fatalf("ticket fallback = %q", got)
	}
	fetched := testSummary(RunSummary{Timeline: []RunEvent{
		{Ts: "t", Stage: "fetch", Level: "info", Message: "Run aaaa1111-2222 starting"},
		{Ts: "t", Stage: "fetch", Level: "info", Message: "Fetched ticket from linear: implement rate limiting"},
	}})
	if got := DeriveLabel(fetched); got != "Fetched ticket from linear: implement rate limiting" {
		t.Fatalf("informative pick = %q", got)
	}
	long := testSummary(RunSummary{Timeline: []RunEvent{
		{Message: strings.Repeat("x", 120)},
	}})
	if len(DeriveLabel(long)) > 90 {
		t.Fatalf("long label = %d runes", len([]rune(DeriveLabel(long))))
	}
	dispatch := testSummary(RunSummary{Timeline: []RunEvent{
		{Ts: "t", Stage: "task", Level: "info", Message: "Dispatching T1: T1"},
		{Ts: "t", Stage: "task", Level: "info", Message: "T1 done (audited)"},
	}})
	if got := DeriveLabel(dispatch); got != "Orchestrator dispatch loop" {
		t.Fatalf("dispatch loop label = %q", got)
	}
	uuidish := testSummary(RunSummary{Timeline: []RunEvent{}})
	if !regexp.MustCompile(`^[0-9a-f]{8}$`).MatchString(DeriveLabel(uuidish)) {
		t.Fatalf("bare uuid fallback = %q", DeriveLabel(uuidish))
	}
}

func TestLoadBoards(t *testing.T) {
	dir := t.TempDir()
	board := map[string]any{
		"goal": "ship payments",
		"tasks": []any{
			map[string]any{"id": "T1", "title": "Add ledger table", "status": "done", "attempts": 2},
			map[string]any{"id": "T2", "prompt": "Wire webhook retries", "status": "ask", "evidenceGaps": []any{"no retry test"}},
			map[string]any{"id": "T3", "title": "Bad row skipped below"},
		},
	}
	raw, _ := json.Marshal(board)
	if err := os.WriteFile(filepath.Join(dir, ".devagent-project.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	boards := LoadBoards([]string{dir, "/nonexistent-da", ""})
	if len(boards) != 1 {
		t.Fatalf("boards = %d", len(boards))
	}
	b := boards[0]
	if b.Goal != "ship payments" {
		t.Fatalf("goal = %q", b.Goal)
	}
	if b.Tasks[0].Attempts == nil || *b.Tasks[0].Attempts != 2 {
		t.Fatalf("attempts = %v", b.Tasks[0].Attempts)
	}
	if b.Tasks[1].Title != "Wire webhook retries" {
		t.Fatalf("prompt fallback = %q", b.Tasks[1].Title)
	}
	if len(b.Tasks[1].EvidenceGaps) != 1 || b.Tasks[1].EvidenceGaps[0] != "no retry test" {
		t.Fatalf("evidenceGaps = %v", b.Tasks[1].EvidenceGaps)
	}
	if b.Tasks[2].Status != "pending" {
		t.Fatalf("default status = %q", b.Tasks[2].Status)
	}
}

func TestLoadBoardsCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".devagent-project.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadBoards([]string{dir}); len(got) != 0 {
		t.Fatalf("corrupt board = %v", got)
	}
}

func TestRenderDashboardHTMLEscapes(t *testing.T) {
	ResetEmbedSeq()
	html := RenderDashboardHTML([]RunSummary{
		testSummary(RunSummary{RunID: "ddd44444", LastLevel: "error", LastMessage: "<script>alert(1)</script>", Ok: false}),
	}, "", nil)
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("message must be escaped")
	}
	if strings.Contains(html, "<script>alert") {
		t.Fatal("raw script injection must not appear")
	}
	if !strings.Contains(html, `details class="run failed"`) {
		t.Fatal("failed runs must carry the triage status class")
	}
}

func TestRenderDashboardHTMLTabs(t *testing.T) {
	ResetEmbedSeq()
	html := RenderDashboardHTML([]RunSummary{
		testSummary(RunSummary{}),
		testSummary(RunSummary{RunID: "bbbb2222", StartedAt: strPtr("2026-08-23T09:00:00Z"), Title: strPtr("Feature X"), PrURL: strPtr("https://github.com/a/b/pull/7")}),
		testSummary(RunSummary{RunID: "cccc3333", StartedAt: strPtr("2026-08-24T05:00:00Z"), Title: strPtr("Feature X"), Ok: false, LastLevel: "error"}),
	}, "", nil)
	for _, tab := range []string{"tb-board", "tb-runs", "tb-features"} {
		if !strings.Contains(html, `id="`+tab+`"`) {
			t.Fatalf("missing tab %s", tab)
		}
	}
	for _, col := range []string{"todo", "in progress", "done", "failed"} {
		if !strings.Contains(html, col) {
			t.Fatalf("missing column %s", col)
		}
	}
	if !regexp.MustCompile(`2026-08-24[\s\S]*?2026-08-23`).MatchString(html) {
		t.Fatal("runs must group under per-day headers, newest first")
	}
	if got := strings.Count(html, `<summary><span class="rid">Feature X</span>`); got != 1 {
		t.Fatalf("Feature X group count = %d, want 1", got)
	}
	if !strings.Contains(html, "https://github.com/a/b/pull/7") {
		t.Fatal("PR link must render")
	}
	if !strings.Contains(html, `type="application/json"`) {
		t.Fatal("lazy detail embeds must be present")
	}
	if regexp.MustCompile(`</script>[^<]*inside`).MatchString(html) {
		t.Fatal("embed must be breakout-proof")
	}
}

func TestRenderDashboardHTMLTimeline(t *testing.T) {
	ResetEmbedSeq()
	html := RenderDashboardHTML([]RunSummary{
		testSummary(RunSummary{Timeline: []RunEvent{{
			Ts: "2026-08-24T03:01:00Z", Stage: "implement", Level: "error",
			Message: "<img src=x onerror=alert(1)>",
		}}}),
	}, "", nil)
	if !strings.Contains(html, `<details class="run inprogress"`) {
		t.Fatal("implement-stage run must be inprogress")
	}
	if !strings.Contains(html, `class="timeline"`) {
		t.Fatal("timeline must render")
	}
	if !strings.Contains(html, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Fatal("timeline content must be escaped")
	}
	if strings.Contains(html, "<img src=x") {
		t.Fatal("raw img must not survive")
	}
}

func TestRenderDashboardHTMLEmpty(t *testing.T) {
	ResetEmbedSeq()
	html := RenderDashboardHTML(nil, "DevAgent Runs", nil)
	if !strings.Contains(html, "(0 runs)") {
		t.Fatal("empty state must report 0 runs")
	}
	if !strings.Contains(html, "No .devagent-project.json boards found") {
		t.Fatal("missing-board note must render")
	}
}

func TestRenderDashboardHTMLEmbedEscaping(t *testing.T) {
	ResetEmbedSeq()
	html := RenderDashboardHTML([]RunSummary{
		testSummary(RunSummary{Title: strPtr("Has </script> inside")}),
	}, "", nil)
	if !strings.Contains(html, `\u003c/script\u003e`) {
		t.Fatal("embed must escape angle brackets")
	}
}

func TestWriteDashboard(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "runs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "r.jsonl"),
		[]byte(`{"runId":"eeee5555","ts":"T","stage":"fetch","level":"info","message":"hello"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ResetEmbedSeq()
	res := WriteDashboard(home, []string{home})
	if res.Runs != 1 {
		t.Fatalf("runs = %d", res.Runs)
	}
	if res.Boards != 0 {
		t.Fatalf("boards = %d", res.Boards)
	}
	if filepath.Base(res.Path) != "dashboard.html" {
		t.Fatalf("path = %s", res.Path)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("dashboard not written: %v", err)
	}
}

// TestDashboardFixtureParity renders the pinned JSONL fixture and asserts the
// board model (not just substrings): identical input must yield the identical
// board model the Node generator produces.
func TestDashboardFixtureParity(t *testing.T) {
	dir := writeRuns(t, map[string]string{})
	fixture, err := os.ReadFile(filepath.Join("testdata", "observe", "runs", "run-parity.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run-parity.jsonl"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	summaries := CollectRunSummaries(dir)
	if len(summaries) != 1 {
		t.Fatalf("summaries = %d", len(summaries))
	}
	got := summaries[0]
	want := RunSummary{
		RunID:       "parity-0001",
		File:        "run-parity.jsonl",
		StartedAt:   strPtr("2026-09-01T10:00:00Z"),
		LastAt:      strPtr("2026-09-01T10:12:00Z"),
		LastStage:   "publish",
		LastLevel:   "info",
		LastMessage: "PR opened: https://github.com/FreePeak/devagent/pull/198.",
		EventCount:  4,
		Ok:          true,
		Title:       strPtr("FR-GO-11 parity fixture"),
		Repo:        strPtr("FreePeak/devagent"),
		Ticket:      strPtr("FR-GO-11"),
		DurationMs:  fptr(720000),
		ExitCode:    fptr(0),
		PrURL:       strPtr("https://github.com/FreePeak/devagent/pull/198"),
	}
	if got.RunID != want.RunID || got.File != want.File ||
		derefStr(got.StartedAt) != derefStr(want.StartedAt) ||
		derefStr(got.LastAt) != derefStr(want.LastAt) ||
		got.LastStage != want.LastStage || got.LastLevel != want.LastLevel ||
		got.LastMessage != want.LastMessage || got.EventCount != want.EventCount ||
		got.Ok != want.Ok || derefStr(got.Title) != derefStr(want.Title) ||
		derefStr(got.Repo) != derefStr(want.Repo) || derefStr(got.Ticket) != derefStr(want.Ticket) ||
		*got.DurationMs != *want.DurationMs || *got.ExitCode != *want.ExitCode ||
		derefStr(got.PrURL) != derefStr(want.PrURL) {
		t.Fatalf("board model diverged:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Timeline) != 4 {
		t.Fatalf("timeline = %d", len(got.Timeline))
	}
	if DeriveRunStatus(got) != CardDone {
		t.Fatal("PR presence must land the run in done")
	}
	if DeriveLabel(got) != "FR-GO-11 parity fixture" {
		t.Fatalf("label = %q", DeriveLabel(got))
	}
}
