package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/ledger"
)

// The checked-in rubric must parse on every commit: an unparseable rubric takes
// the whole ratchet down with it, and CI only finds out when a nightly scores
// nothing.
func TestCheckedInRubricParses(t *testing.T) {
	r, err := LoadRubric("../..", "")
	if err != nil {
		t.Fatalf("checked-in rubric rejected: %v", err)
	}
	if r.Version == "" || len(r.Digest) != 12 {
		t.Fatalf("rubric identity incomplete: version=%q digest=%q", r.Version, r.Digest)
	}
	if r.Total() != 100 {
		t.Fatalf("rubric weights sum to %d, want the 100-point scale the nightly threshold budget assumes", r.Total())
	}
	want := []string{"requirement-coverage", "test-evidence", "diff-relevance", "prd-currency", "no-regression-claim"}
	if len(r.Criteria) != len(want) {
		t.Fatalf("criteria = %+v", r.Criteria)
	}
	for i, id := range want {
		if r.Criteria[i].ID != id {
			t.Fatalf("criterion %d = %q, want %q", i, r.Criteria[i].ID, id)
		}
		if r.Criteria[i].Description == "" {
			t.Fatalf("criterion %q has no description for the judge to score against", id)
		}
	}
}

func TestParseRubricRejectsAmbiguousRubrics(t *testing.T) {
	for name, src := range map[string]string{
		"no version":   "- [100] only: does it work\n",
		"no criteria":  "version: 1\n\nprose only\n",
		"duplicate id": "version: 1\n- [50] a: one\n- [50] a: two\n",
		"zero weight":  "version: 1\n- [0] a: one\n- [100] b: two\n",
	} {
		if _, err := ParseRubric(src, name); err == nil {
			t.Errorf("%s: accepted silently — this is how a baseline gets redefined without anyone noticing", name)
		}
	}
}

func TestRubricDigestTracksMeasurementNotProse(t *testing.T) {
	const base = "version: 1\n- [60] coverage: what the ticket asked for\n- [40] tests: proof it works\n"
	restated, err := ParseRubric("version: 1\n- [60] coverage: what the ticket ASKED FOR (reworded)\n- [40] tests: proof it works\n", "restated")
	if err != nil {
		t.Fatal(err)
	}
	original, err := ParseRubric(base, "base")
	if err != nil {
		t.Fatal(err)
	}
	if restated.Digest != original.Digest {
		t.Fatalf("prose-only edit changed the digest (%s vs %s): the trailing window would split on a typo fix",
			restated.Digest, original.Digest)
	}
	reweighted, err := ParseRubric("version: 1\n- [80] coverage: what the ticket asked for\n- [20] tests: proof it works\n", "reweighted")
	if err != nil {
		t.Fatal(err)
	}
	if reweighted.Digest == original.Digest {
		t.Fatal("re-weighting the rubric kept the digest: scores measured on different scales would share a baseline")
	}
	// A dropped criterion line (the silent-parse-hole case) must move the digest
	// too, or the shrunk scale reads as a guaranteed regression.
	dropped, err := ParseRubric("version: 1\n- [60] coverage — what the ticket asked for\n- [40] tests: proof it works\n", "dropped")
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Digest == original.Digest {
		t.Fatal("malformed criterion line vanished without changing the digest")
	}
}

func TestClassifyGoalAndTaskIdentity(t *testing.T) {
	if got := ClassifyGoal("fix(loopdriver): stop the starvation spin"); got != "fix" {
		t.Fatalf("goal class = %q", got)
	}
	// The loop's own squash titles ("Goal: Implement GitHub issue #NNN (…)")
	// are the loop's feature artifacts: capitalized match, aliased to feat —
	// never the catch-all bucket, where they would share a baseline with
	// unrelated unclassified PRs.
	if got := ClassifyGoal("Goal: Implement GitHub issue #293 (FR-VAL-05: quality-drift ratchet)"); got != "feat" {
		t.Fatalf("loop squash subject = %q, want feat", got)
	}
	if got := ClassifyGoal("Docs(prd): repair the table break"); got != "docs" {
		t.Fatalf("capitalized conventional subject = %q, want docs", got)
	}
	// The documented ceiling: a subject with no conventional prefix at all
	// is unclassifiable and shares the catch-all baseline.
	if got := ClassifyGoal("repair the ratchet"); got != "other" {
		t.Fatalf("unclassifiable subject = %q, want other", got)
	}
	shipped := Evidence{PR: 42, Head: "devagent/TASK-mtvz0se9-281k", Title: "feat(x): y"}
	if got := DeriveTaskID(shipped); got != "TASK-mtvz0se9-281k" {
		t.Fatalf("task id from branch = %q", got)
	}
	// A human PR has no task id: it reports itself as a PR, not as a fake
	// orchestrator task that would join to nothing.
	if got := DeriveTaskID(Evidence{PR: 42, Title: "docs: typo"}); got != "pr-42" {
		t.Fatalf("task id fallback = %q", got)
	}
}

