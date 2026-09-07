// Planner role port (src/orchestrator/planner.ts): a headless planning agent
// decomposes the goal into small precise tasks as JSON. The plan is untrusted
// data: validated field-by-field, ids normalized, dependency cycles rejected,
// and any parse failure falls back to a single-task plan so orchestration
// always makes progress. The worker-dispatch seam is local to this unit
// (mirrors orchestrator.WorkerDispatchRequest→workers.GetWorker(name).Spawn);
// TODO(FR-GO): converge on the shared orchestrator dispatcher when the cli
// wiring lands.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/workers"
)

// plannerSystemPrompt mirrors PLANNER_SYSTEM_PROMPT verbatim.
const plannerSystemPrompt = `You are a software planner. Decompose the given goal into 2-6 small, precise, independently testable implementation tasks for a coding agent.
Rules:
- Each task must be implementable in one focused session in an isolated worktree.
- "acceptanceCriteria" must be a list of machine-checkable completion signals (files that exist, tests that pass, exports present) — an independent auditor will verify each item separately against the environment.
- Optionally add "constraints" for things the executor must NOT do (e.g. touch unrelated modules, change public API).
- Order tasks so dependencies come first; use dependsOn with earlier task ids.
- Respond with ONLY a JSON array (no prose, no markdown fences):
[{"id":"T1","title":"...","prompt":"precise implementation instructions including which files/functions to touch","acceptanceCriteria":["src/x.ts exists and exports y","npm test passes"],"constraints":["do not modify src/other.ts"],"dependsOn":[]}]`

// recoverySystemPrompt mirrors RECOVERY_SYSTEM_PROMPT verbatim.
const recoverySystemPrompt = `You are a software planner. An autonomous coding agent failed this task after exhausting its retries. Write a NEW implementation contract targeting exactly what went wrong — do not repeat the old approach blindly.
Respond with ONLY a JSON object (no prose, no markdown fences):
{"prompt":"precise implementation instructions incorporating what failed and how to avoid it","acceptanceCriteria":["machine-checkable completion signal",...]}`

// rawTask mirrors the TS RawTask interface (untrusted planner output).
type rawTask struct {
	ID                 *string `json:"id"`
	Title              *string `json:"title"`
	Prompt             *string `json:"prompt"`
	Expected           *string `json:"expected"`
	AcceptanceCriteria []any   `json:"acceptanceCriteria"`
	Constraints        []any   `json:"constraints"`
	DependsOn          []any   `json:"dependsOn"`
}

// maxPlanTasks mirrors the TS raw.length > 12 rejection.
const maxPlanTasks = 12

// PlannerDispatcher mirrors the TS `await import('../workers/index.js')`
// getWorker(...).spawn seam. A nil Dispatch falls back to the production
// dispatcher over workers.GetWorker.
type PlannerDispatcher interface {
	Dispatch(req orchestrator.WorkerDispatchRequest) orchestrator.WorkerDispatchResult
}

// PlannerOptions mirrors the TS opts { model?: string; variant?: string }.
type PlannerOptions struct {
	Model   string // "" = unset
	Variant string // "" = unset
	// Dispatch overrides the worker seam (tests inject fakes); nil = spawn
	// the named worker via workers.GetWorker.
	Dispatch PlannerDispatcher
}

// plannerDispatchResult flattens a dispatch result to the fields the planner
// logic consumes (WorkerResult.stderr is folded into ErrorText upstream).
type plannerDispatchResult struct {
	ExitCode   int
	ResultText string
	ErrorText  string
	TimedOut   bool
}

func plnDispatch(d PlannerDispatcher, workerName WorkerName, prompt, repoPath string, timeoutMs int, opts PlannerOptions) plannerDispatchResult {
	if d == nil {
		w, err := workers.GetWorker(workerName)
		if err != nil {
			return plannerDispatchResult{ExitCode: -1, ErrorText: err.Error(), TimedOut: false}
		}
		r := w.Spawn(workers.WorkerSpawnOptions{Prompt: prompt, Cwd: repoPath, TimeoutMs: timeoutMs, Model: opts.Model, Variant: opts.Variant})
		return plannerDispatchResult{ExitCode: r.ExitCode, ResultText: r.ResultText, ErrorText: r.ErrorText, TimedOut: r.TimedOut}
	}
	r := d.Dispatch(orchestrator.WorkerDispatchRequest{Prompt: prompt, Cwd: repoPath, TimeoutMs: timeoutMs, Model: opts.Model, Variant: opts.Variant})
	return plannerDispatchResult{ExitCode: r.ExitCode, ResultText: r.ResultText, ErrorText: r.ErrorText, TimedOut: r.TimedOut}
}

