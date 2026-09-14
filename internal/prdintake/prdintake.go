// Package prdintake turns operator-authored PRD intent into factory work.
//
// docs/PRD.md is a state document (docs/SELF-BUILD-LOOP.md "Tracker + PRD
// policy"): it records what the repo IS, and the loop treats an uncommitted
// edit to it as the operator pausing the factory. That left every real
// "build this next" the operator wrote with no route into the deterministic
// lane — the "long runtime, little value" half of issue #355: an empty lane
// makes the driver invent its own plumbing work.
//
// Intake is the one sanctioned exception, and it is deliberately narrow: an
// OPEN markdown checkbox in the PRD (`- [ ] <text>`) is an explicit request
// to build that item. Parsing is deterministic — no LLM, no network — and
// idempotent: an item's queue id is a hash of its normalized text, so
// re-running intake over an unchanged PRD enqueues nothing, and a shipped
// item the worker ticked to `- [x]` is state, not work.
package prdintake

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/FreePeak/devagent/internal/queue"
)

// DefaultMaxItems caps how many new rows one pass may enqueue. An operator
// who drops a 40-item wishlist into the PRD gets the first DefaultMaxItems
// built, in document order (top of the file = highest priority), instead of
// a queue flood that starves the tracker lane.
const DefaultMaxItems = 5

// goalWordCap mirrors the loop's dispatch contract (internal/loopdriver
// validateGoalShape): a goal over 120 words is refused at the dispatch
// boundary and its queue claim retired, so intake truncates the item text
// into shape instead of handing the loop an undeliverable row.
const goalWordCap = 120

// TickCriterion is carried by every intake row: the shipped PR ticks its own
// source checkbox, so an item cannot be built twice.
const TickCriterion = "Tick the source checkbox in docs/PRD.md from `- [ ]` to `- [x]` in the same PR, so the item never re-enters the intake lane."

// Item is one open checkbox in the PRD, with the context a worker needs to
// implement it without re-reading the whole document.
type Item struct {
	// ID is the deterministic queue id: "PRD-" + 8 hex of the normalized text.
	ID string `json:"id"`
	// Title is the item text with inline markdown stripped.
	Title string `json:"title"`
	// Goal is the queue goal text: "Goal: " prefixed and inside goalWordCap.
	Goal string `json:"goal"`
	// Criteria are the item's sub-bullets plus TickCriterion.
	Criteria []string `json:"acceptanceCriteria,omitempty"`
	// Section is the nearest enclosing heading (the "where did this come
	// from" line).
	Section string `json:"section,omitempty"`
	// Line is the 1-based line of the checkbox in the PRD.
	Line int `json:"line"`
	// PRDMarkdown is the enclosing section, stored as the queue task-PRD
	// sidecar so the dispatch reads the real spec, not just one bullet.
	PRDMarkdown string `json:"-"`
}

// Options configures one Ingest pass.
type Options struct {
	// RepoPath is the checkout whose docs/PRD.md is read.
	RepoPath string
	// MaxItems bounds new rows per pass; 0 = DefaultMaxItems, negative =
	// unbounded.
	MaxItems int
	// DryRun parses and reports without writing queue rows.
	DryRun bool
}

// Report is one Ingest pass outcome.
type Report struct {
	// Open counts every open checkbox found (including ones this pass did
	// not enqueue).
	Open int `json:"open"`
	// Queued lists the rows this pass created (or, with DryRun, would).
	Queued []Item `json:"queued"`
	// Known lists open items the queue store already holds — pending,
	// claimed, done or failed — so "nothing queued" never reads as "nothing
	// found".
	Known []Item `json:"known"`
	// QueueDepth is the row count in the queue store after this pass.
	QueueDepth int `json:"queueDepth"`
	// PRDPath is the file that was read.
	PRDPath string `json:"prdPath"`
}

// DefaultPRDPath is the state document every repo convention points at.
func DefaultPRDPath(repoPath string) string {
	return filepath.Join(repoPath, "docs", "PRD.md")
}

