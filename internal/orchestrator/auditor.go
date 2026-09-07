// Package file mirrors src/orchestrator/auditor.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/FreePeak/devagent/internal/ledger"
)

// Auditor role (LongHorizon-Harness lesson): executor self-reports never
// become trusted state. After an executor finishes, an independent read-only
// auditor checks each acceptance criterion against environmental evidence
// (files, tests, command output) and returns a structured verdict. A task
// flips to 'done' only on verdict=pass AND integrity=clean; any workspace
// mutation observed during the audit voids the report.
//
// The auditor sees the task contract and the executor's summary — never its
// transcript or reasoning — so it must re-derive completion from the
// environment itself.

// auditorSystemPrompt mirrors the TS AUDITOR_SYSTEM_PROMPT byte-for-byte.
const auditorSystemPrompt = `You are an independent software auditor. You verify whether a coding task's acceptance criteria are actually met by inspecting the repository yourself. You are read-only: run only non-mutating commands (cat, ls, grep, find, git log/diff/status/show, test runners). Do NOT create, edit, delete, move, or format any file. Do NOT run install, build-with-side-effects, git add/commit/push/checkout, or anything that changes state.

For each acceptance criterion, collect concrete evidence from the environment (command output snippets, file excerpts, test results) and judge ONLY that evidence — do not trust the executor's claims.

Respond with ONLY a JSON object (no prose, no markdown fences):
{"verdict":"pass|fail|ask","integrity":"clean|suspect|violation","criteriaResults":[{"criterion":"...","met":true,"evidence":"command + output excerpt proving it"}],"summary":"one paragraph on what you inspected and how"}
Rules:
- verdict is "pass" only if EVERY criterion is met with real evidence.
- verdict is "ask" when completion cannot be judged without a human decision (missing credentials, ambiguous requirement needing the task owner, action you are not authorized to take). Put the precise question in "summary".
- integrity is "clean" unless you observed signs the workspace was mutated improperly during your inspection (set "violation" if you did, and explain in summary).
- criteriaResults must contain one entry per acceptance criterion (may be empty for "ask").`

// AuditorInput mirrors the TS AuditorInput interface.
type AuditorInput struct {
	Goal string
	Task OrchestratorTask
	// ExecutorDetail: executor's final report text (its claim — data to
	// check, not evidence).
	ExecutorDetail string
}