func TestEvidenceDerivesTestAndPrdHints(t *testing.T) {
	e := Evidence{Files: []string{"internal/eval/score.go", "internal/eval/eval_test.go", "docs/PRD.md"}}
	if !e.TouchesTests() || !e.TouchesPrd() {
		t.Fatalf("hints not derived: tests=%v prd=%v", e.TouchesTests(), e.TouchesPrd())
	}
	shallow := Evidence{Files: []string{"internal/eval/score.go"}}
	if shallow.TouchesTests() || shallow.TouchesPrd() {
		t.Fatal("unrelated file read as test/PRD evidence")
	}
}

func TestBuildJudgePromptCarriesRubricAndFacts(t *testing.T) {
	r, err := LoadRubric("../..", "")
	if err != nil {
		t.Fatal(err)
	}
	e := Evidence{
		PR: 305, Title: "feat(cli): add eval score", Body: "Implements AC-1.\n\ngo test ./...: ok",
		Files:     []string{"internal/cli/actions_eval.go", "internal/cli/actions_eval_test.go", "docs/PRD.md"},
		Additions: 40, Deletions: 2, Diff: "+func main() {}",
	}
	p := BuildJudgePrompt(r, e)
	for _, want := range []string{
		fmt.Sprintf("## Rubric version %s", r.Version),
		"requirement-coverage", "test-evidence", "no-regression-claim",
		"touches tests: yes", "docs/PRD.md: yes",
		"Implements AC-1.", "+func main() {}",
		`"scores": {`, `"notes":`,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "touches tests: no") {
		t.Fatal("prompt contradicts the derived test hint")
	}
	// The JSON template must enumerate the rubric ids so the judge cannot invent
	// a shape ParseJudgeReport would call inconclusive.
	for _, id := range []string{"requirement-coverage", "test-evidence", "diff-relevance", "prd-currency", "no-regression-claim"} {
		if !strings.Contains(p, fmt.Sprintf("%q: 0", id)) {
			t.Fatalf("scores template missing %q", id)
		}
	}
}

func TestParseJudgeReportIsStrict(t *testing.T) {
	r, err := ParseRubric("version: 1\n- [30] coverage: x\n- [25] tests: y\n", "test")
	if err != nil {
		t.Fatal(err)
	}
	// Fenced, chatty output still parses (brace-scan), out-of-range scores clamp,
	// and criteria come back in rubric order.
	report, ok := ParseJudgeReport("Here is my assessment:\n```json\n{\"scores\": {\"tests\": 99, \"coverage\": -4}, \"notes\": \" thin \"}\n```", r)
	if !ok {
		t.Fatal("fenced report rejected")
	}
	if report.Criteria[0].Criterion != "coverage" || report.Criteria[0].Score != 0 {
		t.Fatalf("negative score not clamped to the floor: %+v", report.Criteria[0])
	}
	if report.Criteria[1].Score != 25 || report.Criteria[1].Max != 25 {
		t.Fatalf("over-weight score not clamped to the ceiling: %+v", report.Criteria[1])
	}
	if report.Notes != "thin" {
		t.Fatalf("notes not trimmed: %q", report.Notes)
	}
	if _, ok := ParseJudgeReport(`{"scores": {"coverage": 30}}`, r); ok {
		t.Fatal("partial report accepted: a missing criterion would be padded into a false regression")
	}
	if _, ok := ParseJudgeReport("I think it looks great, 10/10", r); ok {
		t.Fatal("non-JSON report accepted")
	}
	// Unknown ids are noise from an off-rubric judge; they cannot add scale.
	extra, ok := ParseJudgeReport(`{"scores": {"coverage": 10, "tests": 10, "vibes": 10}}`, r)
	if !ok || len(extra.Criteria) != 2 || extra.Criteria[0].Criterion != "coverage" || extra.Criteria[1].Criterion != "tests" {
		t.Fatalf("unknown criterion leaked into the row: %+v", extra.Criteria)
	}
}

