package loopdriver

import (
	"strings"
	"testing"
)

// Issue #301: prompts-level pins for the goal strings a dispatch is built
// from. The loop tests exercise the merge/implement selection through fake
// worker CLIs; these pin the templates themselves and the rationale
// ride-along directly.
func TestWithPickRationale(t *testing.T) {
	goal := issueGoalTemplate(290, "FR-VAL-02: devagent doctor")
	cases := []struct {
		name      string
		rationale string
		want      string
	}{
		{
			// Nothing to carry: the goal must degrade byte-identically to the
			// pre-#301 dispatch.
			name: "empty rationale is a no-op", rationale: "",
			want: goal,
		},
		{
			name:      "merge-pick rationale rides along",
			rationale: "#290 — merge PR #298, not a rewrite",
			want:      goal + "\nPick rationale from phase 1 (act on it; do not re-derive it): #290 — merge PR #298, not a rewrite",
		},
		{
			// Verbatim passthrough: researchPick caps the rationale
			// (pickRationaleCap) upstream; withPickRationale must not mangle
			// what survives.
			name:      "multi-line rationale passes through verbatim",
			rationale: "land via open PR #298\n(CI green)",
			want:      goal + "\nPick rationale from phase 1 (act on it; do not re-derive it): land via open PR #298\n(CI green)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := withPickRationale(goal, tc.rationale); got != tc.want {
				t.Fatalf("withPickRationale = %q, want %q", got, tc.want)
			}
		})
	}
}

// A merge pick's goal must carry the verify-and-merge dispatch and must never
// emit the from-scratch implement template (issue #301: loop 219 burned a
// full iteration re-implementing #290 while green PR #298 sat open).
func TestMergeGoalTemplateDropsImplementTemplate(t *testing.T) {
	goal := mergeGoalTemplate(298, 290)
	for _, want := range []string{
		"verifying and merging the existing open PR #298",
		"gh pr checks 298",
		"do NOT re-implement",
		"close issue #290 citing the pull request",
	} {
		if !strings.Contains(goal, want) {
			t.Fatalf("merge goal missing %q:\n%s", want, goal)
		}
	}
	if strings.Contains(goal, "in full and verifiably") {
		t.Fatalf("merge goal emitted the implement template:\n%s", goal)
	}
	// The implement marker must survive on the implement template — it is
	// what tells the two dispatches apart in a goal file (and what the loop
	// tests grep for).
	if !strings.Contains(issueGoalTemplate(290, "x"), "in full and verifiably") {
		t.Fatal("implement template lost its marker — the two templates are indistinguishable")
	}
}
