package ledger

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// RenderClustersText/ClustersJSON must produce byte-identical output to the
// Node `devagent ledger --clusters` surface (text + --json) for the same
// fixture input, so the expectations below are the exact strings the TS
// renderer emits.

func clustersFixture(t *testing.T) (repo string) {
	t.Helper()
	return fixtureRepo(t, "clusters-full.jsonl")
}

func TestRenderClustersTextFixture(t *testing.T) {
	repo := clustersFixture(t)
	clusters := ClusterFailures(repo)
	classes := ClusterFailureClasses(repo)
	lines := RenderClustersText(clusters, classes, nil, 5, "5")
	want := []string{
		"failure clusters (top 2 of 2):",
		`- "Tests Green" — 2 occurrence(s) across 2 task(s) (1 still open): T1, T2`,
		`- "schema exists" — 1 occurrence(s) across 1 task(s) (1 still open): T3`,
		"failure classes (top 2 of 2):",
		`- "test-gate" — 3 interrupt(s) across 2 task(s): T1, T2 | exemplar: "npm test: 3 failed (same every time)"`,
		`- "worker-error" — 1 interrupt(s) across 1 task(s): T3 | exemplar: "worker CLI crashed"`,
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("text mismatch:\n got %#v\nwant %#v", lines, want)
	}
}

func TestRenderClustersTextTopN(t *testing.T) {
	repo := clustersFixture(t)
	clusters := ClusterFailures(repo)
	classes := ClusterFailureClasses(repo)
	// --clusters 1 shows only the top entry of each view.
	lines := RenderClustersText(clusters, classes, nil, 1, "1")
	if len(lines) != 4 || lines[0] != "failure clusters (top 1 of 2):" || lines[2] != "failure classes (top 1 of 2):" {
		t.Fatalf("top-N mismatch: %#v", lines)
	}
	// Invalid N prints the exact Node error line with the raw option text.
	if got := RenderClustersText(clusters, classes, nil, 0, "abc"); len(got) != 1 || got[0] != "Nothing to show for --clusters abc." {
		t.Fatalf("invalid top mismatch: %#v", got)
	}
}

func TestRenderClustersTextEmpty(t *testing.T) {
	got := RenderClustersText(nil, nil, nil, 5, "")
	want := []string{
		"No failure clusters. Failed audits with unmet criteria and taskInterrupt executor events cluster here once the ledger has records.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestRenderClustersExemplarBounded(t *testing.T) {
	// The Node renderer slices the exemplar to 120 characters.
	long := strings.Repeat("e", 130)
	classes := []FailureClassCluster{{FailureClass: "fc", Occurrences: 1, Tasks: []string{"T1"}, Exemplar: long}}
	lines := RenderClustersText(nil, classes, nil, 5, "")
	if !strings.Contains(lines[1], `| exemplar: "`+strings.Repeat("e", 120)+`"`) {
		t.Fatalf("exemplar not bounded to 120: %s", lines[1])
	}
	if strings.Count(lines[1], "e") > 120+3 { // +3 from "exemplar" word
		t.Fatal("exemplar overshoot")
	}
}

func TestClustersJSONFixture(t *testing.T) {
	repo := clustersFixture(t)
	clusters := ClusterFailures(repo)
	classes := ClusterFailureClasses(repo)
	data, err := ClustersJSON(clusters, classes, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "clusters": [
    {
      "criterion": "Tests Green",
      "occurrences": 2,
      "tasks": [
        "T1",
        "T2"
      ],
      "openTasks": 1
    },
    {
      "criterion": "schema exists",
      "occurrences": 1,
      "tasks": [
        "T3"
      ],
      "openTasks": 1
    }
  ],
  "failureClasses": [
    {
      "failureClass": "test-gate",
      "occurrences": 3,
      "tasks": [
        "T1",
        "T2"
      ],
      "exemplar": "npm test: 3 failed (same every time)"
    },
    {
      "failureClass": "worker-error",
      "occurrences": 1,
      "tasks": [
        "T3"
      ],
      "exemplar": "worker CLI crashed"
    }
  ],
  "qualityDrift": []
}
`
	if string(data) != want {
		t.Fatalf("json mismatch:\n got %s\nwant %s", data, want)
	}
	// Must round-trip as the same shapes JSON.parse in Node would yield.
	var payload struct {
		Clusters       []FailureCluster      `json:"clusters"`
		FailureClasses []FailureClassCluster `json:"failureClasses"`
		QualityDrift   []QualityDrift        `json:"qualityDrift"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Clusters, clusters) || !reflect.DeepEqual(payload.FailureClasses, classes) {
		t.Fatalf("json payload diverges from analytics output")
	}
}