// plnFenceRe tolerates markdown fences around the JSON array.
var plnFenceRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// ParsePlan mirrors parsePlan: untrusted planner output → validated tasks,
// or nil on any rejection.
func ParsePlan(output string) []orchestrator.OrchestratorTask {
	candidate := output
	if m := plnFenceRe.FindStringSubmatch(output); m != nil {
		candidate = m[1]
	}
	start := strings.Index(candidate, "[")
	end := strings.LastIndex(candidate, "]")
	if start == -1 || end <= start {
		return nil
	}
	var raw []rawTask
	if err := json.Unmarshal([]byte(candidate[start:end+1]), &raw); err != nil {
		return nil
	}
	if len(raw) == 0 || len(raw) > maxPlanTasks {
		return nil
	}

	tasks := make([]orchestrator.OrchestratorTask, 0, len(raw))
	seen := map[string]bool{}
	for i := range raw {
		r := &raw[i]
		if r.Title == nil || strings.TrimSpace(*r.Title) == "" {
			return nil
		}
		if r.Prompt == nil || strings.TrimSpace(*r.Prompt) == "" {
			return nil
		}
		id := fmt.Sprintf("T%d", i+1)
		if r.ID != nil && strings.TrimSpace(*r.ID) != "" {
			id = strings.TrimSpace(*r.ID)
		}
		if seen[id] {
			return nil // duplicate ids: reject whole plan
		}
		seen[id] = true
		task := orchestrator.OrchestratorTask{
			ID:        id,
			Title:     plnSliceRunes(*r.Title, 120),
			Prompt:    *r.Prompt,
			DependsOn: plnStringList(r.DependsOn),
			Status:    orchestrator.TaskStatusPending,
			Attempts:  0,
		}
		if ac := plnTrimmedList(r.AcceptanceCriteria); ac != nil {
			task.AcceptanceCriteria = ac
		}
		if bc := plnTrimmedList(r.Constraints); bc != nil {
			task.BoundaryConstraints = bc
		}
		if r.Expected != nil {
			task.ExpectedOutput = *r.Expected
		}
		tasks = append(tasks, task)
	}
	// Normalize dependsOn to known, non-self ids only
	ids := map[string]bool{}
	for _, t := range tasks {
		ids[t.ID] = true
	}
	for i := range tasks {
		tasks[i].DependsOn = plnNormalizeDeps(tasks[i].DependsOn, ids, tasks[i].ID)
	}
	if PlannerHasCycle(tasks) {
		return nil
	}
	return tasks
}

