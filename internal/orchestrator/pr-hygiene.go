// Package file mirrors src/orchestrator/pr-hygiene.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/ledger"
)

// Landing-evidence triage (closed-or-stale issues): the sweep already owns
// base-superseded and red-across-grace; this extends the same pass with a
// check of the issues a TASK PR cites (`#N` in its body):
// - shipped-elsewhere: a commit on main cites the issue → close citing the sha;
// - superseded/abandoned: issue closed, no landing commit → close as not shipped;
// - genuinely-still-wanted: issue open → leave the PR open, comment evidence.
// Only an actual landing commit may claim a merge; the close comments state it.
// Transport noise on the landing lookup is unknown evidence, not "no commit":
// the PR is left untouched for the next sweep, never closed on a failed listing.

var issueRefRe = regexp.MustCompile(`#[0-9]+`)

// closingRefRe matches the closing keywords that tie an issue to the PR's
// purpose (`Closes #7`, `Fixes: #7`, `resolves #7`) — bare `#N` mentions
// elsewhere in the body are incidental and cite many numbers.
var closingRefRe = regexp.MustCompile(`(?i)\b(?:closes|fixes|resolves)\s*:?\s*#[0-9]+`)

// citedIssues extracts the distinct issue numbers the PR body cites.
// Closing-keyword references win when present; otherwise any `#N` mention
// is used (legacy bodies without keywords).
func citedIssues(body string) []int {
	extract := func(re *regexp.Regexp) []int {
		seen := map[int]bool{}
		out := []int{}
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			ref := m[0]
			if i := strings.LastIndex(ref, "#"); i >= 0 {
				ref = ref[i:]
			}
			n, err := strconv.Atoi(ref[1:])
			if err != nil || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
		return out
	}
	if refs := extract(closingRefRe); len(refs) > 0 {
		return refs
	}
	return extract(issueRefRe)
}

// issueState returns "open"/"closed" via `gh issue view N --json state`, ""
// when the lookup or parse fails (unknown → never triaged on it).
func issueState(repoPath string, n int, run RunGh) string {
	r, err := run([]string{"issue", "view", strconv.Itoa(n), "--json", "state"}, repoPath)
	if err != nil {
		return ""
	}

	var v struct {
		State string `json:"state"`
	}
	if json.Unmarshal([]byte(r.Stdout), &v) != nil {
		return ""
	}
	return v.State
}

