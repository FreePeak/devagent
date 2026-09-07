package loopdriver

import (
	"fmt"
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