var (
	// A checkbox item: `  - [ ] text` / `* [x] text`.
	checkboxRe = regexp.MustCompile(`^([ \t]*)([-*])[ \t]+\[([ xX])\][ \t]*(.*)$`)
	// A bullet that is not a checkbox (sub-bullet criteria, continuation).
	bulletRe = regexp.MustCompile(`^([ \t]*)([-*])[ \t]+(.*)$`)
	// Any heading, for the section context.
	headingRe = regexp.MustCompile(`^#{1,6}[ \t]+(.*)$`)
	// A code-fence marker (``` or ~~~), optionally indented.
	fenceRe = regexp.MustCompile("^[ \\t]*(`{3,}|~{3,})")

	linkRe   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	inlineRe = regexp.MustCompile("[*_`~]")
)

// Parse extracts the open checkbox items from PRD markdown, in document
// order — the PRD's own top-to-bottom reading order is the priority order.
//
// Never work: `- [x]` (shipped state), struck items, checkboxes inside a
// code fence or a blockquote (a `>` line is a state note, not an
// instruction), and bullets without a checkbox (prose).
func Parse(prd string) []Item {
	lines := strings.Split(prd, "\n")
	var items []Item
	section := ""
	fence := ""

	// The cursor advances past a consumed item body, so a nested checkbox
	// can only ever be that item's criterion, never an item of its own.
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")

		if mark := fenceMark(line); mark != "" {
			switch {
			case fence == "":
				fence = mark
			case strings.HasPrefix(mark, fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}

		trimmed := strings.TrimSpace(line)
		if h := headingRe.FindStringSubmatch(trimmed); h != nil {
			section = strings.TrimSpace(h[1])
			continue
		}
		if strings.HasPrefix(trimmed, ">") {
			continue
		}
		m := checkboxRe.FindStringSubmatch(line)
		if m == nil || m[3] != " " {
			continue
		}

		text, sub, last := collectItemBody(lines, i, len(m[1]), m[4])
		title := cleanTitle(text)
		lineNo := i + 1
		i = last
		if title == "" {
			continue
		}
		items = append(items, Item{
			ID:          StableID(title),
			Title:       title,
			Goal:        BuildGoal(title, section, lineNo),
			Criteria:    criteria(sub),
			Section:     section,
			Line:        lineNo,
			PRDMarkdown: sectionBody(lines, lineNo-1),
		})
	}
	return items
}

// collectItemBody folds the lines that belong to the item at start into its
// text and criteria, returning the index of the last line it consumed. A line
// belongs while it is more indented than the checkbox marker: a nested
// checkbox or plain bullet is a criterion, other text is wrapped prose
// continuing the item. A blank line, a heading, a fence, or any line at or
// below the marker's indent ends the item.
func collectItemBody(lines []string, start, markerIndent int, first string) (string, []string, int) {
	text := strings.TrimSpace(first)
	var sub []string
	last := start
	for j := start + 1; j < len(lines); j++ {
		line := strings.TrimRight(lines[j], "\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		if fenceMark(line) != "" || headingRe.MatchString(strings.TrimSpace(line)) {
			break
		}
		indent := indentWidth(line)
		if indent <= markerIndent {
			break
		}
		if cb := checkboxRe.FindStringSubmatch(line); cb != nil {
			if t := cleanTitle(cb[4]); t != "" {
				sub = append(sub, t)
			}
			last = j
			continue
		}
		if b := bulletRe.FindStringSubmatch(line); b != nil {
			if t := cleanTitle(b[3]); t != "" {
				sub = append(sub, t)
			}
			last = j
			continue
		}
		text += " " + strings.TrimSpace(line)
		last = j
	}
	return text, sub, last
}

// criteria renders a row's acceptance criteria: the item's own sub-bullets,
// then the tick-the-box rule every intake row carries.
func criteria(sub []string) []string {
	out := make([]string, 0, len(sub)+1)
	out = append(out, sub...)
	return append(out, TickCriterion)
}

// sectionBody returns the enclosing section (its heading through the next
// heading, or EOF), capped so a 200-line section cannot ride into every
// dispatch. Returns "" when the item sits above the first heading.
func sectionBody(lines []string, itemLine int) string {
	start := -1
	for i := itemLine; i >= 0; i-- {
		if headingRe.MatchString(strings.TrimSpace(strings.TrimRight(lines[i], "\r"))) {
			start = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := itemLine + 1; i < len(lines); i++ {
		if headingRe.MatchString(strings.TrimSpace(strings.TrimRight(lines[i], "\r"))) {
			end = i
			break
		}
	}
	body := strings.TrimSpace(strings.Join(lines[start:end], "\n"))
	// Capped so a 200-line section cannot ride into every dispatch.
	const cap = 6000
	if len(body) > cap {
		body = strings.TrimSpace(body[:cap]) + "\n…"
	}
	return body
}

// Ingest reads the repo's docs/PRD.md and enqueues every open item the queue
// store does not already hold. Re-running over an unchanged PRD enqueues
// nothing: the queue row's id is the item's content hash, and an existing row
// in any status (pending, claimed, done, failed) counts as known.
func Ingest(opts Options) (Report, error) {
	repo := opts.RepoPath
	if repo == "" {
		repo = "."
	}
	prdPath := DefaultPRDPath(repo)
	raw, err := os.ReadFile(prdPath)
	if err != nil {
		return Report{}, err
	}
	report := Report{PRDPath: prdPath}
	items := Parse(string(raw))
	report.Open = len(items)

	max := opts.MaxItems
	if max == 0 {
		max = DefaultMaxItems
	}

	known := map[string]bool{}
	for _, t := range queue.ListTasks(repo, "") {
		known[t.ID] = true
	}

	for _, it := range items {
		if known[it.ID] {
			report.Known = append(report.Known, it)
			continue
		}
		if max >= 0 && len(report.Queued) >= max {
			continue
		}
		if !opts.DryRun {
			if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{
				ID:                 it.ID,
				Title:              it.Title,
				Goal:               it.Goal,
				AcceptanceCriteria: it.Criteria,
				PrdMarkdown:        it.PRDMarkdown,
				Source:             "prd",
			}); err != nil {
				if _, dup := err.(queue.ErrAlreadyQueued); dup {
					report.Known = append(report.Known, it)
					continue
				}
				return report, fmt.Errorf("enqueue %s: %w", it.ID, err)
			}
			known[it.ID] = true
		}
		report.Queued = append(report.Queued, it)
	}

	// Projected depth in a dry run: the point of the number is "what will the
	// lane hold after this pass", and a preview that reports the pre-pass
	// depth contradicts the "would queue N" line beside it.
	if opts.DryRun {
		report.QueueDepth = len(known) + len(report.Queued)
	} else {
		report.QueueDepth = len(queue.ListTasks(repo, ""))
	}
	return report, nil
}

// BuildGoal renders the dispatch statement: "Goal: " prefixed (the contract
// every producer meets) and inside the word cap, pointing the worker at the
// exact PRD line for the full spec. The item text is what shrinks when the
// statement runs long — the reference and the ask never do.
func BuildGoal(title, section string, line int) string {
	suffix := ""
	if section != "" {
		suffix = fmt.Sprintf(", section %q", section)
	}
	format := "Goal: Implement the operator's PRD item %q (docs/PRD.md:%d%s) in full and verifiably, and ship it as a PR."
	for words := 0; ; words++ {
		g := fmt.Sprintf(format, title, line, suffix)
		if len(strings.Fields(g)) <= goalWordCap || words > 40 || title == "" {
			return g
		}
		title = truncateWords(title, titleWords(title)-5)
		if title == "" {
			return fmt.Sprintf(format, "see the referenced PRD item", line, suffix)
		}
	}
}

// StableID is the queue id: "PRD-" + 8 hex of the normalized item text. An
// edit to the text changes the id, so a rewritten item is fresh work and an
// untouched one never double-enqueues.
func StableID(title string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(title), " "))))
	return "PRD-" + hex.EncodeToString(sum[:4])
}

func titleWords(s string) int { return len(strings.Fields(s)) }

// truncateWords keeps the first n whitespace-separated words.
func truncateWords(s string, n int) string {
	if n <= 0 {
		return ""
	}
	w := strings.Fields(s)
	if len(w) <= n {
		return s
	}
	return strings.Join(w[:n], " ") + "…"
}

// cleanTitle strips inline markdown so the queue row and the hash see the
// item's words, not its decoration.
func cleanTitle(s string) string {
	s = linkRe.ReplaceAllString(strings.TrimSpace(s), "$1")
	s = inlineRe.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimSpace(strings.Trim(s, ".,;:"))
}

func indentWidth(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}

// fenceMark returns a line's fence marker, or "" when it is not one.
func fenceMark(line string) string {
	m := fenceRe.FindString(strings.TrimRight(line, "\r"))
	if m == "" {
		return ""
	}
	return strings.TrimSpace(m)
}
