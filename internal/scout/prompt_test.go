package scout

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/trust"
)

// Ported from test/prompt.test.ts and the marker-injection half of
// test/knowledge-context.test.ts.

func healthPlan() ImplementationPlan {
	return PlanFromTicket(TicketSpec{
		ID:                 "ENG-7",
		Title:              "Add GET /health endpoint",
		Description:        "Returns service status JSON with uptime.",
		Labels:             nil,
		AcceptanceCriteria: []string{"returns 200", "includes uptime"},
	})
}

func TestBuildImplementationPrompt(t *testing.T) {
	p := BuildImplementationPrompt(healthPlan(), "")
	for _, want := range []string{"GET /health", "- returns 200", "1. Define route/handler"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "Lessons from previous runs") {
		t.Error("empty lessons must not render the section")
	}
	migration := PlanFromTicket(TicketSpec{ID: "ENG-8", Title: "Alter table users add column", Description: "Schema change adding a nullable column to users table."})
	p2 := BuildImplementationPrompt(migration, "")
	if !strings.Contains(p2, "down-migration") || !strings.Contains(p2, "expand-first") {
		t.Error("migration plan must carry expand-first + down-migration instructions")
	}
	if !strings.Contains(p2, "- (none provided)") {
		t.Error("empty acceptance criteria must render the (none provided) placeholder")
	}
	withLessons := BuildImplementationPrompt(healthPlan(), "Run npm test before claiming done.")
	if !strings.Contains(withLessons, "## Lessons from previous runs") || !strings.Contains(withLessons, "Run npm test before claiming done.") {
		t.Error("lessons must inject when non-empty")
	}
}

func TestBuildRepairPrompt(t *testing.T) {
	p := BuildRepairPrompt(healthPlan(), 2, "FAIL src/health.test.ts\n  expected 200 got 500", "", "")
	for _, want := range []string{"attempt (2)", "expected 200 got 500", "Fix the issues"} {
		if !strings.Contains(p, want) {
			t.Errorf("repair prompt missing %q", want)
		}
	}
	withLessons := BuildRepairPrompt(healthPlan(), 2, "tests failed", "Never drop columns without a down-migration.", "")
	if !strings.Contains(withLessons, "## Lessons from previous runs") || !strings.Contains(withLessons, "Never drop columns without a down-migration.") {
		t.Error("repair prompt must append lessons")
	}
	// Empty failure evidence renders the placeholder.
	empty := BuildRepairPrompt(healthPlan(), 1, "   ", "", "")
	if !strings.Contains(empty, "(no output captured)") {
		t.Error("blank failure evidence must render (no output captured)")
	}
}

// --- lessons feedback loop (PRD Phase 4) ---

func writeLessons(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadLessonsAbsent(t *testing.T) {
	if got := LoadLessons(t.TempDir(), "", 0); got != "" {
		t.Fatalf("absent lessons file must read '' (got %q)", got)
	}
}

func TestLoadLessonsDefaultPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".devagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DefaultLessonsFile), []byte("Keep migrations expand-first.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadLessons(dir, "", 0); got != "Keep migrations expand-first." {
		t.Fatalf("got %q", got)
	}
}

func TestLoadLessonsHonorsOverrideAndLineCap(t *testing.T) {
	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, fmt.Sprintf("lesson line %d", i))
	}
	dir := writeLessons(t, "custom-lessons.md", strings.Join(lines, "\n")+"\n")
	loaded := LoadLessons(dir, "custom-lessons.md", 0)
	got := strings.Split(loaded, "\n")
	if len(got) != 40 {
		t.Fatalf("window = %d lines, want 40", len(got))
	}
	if !strings.Contains(loaded, "lesson line 49") {
		t.Error("newest line must survive the window")
	}
	if strings.Contains(loaded, "lesson line 0\n") {
		t.Error("oldest line must be cut by the window")
	}
}