func TestClustersJSONEmptyViews(t *testing.T) {
	// JSON.stringify({clusters: [], failureClasses: [], qualityDrift: []}) —
	// empty arrays, not nulls (the renderer always slices real arrays).
	data, err := ClustersJSON(nil, nil, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\n  \"clusters\": [],\n  \"failureClasses\": [],\n  \"qualityDrift\": []\n}\n" {
		t.Fatalf("empty json mismatch: %s", data)
	}
}

func TestClustersJSONHTMLNotEscaped(t *testing.T) {
	// JSON.stringify leaves <, >, & raw; Go's default encoder escapes them.
	clusters := []FailureCluster{{Criterion: "a & b < c", Occurrences: 1, Tasks: []string{"T1"}, OpenTasks: 1}}
	data, err := ClustersJSON(clusters, nil, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"a & b < c"`) {
		t.Fatalf("HTML characters escaped: %s", data)
	}
}

func TestRenderClustersTextDriftRendersStandalone(t *testing.T) {
	// A healthy repo has no failed audits and no executor interrupts, yet it can
	// hold an eval-score regression — the drift section must not be swallowed by
	// the "no failure clusters" early return, and it must name the criterion.
	drift := []QualityDrift{{
		GoalClass: "feat", RubricVersion: "1", TaskID: "TASK-b-2", PR: 305,
		Total: 61, Max: 100, Best: 88, BestPR: 299, Drop: 27,
		Criterion: "test-evidence", CriterionScore: 0, CriterionMax: 25,
	}}
	lines := RenderClustersText(nil, nil, drift, 5, "")
	want := []string{
		"quality drift (top 1 of 1):",
		`- "feat" — PR #305 (TASK-b-2) scored 61/100 vs best 88 (PR #299) rubric 1: -27 | weakest: test-evidence 0/25`,
	}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("drift lines mismatch:\n got %#v\nwant %#v", lines, want)
	}
	// A PR nobody scored through the orchestrator has no taskId: the line
	// degrades instead of printing empty parens.
	bare := drift
	bare[0].TaskID = ""
	bare[0].Criterion = ""
	lines = RenderClustersText(nil, nil, bare, 5, "")
	if lines[1] != `- "feat" — PR #305 scored 61/100 vs best 88 (PR #299) rubric 1: -27` {
		t.Fatalf("bare drift line wrong: %s", lines[1])
	}
}

func TestClustersJSONCarriesDrift(t *testing.T) {
	// The self-build driver feeds this payload to its research prompt; the
	// offending criterion must survive even when it scored zero.
	drift := []QualityDrift{{
		GoalClass: "fix", RubricVersion: "1", PR: 7, Total: 40, Max: 100,
		Best: 70, BestPR: 5, Drop: 30, Criterion: "prd-currency", CriterionMax: 15,
	}}
	data, err := ClustersJSON(nil, nil, drift, 5)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{`"qualityDrift": [`, `"goalClass": "fix"`, `"drop": 30`, `"criterion": "prd-currency"`, `"criterionScore": 0`} {
		if !strings.Contains(got, want) {
			t.Fatalf("payload missing %s:\n%s", want, got)
		}
	}
}
