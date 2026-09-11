// Package file mirrors src/integrations/autopr.ts (FR-GO-07, issue #194).
// Lives in internal/orchestrator per the issue-#194 package mapping.
//
// Auto review + auto merge for GitHub PRs: when the operator opts out of
// manual review (config `autoMerge`), DevAgent reviews its own PRs against
// objective evidence — green CI, mergeability, and a static hazard scan of
// the added diff lines — then approves and squash-merges. A PR that fails any
// gate gets a request-changes review instead; nothing is ever merged on red.
//
// gh subprocess calls run behind the RunGh seam (a func, mirroring the TS
// RunGh type); DefaultRunGh execs via spawn.RunCli. evaluateChecks is NOT
// re-ported here — internal/gates.EvaluateChecks is the committed port and
// this file delegates to it. The CI-fixer decision functions are extracted
// as pure helpers (DecideCiFixRetry / CiFixPrompt) so the failed-then-green
// / still-red / no-fixer sequences are fixture-testable without gh.

package orchestrator

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/spawn"
)

// RunGh mirrors the TS RunGh type: (args, cwd) -> {stdout, stderr}; a
// non-nil error maps the TS throw.
type RunGh func(args []string, cwd string) (*GhResult, error)

// GhResult mirrors the TS {stdout, stderr} return object.
type GhResult struct {
	Stdout string
	Stderr string
}

// GhError carries spawn output on failure, mirroring the TS err.stdout /
// err.stderr / err.code attachment in defaultRunGh.
type GhError struct {
	Message string
	Stdout  string
	Stderr  string
	Code    int
}

func (e *GhError) Error() string { return e.Message }

// DefaultRunGh mirrors the TS defaultRunGh: runCli routes through the
// hardened spawn env so `gh` resolves from the fallback PATH when the parent
// has a minimal env (live-smoke lesson). Non-zero exit becomes a GhError.
func DefaultRunGh(args []string, cwd string) (*GhResult, error) {
	r := spawn.RunCli("gh", args, spawn.Options{Dir: cwd, TimeoutMs: 30_000})
	if r.ExitCode != 0 {
		return nil, &GhError{
			Message: fmt.Sprintf("gh %s exited %d: %s", strings.Join(args, " "), r.ExitCode, sliceUTF16(r.Stderr, 200)),
			Stdout:  r.Stdout,
			Stderr:  r.Stderr,
			Code:    r.ExitCode,
		}
	}
	return &GhResult{Stdout: r.Stdout, Stderr: r.Stderr}, nil
}

// PrCheck mirrors the TS CheckRun (autopr.ts local shape, nullable
// conclusion).
type PrCheck struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Conclusion *string `json:"conclusion"`
}

// PrStatus mirrors the TS PrStatus.
type PrStatus struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	HeadRefName    string `json:"headRefName"`
	BaseRefName    string `json:"baseRefName"`
	State          string `json:"state"`
	Mergeable      string `json:"mergeable"`
	ReviewDecision string `json:"reviewDecision"`
	// Head commit SHA (when reported); identifies the superseded merge
	// candidate in sweep comments.
	HeadRefOid string `json:"headRefOid"`
	// ISO last-update timestamp (when reported); grace-window input for the
	// zombie-PR sweep.
	UpdatedAt string `json:"updatedAt"`
	// Author login; when it equals the gh token's viewer, approvals are
	// impossible.
	Author string `json:"author"`
	// PR body text; the pr-hygiene landing-evidence triage reads the cited
	// issue references (`#N`) from it.
	Body   string    `json:"body"`
	Checks []PrCheck `json:"checks"`
}

// prFields mirrors the TS PR_FIELDS.
const prFields = "number,title,headRefName,baseRefName,state,mergeable,reviewDecision,author,statusCheckRollup,headRefOid,updatedAt,body"

