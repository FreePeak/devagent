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
	lines := RenderClustersText(clusters, classes, 5, "5")
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
	lines := RenderClustersText(clusters, classes, 1, "1")
	if len(lines) != 4 || lines[0] != "failure clusters (top 1 of 2):" || lines[2] != "failure classes (top 1 of 2):" {
		t.Fatalf("top-N mismatch: %#v", lines)
	}
	// Invalid N prints the exact Node error line with the raw option text.
	if got := RenderClustersText(clusters, classes, 0, "abc"); len(got) != 1 || got[0] != "Nothing to show for --clusters abc." {
		t.Fatalf("invalid top mismatch: %#v", got)
	}
}

func TestRenderClustersTextEmpty(t *testing.T) {
	got := RenderClustersText(nil, nil, 5, "")
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
	lines := RenderClustersText(nil, classes, 5, "")
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
	data, err := ClustersJSON(clusters, classes, 5)
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
  ]
}
`
	if string(data) != want {
		t.Fatalf("json mismatch:\n got %s\nwant %s", data, want)
	}
	// Must round-trip as the same shapes JSON.parse in Node would yield.
	var payload struct {
		Clusters       []FailureCluster      `json:"clusters"`
		FailureClasses []FailureClassCluster `json:"failureClasses"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(payload.Clusters, clusters) || !reflect.DeepEqual(payload.FailureClasses, classes) {
		t.Fatalf("json payload diverges from analytics output")
	}
}

func TestClustersJSONEmptyViews(t *testing.T) {
	// JSON.stringify({clusters: [], failureClasses: []}) — empty arrays, not
	// nulls (the TS code always slices real arrays).
	data, err := ClustersJSON(nil, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\n  \"clusters\": [],\n  \"failureClasses\": []\n}\n" {
		t.Fatalf("empty json mismatch: %s", data)
	}
}

func TestClustersJSONHTMLNotEscaped(t *testing.T) {
	// JSON.stringify leaves <, >, & raw; Go's default encoder escapes them.
	clusters := []FailureCluster{{Criterion: "a & b < c", Occurrences: 1, Tasks: []string{"T1"}, OpenTasks: 1}}
	data, err := ClustersJSON(clusters, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"a & b < c"`) {
		t.Fatalf("HTML characters escaped: %s", data)
	}
}
