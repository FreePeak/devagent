// Package file mirrors src/orchestrator/pr-hygiene.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/ledger"
)

// Zombie-PR hygiene (PRD §17, surviving half of post-PR lifecycle automation):
// per-task PRs ship via publishTaskPr, but nothing reaped them when their
// context died. This sweep closes the loop for `devagent/TASK-*` PRs:
// - base-superseded: the PR's head base branch was merged or deleted, so the
//   branch can never integrate — auto-close it (dry-run: flag only).
// - red-across-grace: CI stays red (completed failures, nothing pending) for
//   the full grace window — flag the PR and skip autoMerge until a green
//   check arrives.
// Every action writes one ledger row (pr, reason, grace-window age) so the
// run ledger stays the replayable record of what automation did to PRs.
//
// The gh primitives (RunGh, DefaultRunGh, PrStatus, ListOpenPrs,
// BaseBranchGone, PostPrComment, AgeHours) are the autopr.ts port owned by
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

// PrHygieneOutcome mirrors the TS PrHygieneOutcome interface.
type PrHygieneOutcome struct {
	PR     int    `json:"pr"`
	Title  string `json:"title"`
	Action string `json:"action"`
	// Why the action fired: base-superseded | red-across-grace |
	// not-a-task-pr | pending | green | red-within-grace.
	Reason string `json:"reason"`
	// Hours since the PR's last update (grace-window age); null when unknown
	// or not applicable. TS JSON.stringify emits the key with null, so no
	// omitempty.
	GraceAgeHours *float64 `json:"graceAgeHours"`
	Detail        string   `json:"detail"`
}

// PrHygieneOptions mirrors the TS PrHygieneOptions interface.
type PrHygieneOptions struct {
	// Hours a PR may stay red before it is flagged for skip-autoMerge
	// (config prHygiene.graceHours, default 24).
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
		// waiting cannot fix a dead base.
		if BaseBranchGone(repoPath, status.BaseRefName, run) {
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

		if !cv.Pending && !cv.Passed && len(status.Checks) > 0 {
			// Red across the grace window: flag it and hold autoMerge until
			// a green check arrives. Never closed — a fix may still land.
			overdue := age == nil || *age >= graceHours
			if overdue {
				skipAutoMerge = true
				ageStr := "unknown age"
				if age != nil {
					ageStr = fmt.Sprintf("%dh since last update", int(*age))
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
