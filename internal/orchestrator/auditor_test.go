// Package file mirrors test/auditor.test.ts (FR-GO-07, issue #194) — the
// parseAuditReport and buildAuditPrompt describes. The scheduler-transition
// and recovery-contract describes belong to scheduler.ts/fleet.ts ports.

package orchestrator

import (
	"strings"
	"testing"
)

func auditTask(id string, mutate func(*OrchestratorTask)) OrchestratorTask {
	t := OrchestratorTask{ID: id, Title: id, Prompt: "do it", DependsOn: []string{}, Status: TaskStatusPending, Attempts: 0}
	if mutate != nil {
		mutate(&t)
	}
	return t
}

func TestParseAuditReport(t *testing.T) {
	t.Run("parses a valid report with surrounding prose/fences", func(t *testing.T) {
		text := "Here you go:\n```json\n{\"verdict\":\"pass\",\"integrity\":\"clean\",\"criteriaResults\":[{\"criterion\":\"x exists\",\"met\":true,\"evidence\":\"ls shows src/x.ts\"}],\"summary\":\"checked\"}\n```"
		r := ParseAuditReport(text)
		if r == nil {
			t.Fatal("expected a verdict")
		}
		if r.Verdict != "pass" {
			t.Fatalf("verdict = %q", r.Verdict)
		}
		if len(r.CriteriaResults) != 1 {
			t.Fatalf("criteriaResults = %d", len(r.CriteriaResults))
		}
		if !strings.Contains(r.CriteriaResults[0].Evidence, "ls") {
			t.Fatalf("evidence = %q", r.CriteriaResults[0].Evidence)
		}
	})

	t.Run("rejects malformed shapes field-by-field", func(t *testing.T) {
		cases := []string{
			"no json here",
			`{"verdict":"maybe","integrity":"clean","criteriaResults":[{"criterion":"a","met":true,"evidence":"e"}],"summary":"s"}`,
			`{"verdict":"pass","integrity":"clean","criteriaResults":[],"summary":"s"}`,
			`{"verdict":"pass","integrity":"clean","criteriaResults":[{"criterion":"a","met":"yes","evidence":"e"}],"summary":"s"}`,
		}
		for _, c := range cases {
			if r := ParseAuditReport(c); r != nil {
				t.Fatalf("expected nil for %q, got %+v", c, r)
			}
		}
	})

	t.Run("coerces self-contradictory pass-with-unmet-criteria to fail", func(t *testing.T) {
		r := ParseAuditReport(`{"verdict":"pass","integrity":"clean","criteriaResults":[{"criterion":"a","met":true,"evidence":"e1"},{"criterion":"b","met":false,"evidence":"missing"}],"summary":"s"}`)
		if r == nil {
			t.Fatal("expected a verdict")
		}
		if r.Verdict != "fail" {
			t.Fatalf("verdict = %q, want fail", r.Verdict)
		}
	})

	t.Run("accepts ask verdicts with empty criteria but requires the question", func(t *testing.T) {
		r := ParseAuditReport(`{"verdict":"ask","integrity":"clean","criteriaResults":[],"summary":"which DB credentials should the migration use?"}`)
		if r == nil {
			t.Fatal("expected a verdict")
		}
		if r.Verdict != "ask" {
			t.Fatalf("verdict = %q", r.Verdict)
		}
		if r2 := ParseAuditReport(`{"verdict":"ask","integrity":"clean","criteriaResults":[],"summary":"  "}`); r2 != nil {
			t.Fatalf("blank summary must be nil, got %+v", r2)
		}
	})
}

func TestBuildAuditPrompt(t *testing.T) {
	t.Run("includes criteria, constraints, and the untrusted executor claim", func(t *testing.T) {
		task := auditTask("T1", func(t *OrchestratorTask) {
			t.AcceptanceCriteria = []string{"src/x.ts exists"}
			t.BoundaryConstraints = []string{"do not touch src/y.ts"}
			t.Prompt = "implement x"
		})
		p := BuildAuditPrompt(AuditorInput{Goal: "ship it", Task: task, ExecutorDetail: "I am done"})
		for _, want := range []string{"read-only", "1. src/x.ts exists", "- do not touch src/y.ts", "untrusted", "I am done"} {
			if !strings.Contains(p, want) {
				t.Fatalf("prompt missing %q\n---\n%s", want, p)
			}
		}
	})

	t.Run("falls back to expectedOutput when no criteria list", func(t *testing.T) {
		task := auditTask("T1", func(t *OrchestratorTask) {
			t.ExpectedOutput = "npm test passes"
		})
		p := BuildAuditPrompt(AuditorInput{Goal: "g", Task: task})
		if !strings.Contains(p, "npm test passes") {
			t.Fatalf("prompt missing expectedOutput fallback\n---\n%s", p)
		}
	})
}
