// Backlog reconciliation: the Go port of src/task.ts's PRD backlog pick
// reconciliation half (curation run 24 decision) — parseBacklogItems,
// extractCompletionNotes, checkBacklogPick, strikeBacklogItems and
// listMergedPrTitles. Before dispatching a backlog item, validate against
// merged PR titles and PRD completion notes; a shipped item is rejected and
// struck from the Phase 4 backlog section in the same run.

package pipeline

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/FreePeak/devagent/internal/spawn"
)

// BacklogItem mirrors task.ts BacklogItem: one Phase 4 current-backlog bullet.
type BacklogItem struct {
	// ID is the backlog item id, e.g. "Q40".
	ID string
	// Title is the bold title text from the line.
	Title string
	// Line is the full markdown line, e.g. "- **Title** — description (Q40)."
	Line string
	// LineNumber is the 1-based line in docs/PRD.md — what a `PRD:<line>`
	// pick ref resolves to.
	LineNumber int
	// Struck is whether the line is already struck (wrapped in ~~).
	Struck bool
}

// BacklogPickCheck mirrors task.ts BacklogPickCheck.
type BacklogPickCheck struct {
	OK      bool
	Shipped bool
	Message string
	// Prompt is line text suitable as dispatch prompt, set when OK and !Shipped.
	Prompt string
	// StruckIDs lists items confirmed shipped in the current run (for
	// updating docs/PRD.md).
	StruckIDs []string
}

var (
	itemIDRe       = regexp.MustCompile(`\b[A-Z]+\d+\b`)
	headingRe      = regexp.MustCompile(`^#{1,6}\s`)
	bulletRe       = regexp.MustCompile(`^[-*]\s`)
	struckBulletRe = regexp.MustCompile(`^~~[-*]\s`)
	boldTitleRe    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	notePrefixRe   = regexp.MustCompile(`^>\s*`)
	nonAlnumRe     = regexp.MustCompile(`[^a-z0-9\s]`)
	whitespaceRe   = regexp.MustCompile(`\s+`)
	shaPrefixRe    = regexp.MustCompile(`^[0-9a-f]+\s+`)
	prdLineRefRe   = regexp.MustCompile(`(?i)^PRD:\s*(\d+)$`)
)

// PartialCompletionRE mirrors task.ts PARTIAL_COMPLETION_RE: markers in a
// ledger-row goal proving the referenced item is being consumed in parts
// ("PRD:888 remainder — migrate the sibling drivers"), not finished. While
// any goal referencing an item matches this, the ledger tier withholds its
// evidence and the item survives unreduced.
var PartialCompletionRE = regexp.MustCompile(`(?i)\b(remainder|residual|partial|slice|increment|follow-up)\b`)

// lstrip mirrors JS String.prototype.trimStart.
func lstrip(s string) string {
	return strings.TrimLeftFunc(s, unicode.IsSpace)
}

// extractItemID mirrors extractItemId: the LAST standalone [A-Z]+\d+ token
// (e.g. "(Q40)" or "(2026-09-01 human deep-dive; Q38)"). Items conventionally
// carry their id in a trailing parenthetical, but the real PRD has placed it
// mid-parenthetical (GRADIENT, Q38), so the token may appear anywhere.
func extractItemID(line string) string {
	ms := itemIDRe.FindAllString(line, -1)
	if len(ms) == 0 {
		return ""
	}
	return ms[len(ms)-1]
}

// ParseBacklogItems mirrors parseBacklogItems: the Phase 4 current-backlog
// section of docs/PRD.md as structured items. Heading detection is
// case-insensitive on "current backlog" + "phase 4"; the next heading ends
// the section. Bullet lines (including struck ones wrapped in ~~) carry the
// bold **title** and the trailing item id.
func ParseBacklogItems(prd string) []BacklogItem {
	lines := strings.Split(prd, "\n")
	items := []BacklogItem{}
	inSection := false

	for idx, raw := range lines {
		trimmed := lstrip(raw)

		// Track heading boundaries: next heading ends the section.
		if headingRe.MatchString(trimmed) {
			if inSection {
				break
			}
			lower := strings.ToLower(trimmed)
			if strings.Contains(lower, "current backlog") && strings.Contains(lower, "phase 4") {
				inSection = true
			}
			continue
		}
		if !inSection {
			continue
		}

		// Only parse bullet lines (including struck ones wrapped in ~~).
		if !bulletRe.MatchString(trimmed) && !struckBulletRe.MatchString(trimmed) {
			continue
		}
		struck := strings.HasPrefix(trimmed, "~~-") || strings.HasPrefix(trimmed, "~~*")
		body := trimmed
		if struck {
			body = lstrip(strings.TrimSuffix(strings.TrimPrefix(trimmed, "~~"), "~~"))
		}

		id := extractItemID(body)
		if id == "" {
			continue
		}

		title := ""
		if m := boldTitleRe.FindStringSubmatch(body); m != nil {
			title = strings.TrimSpace(m[1])
		}
		items = append(items, BacklogItem{
			ID:         id,
			Title:      title,
			Line:       raw,
			LineNumber: idx + 1,
			Struck:     struck,
		})
	}

	return items
}