// prHygieneFlaggedInApply reports whether the ledger already holds an
// apply-mode pr-hygiene record (detail without the "[dry-run] " prefix) for
// the given PR and reason — the still-wanted evidence comment is posted once
// per PR, not once per sweep.
func prHygieneFlaggedInApply(repoPath string, pr int, reason string) bool {
	data, err := os.ReadFile(filepath.Join(repoPath, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		var r ledger.PrHygieneRecord
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if r.Event == "pr-hygiene" && r.PR == pr && r.Reason == reason && r.Detail != nil && !strings.HasPrefix(*r.Detail, "[dry-run]") {
			return true
		}
	}
	return false
}

// landingCommitOnMain returns the sha of the first commit on main whose
// message cites the issue as `(#N)` (the squash-merge convention). The second
// return says whether the main-branch listing actually answered: (sha, true)
// is a definitive verdict — sha "" then means no landing commit exists —
// while ("", false) is transport noise (the lookup or its parse failed), which
// is never evidence of "not shipped".
func landingCommitOnMain(repoPath string, n int, run RunGh) (string, bool) {
	r, err := run([]string{"api", "repos/{owner}/{repo}/commits?sha=main&per_page=100"}, repoPath)
	if err != nil {
		return "", false
	}
	var commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if json.Unmarshal([]byte(r.Stdout), &commits) != nil {
		return "", false
	}
	for _, c := range commits {
		if strings.Contains(c.Commit.Message, fmt.Sprintf("(#%d)", n)) {
			return c.SHA, true
		}
	}
	return "", true
}

// Zombie-PR hygiene (PRD §17, surviving half of post-PR lifecycle automation):
// per-task PRs ship via publishTaskPr, but nothing reaped them when their
// context died. This sweep closes the loop for `devagent/TASK-*` PRs:
// - base-superseded: the PR's head base branch was merged or deleted, so the
//   branch can never integrate — auto-close it (dry-run: flag only).
// - red-across-grace: CI stays red (completed failures, nothing pending) for
//   the full grace window — flag the PR and skip autoMerge until a green
//   check arrives.
// - age floor: no auto-close happens before the floor (closeAgeFloorHours,
//   capped by the grace window) — a fresh PR is reported untouched instead,
//   whatever the evidence says.
// Every action writes one ledger row (pr, reason, grace-window age) so the
// run ledger stays the replayable record of what automation did to PRs.
//
// The gh primitives (RunGh, DefaultRunGh, PrStatus, ListOpenPrs,
// ProbeBaseBranch, PostPrComment, AgeHours) are the autopr.ts port owned by
// autopr.go — pr-hygiene.ts imports them the same way.

var taskPrRe = regexp.MustCompile(`^devagent/TASK-`)

// strPtr returns a pointer to s (the Go ledger records use *string for the
// optional detail; the module is on go 1.25 so the helper form stays).
func strPtr(s string) *string { return &s }

// formatGraceHours mirrors the TS number-to-string interpolation in the
// red-across-grace detail line (`red across ${graceHours}h ...`): 24 → "24",
// 24.5 → "24.5".
func formatGraceHours(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// GraceAgeHours is the TS graceAgeHours alias of ageHours (pure grace-window
// age in hours; nil when the timestamp is missing or unparseable) — aliased
// to the autopr.go port of ageHours.
var GraceAgeHours = AgeHours

// closeAgeFloorHours caps the age floor before ANY apply-mode auto-close: a PR
// younger than the floor is never closed by the sweep, whatever the evidence
// says (PR #346 was auto-closed 76 seconds after creation on a probe misread).
// The floor defends fresh work from a residual false-gone; the cap keeps a long
// operator-configured grace window from pushing zombie cleanup out by months.
const closeAgeFloorHours = 24.0

// closeUnderAgeFloor reports whether the PR is too young to auto-close for the
// configured grace window, whose own close arm stays authoritative when it is
// shorter. An unknown age counts as floored: no timestamp, no close.
func closeUnderAgeFloor(age *float64, graceHours float64) bool {
	return age == nil || *age < min(graceHours, closeAgeFloorHours)
}

// ageFloorDetail builds the untouched row for a floored close attempt, carrying
// the verdict that would have acted once the PR ages past the floor.
func ageFloorDetail(age *float64, graceHours float64, verdict string) string {
	floor := formatGraceHours(min(graceHours, closeAgeFloorHours))
	if age == nil {
		return fmt.Sprintf("%s, but the PR age is unknown: within the %sh auto-close floor, not closed this sweep", verdict, floor)
	}
	return fmt.Sprintf("%s, but the PR is %.0fh old: within the %sh auto-close floor, not closed this sweep", verdict, *age, floor)
}

// ageLabel renders the grace-window age the sweep's details read it as.
func ageLabel(age *float64) string {
	if age == nil {
		return "unknown age"
	}
	return fmt.Sprintf("%dh since last update", int(*age))
}

// PrHygieneOutcome mirrors the TS PrHygieneOutcome interface.
type PrHygieneOutcome struct {
	PR     int    `json:"pr"`
	Title  string `json:"title"`
	Action string `json:"action"`
	// Why the action fired: base-superseded | red-across-grace |
	// not-a-task-pr | pending | green | red-within-grace | base-unknown |
	// landing-unknown | base-superseded-age-floor | landing-age-floor.
	Reason string `json:"reason"`
	// Hours since the PR's last update (grace-window age); null when unknown
	// or not applicable. TS JSON.stringify emits the key with null, so no
	// omitempty.
	GraceAgeHours *float64 `json:"graceAgeHours"`
	Detail        string   `json:"detail"`
}

// PrHygieneOptions mirrors the TS PrHygieneOptions interface.
type PrHygieneOptions struct {
	// Hours a PR may stay red before it is flagged for skip-autoMerge; also
	// the auto-close age floor, capped by closeAgeFloorHours (config
	// prHygiene.graceHours, default 24).
	GraceHours *float64
	// Report without commenting or closing (config prHygiene.dryRun,
	// default true).
	DryRun *bool
	// Auto-merge flag as seen by the caller; the sweep skips the merge while
	// CI is red across grace.
	AutoMerge *bool
	// Log receives one line per live action. Nil = no-op.
	Log func(msg string)
}

// PrHygieneResult mirrors the TS PrHygieneResult interface.
type PrHygieneResult struct {
	Outcomes []PrHygieneOutcome `json:"outcomes"`
	// True when at least one red-across-grace PR forces autoMerge off for
	// this cycle.
	SkipAutoMerge bool `json:"skipAutoMerge"`
}

// SweepTaskPrHygiene mirrors sweepTaskPrHygiene: zombie-PR hygiene sweep
// over open devagent/TASK-* PRs. Non-task PRs are never touched; dry-run
// (default) flags without acting. Nil run falls back to DefaultRunGh.
func SweepTaskPrHygiene(repoPath string, opts PrHygieneOptions, run RunGh) PrHygieneResult {
	// cfg: NonNullable<DevAgentConfig['prHygiene']> = {} with a try/catch —
	// unreadable config falls through to built-in defaults.
	graceHoursCfg := (*float64)(nil)
	dryRunCfg := (*bool)(nil)
	if cfg, err := config.Load(repoPath); err == nil && cfg.PRHygiene != nil {
		graceHoursCfg = cfg.PRHygiene.GraceHours
		dryRunCfg = cfg.PRHygiene.DryRun
	}
	graceHours := 24.0
	for _, v := range []*float64{opts.GraceHours, graceHoursCfg} {
		if v != nil {
			graceHours = *v
			break
		}
	}
	dryRun := true
	if opts.DryRun != nil {
		dryRun = *opts.DryRun
	} else if dryRunCfg != nil {
		dryRun = *dryRunCfg
	}
	autoMerge := false
	if opts.AutoMerge != nil {
		autoMerge = *opts.AutoMerge
	}
	if run == nil {
		run = DefaultRunGh
	}
	log := opts.Log
	now := timeNowMs()

	prs, err := ListOpenPrs(repoPath, run)
	if err != nil {
		prs = nil
	}
	// .slice().sort((a, b) => a.number - b.number)
	sort.SliceStable(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	outcomes := []PrHygieneOutcome{}
	skipAutoMerge := false

	for _, status := range prs {
		if status.State != "OPEN" {
			outcomes = append(outcomes, PrHygieneOutcome{
				PR: status.Number, Title: status.Title, Action: "skipped",
				Reason:        fmt.Sprintf("state-%s", strings.ToLower(status.State)),
				GraceAgeHours: nil,
				Detail:        fmt.Sprintf("state is %s", status.State),
			})
			continue
		}
		if !taskPrRe.MatchString(status.HeadRefName) {
			outcomes = append(outcomes, PrHygieneOutcome{
				PR: status.Number, Title: status.Title, Action: "untouched",
				Reason:        "not-a-task-pr",
				GraceAgeHours: nil,
				Detail:        fmt.Sprintf("head %s is not a devagent/TASK-* branch", status.HeadRefName),
			})
			continue
		}
		age := AgeHours(status.UpdatedAt, now)
		cv := EvaluateChecksOf(status)

		// Base-superseded first (condition 1): a PR whose head base branch
		// was merged or deleted can never integrate, whatever its CI says —
		// waiting cannot fix a dead base. Only a definitive gh 404 acts: the
		// three-valued probe keeps transport noise from closing a live PR.
		base := ProbeBaseBranch(repoPath, status.BaseRefName, run)
		if base == BaseUnknown {
			outcomes = append(outcomes, PrHygieneOutcome{
				PR: status.Number, Title: status.Title, Action: "untouched",
				Reason:        "base-unknown",
				GraceAgeHours: age,
				Detail:        fmt.Sprintf("base %s lookup failed (transport); not acted on this sweep", status.BaseRefName),
			})
			continue
		}
		if base == BaseGone {
			if closeUnderAgeFloor(age, graceHours) {
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "untouched",
					Reason:        "base-superseded-age-floor",
					GraceAgeHours: age,
					Detail:        ageFloorDetail(age, graceHours, fmt.Sprintf("base %s was merged or deleted", status.BaseRefName)),
				})
				continue
			}
			detail := fmt.Sprintf("base %s was merged or deleted; branch can never integrate", status.BaseRefName)
			if dryRun {
				ledger.AppendPrHygieneRecord(repoPath, ledger.PrHygieneRecord{
					TS: ledger.NowISO(), Kind: "event", Event: "pr-hygiene",
					TaskID: fmt.Sprintf("TASK-%d", status.Number), Attempt: 1,
					PR: status.Number, Action: "flagged", Reason: "base-superseded",
					GraceAgeHours: age,
					Detail:        strPtr(fmt.Sprintf("[dry-run] %s", detail)),
				})
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "flagged",
					Reason: "base-superseded", GraceAgeHours: age,
					Detail: fmt.Sprintf("[dry-run] %s", detail),
				})
			} else {
				_ = PostPrComment(repoPath, status.Number, strings.Join([]string{
					"DevAgent zombie-PR sweep: auto-closing this PR.",
					fmt.Sprintf("Base branch %s was merged or deleted, so this branch can no longer integrate.", status.BaseRefName),
					"Reopen with a re-targeted base if the work is still needed.",
				}, "\n"), run)
				_, _ = run([]string{"pr", "close", fmt.Sprintf("%d", status.Number)}, repoPath)
				ledger.AppendPrHygieneRecord(repoPath, ledger.PrHygieneRecord{
					TS: ledger.NowISO(), Kind: "event", Event: "pr-hygiene",
					TaskID: fmt.Sprintf("TASK-%d", status.Number), Attempt: 1,
					PR: status.Number, Action: "closed", Reason: "base-superseded",
					GraceAgeHours: age,
					Detail:        strPtr(detail),
				})
				if log != nil {
					log(fmt.Sprintf("#%d closed: %s", status.Number, detail))
				}
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "closed",
					Reason: "base-superseded", GraceAgeHours: age, Detail: detail,
				})
			}
			continue
		}

		// Landing-evidence triage: when the PR cites an issue that is
		// closed, CI state no longer matters — the question is whether the
		// work already landed.
		cited := citedIssues(status.Body)
		handled := false
		for _, n := range cited {
			if issueState(repoPath, n, run) != "CLOSED" {
				continue
			}
			var detail, comment, reason string
			sha, known := landingCommitOnMain(repoPath, n, run)
			if !known {
				// Transport noise: the landing evidence is unknown, not
				// absent — closing here would claim "not shipped" for work
				// that may have landed. Leave the PR for the next sweep.
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "untouched",
					Reason:        "landing-unknown",
					GraceAgeHours: age,
					Detail:        fmt.Sprintf("issue #%d is closed but the main-branch commit listing failed (transport); not acted on this sweep", n),
				})
				handled = true
				break
			}
			if sha != "" {
				reason = "shipped-elsewhere"
				detail = fmt.Sprintf("issue #%d is closed and shipped elsewhere: landing commit %s on main", n, sha)
				comment = strings.Join([]string{
					"DevAgent zombie-PR sweep: auto-closing this PR.",
					fmt.Sprintf("Issue #%d is closed and already shipped as commit %s on main.", n, sha),
				}, "\n")
			} else {
				reason = "superseded"
				detail = fmt.Sprintf("issue #%d is closed and no landing commit exists on main: not shipped", n)
				comment = strings.Join([]string{
					"DevAgent zombie-PR sweep: auto-closing this PR.",
					fmt.Sprintf("Issue #%d is closed and no landing commit exists on main, so this work was not shipped.", n),
					"Reopen the issue and this PR if the work is still wanted.",
				}, "\n")
			}
			if closeUnderAgeFloor(age, graceHours) {
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "untouched",
					Reason:        "landing-age-floor",
					GraceAgeHours: age,
					Detail:        ageFloorDetail(age, graceHours, detail),
				})
				handled = true
				break
			}
			if dryRun {
				ledger.AppendPrHygieneRecord(repoPath, ledger.PrHygieneRecord{
					TS: ledger.NowISO(), Kind: "event", Event: "pr-hygiene",
					TaskID: fmt.Sprintf("TASK-%d", status.Number), Attempt: 1,
					PR: status.Number, Action: "flagged", Reason: reason,
					GraceAgeHours: age,
					Detail:        strPtr(fmt.Sprintf("[dry-run] %s", detail)),
				})
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "flagged",
					Reason: reason, GraceAgeHours: age,
					Detail: fmt.Sprintf("[dry-run] %s", detail),
				})
			} else {
				_ = PostPrComment(repoPath, status.Number, comment, run)
				_, _ = run([]string{"pr", "close", fmt.Sprintf("%d", status.Number)}, repoPath)
				ledger.AppendPrHygieneRecord(repoPath, ledger.PrHygieneRecord{
					TS: ledger.NowISO(), Kind: "event", Event: "pr-hygiene",
					TaskID: fmt.Sprintf("TASK-%d", status.Number), Attempt: 1,
					PR: status.Number, Action: "closed", Reason: reason,
					GraceAgeHours: age,
					Detail:        strPtr(detail),
				})
				if log != nil {
					log(fmt.Sprintf("#%d closed: %s", status.Number, detail))
				}
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "closed",
					Reason: reason, GraceAgeHours: age, Detail: detail,
				})
			}
			handled = true
			break
		}
		if handled {
			continue
		}

		if !cv.Pending && !cv.Passed && len(status.Checks) > 0 {
			// Red across the grace window: flag it and hold autoMerge until
			// a green check arrives. Never closed — a fix may still land.
			overdue := age == nil || *age >= graceHours
			if overdue {
				skipAutoMerge = true
				ageStr := ageLabel(age)
				// Genuinely-still-wanted: the cited issues are open, so
				// the PR stays open — record the evidence on the PR.
				if !dryRun && len(cited) > 0 && !prHygieneFlaggedInApply(repoPath, status.Number, "red-across-grace") {
					refs := make([]string, 0, len(cited))
					for _, n := range cited {
						refs = append(refs, fmt.Sprintf("#%d", n))
					}
					_ = PostPrComment(repoPath, status.Number, strings.Join([]string{
						"DevAgent zombie-PR sweep: this PR is red across the grace window and stays open.",
						fmt.Sprintf("Cited issue(s) %s are still open, so the work is still wanted.", strings.Join(refs, ", ")),
					}, "\n"), run)
				}
				detail := fmt.Sprintf("red across %sh grace window (%s); autoMerge skipped until green: %s",
					formatGraceHours(graceHours), ageStr, cv.Summary)
				ledger.AppendPrHygieneRecord(repoPath, ledger.PrHygieneRecord{
					TS: ledger.NowISO(), Kind: "event", Event: "pr-hygiene",
					TaskID: fmt.Sprintf("TASK-%d", status.Number), Attempt: 1,
					PR: status.Number, Action: "flagged", Reason: "red-across-grace",
					GraceAgeHours: age,
					Detail:        strPtr(sliceUTF16(detail, 300)),
				})
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "flagged",
					Reason: "red-across-grace", GraceAgeHours: age,
					Detail: sliceUTF16(detail, 300),
				})
			} else {
				outcomes = append(outcomes, PrHygieneOutcome{
					PR: status.Number, Title: status.Title, Action: "untouched",
					Reason: "red-within-grace", GraceAgeHours: age,
					Detail: fmt.Sprintf("red within grace: %s", cv.Summary),
				})
			}
			continue
		}

		// Base-superseded was handled above; reaching here means the base is
		// intact and the PR is green, pending, or red-within-grace.
		detail := fmt.Sprintf("green and base %s intact: %s", status.BaseRefName, cv.Summary)
		if cv.Pending {
			detail = fmt.Sprintf("checks pending: %s", cv.Summary)
		}
		reason := "green"
		if cv.Pending {
			reason = "pending"
		}
		outcomes = append(outcomes, PrHygieneOutcome{
			PR: status.Number, Title: status.Title, Action: "untouched",
			Reason: reason, GraceAgeHours: age, Detail: detail,
		})
	}

	return PrHygieneResult{Outcomes: outcomes, SkipAutoMerge: skipAutoMerge && autoMerge}
}