// BuildAuditPrompt mirrors buildAuditPrompt byte-for-byte: the TS builds an
// array of segments and joins with '\n' after filter(Boolean), which drops
// EVERY empty-string element — including the intentional blank-line
// separators — so conditional sections carry their own leading '\n'.
func BuildAuditPrompt(input AuditorInput) string {
	goal := input.Goal
	task := input.Task
	criteria := task.AcceptanceCriteria
	if len(criteria) == 0 && task.ExpectedOutput != "" {
		criteria = []string{task.ExpectedOutput}
	}
	segments := []string{
		auditorSystemPrompt,
		"",
		"## Project goal",
		goal,
		"",
		fmt.Sprintf("## Task %s: %s", task.ID, task.Title),
		task.Prompt,
		"",
		"## Acceptance criteria to verify",
	}
	if len(criteria) > 0 {
		numbered := make([]string, len(criteria))
		for i, c := range criteria {
			numbered[i] = fmt.Sprintf("%d. %s", i+1, c)
		}
		segments = append(segments, strings.Join(numbered, "\n"))
	} else {
		segments = append(segments, "(none listed — derive them from the task description and list what you checked)")
	}
	if len(task.BoundaryConstraints) > 0 {
		bullets := make([]string, len(task.BoundaryConstraints))
		for i, c := range task.BoundaryConstraints {
			bullets[i] = "- " + c
		}
		segments = append(segments, fmt.Sprintf("\n## Boundary constraints the executor had to respect\n%s", strings.Join(bullets, "\n")))
	}
	if input.ExecutorDetail != "" {
		segments = append(segments, fmt.Sprintf("\n## Executor claim (untrusted — verify, do not assume)\n%s", sliceUTF16(input.ExecutorDetail, 2000)))
	}
	// .filter(Boolean).join('\n')
	kept := make([]string, 0, len(segments))
	for _, s := range segments {
		if s != "" {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "\n")
}

// ParseAuditReport mirrors parseAuditReport: parse and validate an auditor's
// JSON report field-by-field. The report is untrusted data: malformed shapes
// yield nil so the caller can treat the audit as inconclusive rather than
// trusting a partial verdict.
func ParseAuditReport(text string) *AuditVerdict {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err != nil {
		return nil
	}
	var verdict, integrity string
	if err := json.Unmarshal(raw["verdict"], &verdict); err != nil ||
		(verdict != "pass" && verdict != "fail" && verdict != "ask") {
		return nil
	}
	if err := json.Unmarshal(raw["integrity"], &integrity); err != nil ||
		(integrity != "clean" && integrity != "suspect" && integrity != "violation") {
		return nil
	}
	var criteriaRaw []json.RawMessage
	// ask verdicts may skip criteria; pass/fail must carry at least one
	if err := json.Unmarshal(raw["criteriaResults"], &criteriaRaw); err != nil {
		return nil
	}
	if verdict != "ask" && len(criteriaRaw) == 0 {
		return nil
	}
	criteriaResults := make([]CriterionResult, 0, len(criteriaRaw))
	for _, r := range criteriaRaw {
		var c struct {
			Criterion *string `json:"criterion"`
			Met       *bool   `json:"met"`
			Evidence  *string `json:"evidence"`
		}
		if err := json.Unmarshal(r, &c); err != nil ||
			c.Criterion == nil || c.Met == nil || c.Evidence == nil {
			return nil
		}
		criteriaResults = append(criteriaResults, CriterionResult{
			Criterion: *c.Criterion, Met: *c.Met, Evidence: *c.Evidence,
		})
	}
	var summary string
	if err := json.Unmarshal(raw["summary"], &summary); err != nil || jsTrim(summary) == "" {
		return nil
	}
	// LH-Harness rule: pass requires every criterion met AND clean integrity.
	// A self-contradictory "pass" with unmet criteria coerces to fail.
	allMet := true
	for _, c := range criteriaResults {
		if !c.Met {
			allMet = false
			break
		}
	}
	if verdict == "pass" && !allMet {
		verdict = "fail"
	}
	return &AuditVerdict{
		Verdict:         verdict,
		Integrity:       integrity,
		CriteriaResults: criteriaResults,
		Summary:         summary,
	}
}

// dirtyFiles mirrors the TS dirtyFiles: workspace mutation snapshot used to
// enforce auditor read-only discipline (ordered, deduplicated — the TS Set
// preserves insertion order, which the mutated diff relies on).
func dirtyFiles(runner MergeRunner, cwd string) []string {
	r := mergeGit(runner, []string{"status", "--porcelain"}, cwd, 30_000)
	if r.ExitCode != 0 {
		return []string{"<status-failed>"}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, line := range strings.Split(r.Stdout, "\n") {
		s := jsTrim(line)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// AuditorRunner dispatches the auditor worker with the built prompt and
// returns its raw result text; an error covers the TS worker-crash and
// timeout cases (timedOut || exitCode !== 0) — the report is inconclusive.
// TODO(FR-GO-05 #190): replace with the sibling workers adapter port.
type AuditorRunner func(prompt string) (string, error)

// RunAuditArgs mirrors the TS runAudit args object.
type RunAuditArgs struct {
	Board        ProjectBoard
	Task         OrchestratorTask
	WorktreePath string
	TimeoutMs    int
	Auditor      WorkerName
}

// RunAudit mirrors runAudit: dispatch an independent auditor worker over the
// task contract, void the report on any observed workspace mutation, and
// persist the verdict to the run ledger (best-effort). Returns nil when the
// audit is inconclusive (worker crash or unparsable report) — except a
// mutation is always reported as a fabricated violation.
func RunAudit(args RunAuditArgs, run AuditorRunner, runner MergeRunner) *AuditVerdict {
	before := dirtyFiles(runner, args.WorktreePath)
	var report *AuditVerdict
	if run != nil {
		// TS also forwards cfg.model/cfg.variant into worker.spawn; the
		// AuditorRunner seam owns that selection until FR-GO-05 lands.
		if text, err := run(BuildAuditPrompt(AuditorInput{Goal: args.Board.Goal, Task: args.Task})); err == nil {
			report = ParseAuditReport(text)
		}
	}
	after := dirtyFiles(runner, args.WorktreePath)
	// Harness-enforced integrity (LH lesson): any workspace change while the
	// auditor ran voids the report regardless of what it claims.
	mutated := []string{}
	for _, f := range after {
		found := false
		for _, b := range before {
			if b == f {
				found = true
				break
			}
		}
		if !found {
			mutated = append(mutated, f)
		}
	}
	if len(mutated) > 0 {
		// TS: mutated.slice(0, 5).join(', ')
		first5 := mutated
		if len(first5) > 5 {
			first5 = first5[:5]
		}
		joined := strings.Join(first5, ", ")
		if report != nil {
			report.Integrity = "violation"
			report.Summary = fmt.Sprintf("%s\n[harness] workspace mutated during audit: %s", report.Summary, joined)
		} else {
			report = &AuditVerdict{
				Verdict:   "fail",
				Integrity: "violation",
				CriteriaResults: []CriterionResult{{
					Criterion: "audit completed",
					Met:       false,
					Evidence:  fmt.Sprintf("workspace mutated during audit: %s", joined),
				}},
				Summary: "[harness] workspace mutated during audit",
			}
		}
	}
	// Persist to the run ledger (best-effort): history survives worktree
	// cleanup and board resets. Inconclusive runs record a fail/unknown
	// entry so gaps in the ledger are meaningful, not silent.
	verdict := ledger.Verdict{
		Verdict:   "fail",
		Integrity: "suspect",
		Summary:   "audit inconclusive: worker crash or unparsable report",
	}
	if report != nil {
		crs := make([]ledger.CriterionResult, 0, len(report.CriteriaResults))
		for _, c := range report.CriteriaResults {
			crs = append(crs, ledger.CriterionResult{Criterion: c.Criterion, Met: c.Met, Evidence: c.Evidence})
		}
		verdict = ledger.Verdict{
			Verdict:         report.Verdict,
			Integrity:       report.Integrity,
			CriteriaResults: crs,
			Summary:         report.Summary,
		}
	}
	ledger.AppendAuditRecord(RepoRootFrom(args.WorktreePath, runner), ledger.MakeAuditRecord(args.Task.ID, args.Task.Attempts, verdict, ""))
	return report
}

// RepoRootFrom mirrors repoRootFrom: resolve the main repository root from a
// linked worktree so ledger writes land in one durable place regardless of
// which worktree ran the audit. Falls back to the worktree itself outside a
// git context.
func RepoRootFrom(worktreePath string, runner MergeRunner) string {
	r := mergeGit(runner, []string{"rev-parse", "--git-common-dir"}, worktreePath, 15_000)
	dir := jsTrim(r.Stdout)
	if r.ExitCode != 0 || dir == "" {
		return worktreePath
	}
	if strings.HasSuffix(dir, "/.git") || dir == ".git" {
		return filepath.Join(worktreePath, dir, "..")
	}
	return filepath.Dir(dir)
}