// plnStringList mirrors r.dependsOn.filter((d): d is string => typeof d === 'string').
func plnStringList(items []any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// plnTrimmedList mirrors the TS acceptanceCriteria/constraints arm: every
// item must be a string, else undefined; blanks filtered, capped at 10.
func plnTrimmedList(items []any) []string {
	if items == nil {
		return nil
	}
	strs := make([]string, 0, len(items))
	for _, it := range items {
		s, ok := it.(string)
		if !ok {
			return nil
		}
		strs = append(strs, s)
	}
	out := make([]string, 0, len(strs))
	for _, s := range strs {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

// plnNormalizeDeps mirrors [...new Set(t.dependsOn)].filter(d => ids.has(d) && d !== t.id).
func plnNormalizeDeps(deps []string, ids map[string]bool, selfID string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if seen[d] || !ids[d] || d == selfID {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// plnSliceRunes mirrors the JS String.prototype.slice(0, n).
func plnSliceRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// PlannerHasCycle mirrors hasCycle: DFS with visiting/done coloring.
func PlannerHasCycle(tasks []orchestrator.OrchestratorTask) bool {
	byID := map[string]*orchestrator.OrchestratorTask{}
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	state := map[string]int{} // 1 = visiting, 2 = done
	var visit func(id string) bool
	visit = func(id string) bool {
		switch state[id] {
		case 1:
			return true
		case 2:
			return false
		}
		state[id] = 1
		if t, ok := byID[id]; ok {
			for _, d := range t.DependsOn {
				if _, exists := byID[d]; exists && visit(d) {
					return true
				}
			}
		}
		state[id] = 2
		return false
	}
	for i := range tasks {
		if visit(tasks[i].ID) {
			return true
		}
	}
	return false
}

// FallbackPlan mirrors fallbackPlan: the deterministic single-task plan.
func FallbackPlan(goal string) []orchestrator.OrchestratorTask {
	return []orchestrator.OrchestratorTask{{
		ID:        "T1",
		Title:     fmt.Sprintf("Implement goal: %s", plnSliceRunes(goal, 80)),
		Prompt:    goal,
		DependsOn: []string{},
		Status:    orchestrator.TaskStatusPending,
		Attempts:  0,
	}}
}

// RecoveryContract mirrors planner.ts RecoveryContract.
type RecoveryContract struct {
	Prompt             string   `json:"prompt"`
	AcceptanceCriteria []string `json:"acceptanceCriteria,omitempty"`
}

// ParseRecoveryContract mirrors parseRecoveryContract: untrusted data,
// malformed = nil.
func ParseRecoveryContract(text string) *RecoveryContract {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end <= start {
		return nil
	}
	var raw struct {
		Prompt             *string `json:"prompt"`
		AcceptanceCriteria []any   `json:"acceptanceCriteria"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &raw); err != nil {
		return nil
	}
	if raw.Prompt == nil || strings.TrimSpace(*raw.Prompt) == "" {
		return nil
	}
	var acceptanceCriteria []string
	if raw.AcceptanceCriteria != nil {
		for _, c := range raw.AcceptanceCriteria {
			s, ok := c.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil
			}
			acceptanceCriteria = append(acceptanceCriteria, s)
		}
		if len(acceptanceCriteria) > 10 {
			acceptanceCriteria = acceptanceCriteria[:10]
		}
	}
	return &RecoveryContract{Prompt: *raw.Prompt, AcceptanceCriteria: acceptanceCriteria}
}

// RunRecoveryPlanner mirrors runRecoveryPlanner: manager-style
// re-contracting when retries are exhausted. Returns nil when no recovery
// contract can be produced.
func RunRecoveryPlanner(args struct {
	Goal          string
	Task          orchestrator.OrchestratorTask
	RepoPath      string
	PlannerWorker WorkerName
	TimeoutMs     int
	Opts          PlannerOptions
}) *RecoveryContract {
	t := &args.Task
	var auditNote, gaps, failure string
	if t.Audit != nil {
		var lines []string
		for _, c := range t.Audit.CriteriaResults {
			state := "UNMET"
			if c.Met {
				state = "met"
			}
			lines = append(lines, fmt.Sprintf("- %s: %s — %s", c.Criterion, state, plnSliceRunes(c.Evidence, 200)))
		}
		auditNote = fmt.Sprintf("\nLatest audit (%s/%s):\n%s", t.Audit.Verdict, t.Audit.Integrity, strings.Join(lines, "\n"))
	}
	if len(t.EvidenceGaps) > 0 {
		gaps = "\nEvidence gaps:\n" + plnJoinPrefix(t.EvidenceGaps, "- ")
	}
	if t.FailureDetail != "" {
		failure = fmt.Sprintf("\nFailure detail: %s", plnSliceRunes(t.FailureDetail, 400))
	}
	promptLines := []string{
		recoverySystemPrompt,
		"",
		"## Project goal",
		args.Goal,
		"",
		fmt.Sprintf("## Failed task %s: %s", t.ID, t.Title),
		t.Prompt,
	}
	if len(t.AcceptanceCriteria) > 0 {
		promptLines = append(promptLines, fmt.Sprintf("\nAcceptance criteria were:\n%s", plnJoinPrefix(t.AcceptanceCriteria, "- ")))
	}
	promptLines = append(promptLines, fmt.Sprintf("%s%s%s", auditNote, gaps, failure), fmt.Sprintf("\nAttempts used: %d", t.Attempts))
	// TS: .filter(Boolean).join('\n') — drop empty segments.
	var kept []string
	for _, l := range promptLines {
		if l != "" {
			kept = append(kept, l)
		}
	}
	prompt := strings.Join(kept, "\n")

	result := plnDispatch(args.Opts.Dispatch, args.PlannerWorker, prompt, args.RepoPath, args.TimeoutMs, args.Opts)
	if result.TimedOut || result.ExitCode != 0 {
		return nil
	}
	return ParseRecoveryContract(result.ResultText)
}

// RunPlanner mirrors runPlanner: dispatch the planner worker, parse the
// plan, retry once on the empty-output transient, persist raw output on
// parse failure, and fall back to the deterministic single-task plan (the
// TS function never throws: worker resolution errors surface via the error
// return; parse failures always fall back).
func RunPlanner(goal, repoPath string, plannerWorker WorkerName, timeoutMs int, opts PlannerOptions) ([]orchestrator.OrchestratorTask, error) {
	d, err := plnResolveDispatcher(plannerWorker, opts)
	if err != nil {
		return nil, err
	}
	prompt := fmt.Sprintf("%s\n\n## Goal\n%s", plannerSystemPrompt, goal)
	result := plnDispatch(d, plannerWorker, prompt, repoPath, timeoutMs, opts)
	plan := []orchestrator.OrchestratorTask(nil)
	if !result.TimedOut {
		plan = ParsePlan(result.ResultText)
	}
	// Live-smoke lesson: claude occasionally returns exit 0 with empty stdout
	// (transient). One retry before falling back.
	if plan == nil && !result.TimedOut && result.ResultText == "" {
		retry := plnDispatch(d, plannerWorker, prompt, repoPath, timeoutMs, opts)
		if !retry.TimedOut {
			plan = ParsePlan(retry.ResultText)
		}
		if plan != nil {
			return plan, nil
		}
	}
	if plan != nil {
		return plan, nil
	}
	// Observability: persist raw planner output so parse failures are
	// debuggable (best-effort diagnostics only).
	func() {
		defer func() { _ = recover() }()
		dir := filepath.Join(repoPath, ".devagent-planner")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
		stderr := "n/a"
		if result.ErrorText != "" {
			stderr = plnSliceRunes(result.ErrorText, 200)
		}
		entry := fmt.Sprintf("--- timedOut=%t exitCode=%d resultBytes=%d stderr=%s ---\n%s\n",
			result.TimedOut, result.ExitCode, len(result.ResultText), stderr, plnOrEmpty(result.ResultText))
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("plan-%d.txt", plnNowMillis())), []byte(entry), 0o644)
	}()
	return FallbackPlan(goal), nil
}

// plnResolveDispatcher returns the injected dispatcher or builds the
// production one over workers.GetWorker (mirrors the TS dynamic import +
// getWorker, which throws on an unknown name).
func plnResolveDispatcher(plannerWorker WorkerName, opts PlannerOptions) (PlannerDispatcher, error) {
	if opts.Dispatch != nil {
		return opts.Dispatch, nil
	}
	w, err := workers.GetWorker(plannerWorker)
	if err != nil {
		return nil, err
	}
	return plnWorkerDispatcher{w}, nil
}

// plnWorkerDispatcher adapts a workers.WorkerAdapter to the planner seam.
type plnWorkerDispatcher struct{ w workers.WorkerAdapter }

func (d plnWorkerDispatcher) Dispatch(req orchestrator.WorkerDispatchRequest) orchestrator.WorkerDispatchResult {
	r := d.w.Spawn(workers.WorkerSpawnOptions{
		Prompt: req.Prompt, Cwd: req.Cwd, TimeoutMs: req.TimeoutMs,
		Model: req.Model, Variant: req.Variant,
	})
	return orchestrator.WorkerDispatchResult{
		ExitCode: r.ExitCode, ResultText: r.ResultText, ErrorText: r.ErrorText, TimedOut: r.TimedOut,
	}
}

// plnJoinPrefix renders "- item" lines (TS: map(...).join('\n')).
func plnJoinPrefix(items []string, prefix string) string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, prefix+it)
	}
	return strings.Join(lines, "\n")
}

// plnOrEmpty mirrors `(result.resultText ?? '(empty)')`.
func plnOrEmpty(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}

// plnNowMillis mirrors Date.now() for the diagnostic file name.
func plnNowMillis() int64 { return time.Now().UnixMilli() }
