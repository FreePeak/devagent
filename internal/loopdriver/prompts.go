package loopdriver

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func itoa(n int) string { return strconv.Itoa(n) }

func secsToDuration(secs int) time.Duration { return time.Duration(secs) * time.Second }

// tailLines returns the last n lines of a (multi-line) string.
func tailLines(s string, n int) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// firstLineCapped mirrors `head -1 <<<"$GOAL" | cut -c1-100` (the phase task
// breadcrumb detail).
func firstLineCapped(s string, cap int) string {
	line := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		line = s[:i]
	}
	return truncateRunes(line, cap)
}

// prdPolicy is the PRD-per-PR policy block appended to every task dispatch
// prompt (verbatim from the bash driver).
const prdPolicy = `Repo policy: every PR keeps docs/PRD.md current — update the sections this change affects (status claims, architecture notes, roadmap/completion notes) and bump the *Last updated* footer in the same PR. A PR that changes repo state without reflecting it in docs/PRD.md is incomplete.`

// scanPrompt is the prompt the driver hands to `devagent scan-text` for the
// GRADIENT adjacent-category scan. The subcommand itself ignores arguments
// and prints the canonical text from src/research/scan-text.ts; the argument
// documents the intent (scout-prompt parity) without drifting from the
// canonical source.
const scanPrompt = `Adjacent-category scan: survey adjacent industries (sensors, MCP servers, harness tooling) for automation loops, self-maintenance, and agentic behaviors worth borrowing — the agent-products-only funnel is why sentrux was missed.`

// researchPrompt is the phase-1 research prompt template (verbatim from
// scripts/selfbuild-loop.sh RESEARCH_PROMPT, variables filled per
// iteration).
func researchPrompt(n int, repo, prevTail, lessonsCtx, gradient, clusters string) string {
	return "You are phase 1 (Research) of the DevAgent self-build loop, iteration " + itoa(n) + `.
Repo: ` + repo + `. Use ONLY local evidence — no web searches, no network fetches:
1. GitHub issue tracker — open issues labeled selfbuild (the prioritized task queue; priority:P0 > P1 > P2)
2. Recent loop ledger: ` + prevTail + `
3. Accumulated lessons: ` + lessonsCtx + `
4. git log --oneline -15 (what just shipped, what friction it caused)
5. Failure-cluster report (recurring audit gaps + executor failure classes; prefer goals that fix a top cluster): ` + clusters + `

` + gradient + `

Rank the top 3 open tracker issues by (impact x tractability) for a single iteration. If the tracker is empty, propose ONE new goal instead, derived from recent failures, the PRD open questions (section 18 — state history, not a backlog), and the adjacent-category scan. Consider: does an earlier failed loop already cover this? Does a merged PR already cover it? Output compact markdown (<300 words): your ranked top-3 with one-line rationale each (or the proposed new goal), then THE single pick.

Do NOT edit any files. Output only.`
}

// poPrompt is the phases 2-3 PO prompt template (verbatim from
// scripts/selfbuild-loop.sh PO_PROMPT).
func poPrompt(n int, repo, prevTail, lessonsCtx, gradient, clusters string) string {
	return "You are phases 2-3 (Ideas + Validate) of the DevAgent self-build loop, iteration " + itoa(n) + `
Repo: ` + repo + `. Inputs: .selfbuild/research/loop-` + itoa(n) + `.md, the repo state, recent ledger entries below.
` + prevTail + `
` + lessonsCtx + `
` + gradient + `
` + clusters + `
The GitHub issue tracker has no open selfbuild issue, so derive exactly ONE goal scoped to a single implementable+testable iteration from the research above.
Validation checks (all must pass): no dependency on an earlier failed loop; verifiable by the repo test suite or CLI smoke run.
Output ONLY the goal statement (max 120 words), starting with 'Goal:' — this text is passed directly to devagent task as the implementation prompt.`
}

// issueGoalTemplate is the issue-first goal text (verbatim).
func issueGoalTemplate(num int, title string) string {
	return fmt.Sprintf("Goal: Implement GitHub issue #%d (%s) in full and verifiably. Read the issue body for scope, acceptance criteria, and source links before planning. The PR must close the issue on merge.", num, title)
}

// pickAction is how the driver dispatches a research pick: implement the issue
// from scratch, or land it by verifying and merging an already-open PR.
type pickAction int

const (
	actionImplement pickAction = iota
	actionMergePR
)