// parsePr mirrors the TS parsePr.
func parsePr(raw map[string]any) PrStatus {
	rollup, _ := raw["statusCheckRollup"].([]any)
	author := ""
	if a, ok := raw["author"].(map[string]any); ok {
		if login, ok := a["login"].(string); ok {
			author = login
		}
	}
	checks := make([]PrCheck, 0, len(rollup))
	for _, c := range rollup {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		var conclusion *string
		if s, ok := cm["conclusion"].(string); ok {
			conclusion = &s
		}
		checks = append(checks, PrCheck{
			Name:       jsString(cm["name"], "unknown"),
			Status:     jsString(cm["status"], "UNKNOWN"),
			Conclusion: conclusion,
		})
	}
	return PrStatus{
		Number:         int(jsNumber(raw["number"])),
		Title:          jsString(raw["title"], ""),
		HeadRefName:    jsString(raw["headRefName"], ""),
		BaseRefName:    jsString(raw["baseRefName"], ""),
		State:          jsString(raw["state"], ""),
		Mergeable:      jsString(raw["mergeable"], "UNKNOWN"),
		ReviewDecision: jsString(raw["reviewDecision"], ""),
		HeadRefOid:     jsString(raw["headRefOid"], ""),
		UpdatedAt:      jsString(raw["updatedAt"], ""),
		Author:         author,
		Body:           jsString(raw["body"], ""),
		Checks:         checks,
	}
}

// ParsePr is the exported view of parsePr for sibling files in this
// package (pr-hygiene port consumes it instead of redefining).
func ParsePr(raw map[string]any) PrStatus {
	return parsePr(raw)
}

// ParsePrList decodes a gh `pr list --json <prFields>` payload into
// []PrStatus.
func ParsePrList(data []byte) ([]PrStatus, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]PrStatus, 0, len(raw))
	for _, p := range raw {
		out = append(out, parsePr(p))
	}
	return out, nil
}

// GateChecks converts []PrCheck into gates.CheckRun (conclusion nil ->
// "") for callers feeding gates.EvaluateChecks directly.
func GateChecks(checks []PrCheck) []gates.CheckRun {
	return toGateChecks(checks)
}

// jsString mirrors TS String(x ?? d) for the gh JSON value domain.
func jsString(v any, d string) string {
	if v == nil {
		return d
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// jsNumber mirrors TS Number(x).
func jsNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f
	case int:
		return float64(n)
	default:
		return 0
	}
}

// toGateChecks converts the local PrCheck slice into gates.CheckRun for the
// shared EvaluateChecks (conclusion nil -> "").
func toGateChecks(checks []PrCheck) []gates.CheckRun {
	out := make([]gates.CheckRun, 0, len(checks))
	for _, c := range checks {
		conclusion := ""
		if c.Conclusion != nil {
			conclusion = *c.Conclusion
		}
		out = append(out, gates.CheckRun{Name: c.Name, Status: c.Status, Conclusion: conclusion})
	}
	return out
}

// EvaluateChecksOf mirrors the TS evaluateChecks(status): delegates to the
// committed internal/gates port — never duplicated here.
func EvaluateChecksOf(status PrStatus) gates.ChecksVerdict {
	return gates.EvaluateChecks(toGateChecks(status.Checks))
}

// ReviewEvent mirrors the TS ReviewEvent union.
type ReviewEvent = string

const (
	ReviewEventApprove        ReviewEvent = "APPROVE"
	ReviewEventRequestChanges ReviewEvent = "REQUEST_CHANGES"
)

// ReviewEvidence mirrors the TS ReviewEvidence.
type ReviewEvidence struct {
	Hazards     []gates.Finding
	MergeMethod string
}

// AutoReview mirrors the TS evaluateAutoReview return shape.
type AutoReview struct {
	Event  ReviewEvent
	Reason string
	Body   string
}

