package prdintake

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/queue"
)

// spec is a PRD excerpt exercising every shape the parser must
// distinguish: intent, shipped state, prose, quotes and fence examples.
var spec = strings.Join([]string{
	"# DevAgent PRD",
	"",
	"## 12. CLI Specification",
	"",
	"- [ ] Make `devagent up` start the driver",
	"  - prints the running pid and log path",
	"  - is idempotent when a driver already holds the loop lock",
	"- [x] Ship the loop driver (done — state, not work)",
	"",
	"## 17. Roadmap",
	"",
	"> - [ ] a checkbox inside a blockquote is a state note, not an instruction",
	"",
	"Some prose bullet that is not intent:",
	"",
	"- plain bullet, never work",
	"",
	"~~- [ ] struck item is shipped state~~",
	"",
	"```markdown",
	"- [ ] example inside a code fence is never work",
	"```",
	"",
	"## 21. Simplicity First",
	"",
	"- [ ] Long item: " + strings.Repeat("word ", 200),
	"",
}, "\n")

// TestParseIntentContract pins what counts as operator intent: an open
// checkbox with its heading context and indented sub-bullets; everything the
// PRD already uses for state (done boxes, blockquotes, struck lines, plain
// bullets, fence examples) must never become work.
func TestParseIntentContract(t *testing.T) {
	items := Parse(spec)
	if len(items) != 2 {
		t.Fatalf("parsed %d items, want 2 (intent only): %v", len(items), titles(items))
	}

	first := items[0]
	if first.Title != "Make devagent up start the driver" {
		t.Errorf("title = %q, want inline markdown stripped", first.Title)
	}
	if first.Section != "12. CLI Specification" {
		t.Errorf("section = %q", first.Section)
	}
	if first.Line != 5 {
		t.Errorf("line = %d, want 5 (the checkbox line, not its last sub-bullet)", first.Line)
	}
	want := []string{
		"prints the running pid and log path",
		"is idempotent when a driver already holds the loop lock",
		TickCriterion,
	}
	if strings.Join(first.Criteria, "|") != strings.Join(want, "|") {
		t.Errorf("criteria = %q, want %q", first.Criteria, want)
	}
	if !strings.HasPrefix(first.Goal, "Goal: ") {
		t.Errorf("goal is not Goal: prefixed: %q", first.Goal)
	}
	if !strings.Contains(first.Goal, "docs/PRD.md:5") {
		t.Errorf("goal must cite the PRD line: %q", first.Goal)
	}
	if !strings.Contains(first.Goal, "12. CLI Specification") {
		t.Errorf("goal must cite the section: %q", first.Goal)
	}
	if !strings.Contains(first.PRDMarkdown, "## 12. CLI Specification") {
		t.Errorf("task-PRD sidecar must carry the section heading")
	}

	// Document order is priority order: the Simplicity First item is last.
	if items[1].Section != "21. Simplicity First" {
		t.Errorf("second item section = %q", items[1].Section)
	}
}

// TestGoalShapeAtTheDispatchBoundary: the loop refuses an over-length goal
// and retires its queue claim, so an enormous PRD item must still produce a
// dispatchable statement inside the 120-word cap.
func TestGoalShapeAtTheDispatchBoundary(t *testing.T) {
	items := Parse(spec)
	goal := items[len(items)-1].Goal
	if n := len(strings.Fields(goal)); n > 120 {
		t.Errorf("goal carries %d words, want <= 120: %q", n, goal)
	}
	if !strings.HasPrefix(goal, "Goal: ") || !strings.Contains(goal, "ship it as a PR") {
		t.Errorf("goal lost its contract while shortening: %q", goal)
	}
}