func TestScoreRecordsRubricIdentity(t *testing.T) {
	r, err := ParseRubric("version: 7\n- [50] coverage: x\n- [50] tests: y\n", "test")
	if err != nil {
		t.Fatal(err)
	}
	judge := func(prompt string) (string, error) {
		if !strings.Contains(prompt, "version 7") {
			t.Error("judge never saw the rubric version")
		}
		return `{"scores": {"coverage": 40, "tests": 10}, "notes": "no test evidence"}`, nil
	}
	rec, err := Score(r, Evidence{PR: 12, TaskID: "TASK-a-1", GoalClass: "feat"}, judge, "omp@onegw/free", 1)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RubricVersion != "7" || rec.RubricDigest != r.Digest {
		t.Fatalf("rubric identity not recorded: %s/%s", rec.RubricVersion, rec.RubricDigest)
	}
	if rec.Total != 50 || rec.Max != 100 || rec.PR != 12 || rec.TaskID != "TASK-a-1" || rec.Judge != "omp@onegw/free" {
		t.Fatalf("row shape wrong: %+v", rec)
	}
	// A dispatch failure must not be mistaken for a zero score.
	if _, err := Score(r, Evidence{PR: 12}, func(string) (string, error) { return "", fmt.Errorf("provider down") }, "omp@x", 0); err == nil {
		t.Fatal("judge failure reported as a score")
	}
}

// --- fixture-driven pipeline check ---------------------------------------
//
// These use a deterministic stand-in judge (fixtureJudge) that reads the
// harness-computed facts in the prompt. They verify the PIPELINE: rubric parse →
// evidence fetch → prompt → strict report parse → clamp/order → ledger row →
// ratchet naming the regressed criterion. The issue's "clear margin between a
// known-good and a deliberately shallow PR" claim is a property of the real LLM
// judge and needs one live `devagent eval score --pr <n>` run to be called met.

// fixtureJudge scores from the facts the harness derived, mirroring what the
// rubric asks a real judge to weigh.
func fixtureJudge(prompt string) (string, error) {
	has := func(marker string) bool { return strings.Contains(prompt, marker) }
	scores := map[string]int{
		"requirement-coverage": pick(has("Acceptance"), 30, 6),
		"test-evidence":        pick(has("touches tests: yes"), 25, 3),
		"diff-relevance":       pick(!has("scripts/unrelated"), 20, 5),
		"prd-currency":         pick(has("docs/PRD.md: yes"), 15, 0),
		"no-regression-claim":  pick(has("go test ./...: ok"), 10, 1),
	}
	blob, err := json.Marshal(map[string]any{"scores": scores, "notes": "fixture judge"})
	return string(blob), err
}

func pick(cond bool, yes, no int) int {
	if cond {
		return yes
	}
	return no
}

const goodBody = "Goal: implement the eval surface.\n\nAcceptance: rows land and drift gates.\n\ngo test ./...: ok"
const shallowBody = "Fixed it."

