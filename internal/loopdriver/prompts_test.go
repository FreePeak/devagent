package loopdriver

import (
	"strings"
	"testing"
)

// Issue #301: the phase-1 research pick must survive into goal construction.
// The loop-driver tests exercise the same path through fake worker CLIs; these
// pin the parser directly. The regression is a "land via open PR #N" pick
// routing to verify-and-merge instead of the prompts.go implement template.
func TestParseResearchPick(t *testing.T) {
	cases := []struct {
		name     string
		md       string
		issueNum int
		want     pick
	}{
		{
			name: "merge PR phrasing", issueNum: 290,
			md:   "ranked: #291, #290\n\nTHE single pick: #290 — merge PR #298, not a rewrite",
			want: pick{PR: 298, Action: actionMergePR, Rationale: "#290 — merge PR #298, not a rewrite"},
		},
		{
			name: "land via open PR phrasing", issueNum: 290,
			md:   "**Picks** ...\nThe single pick is #290: land via open PR #298 (CI green).",
			want: pick{PR: 298, Action: actionMergePR, Rationale: "is #290: land via open PR #298 (CI green)."},
		},
		{
			name: "implement pick keeps rationale, no PR", issueNum: 290,
			md:   "top-3 ...\n\nTHE single pick: #290 fix the worktree leak",
			want: pick{Action: actionImplement, Rationale: "#290 fix the worktree leak"},
		},
		{
			// Dilution guard: a merge pick about a DIFFERENT issue must not
			// hijack this goal (would merge an unrelated PR, close wrong issue).
			name: "other issue's merge pick is ignored", issueNum: 290,
			md:   "THE single pick: #295 — merge PR #298",
			want: pick{Action: actionImplement},
		},
		{
			// The PR number must differ from the issue; "#290" alone is not a
			// merge directive (no "PR" token), so it stays an implement pick.
			name: "no PR token keeps implement", issueNum: 290,
			md:   "THE single pick: #290 — close on merge",
			want: pick{Action: actionImplement, Rationale: "#290 — close on merge"},
		},
		{
			name: "extract-aborted diagnostic is not a pick", issueNum: 290,
			md:   "[extract-aborted] no assistant text in 3 NDJSON events",
			want: pick{Action: actionImplement},
		},
		{
			name: "dry-run stub is not a pick", issueNum: 290,
			md:   "# dry-run stub",
			want: pick{Action: actionImplement},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseResearchPick(tc.md, tc.issueNum); got != tc.want {
				t.Fatalf("parseResearchPick = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A merge pick's goal must carry the verify-and-merge dispatch (and the
// rationale), and must never emit the from-scratch implement template.
func TestBuildIssueGoalMergePickDropsImplementTemplate(t *testing.T) {
	goal := buildIssueGoal(290, "Cleanup", pick{
		PR: 298, Action: actionMergePR, Rationale: "merge PR #298, not a rewrite",
	})
	for _, want := range []string{
		"verifying and merging the existing open PR #298",
		"gh pr checks 298",
		"do NOT re-implement",
		"Pick rationale: merge PR #298, not a rewrite",
	} {
		if !strings.Contains(goal, want) {
			t.Fatalf("merge goal missing %q:\n%s", want, goal)
		}
	}
	if strings.Contains(goal, "in full and verifiably") {
		t.Fatalf("merge goal emitted the implement template:\n%s", goal)
	}
}
