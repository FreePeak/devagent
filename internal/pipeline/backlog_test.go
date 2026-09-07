// Tests for backlog.go — the Go port of test/task.test.ts (parse/check/strike
// halves) and the function-level cases of test/backlog-check.test.ts. The
// backlog-check CLI exit codes (0/1/2) are cli wiring, pinned by the
// orchestrator's gates; here the CheckBacklogPick outcomes are pinned
// byte-for-byte.

package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const tblPRDFixture = `## 17. Roadmap

### Phase 4 — Expansion (post-v1)

#### Phase 4 — current backlog (2026-09-02, curation run 23)

- **Cross-board retry memory beyond the SHA guard** — carry the prior board's failure class onto the re-bridged goal so the scout deprioritizes until the root-cause fix lands (Q27).
- **Operator-role provider preflight** — apply the cheap probe + isTransientProviderError gate to curator/warroom/PO loops and emit a ledger row so a degraded factory is visible, not silent (Q40).
- **Lessons impact telemetry** — aggregate accept/reject outcomes against loop results so the lessonsMaxChars digest is ranked by measured effect (Q39).

## 18. Open Questions

> **Completed post-v0.3 (2026-09-02, curation run 23):** Operator hardening — research moved to local-evidence-only (5d8a319), NDJSON assistant-text extraction (d3adf17), all roles defaulted to omp (799fd86).
`

// Same fixture with the Q40 bullet wrapped in ~~ (as the curator strikes
// shipped items).
func tblStruckQ40Fixture() string {
	return strings.Replace(
		strings.Replace(tblPRDFixture, "- **Operator-role provider preflight**", "~~- **Operator-role provider preflight**", 1),
		"(Q40).", "(Q40).~~", 1,
	)
}

const tblCollideFixture = `## 17. Roadmap

#### Phase 4 — current backlog (2026-09-07, collision fixture)

~~- **Cross-board retry memory beyond the SHA guard** — the shipped twin (Q27).~~
- **PRD-backlog reconciliation at pick time** — the open twin (Q27 family; trailing).
- **Consolidate the loop scripts** — fold recovery into src/orchestrator/ (Q19).

## 18. Open Questions
`

// Line 5 = struck Q27 twin, line 6 = open Q27 twin, line 7 = Q19.

func TestParseBacklogItems(t *testing.T) {
	items := ParseBacklogItems(tblPRDFixture)
	gotIDs := make([]string, 0, len(items))
	for _, it := range items {
		gotIDs = append(gotIDs, it.ID)
	}
	if strings.Join(gotIDs, ",") != "Q27,Q40,Q39" {
		t.Fatalf("ids = %v, want [Q27 Q40 Q39]", gotIDs)
	}
	if items[1].Title != "Operator-role provider preflight" || items[1].Struck {
		t.Fatalf("items[1] = %+v, want title 'Operator-role provider preflight', struck=false", items[1])
	}
	if items[0].LineNumber != 7 || items[1].LineNumber != 8 || items[2].LineNumber != 9 {
		t.Fatalf("line numbers = %d/%d/%d, want 7/8/9", items[0].LineNumber, items[1].LineNumber, items[2].LineNumber)
	}
	if !strings.HasPrefix(items[0].Line, "- **Cross-board") {
		t.Fatalf("Line must be the raw markdown line, got %q", items[0].Line)
	}

	struckItems := ParseBacklogItems(tblStruckQ40Fixture())
	for _, it := range struckItems {
		if it.ID == "Q40" && !it.Struck {
			t.Fatalf("struck Q40 fixture must mark Q40 struck")
		}
		if it.ID == "Q27" && it.Struck {
			t.Fatalf("Q27 must stay unstruck")
		}
	}
}

