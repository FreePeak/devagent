// Package file mirrors src/orchestrator/selfbuild-gate.ts (FR-GO-07, issue #194).

package orchestrator

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
)

// ProductiveStatuses mirrors the TS PRODUCTIVE_STATUSES: ledger statuses that
// count as productive (break a starvation streak / mark a goal shipped).
var ProductiveStatuses = []string{"ok", "pr-open", "merged", "pushed"}

// DegradedStatuses mirrors the TS DEGRADED_STATUSES: ledger statuses exempt
// from starvation counting (expected operator/provider pauses).
var DegradedStatuses = []string{"operator-degraded", "operator-diverged", "provider-degraded"}

// ShippedStatuses is the subset of productive statuses that means "the work
// is in main" — the Q27 no-re-burn guard's match set. `pr-open` is productive
// (a pull request exists; the loop is not starved) but NOT shipped: under the
// merged = shipped semantics (loopdriver DECISION.md) the issue stays
// re-pickable so the next iteration can drive the merge instead of skipping
// it as already shipped — #323 Case B, where six PR-open iterations closed
// their issues and left the work on branches.
var ShippedStatuses = []string{"ok", "merged", "pushed"}

// productiveRe / degradedRe / shippedRe mirror the TS PRODUCTIVE_RE /
// DEGRADED_RE (+ the Go-era shipped split): substring regexes over the raw
// JSONL line.
var productiveRe = regexp.MustCompile(`"status":"(?:ok|pr-open|merged|pushed)"`)

var degradedRe = regexp.MustCompile(`"status":"(?:operator-degraded|operator-diverged|provider-degraded)"`)

var shippedRe = regexp.MustCompile(`"status":"(?:ok|merged|pushed)"`)

// StarvationVerdict mirrors the TS StarvationVerdict.
type StarvationVerdict struct {
	// Starved is true when Count non-productive rows since the last
	// productive row reach Limit.
	Starved bool `json:"starved"`
	// Count: consecutive non-productive, non-degraded rows counted from the
	// ledger tail.
	Count int `json:"count"`
	Limit int `json:"limit"`
}

// AlreadyShippedReason mirrors the TS reason union
// 'goal-prefix' | 'subject-id' | null.
type AlreadyShippedReason string

const (
	ShippedReasonGoalPrefix AlreadyShippedReason = "goal-prefix"
	ShippedReasonSubjectID  AlreadyShippedReason = "subject-id"
	ShippedReasonNone       AlreadyShippedReason = "" // TS null
)

// AlreadyShippedVerdict mirrors the TS AlreadyShippedVerdict. Reason is ""
// where the TS has null.
type AlreadyShippedVerdict struct {
	Shipped bool                 `json:"shipped"`
	Reason  AlreadyShippedReason `json:"reason"`
}

// NormalizeGoalText mirrors normalizeGoalText: normalize a goal/row text the
// way the shell did — strip double quotes, collapse whitespace runs to
// single spaces.
func NormalizeGoalText(text string) string {
	noQuotes := strings.ReplaceAll(text, `"`, "")
	return strings.Join(strings.Fields(noQuotes), " ")
}

// GoalSubjectItem mirrors goalSubjectItem: first backlog item id (Q<number>)
// in the goal's SUBJECT — the text before the first "(" capped at 80 chars.
// Empty string when the goal names none.
func GoalSubjectItem(goal string) string {
	want := NormalizeGoalText(goal)
	subject := want
	if paren := strings.Index(want, "("); paren >= 0 {
		subject = want[:paren]
	}
	subject = firstChars(subject, 80)
	m := qItemRe.FindString(subject)
	return m
}

var qItemRe = regexp.MustCompile(`Q[0-9]+`)

// EvaluateStarvation mirrors evaluateStarvation: walk the ledger from the
// tail; a productive row breaks the streak, degraded rows are skipped
// (neither break nor count), anything else increments. Starved when the
// count reaches Limit.
//
// ExtraProductive carries per-driver ledger dialects onto this shared seam
// (warroom-loop's judge-done / spec-refined) instead of forking the gate.
// Extras only extend the productive set; the degraded exemption is not
// caller-configurable — an outage must never read as starvation for any
// driver.
func EvaluateStarvation(ledgerLines []string, limit int, extraProductive ...string) StarvationVerdict {
	productive := productiveRe
	if len(extraProductive) > 0 {
		alts := make([]string, 0, len(ProductiveStatuses)+len(extraProductive))
		for _, s := range ProductiveStatuses {
			alts = append(alts, regexp.QuoteMeta(s))
		}
		for _, s := range extraProductive {
			alts = append(alts, regexp.QuoteMeta(s))
		}
		productive = regexp.MustCompile(`"status":"(?:` + strings.Join(alts, "|") + `)"`)
	}
	count := 0
	for i := len(ledgerLines) - 1; i >= 0; i-- {
		line := ledgerLines[i]
		if productive.MatchString(line) {
			break
		}
		if degradedRe.MatchString(line) {
			continue
		}
		count++
		if count >= limit {
			break
		}
	}
	return StarvationVerdict{Starved: count >= limit, Count: count, Limit: limit}
}

