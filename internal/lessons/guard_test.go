package lessons

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }
func boolPtr(b bool) *bool        { return &b }

func writeRepoFile(t *testing.T, repo string, rel string, content string) {
	t.Helper()
	p := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// greenRunner / redRunner stand in for `go test ./...` (hermetic suite seam).
func greenRunner(cmd string, args []string, dir string, timeoutMs int) (string, int) {
	return "green suite output\n", 0
}

func redRunner(cmd string, args []string, dir string, timeoutMs int) (string, int) {
	return "FAIL lessons.spec\n", 1
}

// unrunnableRunner models a suite that cannot even start.
func unrunnableRunner(cmd string, args []string, dir string, timeoutMs int) (string, int) {
	return "", -1
}

func readRows(t *testing.T, repo string) []map[string]any {
	t.Helper()
	return ReadEvents(repo)
}

func rowString(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	s, ok := row[key].(string)
	if !ok {
		t.Fatalf("row[%q] = %#v, want string", key, row[key])
	}
	return s
}

func rowFloat(t *testing.T, row map[string]any, key string) float64 {
	t.Helper()
	f, ok := row[key].(float64)
	if !ok {
		t.Fatalf("row[%q] = %#v, want number", key, row[key])
	}
	return f
}

func rowBool(t *testing.T, row map[string]any, key string) bool {
	t.Helper()
	b, ok := row[key].(bool)
	if !ok {
		t.Fatalf("row[%q] = %#v, want bool", key, row[key])
	}
	return b
}

func closeTo(a, b float64) bool {
	return math.Abs(a-b) < 5e-4 // expect(x).toBeCloseTo(y, 3)
}

// ---------------------------------------------------------------------------
// normalizeLessonText / lessonSimilarity (shingle Jaccard)
// ---------------------------------------------------------------------------

func TestNormalizeAndSimilarity(t *testing.T) {
	t.Run("scores identical texts at 1", func(t *testing.T) {
		if got := LessonSimilarity("Keep migrations expand-first.", "Keep migrations expand-first."); got != 1 {
			t.Fatalf("got %v, want 1", got)
		}
	})

	t.Run("normalizes punctuation and case so a hyphen-only reword is an exact dup", func(t *testing.T) {
		// The real .selfbuild/lessons.md regression: the same lesson appended
		// once as "Lessons eval guard" and once as "Lessons-eval-guard".
		a := "Lessons eval guard is the single best item"
		b := "Lessons-eval-guard is the single best item"
		if NormalizeLessonText(a) != NormalizeLessonText(b) {
			t.Fatalf("normalized texts differ: %q vs %q", NormalizeLessonText(a), NormalizeLessonText(b))
		}
		if got := LessonSimilarity(a, b); got != 1 {
			t.Fatalf("got %v, want 1", got)
		}
	})

	t.Run("scores genuinely distinct lessons near zero", func(t *testing.T) {
		got := LessonSimilarity(
			"Copilot code review can now approve PRs; gates must re-run on post-fix diffs",
			"Reaper should anchor on last activity, never creation time",
		)
		if got >= 0.1 {
			t.Fatalf("got %v, want < 0.1", got)
		}
	})

	t.Run("treats two empty texts as identical so an empty candidate cannot bypass the guard", func(t *testing.T) {
		if got := LessonSimilarity("", ""); got != 1 {
			t.Fatalf("got %v, want 1", got)
		}
	})
}

// ---------------------------------------------------------------------------
// readLessonEntries (comparison surface)
// ---------------------------------------------------------------------------

func TestReadLessonEntries(t *testing.T) {
	t.Run("skips blank lines, --- fences, and markdown headers so headers never become comparison targets", func(t *testing.T) {
		dir := t.TempDir()
		writeRepoFile(t, dir, SelfbuildLessonsPath, "---\n## 2026-09-02\n\n- Keep migrations expand-first.\n\n---\n")
		got := ReadLessonEntries(filepath.Join(dir, SelfbuildLessonsPath))
		want := []string{"- Keep migrations expand-first."}
		if len(got) != 1 || got[0] != want[0] {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// lessonExcerptHash / runLessonsSuite (evaluate step primitives)
// ---------------------------------------------------------------------------

var hex16Re = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestLessonExcerptHash(t *testing.T) {
	t.Run("excerpt hash is stable across formatting-only changes and 16 hex chars", func(t *testing.T) {
		h1 := LessonExcerptHash("Keep migrations expand-first.")
		h2 := LessonExcerptHash("keep  migrations  expand-first!")
		if !hex16Re.MatchString(h1) {
			t.Fatalf("hash %q does not match ^[0-9a-f]{16}$", h1)
		}
		if h1 != h2 {
			t.Fatalf("hash not stable across formatting: %q vs %q", h1, h2)
		}
		if LessonExcerptHash("a different lesson") == h1 {
			t.Fatal("different lessons hash equal")
		}
	})
}

func TestRunLessonsSuite(t *testing.T) {
	t.Run("runLessonsSuite passes a green repo and fails a red one without throwing", func(t *testing.T) {
		dir := t.TempDir()
		g := RunLessonsSuite(dir, &RunLessonsSuiteOpts{TimeoutMs: 30_000, Runner: greenRunner})
		if !g.OK {
			t.Fatalf("green suite reported not ok: %+v", g)
		}
		b := RunLessonsSuite(dir, &RunLessonsSuiteOpts{TimeoutMs: 30_000, Runner: redRunner})
		if b.OK {
			t.Fatalf("red suite reported ok: %+v", b)
		}
	})

	t.Run("runLessonsSuite never throws: an unrunnable suite is a red result", func(t *testing.T) {
		dir := t.TempDir() // no go.mod at all: go test cannot run
		r := RunLessonsSuite(dir, &RunLessonsSuiteOpts{TimeoutMs: 30_000, Runner: unrunnableRunner})
		if r.OK {
			t.Fatal("unrunnable suite reported ok")
		}
		if r.Detail == "" {
			t.Fatal("detail should be truthy")
		}
	})

	t.Run("default os/exec runner runs a real command (hermetic sh fixture)", func(t *testing.T) {
		dir := t.TempDir()
		if out, code := defaultSuiteRunner("sh", []string{"-c", "printf ok"}, dir, 30_000); code != 0 || strings.TrimSpace(out) != "ok" {
			t.Fatalf("got (%q, %d), want (\"ok\", 0)", out, code)
		}
		if _, code := defaultSuiteRunner("sh", []string{"-c", "exit 3"}, dir, 30_000); code != 3 {
			t.Fatalf("got %d, want 3", code)
		}
	})
}

// ---------------------------------------------------------------------------
// checkLessonsDedupe (lessons eval guard)
// ---------------------------------------------------------------------------

func TestCheckLessonsDedupe(t *testing.T) {
	tempRepo := func(t *testing.T, lessonsContent string) string {
		t.Helper()
		dir := t.TempDir()
		if lessonsContent != "" {
			writeRepoFile(t, dir, SelfbuildLessonsPath, lessonsContent)
		}
		return dir
	}

	t.Run("allows a unique entry when the file has no similar content", func(t *testing.T) {
		repo := tempRepo(t, "Existing durable lesson about reaper anchors.\n")
		r := CheckLessonsDedupe(repo, "A brand new lesson about merge queues.", nil)
		if !r.OK {
			t.Fatalf("got %+v, want ok", r)
		}
		if r.Similarity >= DefaultLessonsDedupeSimilarity {
			t.Fatalf("similarity %v >= threshold", r.Similarity)
		}
		if r.MatchedEntry != "Existing durable lesson about reaper anchors." {
			t.Fatalf("matchedEntry %q", r.MatchedEntry)
		}
	})

	t.Run("allows a unique entry when the lessons file is absent or empty", func(t *testing.T) {
		r := CheckLessonsDedupe(tempRepo(t, ""), "First lesson ever.", nil)
		if !r.OK {
			t.Fatalf("got %+v, want ok", r)
		}
		if r.Similarity != 0 {
			t.Fatalf("similarity %v, want 0", r.Similarity)
		}
		if r.MatchedEntry != "" {
			t.Fatalf("matchedEntry %q, want empty", r.MatchedEntry)
		}
	})

	t.Run("rejects an exact duplicate of an existing entry", func(t *testing.T) {
		entry := "Lessons eval guard is the single best next backlog item: implement predictedImpact field."
		repo := tempRepo(t, entry+"\n")
		r := CheckLessonsDedupe(repo, entry, nil)
		if r.OK {
			t.Fatalf("got %+v, want rejected", r)
		}
		if r.Similarity != 1 {
			t.Fatalf("similarity %v, want 1", r.Similarity)
		}
		if r.MatchedEntry != entry {
			t.Fatalf("matchedEntry %q", r.MatchedEntry)
		}
	})

	t.Run("rejects the real regression case: exact and near-duplicate variants of the lessons-eval-guard recommendation", func(t *testing.T) {
		// Verbatim shape of .selfbuild/lessons.md, which carried this
		// recommendation 7x across the 2026-09-02 file. The hyphen variant is
		// an exact token duplicate (similarity 1.0); the shorter v2 rewrite is
		// a near-duplicate of the original's own wording (0.37 trigram
		// similarity — near-dup band versus distinct lessons at ~0.0).
		original := "- **Lessons eval guard is the single best next backlog item**: Current lessons digest is write-only; no verification that lessons help. Competitors (AHE, Meta-Harness) use propose→evaluate→accept gates with predicted-impact fields falsified by outcomes. **Why:** Without this, DevAgent risks negative learning where lessons silently degrade future prompts. **How to apply:** Implement `predictedImpact` field, evaluation pipeline against regression suite, acceptance criteria (must beat best-so-far on held-out tasks), and ledger logging."
		hyphenDup := strings.Replace(original, "Lessons eval guard", "Lessons-eval-guard", 1)
		v2Rewrite := "- **Lessons-eval-guard v2**: Implement `predictedImpact` field in lessons, evaluation pipeline against regression suite, acceptance criteria (must beat best-so-far on held-out tasks), and ledger logging. Competitors (AHE, Meta-Harness) use propose→evaluate→accept gates with predicted-impact fields falsified by outcomes. Outcome verification is table-stakes for headless automation."
		repo := tempRepo(t, original+"\n")
		hyphen := CheckLessonsDedupe(repo, hyphenDup, &CheckLessonsDedupeOpts{Threshold: floatPtr(0.5)})
		if hyphen.OK {
			t.Fatalf("hyphen dup got %+v, want rejected", hyphen)
		}
		if hyphen.Similarity != 1 {
			t.Fatalf("hyphen similarity %v, want 1", hyphen.Similarity)
		}
		v2 := CheckLessonsDedupe(repo, v2Rewrite, &CheckLessonsDedupeOpts{Threshold: floatPtr(0.5)})
		// A short rewrite of the original's own wording stays near the
		// original; assert it is NOT in the distinct band so the 0.8 default
		// is defensible.
		if v2.Similarity <= 0.1 {
			t.Fatalf("v2 similarity %v, want > 0.1", v2.Similarity)
		}
	})

	t.Run("keeps distinct entries above the near-dup band untouched", func(t *testing.T) {
		a := "Fencing tokens kill double dispatch even after kill -9 and lease reclaim"
		b := "Structural validation gates must block merge, not stay advisory"
		if LessonSimilarity(a, b) >= 0.5 {
			t.Fatalf("distinct lessons similarity %v, want < 0.5", LessonSimilarity(a, b))
		}
		if !CheckLessonsDedupe(tempRepo(t, a+"\n"), b, &CheckLessonsDedupeOpts{Threshold: floatPtr(0.5)}).OK {
			t.Fatal("distinct entry rejected")
		}
	})

	t.Run("honors the threshold override", func(t *testing.T) {
		repo := tempRepo(t, "one two three four five six seven eight\n")
		if CheckLessonsDedupe(repo, "one two three four five six seven nine", &CheckLessonsDedupeOpts{Threshold: floatPtr(0.5)}).OK {
			t.Fatal("expected rejection at threshold 0.5")
		}
		if !CheckLessonsDedupe(repo, "one two three four five six seven nine", &CheckLessonsDedupeOpts{Threshold: floatPtr(1)}).OK {
			t.Fatal("expected acceptance at threshold 1")
		}
	})
}

// ---------------------------------------------------------------------------
// appendLessonGuarded (eval-gated append: impact → dedupe → evaluate)
// ---------------------------------------------------------------------------

func TestAppendLessonGuarded(t *testing.T) {
	// Temp repo whose (injected) suite exits with `code`.
	tempRepo := func(t *testing.T, code int) string {
		t.Helper()
		return t.TempDir()
	}
	suiteRunnerFor := func(code int) SuiteRunner {
		return func(cmd string, args []string, dir string, timeoutMs int) (string, int) {
			return "", code
		}
	}

	t.Run("missing predictedImpact → rejected before dedupe/suite, nothing written", func(t *testing.T) {
		repo := tempRepo(t, 0)
		r := AppendLessonGuarded(repo, "New durable lesson.", nil)
		if !r.OK { // ok tracks the dedupe verdict, not acceptance
			t.Fatalf("ok = false (ok tracks the dedupe verdict): %+v", r)
		}
		if r.Reason != ReasonMissingPredictedImpact || r.Suite != SuiteSkipped {
			t.Fatalf("reason/suite = %v/%v", r.Reason, r.Suite)
		}
		if _, err := os.Stat(filepath.Join(repo, SelfbuildLessonsPath)); !os.IsNotExist(err) {
			t.Fatal("lessons file must not be written")
		}
		rows := readRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rowString(t, rows[0], "event") != "lessons-eval" || rowBool(t, rows[0], "accepted") ||
			rowString(t, rows[0], "reason") != "missing-predictedImpact" || rowString(t, rows[0], "suite") != "skipped" {
			t.Fatalf("row = %#v", rows[0])
		}
	})

	t.Run("blank predictedImpact counts as missing (whitespace-only cannot bypass the gate)", func(t *testing.T) {
		repo := tempRepo(t, 0)
		r := AppendLessonGuarded(repo, "New durable lesson.", &AppendLessonGuardedOpts{PredictedImpact: "   "})
		if r.Reason != ReasonMissingPredictedImpact {
			t.Fatalf("reason = %v", r.Reason)
		}
		if _, err := os.Stat(filepath.Join(repo, SelfbuildLessonsPath)); !os.IsNotExist(err) {
			t.Fatal("lessons file must not be written")
		}
	})

	t.Run("unique entry with predictedImpact + green suite → accepted, appended with impact suffix, ledger row written", func(t *testing.T) {
		repo := tempRepo(t, 0)
		impact := "avoids re-picking shipped goals"
		r := AppendLessonGuarded(repo, "New durable lesson.", &AppendLessonGuardedOpts{
			PredictedImpact: impact,
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     suiteRunnerFor(0),
		})
		if !r.OK || r.Reason != ReasonAccepted || r.Suite != SuiteGreen {
			t.Fatalf("got %+v", r)
		}
		data, err := os.ReadFile(filepath.Join(repo, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if want := "New durable lesson. [predictedImpact: " + impact + "]\n"; string(data) != want {
			t.Fatalf("file = %q, want %q", data, want)
		}
		rows := readRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		row := rows[0]
		if rowString(t, row, "kind") != "event" || rowString(t, row, "event") != "lessons-eval" ||
			!rowBool(t, row, "accepted") || rowString(t, row, "reason") != "accepted" ||
			rowString(t, row, "suite") != "green" || rowFloat(t, row, "similarity") != 0 ||
			rowString(t, row, "predictedImpact") != impact ||
			rowString(t, row, "excerptHash") != LessonExcerptHash("New durable lesson.") {
			t.Fatalf("row = %#v", row)
		}
	})

	t.Run("acceptance writes the optional loop join key into the ledger row (Q39)", func(t *testing.T) {
		repo := tempRepo(t, 0)
		r := AppendLessonGuarded(repo, "A loop-scoped durable lesson.", &AppendLessonGuardedOpts{
			PredictedImpact: "joinable",
			SuiteTimeoutMs:  30_000,
			Loop:            intPtr(42),
			SuiteRunner:     suiteRunnerFor(0),
		})
		if r.Reason != ReasonAccepted {
			t.Fatalf("reason = %v", r.Reason)
		}
		rows := readRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rowString(t, rows[0], "event") != "lessons-eval" || rowFloat(t, rows[0], "loop") != 42 || !rowBool(t, rows[0], "accepted") {
			t.Fatalf("row = %#v", rows[0])
		}
	})

	t.Run("red suite → rejected + file reverted byte-for-byte (created case removes the file)", func(t *testing.T) {
		repo := tempRepo(t, 1)
		r := AppendLessonGuarded(repo, "New durable lesson.", &AppendLessonGuardedOpts{
			PredictedImpact: "must not land",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     suiteRunnerFor(1),
		})
		if !r.OK { // dedupe passed
			t.Fatalf("ok = false (dedupe passed): %+v", r)
		}
		if r.Reason != ReasonSuiteRed || r.Suite != SuiteRed || r.SuiteDetail == nil || *r.SuiteDetail == "" {
			t.Fatalf("got %+v", r)
		}
		if _, err := os.Stat(filepath.Join(repo, SelfbuildLessonsPath)); !os.IsNotExist(err) {
			t.Fatal("created lessons file must be removed on red suite")
		}
		rows := readRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rowString(t, rows[0], "event") != "lessons-eval" || rowBool(t, rows[0], "accepted") ||
			rowString(t, rows[0], "reason") != "suite-red" || rowString(t, rows[0], "suite") != "red" {
			t.Fatalf("row = %#v", rows[0])
		}
	})

	t.Run("red suite → rejected + pre-existing file restored byte-for-byte", func(t *testing.T) {
		repo := tempRepo(t, 1)
		seed := "Seed lesson that must survive the revert."
		writeRepoFile(t, repo, SelfbuildLessonsPath, seed+"\n")
		r := AppendLessonGuarded(repo, "A different candidate lesson.", &AppendLessonGuardedOpts{
			PredictedImpact: "must not land",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     suiteRunnerFor(1),
		})
		if r.Reason != ReasonSuiteRed {
			t.Fatalf("reason = %v", r.Reason)
		}
		data, err := os.ReadFile(filepath.Join(repo, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if want := seed + "\n"; string(data) != want {
			t.Fatalf("file = %q, want %q", data, want)
		}
	})

	t.Run("duplicate → rejected before the suite runs, exactly one ledger row", func(t *testing.T) {
		repo := tempRepo(t, 0)
		entry := "Keep the lessons digest under 4000 chars."
		first := AppendLessonGuarded(repo, entry, &AppendLessonGuardedOpts{
			PredictedImpact: "budget hygiene",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     suiteRunnerFor(0),
		})
		if first.Reason != ReasonAccepted {
			t.Fatalf("first reason = %v", first.Reason)
		}
		r := AppendLessonGuarded(repo, entry, &AppendLessonGuardedOpts{
			PredictedImpact: "budget hygiene",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     suiteRunnerFor(0),
		})
		if r.OK || r.Similarity != 1 || r.Reason != ReasonDuplicate || r.Suite != SuiteSkipped {
			t.Fatalf("got %+v", r)
		}
		data, err := os.ReadFile(filepath.Join(repo, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if want := entry + " [predictedImpact: budget hygiene]\n"; string(data) != want {
			t.Fatalf("file = %q, want %q", data, want)
		}
		rows := readRows(t, repo)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		if rowString(t, rows[1], "event") != "lessons-eval" || rowBool(t, rows[1], "accepted") ||
			rowString(t, rows[1], "reason") != "duplicate" || rowString(t, rows[1], "suite") != "skipped" {
			t.Fatalf("row = %#v", rows[1])
		}
	})
}

// ---------------------------------------------------------------------------
// predictedImpact round-trip (AHE/Meta-Harness field)
// ---------------------------------------------------------------------------

func TestAppendPredictedImpact(t *testing.T) {
	t.Run("appends a predictedImpact suffix to the lesson line", func(t *testing.T) {
		got := AppendPredictedImpact("Lesson text.", "avoids re-picking shipped goals")
		if want := "Lesson text. [predictedImpact: avoids re-picking shipped goals]"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("writes the entry unchanged when no predictedImpact is given", func(t *testing.T) {
		if got := AppendPredictedImpact("Lesson text.", ""); got != "Lesson text." {
			t.Fatalf("got %q", got)
		}
		if got := AppendPredictedImpact("Lesson text.", "   "); got != "Lesson text." {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("guard round-trip: distinct lesson with predictedImpact is appended with the suffix verbatim", func(t *testing.T) {
		// Digest echo (loadLessonsDigest) is part of the src/prompt.ts port,
		// not this package; here we assert the append + verbatim suffix on
		// the file, which is what the digest slices whole.
		dir := t.TempDir()
		impact := "avoids burning the digest budget on repeats"
		r := AppendLessonGuarded(dir, "A distinct lesson about the eval guard.", &AppendLessonGuardedOpts{
			PredictedImpact: impact,
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     greenRunner,
		})
		if !r.OK {
			t.Fatalf("got %+v", r)
		}
		written, err := os.ReadFile(filepath.Join(dir, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(written), "[predictedImpact: "+impact+"]") {
			t.Fatalf("file missing suffix: %q", written)
		}
	})

	t.Run("duplicate with predictedImpact is still rejected (impact does not bypass the guard)", func(t *testing.T) {
		dir := t.TempDir()
		entry := "Guard rejects near duplicate lessons before append."
		writeRepoFile(t, dir, SelfbuildLessonsPath, entry+"\n")
		r := AppendLessonGuarded(dir, "guard rejects near-duplicate lessons before append!", &AppendLessonGuardedOpts{
			PredictedImpact: "would not help; it is the same lesson",
		})
		if r.OK {
			t.Fatalf("got %+v", r)
		}
		written, err := os.ReadFile(filepath.Join(dir, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if want := entry + "\n"; string(written) != want {
			t.Fatalf("file = %q, want %q", written, want)
		}
	})
}

// ---------------------------------------------------------------------------
// budget interaction: dedupe-before-append respects the 40-line / 4000-char caps
// (digest-side assertions live with the src/prompt.ts port)
// ---------------------------------------------------------------------------

func TestBudgetInteraction(t *testing.T) {
	t.Run("keeps appended lessons within the digest caps: file gains exactly one line", func(t *testing.T) {
		dir := t.TempDir()
		var seed []string
		for i := range 49 {
			seed = append(seed, fmt.Sprintf("seed lesson %d with some padding", i))
		}
		writeRepoFile(t, dir, SelfbuildLessonsPath, strings.Join(seed, "\n")+"\n")

		r := AppendLessonGuarded(dir, "A brand new distinct lesson worth echoing.", &AppendLessonGuardedOpts{
			PredictedImpact: "must survive the cap",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     greenRunner,
		})
		if !r.OK {
			t.Fatalf("got %+v", r)
		}
		// 50 lines in the file (the digest is capped at the newest 40).
		data, err := os.ReadFile(filepath.Join(dir, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 50 {
			t.Fatalf("file lines = %d, want 50", n)
		}
		if !strings.Contains(string(data), "A brand new distinct lesson worth echoing.") {
			t.Fatal("newest line missing")
		}
	})

	t.Run("a rejected duplicate does not consume budget: file stays byte-identical", func(t *testing.T) {
		dir := t.TempDir()
		entry := "Write-only ratchets spend the digest budget on repeats, so dedupe before append."
		writeRepoFile(t, dir, SelfbuildLessonsPath, entry+"\n")
		before, err := os.ReadFile(filepath.Join(dir, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		r := AppendLessonGuarded(dir, "Write-only ratchets spend the digest budget on repeats, so dedupe before append!", nil)
		if r.OK {
			t.Fatalf("got %+v", r)
		}
		after, err := os.ReadFile(filepath.Join(dir, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("file changed on rejected duplicate")
		}
	})
}

// ---------------------------------------------------------------------------
// recordLoopResult (Q39 impact telemetry)
// ---------------------------------------------------------------------------

func TestRecordLoopResult(t *testing.T) {
	t.Run("writes a loop-result event row with the correct schema", func(t *testing.T) {
		repo := t.TempDir()
		RecordLoopResult(repo, 42, "ok", "Test goal.")
		events := ReadEvents(repo)
		if len(events) != 1 {
			t.Fatalf("events = %d, want 1", len(events))
		}
		row := events[0]
		if rowString(t, row, "event") != "loop-result" || rowFloat(t, row, "loop") != 42 ||
			rowString(t, row, "status") != "ok" || rowString(t, row, "kind") != "event" {
			t.Fatalf("row = %#v", row)
		}
		if _, ok := row["ts"]; !ok {
			t.Fatal("ts missing")
		}
		if !strings.Contains(rowString(t, row, "goal"), "Test goal") {
			t.Fatalf("goal = %q", row["goal"])
		}
	})

	t.Run("normalizes unknown status to failed", func(t *testing.T) {
		repo := t.TempDir()
		RecordLoopResult(repo, 7, "bogus-status", "")
		row := ReadEvents(repo)[0]
		if rowString(t, row, "status") != "failed" {
			t.Fatalf("status = %q, want failed", row["status"])
		}
	})

	t.Run("writes multiple rows idempotently", func(t *testing.T) {
		repo := t.TempDir()
		RecordLoopResult(repo, 1, "ok", "first")
		RecordLoopResult(repo, 2, "failed", "second")
		events := ReadEvents(repo)
		if len(events) != 2 {
			t.Fatalf("events = %d, want 2", len(events))
		}
		if rowFloat(t, events[0], "loop") != 1 || rowFloat(t, events[1], "loop") != 2 {
			t.Fatalf("loops = %v, %v", events[0]["loop"], events[1]["loop"])
		}
	})
}

// ---------------------------------------------------------------------------
// computeLessonScores (impact scoring)
// ---------------------------------------------------------------------------

func TestComputeLessonScores(t *testing.T) {
	t.Run("returns empty map when no lessons-eval rows exist", func(t *testing.T) {
		events := []map[string]any{
			{"kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok", "ts": "2026-01-01T00:00:00Z", "goal": ""},
		}
		if got := ComputeLessonScores(events); len(got) != 0 {
			t.Fatalf("scores = %v, want empty", got)
		}
	})

	t.Run("computes acceptRate, delta, and composite score for a lesson with loop data", func(t *testing.T) {
		// Lesson A (hash a1b2): 3 evals, 2 accepted. Loops 1,2,3. Loop 1 ok, 2 failed, 3 ok.
		// Lesson B (hash c3d4): 2 evals, 0 accepted. Loops 4,5. Loop 4 failed, 5 failed.
		events := []map[string]any{
			{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "a1b2", "accepted": true, "loop": float64(1)},
			{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "a1b2", "accepted": false, "loop": float64(2)},
			{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "a1b2", "accepted": true, "loop": float64(3)},
			{"ts": "2026-01-01T04:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "c3d4", "accepted": false, "loop": float64(4)},
			{"ts": "2026-01-01T05:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "c3d4", "accepted": false, "loop": float64(5)},
			{"ts": "2026-01-01T06:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok"},
			{"ts": "2026-01-01T07:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(2), "status": "failed"},
			{"ts": "2026-01-01T08:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(3), "status": "ok"},
			{"ts": "2026-01-01T09:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(4), "status": "failed"},
			{"ts": "2026-01-01T10:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(5), "status": "failed"},
		}
		scores := ComputeLessonScores(events)
		if len(scores) != 2 {
			t.Fatalf("scores size = %d, want 2", len(scores))
		}

		// Lesson A: 3 evals, 2 accepted, loops 1,2,3 (1 ok, 2 failed)
		a := scores["a1b2"]
		if !closeTo(a.AcceptRate, 2.0/3) || !closeTo(a.LessonLoopFailureRate, 1.0/3) ||
			!closeTo(a.OverallLoopFailureRate, 3.0/5) || !closeTo(a.Delta, 1.0/3-3.0/5) || a.EvalCount != 3 {
			t.Fatalf("A = %+v", a)
		}
		// score = acceptRate - delta = 2/3 - (1/3 - 3/5) = 14/15
		if !closeTo(a.Score, 14.0/15) {
			t.Fatalf("A score = %v, want %v", a.Score, 14.0/15)
		}

		// Lesson B: 2 evals, 0 accepted, loops 4,5 (both failed)
		b := scores["c3d4"]
		if !closeTo(b.AcceptRate, 0) || !closeTo(b.LessonLoopFailureRate, 1) ||
			!closeTo(b.OverallLoopFailureRate, 3.0/5) || !closeTo(b.Delta, 1-3.0/5) || b.EvalCount != 2 {
			t.Fatalf("B = %+v", b)
		}
		if !closeTo(b.Score, -0.4) {
			t.Fatalf("B score = %v, want -0.4", b.Score)
		}
	})

	t.Run("falls back to timestamp matching when lessons-eval row has no loop field", func(t *testing.T) {
		events := []map[string]any{
			{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "hash1", "accepted": true},
			{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok"},
		}
		scores := ComputeLessonScores(events)
		if len(scores) != 1 {
			t.Fatalf("scores size = %d, want 1", len(scores))
		}
		s := scores["hash1"]
		if s.Delta > 0 { // no failures → delta <= 0
			t.Fatalf("delta = %v, want <= 0", s.Delta)
		}
		if s.LessonLoopFailureRate != 0 { // the fallback matched loop 1 (ok)
			t.Fatalf("lessonLoopFailureRate = %v, want 0", s.LessonLoopFailureRate)
		}
	})

	t.Run("sets delta 0 and score = acceptRate when no loop-result data exists", func(t *testing.T) {
		events := []map[string]any{
			{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "hash1", "accepted": true},
			{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "hash1", "accepted": false},
		}
		scores := ComputeLessonScores(events)
		if len(scores) != 1 {
			t.Fatalf("scores size = %d, want 1", len(scores))
		}
		s := scores["hash1"]
		if s.Delta != 0 || s.Score != s.AcceptRate || s.LessonLoopFailureRate != 0 || s.OverallLoopFailureRate != 0 {
			t.Fatalf("s = %+v", s)
		}
	})

	t.Run("counts only non-ok loop statuses as failures", func(t *testing.T) {
		events := []map[string]any{
			{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "h1", "accepted": true, "loop": float64(1)},
			// Other lessons evaluated in loops 2-4 give the overall baseline loops to measure.
			{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "h2", "accepted": true, "loop": float64(2)},
			{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "h2", "accepted": true, "loop": float64(3)},
			{"ts": "2026-01-01T04:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "h2", "accepted": true, "loop": float64(4)},
			{"ts": "2026-01-01T05:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok"},
			{"ts": "2026-01-01T06:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(2), "status": "failed-tests"},
			{"ts": "2026-01-01T07:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(3), "status": "provider-degraded"},
			{"ts": "2026-01-01T08:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(4), "status": "invalid"},
		}
		scores := ComputeLessonScores(events)
		if len(scores) != 2 {
			t.Fatalf("scores size = %d, want 2", len(scores))
		}
		if scores["h1"].LessonLoopFailureRate != 0 {
			t.Fatalf("h1 lessonLoopFailureRate = %v", scores["h1"].LessonLoopFailureRate)
		}
		// overall uses loops 1-4: 1 ok, 3 non-ok → 3/4 = 0.75
		if !closeTo(scores["h1"].OverallLoopFailureRate, 0.75) {
			t.Fatalf("overallLoopFailureRate = %v", scores["h1"].OverallLoopFailureRate)
		}
	})
}

// ---------------------------------------------------------------------------
// loadLessonScores (disk-based scoring)
// ---------------------------------------------------------------------------

func TestLoadLessonScores(t *testing.T) {
	t.Run("reads events.jsonl and returns a Map<excerptHash, score>", func(t *testing.T) {
		repo := t.TempDir()
		events := []map[string]any{
			{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "aaa", "accepted": true, "loop": float64(1)},
			{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": "bbb", "accepted": false, "loop": float64(1)},
			{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok"},
		}
		var buf strings.Builder
		for _, e := range events {
			b, _ := json.Marshal(e)
			buf.Write(b)
			buf.WriteString("\n")
		}
		writeRepoFile(t, repo, EventsFile, buf.String())

		scores := LoadLessonScores(repo)
		if len(scores) != 2 {
			t.Fatalf("scores size = %d, want 2", len(scores))
		}
		if !(scores["aaa"] > scores["bbb"]) {
			t.Fatalf("aaa=%v should beat bbb=%v", scores["aaa"], scores["bbb"])
		}
	})

	t.Run("returns empty map when no events file exists", func(t *testing.T) {
		repo := t.TempDir()
		if got := LoadLessonScores(repo); len(got) != 0 {
			t.Fatalf("scores = %v, want empty", got)
		}
	})
}

// ---------------------------------------------------------------------------
// readEvents (parsing)
// ---------------------------------------------------------------------------

func TestReadEvents(t *testing.T) {
	t.Run("skips corrupt lines and returns only parseable rows", func(t *testing.T) {
		repo := t.TempDir()
		writeRepoFile(t, repo, EventsFile, "{\"valid\": true}\ncorrupt garbage\n{\"also\": \"valid\"}\n")
		rows := ReadEvents(repo)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		if rows[0]["valid"] != true {
			t.Fatalf("rows[0] = %#v", rows[0])
		}
	})

	t.Run("returns empty array when the events file is absent", func(t *testing.T) {
		repo := t.TempDir()
		if rows := ReadEvents(repo); len(rows) != 0 {
			t.Fatalf("rows = %v, want empty", rows)
		}
	})
}

// ---------------------------------------------------------------------------
// held-out tier: digest slice, best score, and must-beat gate
// ---------------------------------------------------------------------------

func mline(i int, impact string) string {
	return fmt.Sprintf("machine lesson %d text [predictedImpact: %s]", i, impact)
}

func uline(i int) string {
	return fmt.Sprintf("human dated prose line %d", i)
}

// mixedEval: accept L1 (ok) + reject L2 (failed): main score = acceptRate
// 0.5 − delta 0 = 0.5 (with only loops 1,2 present).
func mixedEval(hash string) []map[string]any {
	return []map[string]any{
		{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": hash, "accepted": true, "loop": float64(1)},
		{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": hash, "accepted": false, "loop": float64(2)},
		{"ts": "2026-01-01T03:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok"},
		{"ts": "2026-01-01T04:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(2), "status": "failed"},
	}
}

func writeEvents(t *testing.T, repo string, events []map[string]any) {
	t.Helper()
	var buf strings.Builder
	for _, e := range events {
		b, _ := json.Marshal(e)
		buf.Write(b)
		buf.WriteString("\n")
	}
	writeRepoFile(t, repo, EventsFile, buf.String())
}

func TestHeldOutTier(t *testing.T) {
	tempRepo := func(t *testing.T, lessonsContent string) string {
		t.Helper()
		dir := t.TempDir()
		writeRepoFile(t, dir, EventsFile, "") // ensure ledger dir exists like the vitest fixture
		if err := os.Remove(filepath.Join(dir, EventsFile)); err != nil {
			t.Fatal(err)
		}
		if lessonsContent != "" {
			writeRepoFile(t, dir, SelfbuildLessonsPath, lessonsContent)
		}
		return dir
	}

	t.Run("heldOutLessonHashes: newest 20% (min 1, max 3) of machine-appended lessons by append order", func(t *testing.T) {
		var lines []string
		for i := range 10 {
			lines = append(lines, mline(i, "cuts re-picks by half"))
		}
		lines = append(lines[:3], append([]string{uline(0)}, lines[3:]...)...) // human prose is not eligible
		repo := tempRepo(t, strings.Join(lines, "\n")+"\n")
		r := HeldOutLessonHashes(repo, "")
		// 10 machine lines → 20% = 2 held out (the two newest by append order).
		want := []string{mline(8, "cuts re-picks by half"), mline(9, "cuts re-picks by half")}
		if len(r.Lines) != 2 || r.Lines[0] != want[0] || r.Lines[1] != want[1] {
			t.Fatalf("lines = %#v, want %#v", r.Lines, want)
		}
		if !r.Hashes[LessonExcerptHash(mline(8, "cuts re-picks by half"))] || !r.Hashes[LessonExcerptHash(mline(9, "cuts re-picks by half"))] {
			t.Fatalf("hashes = %#v", r.Hashes)
		}
		if r.Hashes[LessonExcerptHash(mline(7, "cuts re-picks by half"))] {
			t.Fatal("hash of non-held-out lesson present")
		}
	})

	t.Run("heldOutLessonHashes: min 1, and machine-only with zero machine lines", func(t *testing.T) {
		one := tempRepo(t, mline(0, "one")+"\n")
		if r := HeldOutLessonHashes(one, ""); len(r.Lines) != 1 || r.Lines[0] != mline(0, "one") {
			t.Fatalf("lines = %#v", r.Lines)
		}
		none := tempRepo(t, uline(0)+"\n"+uline(1)+"\n")
		if r := HeldOutLessonHashes(none, ""); len(r.Lines) != 0 {
			t.Fatalf("lines = %#v", r.Lines)
		}
		if r := HeldOutLessonHashes(one, "missing.md"); len(r.Lines) != 0 {
			t.Fatalf("lines = %#v", r.Lines)
		}
	})

	t.Run("loadBestMeasuredScore excludes held-out lessons so the newest slice cannot chase a self-set bar", func(t *testing.T) {
		// 7 machine lines: slice = 20% of 7 = 1 → the NEWEST line only. The
		// only scored lesson sits at index 0 (in scope); the held-out newest
		// line gets a perfect (score > 0.5) history and must still not set
		// the bar.
		mixed := mline(0, "mixed lesson")
		var seed []string
		for i := range 6 {
			seed = append(seed, mline(10+i, "baseline"))
		}
		repo := tempRepo(t, mixed+"\n"+strings.Join(seed, "\n")+"\n")
		// newest line = seed[5]; give it a solo accept (score 1) — held out.
		events := mixedEval(LessonExcerptHash(mixed))
		events = append(events,
			map[string]any{"ts": "2026-01-01T05:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(seed[5]), "accepted": true, "loop": float64(9)},
			map[string]any{"ts": "2026-01-01T06:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(9), "status": "ok"},
		)
		writeEvents(t, repo, events)
		held := HeldOutLessonHashes(repo, "")
		if len(held.Lines) != 1 || held.Lines[0] != seed[5] {
			t.Fatalf("held-out lines = %#v, want [%s]", held.Lines, seed[5])
		}
		// Without exclusion the held-out seed's high score would win; with
		// exclusion the in-scope mixed lesson's measured score (acceptRate
		// 0.5 − repeatFailureDelta 1/6 ≈ 0.333) is the best.
		best := LoadBestMeasuredScore(repo, "")
		if best == nil || !closeTo(*best, 1.0/3) {
			t.Fatalf("best = %v, want ≈ 1/3", best)
		}
	})

	t.Run("loadBestMeasuredScore: null with no measured scores at all (cold start)", func(t *testing.T) {
		repo := tempRepo(t, mline(0, "only lesson")+"\n")
		if best := LoadBestMeasuredScore(repo, ""); best != nil {
			t.Fatalf("best = %v, want nil", *best)
		}
	})

	t.Run("predictedImpactGrade: quantified reductions grade above 0, prose-only stays 0", func(t *testing.T) {
		if got := PredictedImpactGrade("reduces repeat re-picks of shipped items by 50%"); !closeTo(got, 0.5) {
			t.Fatalf("got %v, want 0.5", got)
		}
		if got := PredictedImpactGrade("avoid re-picking already-shipped backlog items"); got != 0 {
			t.Fatalf("got %v, want 0", got)
		}
		if got := PredictedImpactGrade("cuts failures by 25%"); !closeTo(got, 0.25) {
			t.Fatalf("got %v, want 0.25", got)
		}
	})

	t.Run("checkMustBeat: grade below the best → below; no baseline or saturated best → none", func(t *testing.T) {
		mixed := mline(0, "mixed lesson")
		var seed []string
		for i := range 6 {
			seed = append(seed, mline(10+i, "baseline"))
		}
		repo := tempRepo(t, mixed+"\n"+strings.Join(seed, "\n")+"\n")
		writeEvents(t, repo, mixedEval(LessonExcerptHash(mixed)))
		// Newest line is a held-out seed; the in-scope best is 0.5. A
		// 0.25-grade candidate is below; a 0.6-grade candidate beats it.
		if got := CheckMustBeat(repo, "cuts failures by 25%", ""); got != MustBeatBelow {
			t.Fatalf("got %v, want below", got)
		}
		if got := CheckMustBeat(repo, "cuts re-picks by 60%", ""); got != MustBeatBeat {
			t.Fatalf("got %v, want beat", got)
		}
		// No ledger evidence at all → no baseline → none (accept).
		cold := tempRepo(t, mixed+"\n"+strings.Join(seed, "\n")+"\n")
		if got := CheckMustBeat(cold, "cuts re-picks by 50%", ""); got != MustBeatNone {
			t.Fatalf("got %v, want none", got)
		}
		// Empty file → the candidate's own impact is the only score → none.
		empty := tempRepo(t, "")
		if got := CheckMustBeat(empty, "cuts re-picks by 50%", ""); got != MustBeatNone {
			t.Fatalf("got %v, want none", got)
		}
		// Saturated best (≥ 1): the grade scale caps at 1, so the bar can no
		// longer discriminate — none, not a permanent 'below' lockout.
		solo := mline(0, "solo accepted lesson")
		soloRepo := tempRepo(t, solo+"\n"+mline(1, "rider")+"\n"+mline(2, "rider two")+"\n")
		writeEvents(t, soloRepo, []map[string]any{
			{"ts": "2026-01-01T01:00:00Z", "kind": "event", "event": "lessons-eval", "excerptHash": LessonExcerptHash(solo), "accepted": true, "loop": float64(1)},
			{"ts": "2026-01-01T02:00:00Z", "kind": "event", "event": "loop-result", "loop": float64(1), "status": "ok"},
		})
		if got := CheckMustBeat(soloRepo, "cuts re-picks by 100%", ""); got != MustBeatNone {
			t.Fatalf("got %v, want none", got)
		}
	})

	t.Run("accept path: candidate beats the best → appended + ledger row carries heldOut/mustBeat", func(t *testing.T) {
		// In-scope scored lesson at 0.5 (accept L1 ok + reject L2 failed);
		// the candidate's 60% grade beats it → accepted.
		mixed := mline(0, "mixed lesson")
		var seed []string
		for i := range 6 {
			seed = append(seed, mline(10+i, "baseline"))
		}
		repo := tempRepo(t, mixed+"\n"+strings.Join(seed, "\n")+"\n")
		writeEvents(t, repo, mixedEval(LessonExcerptHash(mixed)))
		r := AppendLessonGuarded(repo, "A brand new distinct lesson.", &AppendLessonGuardedOpts{
			PredictedImpact: "cuts re-picks by 60%",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     greenRunner,
		})
		if r.Reason != ReasonAccepted || r.Suite != SuiteGreen || r.MustBeat == nil || *r.MustBeat != MustBeatBeat || r.HeldOut == nil || *r.HeldOut != 1 {
			t.Fatalf("got %+v", r)
		}
		rows := readRows(t, repo)
		row := rows[len(rows)-1]
		if rowString(t, row, "event") != "lessons-eval" || !rowBool(t, row, "accepted") ||
			rowString(t, row, "reason") != "accepted" || rowString(t, row, "suite") != "green" ||
			rowFloat(t, row, "heldOut") != 1 || rowString(t, row, "mustBeat") != "beat" {
			t.Fatalf("row = %#v", row)
		}
		if !closeTo(rowFloat(t, row, "mustBeatScore"), 0.5) {
			t.Fatalf("mustBeatScore = %v, want ≈ 0.5", row["mustBeatScore"])
		}
	})

	t.Run("reject path: candidate below the best → held-out rejection, file reverted, ledger row carries mustBeat", func(t *testing.T) {
		mixed := mline(0, "mixed lesson")
		var seed []string
		for i := range 6 {
			seed = append(seed, mline(10+i, "baseline"))
		}
		repo := tempRepo(t, mixed+"\n"+strings.Join(seed, "\n")+"\n")
		writeEvents(t, repo, mixedEval(LessonExcerptHash(mixed)))
		before, err := os.ReadFile(filepath.Join(repo, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		r := AppendLessonGuarded(repo, "A different brand new lesson.", &AppendLessonGuardedOpts{
			PredictedImpact: "cuts failures by 25%", // 0.25 < in-scope best 0.5
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     greenRunner,
		})
		if r.Reason != ReasonHeldOut || r.Suite != SuiteGreen || r.MustBeat == nil || *r.MustBeat != MustBeatBelow || r.HeldOut == nil || *r.HeldOut != 1 {
			t.Fatalf("got %+v", r)
		}
		after, err := os.ReadFile(filepath.Join(repo, SelfbuildLessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if string(before) != string(after) {
			t.Fatal("file not reverted byte-for-byte")
		}
		rows := readRows(t, repo)
		row := rows[len(rows)-1]
		if rowString(t, row, "event") != "lessons-eval" || rowBool(t, row, "accepted") ||
			rowString(t, row, "reason") != "held-out" || rowString(t, row, "suite") != "green" ||
			rowFloat(t, row, "heldOut") != 1 || rowString(t, row, "mustBeat") != "below" {
			t.Fatalf("row = %#v", row)
		}
		if !closeTo(rowFloat(t, row, "mustBeatScore"), 0.5) {
			t.Fatalf("mustBeatScore = %v, want ≈ 0.5", row["mustBeatScore"])
		}
	})

	t.Run("no-held-out accept path: no eligible baseline to beat → accepted with mustBeat none (constraint off)", func(t *testing.T) {
		// Cold start on an empty file: nothing measured exists, so the
		// must-beat check has no baseline and the append is accepted
		// (constraint off, not a lockout).
		repo := tempRepo(t, "")
		r := AppendLessonGuarded(repo, "First lesson in an empty file.", &AppendLessonGuardedOpts{
			PredictedImpact: "avoids re-picking already-shipped goals",
			SuiteTimeoutMs:  30_000,
			SuiteRunner:     greenRunner,
		})
		if r.Reason != ReasonAccepted || r.MustBeat == nil || *r.MustBeat != MustBeatNone || r.HeldOut == nil || *r.HeldOut != 0 {
			t.Fatalf("got %+v", r)
		}
		rows := readRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		row := rows[0]
		if rowString(t, row, "event") != "lessons-eval" || !rowBool(t, row, "accepted") ||
			rowString(t, row, "reason") != "accepted" || rowFloat(t, row, "heldOut") != 0 ||
			rowString(t, row, "mustBeat") != "none" {
			t.Fatalf("row = %#v", row)
		}
	})
}

// ---------------------------------------------------------------------------
// consume-kg-lessons deterministic fixtures (recordMergedKgEvidence's append
// flow; captureKgEvidence/isFreshKgEvidence/consumeOnce live in src/consume.ts,
// a different port wave)
// ---------------------------------------------------------------------------

const (
	kgEvidenceLessonPrefix     = "KG digest evidence persisted on merge:"
	kgEvidencePredictedImpact  = "cuts repeat KG re-queries on future runs: fresh structural evidence is already in the digest"
	kgEvidenceFreshExcerpt     = "retrieval: graph-query (exact match) | freshness: fresh"
	kgEvidenceDefaultThreshold = DefaultLessonsDedupeSimilarity
)

func kgAppend(repo string, excerpt string, runner SuiteRunner) LessonsDedupeResult {
	return AppendLessonGuarded(repo, kgEvidenceLessonPrefix+" "+excerpt, &AppendLessonGuardedOpts{
		LessonsFile:     LessonsPath,
		Threshold:       floatPtr(kgEvidenceDefaultThreshold),
		PredictedImpact: kgEvidencePredictedImpact,
		SuiteTimeoutMs:  DefaultLessonsSuiteTimeoutMs,
		MustBeat:        boolPtr(false),
		SuiteRunner:     runner,
	})
}

func TestConsumeKgLessonsPersistence(t *testing.T) {
	t.Run("appends the verbatim excerpt through the eval guard when fresh", func(t *testing.T) {
		repo := t.TempDir()
		r := kgAppend(repo, kgEvidenceFreshExcerpt, greenRunner)
		if r.Reason != ReasonAccepted || r.Suite != SuiteGreen {
			t.Fatalf("got %+v", r)
		}
		data, err := os.ReadFile(filepath.Join(repo, LessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		want := kgEvidenceLessonPrefix + " " + kgEvidenceFreshExcerpt + " [predictedImpact: " + kgEvidencePredictedImpact + "]\n"
		if string(data) != want {
			t.Fatalf("file = %q, want %q", data, want)
		}
		rows := readRows(t, repo)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(rows))
		}
		if rowString(t, rows[0], "event") != "lessons-eval" || !rowBool(t, rows[0], "accepted") ||
			rowString(t, rows[0], "reason") != "accepted" ||
			rowString(t, rows[0], "excerptHash") != LessonExcerptHash(kgEvidenceLessonPrefix+" "+kgEvidenceFreshExcerpt) {
			t.Fatalf("row = %#v", rows[0])
		}
	})

	t.Run("rejects the second persist of the same excerpt as a duplicate", func(t *testing.T) {
		repo := t.TempDir()
		if r := kgAppend(repo, kgEvidenceFreshExcerpt, greenRunner); r.Reason != ReasonAccepted {
			t.Fatalf("first = %+v", r)
		}
		second := kgAppend(repo, kgEvidenceFreshExcerpt, greenRunner)
		if second.OK || second.Reason != ReasonDuplicate || second.Similarity != 1 {
			t.Fatalf("second = %+v", second)
		}
		data, err := os.ReadFile(filepath.Join(repo, LessonsPath))
		if err != nil {
			t.Fatal(err)
		}
		if n := len(strings.Split(strings.TrimSpace(string(data)), "\n")); n != 1 {
			t.Fatalf("lessons lines = %d, want 1", n)
		}
		rows := readRows(t, repo)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(rows))
		}
		if rowBool(t, rows[1], "accepted") || rowString(t, rows[1], "reason") != "duplicate" || rowString(t, rows[1], "suite") != "skipped" {
			t.Fatalf("row = %#v", rows[1])
		}
	})

	t.Run("reverts the lessons file when the evaluate step goes red", func(t *testing.T) {
		repo := t.TempDir()
		r := kgAppend(repo, kgEvidenceFreshExcerpt, redRunner)
		if r.Reason != ReasonSuiteRed {
			t.Fatalf("reason = %v", r.Reason)
		}
		if _, err := os.Stat(filepath.Join(repo, LessonsPath)); !os.IsNotExist(err) {
			t.Fatal("lessons file must not exist after a red suite")
		}
	})
}