// EvaluateAutoReview mirrors the TS evaluateAutoReview: pure verdict +
// review body from the collected evidence.
func EvaluateAutoReview(status PrStatus, evidence ReviewEvidence) AutoReview {
	cv := EvaluateChecksOf(status)
	conflict := status.Mergeable == "CONFLICTING"
	highHazards := 0
	for _, f := range evidence.Hazards {
		if f.Severity == gates.SeverityHigh {
			highHazards++
		}
	}

	gatesLines := []string{
		fmt.Sprintf("CI: %s", cv.Summary),
		fmt.Sprintf("Mergeable: %s", status.Mergeable),
	}
	var problems []string
	if !cv.Passed {
		problems = append(problems, fmt.Sprintf("CI failed (%s)", strings.Join(cv.FailedChecks, ", ")))
	}
	if conflict {
		problems = append(problems, "branch conflicts with base")
	}

	event := ReviewEventApprove
	head := "Auto-review approved: all evidence gates passed"
	if len(problems) > 0 {
		event = ReviewEventRequestChanges
		head = "Auto-review blocked: " + strings.Join(problems, "; ")
	}
	lines := []string{head, ""}
	for _, g := range gatesLines {
		lines = append(lines, "- "+g)
	}
	lines = append(lines, fmt.Sprintf("- Hazard scan (advisory): %d finding(s), %d high-severity", len(evidence.Hazards), highHazards))
	for _, f := range hazardPreview(evidence.Hazards, 10) {
		lines = append(lines, fmt.Sprintf("  - %s (%s) %s:%s %s", f.RuleID, f.Severity, f.File, findingLine(f), f.Message))
	}
	lines = append(lines, fmt.Sprintf("- Merge strategy: %s", evidence.MergeMethod))
	lines = append(lines, "")
	lines = append(lines, "Reviewed automatically by DevAgent (manual review disabled via autoMerge).")
	return AutoReview{Event: event, Reason: head, Body: strings.Join(lines, "\n")}
}

// hazardPreview mirrors evidence.hazards.slice(0, 10).
func hazardPreview(hazards []gates.Finding, n int) []gates.Finding {
	if len(hazards) <= n {
		return hazards
	}
	return hazards[:n]
}

// findingLine renders the TS `f.line` (number | undefined) — empty when
// unset.
func findingLine(f gates.Finding) string {
	if f.Line == nil {
		return ""
	}
	return strconv.Itoa(*f.Line)
}

// ScanAddedLinesForHazards mirrors the TS scanAddedLinesForHazards: a
// static hazard scan over the PR's added lines. Feeds only the `+` lines of
// the patch to the async-hazard analyzer, so pre-existing hazards in
// untouched code never surface. Findings are ADVISORY in the review verdict:
// the line-level view cannot see multi-line catch handlers, so blocking here
// would stall PRs on false positives; real regressions are caught by CI.
//
// The Go port runs the same four heuristic rules (DA101-DA104) locally —
// the TS analyzer module (src/validation/async-review.ts) has no Go port on
// main yet.
func ScanAddedLinesForHazards(diff string) []gates.Finding {
	byFile := map[string][]string{}
	var order []string
	current := ""
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ b/") {
			current = line[6:]
			if _, seen := byFile[current]; !seen {
				byFile[current] = []string{}
				order = append(order, current)
			}
		} else if strings.HasPrefix(line, "+") && current != "" {
			byFile[current] = append(byFile[current], line[1:])
		}
	}
	files := make([]AsyncSourceFile, 0, len(order))
	for _, path := range order {
		files = append(files, AsyncSourceFile{Path: path, Content: strings.Join(byFile[path], "\n")})
	}
	return AnalyzeAsyncHazards(files)
}

// AsyncSourceFile mirrors the TS AsyncSourceFile (async-review.ts).
type AsyncSourceFile struct {
	Path    string
	Content string
}

// Severity literals used by the hazard rules (mirrors gates severity scale).
const (
	HazardSeverityHigh   = "high"
	HazardSeverityMedium = "medium"
)

