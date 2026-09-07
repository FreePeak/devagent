// Package file mirrors test/orchestrator/selfbuild-gate.test.ts (FR-GO-07, issue #194).
// The CLI-spawn (`devagent selfbuild-gate`) and shell-wiring describes are
// CliWiring-sibling scope and intentionally not ported here.
package orchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sgRow mirrors the TS row(loop, status, goal) fixture.
func sgRow(loop int, status, goal string) string {
	return `{"loop":` + sgItoa(loop) + `,"ts":"2026-09-06T00:00:0` + sgItoa(loop%10) + `Z","status":"` + status + `","goal":"` + goal + `"}`
}

func sgItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// sgLedgerRepo mirrors ledgerRepo: a repo whose .selfbuild/ledger.jsonl holds lines.
func sgLedgerRepo(t *testing.T, lines []string) (string, string) {
	t.Helper()
	repo := t.TempDir()
	dir := filepath.Join(repo, ".selfbuild")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ledger.jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo, path
}

func TestSelfbuildGateStarvation(t *testing.T) {
	t.Run("counts consecutive non-productive rows from the tail and starves at the limit", func(t *testing.T) {
		lines := []string{sgRow(1, "ok", "shipped work"), sgRow(2, "failed", "x"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x"), sgRow(5, "failed", "x"), sgRow(6, "failed", "x")}
		v := EvaluateStarvation(lines, 5)
		if !v.Starved || v.Count != 5 {
			t.Errorf("verdict = %+v, want starved/5", v)
		}
	})

	t.Run("a productive row breaks the streak — one short of the limit is not starved", func(t *testing.T) {
		lines := []string{sgRow(1, "failed", "x"), sgRow(2, "ok", "shipped"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x"), sgRow(5, "failed", "x"), sgRow(6, "failed", "x")}
		v := EvaluateStarvation(lines, 5)
		if v.Starved || v.Count != 4 {
			t.Errorf("verdict = %+v, want not-starved/4", v)
		}
	})

	t.Run("every documented productive class breaks: ok | pr-open | merged | pushed", func(t *testing.T) {
		for _, status := range []string{"ok", "pr-open", "merged", "pushed"} {
			lines := []string{sgRow(1, "failed", "x"), sgRow(2, "failed", "x"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x"), sgRow(5, "failed", "x"), sgRow(6, status, "handed off")}
			if EvaluateStarvation(lines, 5).Starved {
				t.Errorf("status %q must break the streak", status)
			}
		}
	})

	t.Run("statuses merely containing a productive word do not break (push-failed counts)", func(t *testing.T) {
		lines := []string{sgRow(1, "failed", "x"), sgRow(2, "failed", "x"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x"), sgRow(5, "push-failed", "x")}
		v := EvaluateStarvation(lines, 5)
		if !v.Starved || v.Count != 5 {
			t.Errorf("verdict = %+v, want starved/5", v)
		}
	})

	t.Run("degraded rows are exempt: they neither count nor break the streak", func(t *testing.T) {
		// 2026-09-05 class: three provider-degraded rows must not read as strikes.
		exempt := []string{"operator-degraded", "operator-diverged", "provider-degraded"}
		lines := []string{sgRow(1, "ok", "shipped")}
		for n := 2; n <= 4; n++ {
			lines = append(lines, sgRow(n, exempt[n%3], "pause"))
		}
		lines = append(lines, sgRow(5, "failed", "x"), sgRow(6, "failed", "x"))
		v := EvaluateStarvation(lines, 5)
		if v.Starved || v.Count != 2 {
			t.Errorf("verdict = %+v, want not-starved/2", v)
		}
		// A degraded tail cannot hide an already-starved streak beneath it.
		buried := []string{sgRow(1, "failed", "x"), sgRow(2, "failed", "x"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x"), sgRow(5, "failed", "x"), sgRow(6, "provider-degraded", "pause"), sgRow(7, "provider-degraded", "pause")}
		v = EvaluateStarvation(buried, 5)
		if !v.Starved || v.Count != 5 {
			t.Errorf("buried verdict = %+v, want starved/5", v)
		}
	})

	t.Run("empty ledger is not starved; limit 0 starves trivially (shell -ge parity)", func(t *testing.T) {
		if v := EvaluateStarvation(nil, 5); v.Starved || v.Count != 0 {
			t.Errorf("verdict = %+v, want not-starved/0", v)
		}
		if v := EvaluateStarvation(nil, 0); !v.Starved {
			t.Errorf("limit 0 must starve trivially, got %+v", v)
		}
	})
}

func TestSelfbuildGateShipped(t *testing.T) {
	q41Candidate := "Goal: Q41 — surface consecutive cross-role provider degradation. Loops 106–108 logged three `provider-degraded` rows in 75s; those rows are starvation-gate-exempt"
	// Real loop-100 row: mentions "+ Q41" only inside the parenthetical.
	loop100Row := sgRow(100, "ok", "Goal: Divergence-resilient doc-sync (PRD §17 doc-sync defect + Q41 degradation surface; loops 95–99 burned on generic divergence misclassification). In `src/git")

	t.Run("matches the real re-burn class: goal prefix on a productive row", func(t *testing.T) {
		goal := "Cross-board retry memory beyond the SHA guard so the scout deprioritizes until the root-cause fix lands (Q27)."
		lines := []string{sgRow(53, "ok", "Goal: "+goal)}
		v := AlreadyShipped("Goal: "+goal, lines)
		if !v.Shipped || v.Reason != ShippedReasonGoalPrefix {
			t.Errorf("verdict = %+v, want shipped/goal-prefix", v)
		}
	})

	t.Run("normalizes before matching: quotes and newlines in the candidate still hit", func(t *testing.T) {
		recorded := sgRow(70, "ok", "Goal: fold selfbuild gates — starvation + Q27 re-burn guard into typed code with fixtures and CLI smoke coverage")
		candidate := "Goal: fold selfbuild gates\n\t— \"starvation\" + Q27 re-burn guard into typed code with fixtures and CLI smoke coverage"
		if got := NormalizeGoalText(candidate); !strings.Contains(got, " starvation ") {
			t.Errorf("normalizeGoalText = %q, want to contain ' starvation '", got)
		}
		if !AlreadyShipped(candidate, []string{recorded}).Shipped {
			t.Error("normalized candidate must match")
		}
	})

	t.Run("subject-id match survives goal rewrites (loop-58/71 Q35 class)", func(t *testing.T) {
		lines := []string{sgRow(110, "ok", "Goal: Q41 — surface consecutive cross-role provider degradation. Loops 106–108 logged three `provider-degraded` rows in 75s, and those rows are starvation-gate-")}
		v := AlreadyShipped("Goal: Q41 — surface provider degradation across roles (follow-up slice)", lines)
		if !v.Shipped || v.Reason != ShippedReasonSubjectID {
			t.Errorf("verdict = %+v, want shipped/subject-id", v)
		}
	})

	t.Run("loop-100 guard: an incidental \"+ Q41\" inside a parenthetical is NOT a match", func(t *testing.T) {
		v := AlreadyShipped(q41Candidate, []string{loop100Row})
		if v.Shipped || v.Reason != ShippedReasonNone {
			t.Errorf("verdict = %+v, want not-shipped", v)
		}
	})

	t.Run("non-productive rows never match (skipped/failed rows are not evidence)", func(t *testing.T) {
		lines := []string{sgRow(109, "skipped", q41Candidate), sgRow(110, "failed", q41Candidate)}
		if AlreadyShipped(q41Candidate, lines).Shipped {
			t.Error("non-productive rows must not match")
		}
	})

	t.Run("unrelated goal, empty goal, and missing goal marker all read as not shipped", func(t *testing.T) {
		if AlreadyShipped("Goal: something entirely new (Q99)", []string{loop100Row}).Shipped {
			t.Error("unrelated goal must not match")
		}
		if AlreadyShipped("", []string{loop100Row}).Shipped {
			t.Error("empty goal must never match (intentional awk divergence)")
		}
		if AlreadyShipped("Goal: Q41 x", nil).Shipped {
			t.Error("empty ledger must read not shipped")
		}
	})

	t.Run("goalSubjectItem: id must sit before the first paren, capped at 80 chars", func(t *testing.T) {
		if got := GoalSubjectItem("Goal: Q35 cross-board retry memory (shipped as #100)"); got != "Q35" {
			t.Errorf("item = %q, want Q35", got)
		}
		if got := GoalSubjectItem("Goal: doc-sync surface (PRD §17 + Q41 degradation surface)"); got != "" {
			t.Errorf("item = %q, want empty (id in parenthetical)", got)
		}
		if got := GoalSubjectItem("Goal: " + strings.Repeat("x", 90) + " Q42"); got != "" {
			t.Errorf("item = %q, want empty (id beyond the 80-char cap)", got)
		}
		if got := GoalSubjectItem("Goal: plain goal, no id"); got != "" {
			t.Errorf("item = %q, want empty", got)
		}
	})
}

func TestSelfbuildGateExtras(t *testing.T) {
	t.Run("a dialect status does not break the streak by default, but breaks it when passed as an extra", func(t *testing.T) {
		lines := []string{sgRow(1, "failed", "x"), sgRow(2, "failed", "x"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x"), sgRow(5, "judge-done", "all criteria evidenced")}
		if !EvaluateStarvation(lines, 5).Starved {
			t.Error("judge-done must count as a strike by default")
		}
		if EvaluateStarvation(lines, 5, "judge-done", "spec-refined").Starved {
			t.Error("judge-done must break the streak when passed as an extra")
		}
	})

	t.Run("extras extend the productive set without touching the degraded exemption", func(t *testing.T) {
		// provider-degraded rows still neither count nor break, extras or not —
		// the outage class the sibling awk copies miscounted as starvation.
		lines := []string{sgRow(1, "ok", "shipped")}
		for n := 2; n <= 4; n++ {
			lines = append(lines, sgRow(n, "provider-degraded", "pause"))
		}
		lines = append(lines, sgRow(5, "failed", "x"), sgRow(6, "failed", "x"))
		v := EvaluateStarvation(lines, 5, "judge-done")
		if v.Starved || v.Count != 2 {
			t.Errorf("verdict = %+v, want not-starved/2", v)
		}
	})

	t.Run("extras are regex-escaped: \"a.b\" cannot match a row whose status is \"axb\"", func(t *testing.T) {
		tail := []string{sgRow(1, "failed", "x"), sgRow(2, "failed", "x"), sgRow(3, "failed", "x"), sgRow(4, "failed", "x")}
		if !EvaluateStarvation(append(tail, sgRow(5, "axb", "x")), 5, "a.b").Starved {
			t.Error("a.b must not match axb")
		}
		if EvaluateStarvation(append(tail, sgRow(5, "a.b", "x")), 5, "a.b").Starved {
			t.Error("a.b must match a.b")
		}
	})
}

func TestSelfbuildGateReadLedgerLines(t *testing.T) {
	t.Run("readLedgerLines: trailing newline yields no phantom empty row", func(t *testing.T) {
		_, path := sgLedgerRepo(t, []string{sgRow(1, "ok", "x")})
		lines := ReadLedgerLines(path)
		if len(lines) != 1 {
			t.Errorf("lines = %d, want 1 (%q)", len(lines), lines)
		}
		if got := ReadLedgerLines(filepath.Join(filepath.Dir(path), "nope.jsonl")); len(got) != 0 {
			t.Errorf("missing ledger = %v, want empty", got)
		}
	})
}