func TestParseBacklogItemsTestdata(t *testing.T) {
	basic, err := os.ReadFile(filepath.Join("testdata", "backlog-basic.md"))
	if err != nil {
		t.Fatal(err)
	}
	items := ParseBacklogItems(string(basic))
	gotIDs := make([]string, 0, len(items))
	struckByID := map[string]bool{}
	for _, it := range items {
		gotIDs = append(gotIDs, it.ID)
		struckByID[it.ID] = it.Struck
	}
	if strings.Join(gotIDs, ",") != "Q33,Q44,Q27,Q46" {
		t.Fatalf("basic fixture ids = %v, want [Q33 Q44 Q27 Q46]", gotIDs)
	}
	if !struckByID["Q33"] || struckByID["Q44"] {
		t.Fatalf("Q33 must be struck and Q44 open: %v", struckByID)
	}
	// Mid-parenthetical Q-token: the id sits inside the parenthetical, not at
	// its end — the LAST [A-Z]+\d+ token wins.
	var q27 *BacklogItem
	for i := range items {
		if items[i].ID == "Q27" {
			q27 = &items[i]
		}
	}
	if q27 == nil || !strings.Contains(q27.Line, "(Q27 family; trailing)") {
		t.Fatalf("mid-parenthetical Q27 item missing: %+v", q27)
	}

	collide, err := os.ReadFile(filepath.Join("testdata", "backlog-collide.md"))
	if err != nil {
		t.Fatal(err)
	}
	citems := ParseBacklogItems(string(collide))
	cgotIDs := make([]string, 0, len(citems))
	for _, it := range citems {
		cgotIDs = append(cgotIDs, it.ID)
	}
	if strings.Join(cgotIDs, ",") != "Q27,Q27,Q19,Q47" {
		t.Fatalf("collide fixture ids = %v, want [Q27 Q27 Q19 Q47]", cgotIDs)
	}
	if !citems[0].Struck || citems[1].Struck {
		t.Fatalf("collide twins must be struck=true (twin 1) / false (twin 2): %+v / %+v", citems[0], citems[1])
	}
}