// AnalyzeAsyncHazards mirrors analyzeAsyncHazards (src/validation/
// async-review.ts): heuristic async/concurrency review. Regex-based —
// advisory by design; catches the common Node hazards, not a substitute for
// type-aware linting.
func AnalyzeAsyncHazards(files []AsyncSourceFile) []gates.Finding {
	findings := []gates.Finding{}
	for _, file := range files {
		lines := strings.Split(file.Content, "\n")
		for i, line := range lines {
			lineNo := i + 1
			stripped := stripJSComment(line)

			// DA101 high: .then( chain without a .catch( anywhere on the same line/statement
			if hazardThenRe.MatchString(stripped) && !hazardCatchRe.MatchString(stripped) {
				findings = append(findings, gates.Finding{
					RuleID:   "DA101",
					Severity: HazardSeverityHigh,
					Message:  "promise chain uses .then() without .catch(); rejections will be unhandled",
					File:     file.Path,
					Line:     intPtr(lineNo),
				})
			}
			// DA102 medium: async callback passed to forEach (concurrent mutation hazard)
			if hazardForEachAsyncRe.MatchString(stripped) {
				findings = append(findings, gates.Finding{
					RuleID:   "DA102",
					Severity: HazardSeverityMedium,
					Message:  "async callback in forEach(): iterations run concurrently; use for..of + await",
					File:     file.Path,
					Line:     intPtr(lineNo),
				})
			}
			// DA103 high: setInterval without clearInterval in the same file
			if hazardSetIntervalRe.MatchString(stripped) && !hazardClearIntervalRe.MatchString(file.Content) {
				findings = append(findings, gates.Finding{
					RuleID:   "DA103",
					Severity: HazardSeverityHigh,
					Message:  "setInterval() with no clearInterval() in this file; timer leaks across reloads",
					File:     file.Path,
					Line:     intPtr(lineNo),
				})
			}
			// DA104 high: void-cast call (fire-and-forget) swallows rejections
			if hazardVoidCallRe.MatchString(line) {
				findings = append(findings, gates.Finding{
					RuleID:   "DA104",
					Severity: HazardSeverityHigh,
					Message:  "void-cast call discards the promise; errors are silently swallowed",
					File:     file.Path,
					Line:     intPtr(lineNo),
				})
			}
		}
	}
	return findings
}

// hazard rule regexes mirror the TS literals in analyzeAsyncHazards.
var (
	hazardThenRe          = regexp.MustCompile(`\.then\s*\(`)
	hazardCatchRe         = regexp.MustCompile(`\.catch\s*\(`)
	hazardForEachAsyncRe  = regexp.MustCompile(`forEach\s*\(\s*(async\b|\([^)]*\)\s*=>)`)
	hazardSetIntervalRe   = regexp.MustCompile(`\bsetInterval\s*\(`)
	hazardClearIntervalRe = regexp.MustCompile(`\bclearInterval\s*\(`)
	hazardVoidCallRe      = regexp.MustCompile(`^\s*void\s+[A-Za-z_$][\w$]*\s*\(`)
)

func stripJSComment(line string) string {
	idx := strings.Index(line, "//")
	if idx >= 0 {
		return line[:idx]
	}
	return line
}

// MergeQueueSkipReason mirrors the TS MergeQueueSkipReason union.
type MergeQueueSkipReason = string

const (
	SkipReasonRedAcrossGrace MergeQueueSkipReason = "red-across-grace"
	SkipReasonSuperseded     MergeQueueSkipReason = "superseded"
)

// MergeQueueGateOptions mirrors the TS MergeQueueGateOptions.
type MergeQueueGateOptions struct {
	// Hours a PR may stay red before the queue skips it (config
	// prHygiene.graceHours, default 24). nil = default.
	GraceHours *float64
	// PR number superseding this one on the same base (batch view), when
	// known. nil = unknown.
	SupersedingCandidate *int
	// Wall clock for grace-window math (ms since epoch); injectable for
	// tests. nil = time.Now.
	Now *int64
}