// TestIngestIsIdempotent: the queue id is a content hash, so re-running
// intake over an unchanged PRD enqueues nothing and reports the rows it
// already knew — "nothing queued" must never read as "nothing found".
func TestIngestIsIdempotent(t *testing.T) {
	repo := t.TempDir()
	writePRD(t, repo, spec)

	first, err := Ingest(Options{RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Queued) != 2 || first.Open != 2 {
		t.Fatalf("first pass queued %d of %d", len(first.Queued), first.Open)
	}
	second, err := Ingest(Options{RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Queued) != 0 {
		t.Errorf("second pass queued %d rows, want 0", len(second.Queued))
	}
	if len(second.Known) != 2 {
		t.Errorf("second pass knew %d rows, want 2", len(second.Known))
	}
	if second.QueueDepth != 2 {
		t.Errorf("queue depth = %d, want 2", second.QueueDepth)
	}

	rows := queue.ListTasks(repo, "")
	if len(rows) != 2 {
		t.Fatalf("queue holds %d rows, want 2", len(rows))
	}
	if got := *rows[0].Source; got != "prd" {
		t.Errorf("source = %q, want prd", got)
	}
	if rows[0].PrdPath == nil {
		t.Errorf("a prd-sourced row must carry its section sidecar")
	} else if raw, err := os.ReadFile(*rows[0].PrdPath); err != nil || !strings.Contains(string(raw), "## 12. CLI Specification") {
		t.Errorf("sidecar %v: %v", rows[0].PrdPath, err)
	}
}

// TestIngestRespectsMaxItems: a wishlist cannot flood the lane — the cap
// keeps document order (top of the PRD wins) and leaves the rest for later
// passes.
func TestIngestRespectsMaxItems(t *testing.T) {
	repo := t.TempDir()
	writePRD(t, repo, "- [ ] one\n- [ ] two\n- [ ] three\n")

	rep, err := Ingest(Options{RepoPath: repo, MaxItems: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(rep.Queued); strings.Join(got, ",") != "one,two" {
		t.Errorf("queued %v, want the first two in document order", got)
	}
	if len(queue.ListTasks(repo, "")) != 2 {
		t.Errorf("queue holds %d rows, want 2", len(queue.ListTasks(repo, "")))
	}
}

// TestIngestReclaimsEditedItem: rewording an item is new intent. The old row
// stays retired and the edited text enqueues once under a fresh id.
func TestIngestReclaimsEditedItem(t *testing.T) {
	repo := t.TempDir()
	writePRD(t, repo, "## Scope\n\n- [ ] Add flag --a\n")
	if _, err := Ingest(Options{RepoPath: repo}); err != nil {
		t.Fatal(err)
	}
	writePRD(t, repo, "## Scope\n\n- [ ] Add flag --b\n")
	rep, err := Ingest(Options{RepoPath: repo})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Queued) != 1 || rep.Queued[0].Title != "Add flag --b" {
		t.Fatalf("edited item must re-queue once: %v", titles(rep.Queued))
	}
	if StableID("Add flag --a") == StableID("Add flag --b") {
		t.Errorf("ids collide across different text")
	}
}

// TestIngestMissingPRDIsAnError: intake must say why it found nothing rather
// than report a clean empty lane.
func TestIngestMissingPRDIsAnError(t *testing.T) {
	if _, err := Ingest(Options{RepoPath: t.TempDir()}); err == nil {
		t.Fatal("want an error when docs/PRD.md is absent")
	}
}

// TestStableIDIsStableUnderRewrap: the hash reads the item's words, so
// re-wrapping a bullet across lines or changing its emphasis cannot fork a
// second queue row for the same request.
func TestStableIDIsStableUnderRewrap(t *testing.T) {
	a := Parse("- [ ] build the thing\n")[0]
	b := Parse("- [ ] build   the **thing**\n")[0]
	if a.ID != b.ID {
		t.Errorf("ids differ for the same wording: %s vs %s", a.ID, b.ID)
	}
	if !strings.HasPrefix(a.ID, "PRD-") || len(a.ID) != 12 {
		t.Errorf("id %q is not a PRD-<8hex> queue id", a.ID)
	}
	if _, err := queue.SanitizeID(a.ID); err != nil {
		t.Errorf("id %q is not a legal queue id: %v", a.ID, err)
	}
}

func titles(items []Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Title
	}
	return out
}

func writePRD(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DefaultPRDPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Report is JSON-serializable for `prd-intake --json`; pin the shape a script
// consumes.
func TestReportJSONShape(t *testing.T) {
	repo := t.TempDir()
	writePRD(t, repo, "## Scope\n\n- [ ] do it\n")
	rep, err := Ingest(Options{RepoPath: repo, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Queued) != 1 {
		t.Fatalf("dry run must report the item it would queue")
	}
	if len(queue.ListTasks(repo, "")) != 0 {
		t.Fatalf("dry run wrote queue rows")
	}
	blob, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"open", "queued", "known", "queueDepth", "prdPath"} {
		if !strings.Contains(string(blob), `"`+key+`"`) {
			t.Errorf("report json missing %q: %s", key, blob)
		}
	}
}