func TestBacklogExtractCompletionNotes(t *testing.T) {
	notes := ExtractCompletionNotes(tblPRDFixture)
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(notes))
	}
	if !strings.HasPrefix(notes[0], "**Completed post-v0.3") {
		t.Fatalf("note[0] = %q", notes[0])
	}
	if !strings.Contains(notes[0], "all roles defaulted to omp (799fd86)") {
		t.Fatalf("note text truncated: %q", notes[0])
	}

	// Multi-line blockquotes are joined with a single space; non-completed
	// blockquotes are ignored; a new **Completed line starts a fresh note.
	const multi = `# T

> **Completed run 25:** shipped the reconciliation wiring
> with follow-through on the sibling drivers

> **Not a completion** — just a quote

> **Completed run 26:** second note
`
	got := ExtractCompletionNotes(multi)
	want := []string{
		"**Completed run 25:** shipped the reconciliation wiring with follow-through on the sibling drivers",
		"**Completed run 26:** second note",
	}
	if len(got) != len(want) {
		t.Fatalf("notes = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("note[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBacklogNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Operator-role provider preflight (Q40)!": "operator role provider preflight q40",
		"  Multiple   spaces\tand — dashes  ":     "multiple spaces and dashes",
		"":                                        "",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Fatalf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckBacklogPick(t *testing.T) {
	t.Run("rejects a pick whose id appears in a merged PR title", func(t *testing.T) {
		r := CheckBacklogPick("Q40", tblPRDFixture, []string{"Q40 is shipped — preflight CLI at src/cli.ts (#120)"}, nil)
		if r.OK || !r.Shipped {
			t.Fatalf("ok/shipped = %v/%v, want false/true", r.OK, r.Shipped)
		}
		want := "Q40 already shipped: Q40 is shipped — preflight CLI at src/cli.ts (#120)"
		if r.Message != want {
			t.Fatalf("message = %q, want %q", r.Message, want)
		}
		if len(r.StruckIDs) != 1 || r.StruckIDs[0] != "Q40" {
			t.Fatalf("struckIds = %v, want [Q40]", r.StruckIDs)
		}
	})

	t.Run("rejects a pick whose bold title text matches a merged PR title (curator title-match rule)", func(t *testing.T) {
		r := CheckBacklogPick("Q40", tblPRDFixture, []string{"Operator-role provider preflight: probe stdin + circuit advance (#120)"}, nil)
		if !r.Shipped || !strings.Contains(r.Message, "already shipped") {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("accepts a pick still in the current backlog when no merged PR matches", func(t *testing.T) {
		r := CheckBacklogPick("Q27", tblPRDFixture, []string{"feat(lessons): eval-guard dedupe gate before any append (#116)"}, nil)
		if !r.OK || r.Shipped {
			t.Fatalf("ok/shipped = %v/%v, want true/false", r.OK, r.Shipped)
		}
		if r.Message != "Q27 is current backlog — dispatch ok" {
			t.Fatalf("message = %q", r.Message)
		}
		if !strings.Contains(r.Prompt, "(Q27)") {
			t.Fatalf("prompt = %q", r.Prompt)
		}
	})

	t.Run("rejects a pick already struck from the backlog", func(t *testing.T) {
		r := CheckBacklogPick("Q40", tblStruckQ40Fixture(), nil, nil)
		if !r.Shipped || r.OK {
			t.Fatalf("result = %+v", r)
		}
		if r.Message != "Q40 already shipped (struck in docs/PRD.md)" {
			t.Fatalf("message = %q", r.Message)
		}
	})

	t.Run("rejects a pick not found in the current backlog section", func(t *testing.T) {
		r := CheckBacklogPick("Q99", tblPRDFixture, nil, nil)
		if r.OK || r.Shipped {
			t.Fatalf("result = %+v", r)
		}
		if r.Message != "backlog item Q99 not found in the Phase 4 current backlog" {
			t.Fatalf("message = %q", r.Message)
		}
	})

	t.Run("reconciles the whole backlog: shipped siblings ride along in struckIds", func(t *testing.T) {
		r := CheckBacklogPick("Q27", tblPRDFixture, []string{"Q40 done (#1)", "Q39 done too (#2)"}, nil)
		if !r.OK || r.Shipped {
			t.Fatalf("result = %+v", r)
		}
		if strings.Join(r.StruckIDs, ",") != "Q40,Q39" {
			t.Fatalf("struckIds = %v, want [Q40 Q39]", r.StruckIDs)
		}
	})

	t.Run("completion-note title match ships the item; an id-only mention never does", func(t *testing.T) {
		titleNote := strings.Replace(tblPRDFixture,
			"## 18. Open Questions",
			"> **Completed run 26:** Lessons impact telemetry — accept/reject aggregation landed (a1b2c3d).\n\n## 18. Open Questions", 1)
		r := CheckBacklogPick("Q27", titleNote, nil, nil)
		if !r.OK {
			t.Fatalf("Q27 must stay current: %+v", r)
		}
		if strings.Join(r.StruckIDs, ",") != "Q39" {
			t.Fatalf("struckIds = %v, want [Q39] via completion-note title match", r.StruckIDs)
		}

		idNote := strings.Replace(tblPRDFixture,
			"## 18. Open Questions",
			"> **Completed run 26:** the Q40 deeper failure-class carryover stays on the backlog.\n\n## 18. Open Questions", 1)
		r2 := CheckBacklogPick("Q27", idNote, nil, nil)
		if len(r2.StruckIDs) != 0 {
			t.Fatalf("id-only note mention must not strike Q40: %v", r2.StruckIDs)
		}
	})

	t.Run("PRD line ref pick resolves to the bullet; a productive goal naming the line is shipped evidence", func(t *testing.T) {
		r := CheckBacklogPick("PRD:6", tblCollideFixture,
			[]string{"devagent(TASK-x): auto-cleanup snapshot (#162)"},
			[]string{"Goal: PRD:6 — wire checkBacklogPick into the selfbuild driver"})
		if r.OK || !r.Shipped {
			t.Fatalf("result = %+v", r)
		}
		want := "PRD:6 already shipped: Goal: PRD:6 — wire checkBacklogPick into the selfbuild driver"
		if r.Message != want {
			t.Fatalf("message = %q, want %q", r.Message, want)
		}
		if strings.Join(r.StruckIDs, ",") != "Q27" {
			t.Fatalf("struckIds = %v, want [Q27]", r.StruckIDs)
		}
	})

	t.Run("ledger strike hits only the referenced line, not its struck-id twin", func(t *testing.T) {
		r := CheckBacklogPick("PRD:7", tblCollideFixture, nil,
			[]string{"Goal: PRD:6 — shipped the reconciliation wiring"})
		if !r.OK || r.Shipped {
			t.Fatalf("pick PRD:7 (Q19) must stay current: %+v", r)
		}
		if r.Message != "PRD:7 is current backlog — dispatch ok" {
			t.Fatalf("message = %q", r.Message)
		}
		if strings.Join(r.StruckIDs, ",") != "Q27" {
			t.Fatalf("struckIds = %v, want [Q27] (the open twin via the ledger goal)", r.StruckIDs)
		}
		struck := StrikeBacklogItems(tblCollideFixture, r.StruckIDs)
		if !strings.Contains(struck, "~~- **PRD-backlog reconciliation at pick time**") {
			t.Fatalf("open twin must be struck:\n%s", struck)
		}
		if strings.Contains(struck, "~~- **Consolidate the loop scripts**") {
			t.Fatalf("Q19 must not be struck:\n%s", struck)
		}
	})

	t.Run("partial-completion guard: a remainder goal referencing the line keeps the item open", func(t *testing.T) {
		r := CheckBacklogPick("PRD:7", tblCollideFixture, nil, []string{
			"Goal: PRD:7 — fold starved() out of shell into typed code",
			"Goal: PRD:7 remainder — migrate the three sibling drivers",
		})
		if !r.OK || r.Shipped || len(r.StruckIDs) != 0 {
			t.Fatalf("result = %+v", r)
		}
		if r.Message != "PRD:7 is current backlog — dispatch ok" {
			t.Fatalf("message = %q", r.Message)
		}
	})

	t.Run("ledger evidence is the strict PRD:<line> ref — a goal quoting the title without the ref cannot strike", func(t *testing.T) {
		r := CheckBacklogPick("PRD:7", tblCollideFixture, nil, []string{
			"Goal: Consolidate stuck-board recovery into typed code per backlog item Consolidate the loop scripts",
		})
		if !r.OK || len(r.StruckIDs) != 0 {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("a ledger goal naming a different line cannot strike", func(t *testing.T) {
		r := CheckBacklogPick("PRD:7", tblCollideFixture, nil, []string{
			"Goal: PRD:60 — unrelated line that shares the 6 prefix",
		})
		if !r.OK || len(r.StruckIDs) != 0 {
			t.Fatalf("result = %+v", r)
		}
	})

	t.Run("no ledger evidence at all keeps the referenced line open", func(t *testing.T) {
		// The failed-row exclusion happens upstream (productiveGoals); with no
		// productive goals passed, the referenced line stays current.
		r := CheckBacklogPick("PRD:6", tblCollideFixture, nil, nil)
		if !r.OK || r.Shipped {
			t.Fatalf("result = %+v", r)
		}
		if r.Message != "PRD:6 is current backlog — dispatch ok" {
			t.Fatalf("message = %q", r.Message)
		}
	})

	t.Run("id collision resolves toward the unstruck twin", func(t *testing.T) {
		r := CheckBacklogPick("Q27", tblCollideFixture, []string{"chore: unrelated"}, nil)
		if !r.OK || r.Shipped {
			t.Fatalf("result = %+v", r)
		}
		if !strings.Contains(r.Message, "Q27 is current backlog") {
			t.Fatalf("message = %q", r.Message)
		}
		if !strings.Contains(r.Prompt, "the open twin (Q27 family; trailing)") {
			t.Fatalf("prompt must be the open twin line, got %q", r.Prompt)
		}
	})

	t.Run("a PRD:<line> ref pointing outside the backlog section is unresolved", func(t *testing.T) {
		r := CheckBacklogPick("PRD:1", tblCollideFixture, []string{"chore: unrelated"}, nil)
		if r.OK || r.Shipped {
			t.Fatalf("result = %+v", r)
		}
		if !strings.Contains(r.Message, "not found") {
			t.Fatalf("message = %q", r.Message)
		}
	})
}

func TestStrikeBacklogItems(t *testing.T) {
	updated := StrikeBacklogItems(tblPRDFixture, []string{"Q40", "Q39"})
	if !strings.Contains(updated, "~~- **Operator-role provider preflight**") ||
		!strings.Contains(updated, "(Q40).~~") {
		t.Fatalf("Q40 not struck:\n%s", updated)
	}
	if !strings.Contains(updated, "~~- **Lessons impact telemetry**") {
		t.Fatalf("Q39 not struck:\n%s", updated)
	}
	if strings.Contains(updated, "~~- **Cross-board") ||
		!strings.Contains(updated, "\n- **Cross-board retry memory beyond the SHA guard** — carry") {
		t.Fatalf("Q27 line must survive unmodified:\n%s", updated)
	}

	// No double-strike.
	once := StrikeBacklogItems(tblPRDFixture, []string{"Q40"})
	twice := StrikeBacklogItems(once, []string{"Q40"})
	if twice != once {
		t.Fatalf("strike must be idempotent")
	}

	// Leading/trailing whitespace preserved exactly (TS regex surgery).
	ws := StrikeBacklogItems("  - **T** (Q5)  \nnext\n", []string{"Q5"})
	if ws != "  ~~- **T** (Q5)~~  \nnext\n" {
		t.Fatalf("whitespace surgery wrong: %q", ws)
	}
}

func TestBacklogListMergedPrTitles(t *testing.T) {
	repo := t.TempDir()
	tblGit(t, repo, "init", "-b", "main")
	tblGit(t, repo, "config", "user.email", "t@t")
	tblGit(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tblGit(t, repo, "add", "-A")
	tblGit(t, repo, "commit", "-m", "feat: first (#1)")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tblGit(t, repo, "add", "-A")
	tblGit(t, repo, "commit", "-m", "fix: second (#2)")
	tblGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")

	titles := ListMergedPrTitles(repo)
	if len(titles) != 2 || titles[0] != "fix: second (#2)" || titles[1] != "feat: first (#1)" {
		t.Fatalf("titles = %q, want shas stripped, newest first", titles)
	}

	// No origin/main (or not a repo at all) degrades to an empty list.
	bare := t.TempDir()
	tblGit(t, bare, "init", "-b", "main")
	if got := ListMergedPrTitles(bare); len(got) != 0 {
		t.Fatalf("no-origin repo must yield empty list, got %q", got)
	}
	if got := ListMergedPrTitles(t.TempDir()); len(got) != 0 {
		t.Fatalf("non-repo must yield empty list, got %q", got)
	}
}