// AlreadyShipped mirrors alreadyShipped — the Q27 re-burn guard: does any
// productive ledger row already carry this goal?
//
// Rule 1 (prefix): the first 60 normalized chars of the goal appear anywhere
// in a normalized productive row. Rule 2 (subject-id): when the goal's
// subject names a backlog id, a productive row whose own subject (goal field,
// before the first "(") contains that id matches — the id must sit in the
// SUBJECT of both sides, so an incidental "+ Q41" mention inside a
// parenthetical cannot false-positive (loop-100 guard).
//
// One intentional divergence from the awk original: an EMPTY goal never
// matches (the awk `index($0, "")` matched every productive row).
//
// The 60/90/80-char caps slice UTF-16 code units exactly like JS (via
// sliceUTF16), so non-ASCII goal text truncates byte-identically to the TS.

func AlreadyShipped(goal string, ledgerLines []string) AlreadyShippedVerdict {
	want := NormalizeGoalText(goal)
	item := GoalSubjectItem(goal)
	key := firstChars(want, 60)
	for _, raw := range ledgerLines {
		// merged = shipped: only ok|merged|pushed rows prove the issue's work
		// is in main. A `pr-open` row is productive for the starvation gate
		// but must NOT block the issue's next pick — that pick is the
		// verify-and-merge dispatch that lands it (#323 Case B).
		if !shippedRe.MatchString(raw) {
			continue
		}
		row := NormalizeGoalText(raw)
		if key != "" && strings.Contains(row, key) {
			return AlreadyShippedVerdict{Shipped: true, Reason: ShippedReasonGoalPrefix}
		}
		// Isolate the goal field value: drop everything up to the LAST
		// "goal:" (greedy, mirroring the awk `gsub(/.*goal:/, "", $0)`);
		// a row without the marker is checked whole.
		if gi := strings.LastIndex(row, "goal:"); gi >= 0 {
			row = row[gi+len("goal:"):]
		}
		head := firstChars(row, 90)
		if pi := strings.Index(head, "("); pi >= 0 {
			head = head[:pi]
		}
		if item != "" && strings.Contains(head, item) {
			return AlreadyShippedVerdict{Shipped: true, Reason: ShippedReasonSubjectID}
		}
	}
	return AlreadyShippedVerdict{Shipped: false, Reason: ShippedReasonNone}
}

// ProductiveGoals mirrors productiveGoals: goal texts of productive ledger
// rows. Rows that are not JSON or carry no non-empty `goal` field (release
// records, malformed lines) are skipped — they are not goal evidence.
func ProductiveGoals(ledgerLines []string) []string {
	goals := []string{}
	for _, raw := range ledgerLines {
		if !productiveRe.MatchString(raw) {
			continue
		}
		var row struct {
			Goal any `json:"goal"`
		}
		if json.Unmarshal([]byte(raw), &row) != nil {
			continue // unparseable row: no goal evidence
		}
		if g, ok := row.Goal.(string); ok && strings.TrimSpace(g) != "" {
			goals = append(goals, strings.TrimSpace(g))
		}
	}
	return goals
}

// ReadLedgerLines mirrors readLedgerLines: read ledger lines from a JSONL
// file. A missing/unreadable ledger reads as empty — the same fallback the
// shell applied (`[ -f ... ] || return 1` → not starved / not shipped →
// continue). A trailing newline terminates the last record and does not
// produce a phantom empty line (awk record semantics).
func ReadLedgerLines(ledgerPath string) []string {
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		return []string{}
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// firstChars mirrors the TS String.prototype.slice(0, n) caps (60/90/80):
// n counts UTF-16 code units, not bytes or runes. Delegates to merge.go's
// sliceUTF16 so the package has one JS slicing primitive.
func firstChars(s string, n int) string {
	return sliceUTF16(s, n)
}