// MergeQueueGateVerdict mirrors the TS MergeQueueGateVerdict.
type MergeQueueGateVerdict struct {
	// True when the PR must be skipped with a reason instead of entering the
	// merge path.
	Skip bool
	// Why the skip fired: red-across-grace | superseded. "" when not
	// skipping.
	Reason MergeQueueSkipReason
	Detail string
}

// AgeHours mirrors the TS ageHours: pure grace-window age in hours; nil
// when the timestamp is missing or unparseable.
func AgeHours(updatedAt string, now int64) *float64 {
	ms, ok := jsDateParse(updatedAt)
	if !ok {
		return nil
	}
	age := float64(now-ms) / 3_600_000.0
	if age < 0 {
		age = 0
	}
	return &age
}

// timeNowMs is the wall clock (ms since epoch) used for grace windows; a
// package var so tests can freeze it.
var timeNowMs = func() int64 { return time.Now().UnixMilli() }

// jsDateParse mirrors Date.parse for the ISO/UTC string domain gh emits;
// ok=false for anything unparseable (TS NaN).
func jsDateParse(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	layouts := []string{
		"2006-01-02T15:04:05.000Z07:00",
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// formatHours renders the TS template-literal number for the grace window
// (24 -> "24", 1.5 -> "1.5").
func formatHours(h float64) string {
	if h == math.Trunc(h) {
		return strconv.FormatInt(int64(h), 10)
	}
	return strconv.FormatFloat(h, 'g', -1, 64)
}

func mathFloor(v float64) float64 { return math.Floor(v) }

// EvaluateMergeQueueGate mirrors the TS evaluateMergeQueueGate: merge-queue
// gate (pure) — decides whether an open PR should be skipped with a reason
// instead of parking the auto-merge pipeline:
//   - red-across-grace: CI is red (completed failures, nothing pending) for
//     the full grace window — the CI-Fixer has already had its shot by the
//     time this gate runs, so waiting longer only parks the queue.
//   - superseded: another open PR on the same base is a mergeable candidate
//     while this head is not (conflicting/blocked) — the base will move
//     under it, so merging this head first would ship the wrong candidate.
//
// Green, pending, and red-within-grace PRs pass through untouched.
func EvaluateMergeQueueGate(status PrStatus, opts MergeQueueGateOptions) MergeQueueGateVerdict {
	cv := EvaluateChecksOf(status)
	if opts.SupersedingCandidate != nil &&
		*opts.SupersedingCandidate != status.Number &&
		status.Mergeable != "MERGEABLE" &&
		!cv.Pending &&
		cv.Passed {
		return MergeQueueGateVerdict{
			Skip:   true,
			Reason: SkipReasonSuperseded,
			Detail: fmt.Sprintf("head is not a mergeable candidate (mergeable=%s); base %s superseded by open PR #%d", status.Mergeable, status.BaseRefName, *opts.SupersedingCandidate),
		}
	}
	if !cv.Pending && !cv.Passed && len(status.Checks) > 0 {
		nowMs := int64(0)
		if opts.Now != nil {
			nowMs = *opts.Now
		} else {
			nowMs = timeNowMs()
		}
		graceHours := 24.0
		if opts.GraceHours != nil {
			graceHours = *opts.GraceHours
		}
		age := AgeHours(status.UpdatedAt, nowMs)
		if age == nil || *age >= graceHours {
			ageLabel := "unknown age"
			if age != nil {
				ageLabel = fmt.Sprintf("%dh since last update", int(mathFloor(*age)))
			}
			return MergeQueueGateVerdict{
				Skip:   true,
				Reason: SkipReasonRedAcrossGrace,
				Detail: fmt.Sprintf("red across %sh grace window (%s): %s", formatHours(graceHours), ageLabel, cv.Summary),
			}
		}
	}
	return MergeQueueGateVerdict{Skip: false}
}

// GetPrStatus mirrors the TS getPrStatus.
func GetPrStatus(repoPath string, pr int, run RunGh) (PrStatus, error) {
	r, err := run([]string{"pr", "view", strconv.Itoa(pr), "--json", prFields}, repoPath)
	if err != nil {
		return PrStatus{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &raw); err != nil {
		return PrStatus{}, err
	}
	return parsePr(raw), nil
}

// BaseBranchGone mirrors the TS baseBranchGone: a base branch is gone when
// gh cannot resolve the ref (merged+deleted or deleted directly). Shared by
// the merge-queue gate and the TASK-PR hygiene sweep: a PR whose head base
// branch died can never integrate.
func BaseBranchGone(repoPath string, base string, run RunGh) bool {
	_, err := run([]string{"api", fmt.Sprintf("repos/{owner}/{repo}/branches/%s", base)}, repoPath)
	return err != nil
}

// ListOpenPrs mirrors the TS listOpenPrs.
func ListOpenPrs(repoPath string, run RunGh) ([]PrStatus, error) {
	r, err := run([]string{"pr", "list", "--state", "open", "--json", prFields}, repoPath)
	if err != nil {
		return nil, err
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(r.Stdout), &raw); err != nil {
		return nil, err
	}
	out := make([]PrStatus, 0, len(raw))
	for _, p := range raw {
		out = append(out, parsePr(p))
	}
	return out, nil
}

// GetViewerLogin mirrors the TS getViewerLogin: login owning the gh token;
// used to detect self-authored PRs.
func GetViewerLogin(repoPath string, run RunGh) (string, error) {
	r, err := run([]string{"api", "user", "--jq", ".login"}, repoPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(r.Stdout), nil
}

// GetPrDiff mirrors the TS getPrDiff.
func GetPrDiff(repoPath string, pr int, run RunGh) (string, error) {
	r, err := run([]string{"pr", "diff", strconv.Itoa(pr)}, repoPath)
	if err != nil {
		return "", err
	}
	return r.Stdout, nil
}

// PostPrReview mirrors the TS postPrReview.
func PostPrReview(repoPath string, pr int, event ReviewEvent, body string, run RunGh) error {
	flag := "--request-changes"
	if event == ReviewEventApprove {
		flag = "--approve"
	}
	_, err := run([]string{"pr", "review", strconv.Itoa(pr), flag, "-b", body}, repoPath)
	return err
}

// PostPrComment mirrors the TS postPrComment: evidence delivery for
// self-authored PRs — GitHub rejects self-approvals ("Review Can not approve
// your own pull request"), so the verdict is posted as a plain comment
// instead and merging relies on the gates alone.
func PostPrComment(repoPath string, pr int, body string, run RunGh) error {
	_, err := run([]string{"pr", "comment", strconv.Itoa(pr), "--body", body}, repoPath)
	return err
}

// mergeProtectionRe mirrors the TS /not mergeable|required|protected|not
// authorized/i retry predicate.
var mergeProtectionRe = regexp.MustCompile(`(?i)not mergeable|required|protected|not authorized`)

// MergePr mirrors the TS mergePr.
func MergePr(repoPath string, pr int, method string, deleteBranch bool, run RunGh) error {
	args := []string{"pr", "merge", strconv.Itoa(pr), "--" + method}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	_, err := run(args, repoPath)
	if err != nil {
		// Branch protection can refuse a direct merge while --auto would queue it
		msg := err.Error()
		if mergeProtectionRe.MatchString(msg) {
			_, autoErr := run(append(append([]string{}, args...), "--auto"), repoPath)
			return autoErr
		}
		return fmt.Errorf("gh pr merge %d failed: %s", pr, msg)
	}
	return nil
}
