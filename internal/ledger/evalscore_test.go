package ledger

import (
	"encoding/json"
	"strings"
	"testing"
)

// The FR-VAL-05 (issue #293) ratchet reads eval-score event rows off the same
// events.jsonl stream as every other record family and reports any score below
// the best its own goal class recently achieved, naming the criterion that fell
// furthest.

func scoreRow(pr int, taskID, class, rubric, digest string, criteria ...EvalCriterionScore) EvalScoreRecord {
	if len(criteria) == 0 {
		criteria = []EvalCriterionScore{{Criterion: "overall", Score: pr % 100, Max: 100}}
	}
	return MakeEvalScoreRecord(EvalScoreArgs{
		TaskID: taskID, Attempt: 1, PR: pr, GoalClass: class,
		RubricVersion: rubric, RubricDigest: digest, Judge: "omp@onegw/free",
		Criteria: criteria, TS: "2026-09-11T00:00:00.000Z",
	})
}

func rowWith(pr, total int) EvalScoreRecord {
	return scoreRow(pr, "T"+string(rune('A'+pr%26)), "feat", "1", "abc123",
		EvalCriterionScore{Criterion: "overall", Score: total, Max: 100})
}

func TestEvalScoreRecordRoundTrip(t *testing.T) {
	repo := t.TempDir()
	rec := MakeEvalScoreRecord(EvalScoreArgs{
		TaskID: "TASK-x-1", Attempt: 2, PR: 305, GoalClass: "fix",
		RubricVersion: "1", RubricDigest: "deadbeef01", Judge: "omp@onegw/free",
		Criteria: []EvalCriterionScore{
			{Criterion: "requirement-coverage", Score: 25, Max: 30},
			{Criterion: "test-evidence", Score: 5, Max: 25},
		},
		Notes: "tests never exercise the new path", TS: "2026-09-11T00:00:00.000Z",
	})
	if rec.Total != 30 || rec.Max != 55 {
		t.Fatalf("derived totals wrong: total=%d max=%d", rec.Total, rec.Max)
	}
	AppendEvalScoreRecord(repo, rec)
	lines := readLines(repo)
	if len(lines) != 1 {
		t.Fatalf("expected 1 ledger line, got %d", len(lines))
	}
	line := lines[0]
	// Key order is this ledger's serialization contract (declaration order).
	for i, want := range []string{"ts", "kind", "taskId", "attempt", "event", "pr", "goalClass",
		"rubricVersion", "rubricDigest", "judge", "criteria", "total", "max", "notes"} {
		if got := keyAt(t, line, i); got != want {
			t.Fatalf("key %d = %q, want %q (line %s)", i, got, want, line)
		}
	}
	if !strings.Contains(line, `"kind":"event"`) || !strings.Contains(line, `"event":"eval-score"`) {
		t.Fatalf("row not addressable as an eval-score event: %s", line)
	}
	got := ReadEvalScores(repo)
	if len(got) != 1 {
		t.Fatalf("ReadEvalScores returned %d rows", len(got))
	}
	if got[0].PR != 305 || got[0].TaskID != "TASK-x-1" || got[0].GoalClass != "fix" ||
		got[0].RubricVersion != "1" || got[0].RubricDigest != "deadbeef01" || got[0].Judge != "omp@onegw/free" {
		t.Fatalf("row identity lost: %+v", got[0])
	}
	if len(got[0].Criteria) != 2 || got[0].Criteria[1].Score != 5 || got[0].Criteria[1].Max != 25 {
		t.Fatalf("per-criterion scores lost: %+v", got[0].Criteria)
	}
}