// pick is the phase-1 "THE single pick" selection carried into goal
// construction (issue #301): the research rationale survived only in
// .selfbuild/research/loop-N.md and was dropped when the driver rendered the
// goal, so a pick that said "merge PR #N, not a rewrite" still dispatched a
// from-scratch re-implementation of work that had already landed.
type pick struct {
	PR        int
	Rationale string
	Action    pickAction
}

var (
	// singlePickRe marks the "then THE single pick" statement the research
	// prompt asks for; the pick is the final block, so its last occurrence wins.
	singlePickRe = regexp.MustCompile(`(?i)\b(?:the\s+)?single\s+pick\b`)
	// pickMergeRe is the "already landed — just merge it" directive.
	pickMergeRe = regexp.MustCompile(`(?i)\b(?:merge|merges|merged|merging|land|lands|landing|ship|ships|shipping)\b`)
	// prRefRe grabs the PR number the merge directive points at ("PR #298").
	prRefRe = regexp.MustCompile(`(?i)\bpr\b\D{0,4}?#?(\d{1,7})`)
)

// parseResearchPick reads the phase-1 research markdown and returns the single
// pick for the *tracker-picked* issue (issueNum): its rationale and — when the
// pick says to merge/land an open PR for that same issue — the action and PR
// number. The pick region MUST reference `#issueNum`; a pick about some other
// issue (even one naming a "PR #N") yields a zero pick, so goal construction
// falls back to the plain implement template — the coupling prevents an
// unrelated research PR mention from hijacking this goal and closing the wrong
// issue. An unparsable artifact (extract-aborted diagnostic, stub) likewise
// yields a zero pick, byte-identical to the pre-#301 driver.
func parseResearchPick(md string, issueNum int) pick {
	if issueNum <= 0 {
		return pick{Action: actionImplement}
	}
	region := singlePickRegion(md)
	if region == "" {
		return pick{Action: actionImplement}
	}
	// Anchor: only the pick that names the dispatched issue is its rationale.
	issueRefRe := regexp.MustCompile("#" + strconv.Itoa(issueNum) + `\b`)
	if !issueRefRe.MatchString(region) {
		return pick{Action: actionImplement}
	}
	p := pick{Action: actionImplement, Rationale: strings.Join(strings.Fields(region), " ")}
	if pickMergeRe.MatchString(region) {
		if m := prRefRe.FindStringSubmatch(region); m != nil {
			if pr, err := strconv.Atoi(m[1]); err == nil && pr > 0 && pr != issueNum {
				p.PR = pr
				p.Action = actionMergePR
			}
		}
	}
	return p
}

// singlePickRegion returns the pick statement: the text after the last "single
// pick" marker, up to the end of its block (a blank line or the document end).
func singlePickRegion(md string) string {
	all := singlePickRe.FindAllStringIndex(md, -1)
	if all == nil {
		return ""
	}
	rest := strings.TrimLeft(md[all[len(all)-1][1]:], " \t:—-\u2013")
	var out []string
	for _, ln := range strings.Split(rest, "\n") {
		if strings.TrimSpace(ln) == "" && len(out) > 0 {
			break
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

// buildIssueGoal renders the dispatched goal for the tracker pick of issue num
// (issue #301): a "merge existing PR" research pick routes to a verify-and-
// merge dispatch; every pick carries the research rationale into the goal text
// so the selection reasoning is no longer dropped.
func buildIssueGoal(num int, title string, p pick) string {
	if p.Action == actionMergePR {
		return mergePRGoalTemplate(num, p.PR, p.Rationale)
	}
	goal := issueGoalTemplate(num, title)
	if r := strings.TrimSpace(p.Rationale); r != "" {
		goal += " Pick rationale: " + r
	}
	return goal
}

// mergePRGoalTemplate is the verify-and-merge dispatch for a pick that lands
// issue num through the already-open PR prNum: check the PR is green, merge it,
// close the issue, sync the PRD — the code exists, so the loop's job is to
// land it, not rewrite it.
func mergePRGoalTemplate(issueNum, prNum int, rationale string) string {
	goal := fmt.Sprintf("Goal: Close GitHub issue #%d by verifying and merging the existing open PR #%d — do NOT re-implement it. Run `gh pr checks %d`; if every check is green, `gh pr merge %d --squash --delete-branch`; if a check is red, fix it on the PR branch and re-check. After the merge lands, close issue #%d and update docs/PRD.md to reflect the shipped change.", issueNum, prNum, prNum, prNum, issueNum)
	if r := strings.TrimSpace(rationale); r != "" {
		goal += " Pick rationale: " + r
	}
	return goal
}