// ExtractCompletionNotes mirrors extractCompletionNotes: blockquote
// paragraphs starting with `> **Completed` — these list what shipped in each
// curation run. Multi-line blockquotes are joined with a space so the full
// note text is matchable.
func ExtractCompletionNotes(prd string) []string {
	notes := []string{}
	cur := ""
	hasCur := false
	flush := func() {
		if hasCur {
			notes = append(notes, cur)
		}
		cur = ""
		hasCur = false
	}
	for _, raw := range strings.Split(prd, "\n") {
		t := strings.TrimSpace(raw)
		if !strings.HasPrefix(t, ">") {
			flush()
			continue
		}
		text := notePrefixRe.ReplaceAllString(t, "")
		if strings.HasPrefix(text, "**Completed") {
			flush()
			cur = text
			hasCur = true
		} else if hasCur {
			cur += " " + text
		}
	}
	flush()
	return notes
}

// NormalizeTitle mirrors task.ts normalizeTitle: lowercase, non-alphanumerics
// to spaces, whitespace collapsed, trimmed.
func NormalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = nonAlnumRe.ReplaceAllString(s, " ")
	s = whitespaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// CheckBacklogPick mirrors checkBacklogPick: check a backlog pick ("Q40" or
// "PRD:889") against merged PR titles, PRD completion notes, and — as
// fallback evidence when merged PR titles are generic (auto-cleanup
// snapshots name nothing) — productive ledger-row goals.
//
// The whole current backlog is reconciled in the same pass: every
// confirmed-shipped (non-struck) item is returned in StruckIDs so the caller
// can strike them from the Phase 4 backlog section in the same run — the
// pick being among them rejects it.
//
// Completion notes are title-matched only (never id-matched): notes
// reference open items by id too (e.g. the run-21 note "Q27 ... deeper
// failure-class carryover stays on the backlog"), so an id hit there would
// strike an item that is still current.
//
// The ledger tier is the strict `PRD:<line>` reference only — an incidental
// title or id mention in a goal cannot strike — and any referencing goal
// with partial-completion language suppresses the tier for the item.
// Id collisions (two bullets parsing to the same Q-token) resolve toward
// the unstruck item: the struck twin is shipped state, not the target.
func CheckBacklogPick(pickID, prd string, mergedTitles, ledgerGoals []string) BacklogPickCheck {
	items := ParseBacklogItems(prd)
	var pick *BacklogItem
	if m := prdLineRefRe.FindStringSubmatch(strings.TrimSpace(pickID)); m != nil {
		line, convErr := strconv.Atoi(m[1])
		if convErr == nil {
			for i := range items {
				if items[i].LineNumber == line {
					pick = &items[i]
					break
				}
			}
		}
	} else {
		// Unstruck-preferred id resolution (struck twin = shipped state).
		for i := range items {
			if strings.EqualFold(items[i].ID, pickID) && !items[i].Struck {
				pick = &items[i]
				break
			}
		}
		if pick == nil {
			for i := range items {
				if strings.EqualFold(items[i].ID, pickID) {
					pick = &items[i]
					break
				}
			}
		}
	}

	if pick == nil {
		return BacklogPickCheck{
			OK:        false,
			Shipped:   false,
			Message:   fmt.Sprintf("backlog item %s not found in the Phase 4 current backlog", pickID),
			StruckIDs: []string{},
		}
	}
	if pick.Struck {
		return BacklogPickCheck{
			OK:        false,
			Shipped:   true,
			Message:   fmt.Sprintf("%s already shipped (struck in docs/PRD.md)", pickID),
			StruckIDs: []string{},
		}
	}

	notes := ExtractCompletionNotes(prd)
	evidenceFor := func(item BacklogItem) string {
		idRe := regexp.MustCompile(`\b` + strings.ToLower(item.ID) + `\b`)
		titleNorm := NormalizeTitle(item.Title)
		for _, raw := range mergedTitles {
			h := NormalizeTitle(raw)
			if idRe.MatchString(h) {
				return strings.TrimSpace(raw)
			}
			if titleNorm != "" && strings.Contains(h, titleNorm) {
				return strings.TrimSpace(raw)
			}
		}
		// Notes are title-matched only: an id hit can strike an item that a
		// note merely references while it stays open.
		for _, raw := range notes {
			h := NormalizeTitle(raw)
			if titleNorm != "" && strings.Contains(h, titleNorm) {
				return strings.TrimSpace(raw)
			}
		}
		// Ledger tier: productive-row goals as shipped evidence. Strict
		// `PRD:<line>` reference only; a goal carrying partial-completion
		// language withholds the tier (sliced — consumed in parts).
		refRe := regexp.MustCompile(`(?i)PRD:\s*` + strconv.Itoa(item.LineNumber) + `\b`)
		shippedGoal := ""
		for _, goal := range ledgerGoals {
			if !refRe.MatchString(goal) {
				continue
			}
			if PartialCompletionRE.MatchString(goal) {
				return ""
			}
			if shippedGoal == "" {
				shippedGoal = strings.TrimSpace(goal)
			}
		}
		return shippedGoal
	}

	// Reconcile the whole backlog; the picked item being confirmed-shipped
	// rejects it.
	struckIDs := []string{}
	for i := range items {
		item := items[i]
		if item.Struck {
			continue
		}
		evidence := evidenceFor(item)
		if evidence == "" {
			continue
		}
		if item.LineNumber == pick.LineNumber {
			return BacklogPickCheck{
				OK:        false,
				Shipped:   true,
				Message:   fmt.Sprintf("%s already shipped: %s", pickID, evidence),
				StruckIDs: append(struckIDs, item.ID),
			}
		}
		struckIDs = append(struckIDs, item.ID)
	}

	return BacklogPickCheck{
		OK:        true,
		Shipped:   false,
		Message:   fmt.Sprintf("%s is current backlog — dispatch ok", pickID),
		Prompt:    pick.Line,
		StruckIDs: struckIDs,
	}
}