func TestLoadLessonsCharBudgetDropsOldestWhole(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, strings.Repeat("x", 900)+fmt.Sprintf(" entry-%d", i))
	}
	dir := writeLessons(t, "custom-lessons.md", strings.Join(lines, "\n")+"\n")
	loaded := LoadLessons(dir, "custom-lessons.md", 2000)
	got := strings.Split(loaded, "\n")
	if len(got) != 2 {
		t.Fatalf("kept %d entries, want 2 (a third would overflow)", len(got))
	}
	if !strings.Contains(loaded, "entry-9") || !strings.Contains(loaded, "entry-8") {
		t.Error("newest entries must survive")
	}
	if strings.Contains(loaded, "entry-7") {
		t.Error("budget must drop entries whole, newest-first")
	}
}

func TestLoadLessonsDropsOldOversizedEntryWhole(t *testing.T) {
	huge := strings.Repeat("y", 5000) + " tail-marker"
	dir := writeLessons(t, "custom-lessons.md", huge+"\nsmall note\n")
	if got := LoadLessons(dir, "custom-lessons.md", 100); got != "small note" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadLessonsKeepsSingleOversizedLineWhole(t *testing.T) {
	huge := strings.Repeat("z", 5000) + " keep-marker"
	dir := writeLessons(t, "custom-lessons.md", huge+"\n")
	if got := LoadLessons(dir, "custom-lessons.md", 100); got != huge {
		t.Fatal("a single newest oversized line must surface whole, never split or empty")
	}
}

func TestLoadLessonsShortFileUntouched(t *testing.T) {
	dir := writeLessons(t, "custom-lessons.md", "a\nb\nc\n")
	if got := LoadLessons(dir, "custom-lessons.md", 4000); got != "a\nb\nc" {
		t.Fatalf("got %q", got)
	}
}

// --- lessons digest ranking by measured impact (Q39) ---

func writeEvents(t *testing.T, dir string, events []map[string]any) {
	t.Helper()
	eventsDir := filepath.Join(dir, ".devagent", "runs", "orchestration")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range events {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(eventsDir, "events.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLessonsDigestRanksByScore(t *testing.T) {
	high := "First lesson that prevents failures."
	low := "Second lesson that correlates with failures."
	unscored := "Third lesson with no ledger rows yet."
	dir := writeLessons(t, "lessons.md", strings.Join([]string{high, low, unscored}, "\n")+"\n")
	writeEvents(t, dir, []map[string]any{
		{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(high), "accepted": true, "loop": 1},
		{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(low), "accepted": false, "loop": 2},
		{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "loop-result", "loop": 1, "status": "ok"},
		{"ts": "2026-01-01T04:00:00Z", "kind": "event", "event": "loop-result", "loop": 2, "status": "failed"},
	})
	// Budget fits roughly one line: the highest-scored lesson survives whole.
	digest := LoadLessonsDigest(dir, "lessons.md", 50)
	if !strings.Contains(digest, high) {
		t.Errorf("high-score lesson must survive: %q", digest)
	}
	if strings.Contains(digest, low) || strings.Contains(digest, unscored) {
		t.Errorf("low/unscored lessons must be dropped: %q", digest)
	}
}

func TestLoadLessonsDigestOutputsFileOrder(t *testing.T) {
	low := "Low score lesson first in file."
	high := "High score lesson later in file."
	dir := writeLessons(t, "lessons.md", strings.Join([]string{low, high}, "\n")+"\n")
	writeEvents(t, dir, []map[string]any{
		{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(high), "accepted": true, "loop": 1},
		{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(low), "accepted": false, "loop": 2},
		{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "loop-result", "loop": 1, "status": "ok"},
		{"ts": "2026-01-01T04:00:00Z", "kind": "event", "event": "loop-result", "loop": 2, "status": "failed"},
	})
	digest := LoadLessonsDigest(dir, "lessons.md", 4000)
	if digest != strings.Join([]string{low, high}, "\n") {
		t.Fatalf("surviving lines must keep file order, got %q", digest)
	}
}

func TestLoadLessonsDigestTiesBreakOldestFirst(t *testing.T) {
	older := "Older lesson with a perfect record."
	newer := "Newer lesson with a perfect record."
	dir := writeLessons(t, "lessons.md", strings.Join([]string{older, newer}, "\n")+"\n")
	writeEvents(t, dir, []map[string]any{
		{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(older), "accepted": true, "loop": 1},
		{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(newer), "accepted": true, "loop": 2},
		{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "loop-result", "loop": 1, "status": "ok"},
		{"ts": "2026-01-01T04:00:00Z", "kind": "event", "event": "loop-result", "loop": 2, "status": "ok"},
	})
	// Both score 1 (accepted, in ok loops, no overall failures). Budget fits one line.
	digest := LoadLessonsDigest(dir, "lessons.md", len(older)+5)
	if digest != older {
		t.Fatalf("score ties must break oldest-first, got %q", digest)
	}
}

func TestLoadLessonsDigestRecencyWithoutScores(t *testing.T) {
	var lines []string
	for i := 0; i < 10; i++ {
		lines = append(lines, strings.Repeat("x", 900)+fmt.Sprintf(" entry-%d", i))
	}
	dir := writeLessons(t, "lessons.md", strings.Join(lines, "\n")+"\n")
	digest := LoadLessonsDigest(dir, "lessons.md", 2000)
	if !strings.Contains(digest, "entry-9") || !strings.Contains(digest, "entry-8") || strings.Contains(digest, "entry-7") {
		t.Fatalf("unscored digest must keep newest-first recency: %q", digest[:80])
	}
}

func TestLessonExcerptHashNormalizes(t *testing.T) {
	base := "Lessons eval guard rejects duplicates."
	variants := []string{
		"Lessons-eval-guard rejects duplicates!",
		"LESSONS EVAL GUARD REJECTS DUPLICATES.",
		base + " [predictedImpact: high]",
	}
	want := LessonExcerptHash(base)
	for _, v := range variants {
		if got := LessonExcerptHash(v); got != want {
			t.Errorf("normalized hash mismatch for %q: %s != %s", v, got, want)
		}
	}
	if len(want) != 16 {
		t.Errorf("hash length = %d, want 16 hex chars", len(want))
	}
	if LessonExcerptHash("a completely different lesson about migrations") == want {
		t.Error("different content must hash differently")
	}
}

// --- knowledge digest (FR-CTX-01/02/03) ---

func TestKnowledgeDigestBaseline(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, KnowledgeContextDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a-domain.md"), []byte("first line\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b-arch.md"), []byte("second line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := BuildKnowledgeContext(repo, KnowledgeOptions{})
	want := KnowledgeContextHeader + "\nfirst line\nsecond line"
	if got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
	if strings.Contains(got, "ignored") {
		t.Error("non-markdown files must stay out of the digest")
	}
}

func TestKnowledgeDigestAbsentDir(t *testing.T) {
	if got := BuildKnowledgeContext(t.TempDir(), KnowledgeOptions{}); got != "" {
		t.Fatalf("absent context dir must degrade to noop, got %q", got)
	}
}

func TestKnowledgeDigestRatchet(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, KnowledgeContextDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 0; i < 10; i++ {
		fmt.Fprintf(&b, "entry-%d %s\n", i, strings.Repeat("x", 500))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.md"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	maxChars := 1200
	got := BuildKnowledgeContext(repo, KnowledgeOptions{MaxChars: &maxChars})
	body := strings.TrimPrefix(got, KnowledgeContextHeader+"\n")
	lines := strings.Split(body, "\n")
	if len(lines) != 2 {
		t.Fatalf("kept %d entries, want the newest 2 only", len(lines))
	}
	if !strings.Contains(lines[0], "entry-8") || !strings.Contains(lines[1], "entry-9") {
		t.Errorf("ratchet must drop oldest whole: %q", body[:60])
	}
}

func TestKgLayerIsOptInAndNeverBlocks(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, KnowledgeContextDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.md"), []byte("baseline\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// off by default: the provider is never consulted.
	called := false
	_ = BuildKnowledgeContext(repo, KnowledgeOptions{KgProvider: func() string { called = true; return "x" }})
	if called {
		t.Fatal("kg=off must not consult the provider (FR-CTX-03)")
	}
	// unreachable provider (panic = JS throw) degrades to baseline-only.
	got := BuildKnowledgeContext(repo, KnowledgeOptions{Kg: "leankg", KgProvider: func() string { panic("ECONNREFUSED") }})
	if got != KnowledgeContextHeader+"\nbaseline" {
		t.Fatalf("panicking provider must degrade to baseline, got %q", got)
	}
	// absent provider under kg=leankg still yields the baseline digest.
	got = BuildKnowledgeContext(repo, KnowledgeOptions{Kg: "leankg"})
	if got != KnowledgeContextHeader+"\nbaseline" {
		t.Fatalf("absent provider must yield baseline, got %q", got)
	}
	// leankg content layers under its sub-header.
	got = BuildKnowledgeContext(repo, KnowledgeOptions{Kg: "leankg", KgProvider: func() string { return "kg line" }})
	want := KnowledgeContextHeader + "\nbaseline\n" + KgContextSubheader + "\nkg line"
	if got != want {
		t.Fatalf("kg layer = %q, want %q", got, want)
	}
}

func TestAgentsMdLayerBehindTrustGate(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".devagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, trust.AgentsMdFile), []byte("use gofumpt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ask (default) without the one-time confirm: nothing injected.
	if got := BuildKnowledgeContext(repo, KnowledgeOptions{}); got != "" {
		t.Fatalf("untrusted AGENTS.md must not inject, got %q", got)
	}
	// on: injects without the confirm.
	got := BuildKnowledgeContext(repo, KnowledgeOptions{AgentsMd: trust.ModeOn})
	if got != KnowledgeContextHeader+"\n"+AgentsMdSubheader+"\nuse gofumpt" {
		t.Fatalf("agentsMd=on digest = %q", got)
	}
	// off: never reads the file.
	if got := BuildKnowledgeContext(repo, KnowledgeOptions{AgentsMd: trust.ModeOff}); got != "" {
		t.Fatalf("agentsMd=off digest = %q", got)
	}
}

// --- injection at COMPACT_CONTEXT_MARKER (FR-CTX-01) ---

func TestPlannerPromptCarriesKnowledgeBelowGoal(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, KnowledgeContextDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ctx.md"), []byte("domain fact\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := BuildPlannerPrompt("Ship the feature", repo, PlannerPromptOptions{})
	if !strings.Contains(prompt, "## Goal\nShip the feature") {
		t.Fatal("goal section missing")
	}
	idx := strings.Index(prompt, "## Goal")
	kIdx := strings.Index(prompt, KnowledgeContextHeader)
	if kIdx < idx {
		t.Fatal("knowledge digest must splice below the goal section")
	}
	if !strings.Contains(prompt, "domain fact") {
		t.Fatal("digest content missing from planner prompt")
	}
}

func TestPlannerPromptMergesTrailsAndKnowledgeAtMarker(t *testing.T) {
	repo := t.TempDir()
	kdir := filepath.Join(repo, KnowledgeContextDir)
	if err := os.MkdirAll(kdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kdir, "ctx.md"), []byte("knowledge line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trailDir := filepath.Join(repo, TrailsRoot, "loop-7")
	if err := os.MkdirAll(trailDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(trailDir, "T1.jsonl"), []byte("{\"event\":\"done\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prompt := BuildPlannerPrompt("goal", repo, PlannerPromptOptions{LoopID: "loop-7", TaskID: "T1"})
	trailIdx := strings.Index(prompt, CompactContextMarker)
	kIdx := strings.Index(prompt, KnowledgeContextHeader)
	if trailIdx < 0 || kIdx < 0 || kIdx < trailIdx {
		t.Fatalf("trail section must precede the knowledge section in the marker slot (trail=%d knowledge=%d)", trailIdx, kIdx)
	}
	if !strings.Contains(prompt, "### Trail for T1") {
		t.Fatal("trail block missing")
	}
	// The marker sits at a fixed offset: the prefix above it is stable.
	if !strings.HasPrefix(prompt, PlannerSystemPrompt) {
		t.Fatal("planner prefix must stay cacheable above the marker")
	}
}

func TestPlannerPromptUnchangedWithoutContext(t *testing.T) {
	repo := t.TempDir()
	prompt := BuildPlannerPrompt("goal text", repo, PlannerPromptOptions{})
	want := PlannerSystemPrompt + "\n\n## Goal\ngoal text\n\n" + CompactContextMarker
	if prompt != want {
		t.Fatalf("no-context planner prompt must be byte-identical to the pre-feature shape:\ngot:  %q\nwant: %q", prompt, want)
	}
}

func TestRepairPromptCarriesKnowledgeAndStaysByteIdenticalWithout(t *testing.T) {
	plan := healthPlan()
	without := BuildRepairPrompt(plan, 1, "boom", "", "")
	if strings.Contains(without, KnowledgeContextHeader) {
		t.Fatal("empty knowledge must leave the repair prompt unchanged")
	}
	with := BuildRepairPrompt(plan, 1, "boom", "", KnowledgeContextHeader+"\nfact")
	if !strings.HasSuffix(with, KnowledgeContextHeader+"\nfact") {
		t.Fatalf("knowledge must append at the tail (marker absent), got %q", with[len(with)-60:])
	}
	if !strings.HasPrefix(with, without) {
		t.Fatal("the pre-knowledge prompt must stay a prefix of the spliced one")
	}
}

func TestIngestChildTrailsDrainsWorklogs(t *testing.T) {
	repo := t.TempDir()
	w1 := filepath.Join(repo, ".selfbuild", "loops", "loop-9", "workers", "alpha")
	w2 := filepath.Join(repo, ".selfbuild", "loops", "loop-9", "workers", "beta")
	for _, d := range []string{w1, w2} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(w1, "worklog.jsonl"), []byte("{\"a\":1}\n{\"b\":2}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w2, "worklog.jsonl"), []byte("{\"c\":3}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := IngestChildTrails("loop-9", "T1", nil, repo); n != 3 {
		t.Fatalf("ingested %d lines, want 3", n)
	}
	raw, err := os.ReadFile(filepath.Join(repo, TrailsRoot, "loop-9", "T1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), "\n") != 3 {
		t.Fatalf("trail file content = %q", raw)
	}
	// The next planner prompt sees the freshly ingested trail.
	prompt := BuildPlannerPrompt("g", repo, PlannerPromptOptions{LoopID: "loop-9", TaskID: "T1"})
	if !strings.Contains(prompt, "### Trail for T1") {
		t.Fatal("planner prompt must carry the ingested trail")
	}
	// No loop/task ids: no-op.
	if n := IngestChildTrails("", "", nil, repo); n != 0 {
		t.Fatalf("empty ids must no-op, got %d", n)
	}
}

func TestBuildChildTrailsDigest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trail.jsonl")
	var b strings.Builder
	for i := 0; i < 6; i++ {
		fmt.Fprintf(&b, "{\"i\":%d} %s\n", i, strings.Repeat("y", 300))
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	digest, dropped, total := BuildChildTrailsDigest([]string{p, filepath.Join(dir, "missing.jsonl")}, 700)
	if total != 6 {
		t.Fatalf("total = %d", total)
	}
	if dropped == 0 {
		t.Fatal("budget must drop oldest entries")
	}
	lines := strings.Split(digest, "\n")
	if !strings.Contains(lines[len(lines)-1], "\"i\":5") {
		t.Fatal("newest entry must survive")
	}
	if strings.Contains(digest, "\"i\":0") {
		t.Fatal("oldest entry must be dropped whole")
	}
	// Empty input set: noop digest.
	d2, dr2, t2 := BuildChildTrailsDigest([]string{filepath.Join(dir, "nope.jsonl")}, 0)
	if d2 != "" || dr2 != 0 || t2 != 0 {
		t.Fatalf("missing files must yield the empty digest, got %q %d %d", d2, dr2, t2)
	}
}

func TestSpliceCompactContextNoopAndAppend(t *testing.T) {
	// No marker, no sections: byte-identical.
	if got := SpliceCompactContext("prompt", "", "", nil); got != "prompt" {
		t.Fatalf("noop splice = %q", got)
	}
	// No marker, knowledge present: appended at the tail.
	got := SpliceCompactContext("prompt", "", "", &SpliceOptions{Knowledge: "  ## Knowledge Context\nfact  "})
	if got != "prompt\n\n## Knowledge Context\nfact" {
		t.Fatalf("append splice = %q", got)
	}
	// Marker present: replaced in place, first occurrence only.
	got = SpliceCompactContext("head\n"+CompactContextMarker+"\ntail", "", "", &SpliceOptions{Knowledge: "## Knowledge Context\nfact"})
	if got != "head\n## Knowledge Context\nfact\ntail" {
		t.Fatalf("marker splice = %q", got)
	}
}