// fixtureRunner answers the three `gh` shapes eval issues: `pr list --state
// merged`, `pr view <n> --json …`, `pr diff <n>`. Anything else is a test
// failure, so a changed call shape shows up as an error rather than as a
// silently skipped PR.
func fixtureRunner(prs map[int]Evidence) Runner {
	return func(args []string, cwd string) (string, error) {
		if len(args) >= 3 && args[0] == "pr" && args[1] == "list" {
			var nums []int
			for pr := range prs {
				nums = append(nums, pr)
			}
			// Map order is randomized, so sort: descending like gh lists them,
			// leaving ListMergedPrs' own newest-first→merge-order reversal as
			// the thing under test (an unsorted fixture would make the ratchet
			// case flaky in the other direction).
			sort.Slice(nums, func(i, j int) bool { return nums[i] > nums[j] })
			var out []map[string]int
			for _, pr := range nums {
				out = append(out, map[string]int{"number": pr})
			}
			blob, _ := json.Marshal(out)
			return string(blob), nil
		}
		if len(args) >= 3 && args[0] == "pr" && (args[1] == "view" || args[1] == "diff") {
			var pr int
			if _, err := fmt.Sscanf(args[2], "%d", &pr); err != nil {
				return "", fmt.Errorf("gh: bad PR argument %q", args[2])
			}
			e, ok := prs[pr]
			if !ok {
				return "", fmt.Errorf("gh: no PR #%d", pr)
			}
			if args[1] == "diff" {
				return e.Diff, nil
			}
			var files []map[string]any
			for _, f := range e.Files {
				files = append(files, map[string]any{"path": f})
			}
			blob, _ := json.Marshal(map[string]any{
				"number": e.PR, "title": e.Title, "body": e.Body, "mergedAt": e.MergedAt,
				"headRefName": e.Head, "additions": e.Additions, "deletions": e.Deletions, "files": files,
			})
			return string(blob), nil
		}
		return "", fmt.Errorf("unexpected gh args %v", args)
	}
}

// fixtureRepo stages a repo with the real checked-in rubric copied in, so the
// scoring path runs against the rubric of record rather than a toy.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", RubricPath))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, filepath.Dir(RubricPath))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, RubricPath), src, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

func knownGood() Evidence {
	return Evidence{
		PR: 501, Title: "feat(eval): score shipped artifacts", Body: goodBody,
		Head: "devagent/TASK-good-1", MergedAt: "2026-09-10T00:00:00Z",
		Files:     []string{"internal/eval/score.go", "internal/eval/eval_test.go", "docs/PRD.md"},
		Additions: 400, Deletions: 5,
		Diff: "@@ -0,0 +1,400 @@\n+func ScoreShipped(opts Options) ([]ledger.EvalScoreRecord, error) {\n+// +400 lines, tests included, PRD footer bumped",
	}
}

func deliberatelyShallow() Evidence {
	return Evidence{
		PR: 502, Title: "feat(eval): scoring", Body: shallowBody,
		Head: "devagent/TASK-shallow-2", MergedAt: "2026-09-11T00:00:00Z",
		Files:     []string{"internal/eval/score.go", "scripts/unrelated-churn.sh"},
		Additions: 12, Deletions: 0,
		Diff: "@@ -1,3 +1,12 @@\n+// TODO: implement the rest\n+echo churn",
	}
}

// minMargin is the separation the issue's acceptance asks for between a
// known-good artifact and a deliberately shallow one ("a clear margin"). The
// fixture judge scores 100 vs 15, so this pins the direction and refuses a
// collapse toward noise without over-specifying the fixture's arithmetic.
const minMargin = 30