// StrikeBacklogItems mirrors strikeBacklogItems: wrap confirmed-shipped
// backlog lines in ~~...~~, preserving leading/trailing whitespace exactly
// (the TS regex surgery). Only strikes items not already struck.
func StrikeBacklogItems(prd string, ids []string) string {
	idSet := make(map[string]bool, len(ids))
	for _, id := range ids {
		idSet[strings.ToLower(id)] = true
	}
	lines := strings.Split(prd, "\n")
	for i, line := range lines {
		trimmed := lstrip(line)
		if !bulletRe.MatchString(trimmed) {
			continue
		}
		if strings.HasPrefix(trimmed, "~~-") || strings.HasPrefix(trimmed, "~~*") {
			continue // already struck
		}
		body := lstrip(strings.TrimSuffix(strings.TrimPrefix(trimmed, "~~"), "~~"))
		id := extractItemID(body)
		if id == "" || !idSet[strings.ToLower(id)] {
			continue
		}
		lines[i] = strikeWrap(line)
	}
	return strings.Join(lines, "\n")
}

// strikeWrap mirrors line.replace(/^(\s*)/, '$1~~').replace(/\s*$/, (sp) =>
// '~~' + sp): insert ~~ after leading and before trailing whitespace.
func strikeWrap(line string) string {
	lead := 0
	for lead < len(line) {
		r, size := utf8.DecodeRuneInString(line[lead:])
		if !unicode.IsSpace(r) {
			break
		}
		lead += size
	}
	tail := len(line)
	for tail > lead {
		r, size := utf8.DecodeLastRuneInString(line[:tail])
		if !unicode.IsSpace(r) {
			break
		}
		tail -= size
	}
	return line[:lead] + "~~" + line[lead:tail] + "~~" + line[tail:]
}

// ListMergedPrTitles mirrors listMergedPrTitles: the last 30 subjects of
// origin/main commits (squash-merge PR titles), leading shas stripped.
// Returns an empty list on any failure (no origin, offline, not a repo).
func ListMergedPrTitles(repoPath string) []string {
	r := spawn.RunCli("git", []string{"log", "origin/main", "--oneline", "-30"}, spawn.Options{Dir: repoPath, TimeoutMs: 30000})
	if r.ExitCode != 0 {
		return []string{}
	}
	titles := []string{}
	for _, l := range strings.Split(r.Stdout, "\n") {
		l = strings.TrimSpace(shaPrefixRe.ReplaceAllString(l, ""))
		if l != "" {
			titles = append(titles, l)
		}
	}
	return titles
}