func TestMakeEvalScoreRecordNormalizesEmptyCriteria(t *testing.T) {
	// This ledger serializes empty collections as [], never null (see
	// AuditRecord's unmetCriteria contract).
	rec := MakeEvalScoreRecord(EvalScoreArgs{TaskID: "T1", PR: 1, GoalClass: "feat", RubricVersion: "1"})
	blob, err := marshalLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"criteria":[]`) {
		t.Fatalf("nil criteria serialized as null: %s", blob)
	}
	if rec.Total != 0 || rec.Max != 0 {
		t.Fatalf("empty criteria must derive zero totals, got %d/%d", rec.Total, rec.Max)
	}
}

func TestReadEvalScoresSkipsForeignAndCorruptRows(t *testing.T) {
	repo := repoWithLedger(t,
		`{"ts":"1","kind":"audit","taskId":"T1","attempt":1,"verdict":"pass","integrity":"clean","unmetCriteria":[],"summary":""}`,
		`{"ts":"2","kind":"event","taskId":"T2","attempt":1,"event":"loop-result","loop":7,"status":"ok","goal":"g"}`,
		`not json`,
		// A row whose total is a string is schema drift, not a zero: it must be
		// dropped rather than scored as 0 and fire a false drift alarm.
		`{"ts":"3","kind":"event","taskId":"T3","attempt":1,"event":"eval-score","pr":9,"goalClass":"fix","rubricVersion":"1","total":"n/a"}`,
		`{"ts":"4","kind":"event","taskId":"T4","attempt":1,"event":"eval-score","pr":10,"goalClass":"fix","rubricVersion":"1","total":40,"max":100}`,
	)
	got := ReadEvalScores(repo)
	if len(got) != 1 || got[0].TaskID != "T4" || got[0].Total != 40 {
		t.Fatalf("unexpected eval rows: %+v", got)
	}
}

func TestQualityDriftNeedsBaselineAndBeatsNothing(t *testing.T) {
	// One score has no trailing window, so it can never be drift; an equal or
	// higher score is not "below the best" either.
	one := []EvalScoreRecord{rowWith(100, 80)}
	if got := QualityDriftFromScores(one, 10); len(got) != 0 {
		t.Fatalf("first score of a class reported as drift: %+v", got)
	}
	equal := append(append([]EvalScoreRecord{}, one...), rowWith(101, 80))
	if got := QualityDriftFromScores(equal, 10); len(got) != 0 {
		t.Fatalf("equal score reported as drift: %+v", got)
	}
	better := append(append([]EvalScoreRecord{}, equal...), rowWith(102, 95))
	if got := QualityDriftFromScores(better, 10); len(got) != 0 {
		t.Fatalf("improvement reported as drift: %+v", got)
	}
}

func TestQualityDriftNeverComparesAPRWithItself(t *testing.T) {
	// A re-scored PR (judge variance on identical input) must not become its own
	// baseline, and a bucket holding only that PR has no baseline at all.
	records := []EvalScoreRecord{rowWith(100, 90), rowWith(100, 60)}
	if got := QualityDriftFromScores(records, 10); len(got) != 0 {
		t.Fatalf("PR compared against its own earlier score: %+v", got)
	}
}

func TestQualityDriftIsScopedPerClassAndRubric(t *testing.T) {
	feat := scoreRow(100, "T1", "feat", "1", "abc123", EvalCriterionScore{Criterion: "overall", Score: 90, Max: 100})
	worseFeat := scoreRow(101, "T2", "feat", "1", "abc123", EvalCriterionScore{Criterion: "overall", Score: 60, Max: 100})
	docs := scoreRow(102, "T3", "docs", "1", "abc123", EvalCriterionScore{Criterion: "overall", Score: 61, Max: 100})
	// Same class, same declared version, different measured criteria: the old
	// best is not this rubric's baseline (the forgotten `version:` bump case).
	newRubric := scoreRow(103, "T4", "feat", "1", "fff000", EvalCriterionScore{Criterion: "overall", Score: 62, Max: 100})
	v2 := scoreRow(104, "T5", "feat", "2", "abc123", EvalCriterionScore{Criterion: "overall", Score: 63, Max: 100})
	got := QualityDriftFromScores([]EvalScoreRecord{feat, worseFeat, docs, newRubric, v2}, 10)
	if len(got) != 1 {
		t.Fatalf("expected exactly the feat/v1/abc123 regression, got %+v", got)
	}
	if got[0].PR != 101 || got[0].Best != 90 || got[0].Drop != 30 || got[0].BestPR != 100 {
		t.Fatalf("wrong violation: %+v", got[0])
	}
}

func TestQualityDriftExcludesUnmeasurableRows(t *testing.T) {
	// A row whose criteria carried no weights (max 0) is malformed; treating it
	// as a real 0-point score would read as a 100-point cliff.
	bad := MakeEvalScoreRecord(EvalScoreArgs{TaskID: "T1", PR: 1, GoalClass: "feat", RubricVersion: "1"})
	good := rowWith(2, 70)
	if got := QualityDriftFromScores([]EvalScoreRecord{bad, good}, 10); len(got) != 0 {
		t.Fatalf("malformed row participated in the ratchet: %+v", got)
	}
	if got := QualityDriftFromScores([]EvalScoreRecord{good, bad}, 10); len(got) != 0 {
		t.Fatalf("malformed row reported as drift: %+v", got)
	}
}

func TestQualityDriftNamesOffendingCriterion(t *testing.T) {
	// Requirement coverage holds up (28/30 → 26/30). Two criteria collapse, and
	// they disagree about who is worse: test-evidence loses 21 points (24 → 3 of
	// 25) while prd-currency loses its whole scale (15 → 0 of 15). The warning
	// reports the drop in points, so it must name the criterion that explains the
	// largest share of it — test-evidence — not the one with the worst rate.
	best := scoreRow(200, "T1", "fix", "1", "abc123",
		EvalCriterionScore{Criterion: "requirement-coverage", Score: 28, Max: 30},
		EvalCriterionScore{Criterion: "test-evidence", Score: 24, Max: 25},
		EvalCriterionScore{Criterion: "prd-currency", Score: 15, Max: 15},
	)
	regressed := scoreRow(201, "T2", "fix", "1", "abc123",
		EvalCriterionScore{Criterion: "requirement-coverage", Score: 26, Max: 30},
		EvalCriterionScore{Criterion: "test-evidence", Score: 3, Max: 25},
		EvalCriterionScore{Criterion: "prd-currency", Score: 0, Max: 15},
	)
	got := QualityDriftFromScores([]EvalScoreRecord{best, regressed}, 10)
	if len(got) != 1 {
		t.Fatalf("expected one violation, got %+v", got)
	}
	d := got[0]
	if d.Total != 29 || d.Best != 67 || d.Drop != 38 {
		t.Fatalf("totals not derived from criteria: %+v", d)
	}
	if d.Criterion != "test-evidence" || d.CriterionScore != 3 || d.CriterionMax != 25 {
		t.Fatalf("offending criterion misattributed: %+v", d)
	}
	// A zeroed criterion must survive the JSON encoding: the research prompt and
	// the nightly summary read this shape, and 0 is the finding that matters.
	zeroed := d
	zeroed.Criterion, zeroed.CriterionScore = "prd-currency", 0
	blob, err := json.Marshal(zeroed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blob), `"criterionScore":0`) || !strings.Contains(string(blob), `"criterion":"prd-currency"`) {
		t.Fatalf("zeroed criterion dropped from JSON: %s", blob)
	}
}

func TestQualityDriftWindowBoundsTheBaseline(t *testing.T) {
	// Window 2 over [99, 70, 70, 90]: PR 302 is drift (its 2-row window still
	// reaches 300's 99); PR 303 is not — its window holds only the two 70s, so
	// 99 is out of reach as a baseline. Widen the window and 300's 99 becomes
	// everyone's baseline, including 303's.
	records := []EvalScoreRecord{rowWith(300, 99), rowWith(301, 70), rowWith(302, 70), rowWith(303, 90)}
	got := QualityDriftFromScores(records, 2)
	if len(got) != 1 || got[0].PR != 302 || got[0].Best != 99 {
		t.Fatalf("window not honored: %+v", got)
	}
	all := QualityDriftFromScores(records, 10)
	if len(all) != 3 || all[0].PR != 301 || all[1].PR != 302 || all[2].PR != 303 {
		t.Fatalf("window 10 should flag 301, 302 and 303, got %+v", all)
	}
	if all[2].Drop != 9 {
		t.Fatalf("303 drop wrong against the 99 baseline: %+v", all[2])
	}
}

func TestClusterQualityDriftRanksByDrop(t *testing.T) {
	repo := repoWithLedger(t,
		ledgerLine(t, rowWith(400, 90)),
		ledgerLine(t, rowWith(401, 80)),
		ledgerLine(t, rowWith(402, 30)),
	)
	got := ClusterQualityDrift(repo, 0)
	if len(got) != 2 {
		t.Fatalf("expected 2 violations, got %+v", got)
	}
	if got[0].PR != 402 || got[1].PR != 401 {
		t.Fatalf("violations not ranked by drop: %+v", got)
	}
	if got[0].Drop != 60 || got[1].Drop != 10 {
		t.Fatalf("drops wrong: %+v", got)
	}
}

// ledgerLine renders one record as a ledger line (fixtures are line-shaped).
func ledgerLine(t *testing.T, rec EvalScoreRecord) string {
	t.Helper()
	blob, err := marshalLine(rec)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(string(blob), "\n")
}

// keyAt returns the i-th JSON object key of line, in serialization order.
func keyAt(t *testing.T, line string, i int) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil { // consume '{'
		t.Fatalf("line is not a JSON object: %v", err)
	}
	for n := 0; ; n++ {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := tok.(string)
		if !ok {
			continue
		}
		if n == i {
			return key
		}
		var v any
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("decode after %q: %v", key, err)
		}
	}
}