func TestScoreShippedRecordsReadableRowsAndSkipsDuplicates(t *testing.T) {
	repo := fixtureRepo(t)
	prs := map[int]Evidence{501: knownGood(), 502: deliberatelyShallow()}
	rows, err := ScoreShipped(Options{
		RepoPath: repo, PRs: []int{501, 502}, GH: fixtureRunner(prs), Judge: fixtureJudge, JudgeName: "omp@fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if margin := rows[0].Total - rows[1].Total; margin < minMargin {
		t.Fatalf("fixture judge did not separate the artifacts by a clear margin: good=%d shallow=%d",
			rows[0].Total, rows[1].Total)
	}
	if rows[0].Max != 100 || rows[0].RubricDigest == "" {
		t.Fatalf("scale/rubric identity missing: %+v", rows[0])
	}
	if rows[0].TaskID != "TASK-good-1" || rows[0].GoalClass != "feat" {
		t.Fatalf("identity not derived from the PR: %+v", rows[0])
	}
	// The rows must survive the ledger: same file, same reader the loop uses.
	got := ledger.ReadEvalScores(repo)
	if len(got) != 2 || got[0].Total != rows[0].Total {
		t.Fatalf("ledger round-trip lost rows: %+v", got)
	}
	// Re-running scores nothing new: one artifact occupies one window slot.
	var logged strings.Builder
	again, err := ScoreShipped(Options{
		RepoPath: repo, PRs: []int{501, 502}, GH: fixtureRunner(prs), Judge: fixtureJudge, JudgeName: "omp@fixture",
		Log: func(format string, a ...any) { fmt.Fprintf(&logged, format+"\n", a...) },
	})
	if err == nil {
		t.Fatalf("re-score produced rows: %+v", again)
	}
	if !strings.Contains(logged.String(), "already scored") {
		t.Fatalf("skip not reported: %s", logged.String())
	}
}

// TestPlantedRegressionNamesTheCriterion is the issue's second acceptance case as
// far as a hermetic test can carry it: score a known-good artifact, then ship a
// shallow one, and the ratchet reports the drop with the offending criterion.
func TestPlantedRegressionNamesTheCriterion(t *testing.T) {
	repo := fixtureRepo(t)
	prs := map[int]Evidence{501: knownGood(), 502: deliberatelyShallow()}
	if _, err := ScoreShipped(Options{RepoPath: repo, Last: 2, GH: fixtureRunner(prs), Judge: fixtureJudge, JudgeName: "omp@fixture"}); err != nil {
		t.Fatal(err)
	}
	drift := ledger.ClusterQualityDrift(repo, 0)
	if len(drift) != 1 {
		t.Fatalf("expected the planted regression only, got %+v", drift)
	}
	d := drift[0]
	if d.PR != 502 || d.BestPR != 501 || d.Drop <= 0 || d.GoalClass != "feat" {
		t.Fatalf("wrong violation: %+v", d)
	}
	// The named criterion is the largest point loss against the best: the
	// shallow PR claimed no acceptance coverage (30 → 6) and no test evidence
	// (25 → 3), and coverage is the wider of the two in points.
	if d.Criterion != "requirement-coverage" {
		t.Fatalf("offending criterion misattributed: %+v", d)
	}
	if d.CriterionMax <= 0 || d.CriterionScore > d.CriterionMax {
		t.Fatalf("criterion scale lost on the way to the ratchet: %+v", d)
	}
	lines := ledger.RenderClustersText(nil, nil, drift, 5, "")
	if len(lines) != 2 || !strings.Contains(lines[1], d.Criterion) {
		t.Fatalf("clusters view does not name the criterion: %#v", lines)
	}
}

func TestScoreShippedFailsLoudlyOnUnusableInput(t *testing.T) {
	repo := fixtureRepo(t)
	// No targets: must not "pass" by scoring nothing.
	if _, err := ScoreShipped(Options{RepoPath: repo, GH: fixtureRunner(nil), Judge: fixtureJudge}); err == nil {
		t.Fatal("empty request accepted")
	}
	// Every PR un-fetchable: still a failed run, reported per PR.
	var logged strings.Builder
	_, err := ScoreShipped(Options{
		RepoPath: repo, PRs: []int{999}, GH: fixtureRunner(map[int]Evidence{}), Judge: fixtureJudge,
		Log: func(format string, a ...any) { fmt.Fprintf(&logged, format+"\n", a...) },
	})
	if err == nil {
		t.Fatal("run scoring nothing reported success")
	}
	if !strings.Contains(logged.String(), "warn") {
		t.Fatalf("per-PR failure not logged: %s", logged.String())
	}
	// A rubric that does not exist is an error before any dispatch.
	if _, err := ScoreShipped(Options{RepoPath: t.TempDir(), PRs: []int{1}, Judge: fixtureJudge}); err == nil {
		t.Fatal("missing rubric accepted")
	}
}

func TestListMergedPrsOrdersOldestFirst(t *testing.T) {
	run := func(args []string, cwd string) (string, error) {
		return `[{"number":3},{"number":2},{"number":1}]`, nil
	}
	prs, err := ListMergedPrs(run, ".", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 3 || prs[0] != 1 || prs[2] != 3 {
		t.Fatalf("gh returns newest first; scoring must follow merge order: %v", prs)
	}
}
