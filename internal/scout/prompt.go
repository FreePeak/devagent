// prompt.go is the Go port of src/prompt.ts for FR-GO-06: the
// COMPACT_CONTEXT_MARKER splice seam, the lessons digest (bounded + impact
// ranked, Q39), and the layered knowledge-context digest (FR-CTX-01..03).
// The trust-gated AGENTS.md layer reuses the already-ported internal/trust
// package (same file, same gate semantics). See extract.go for the
// canonical package comment.

package scout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/FreePeak/devagent/internal/trust"
)

// DefaultLessonsFile is the repo-local lessons file (config `lessonsFile`
// overrides). The selfbuild loop's machine appends land in
// .selfbuild/lessons.md (the scout prompt reads that path directly).
const DefaultLessonsFile = ".devagent/lessons.md"

// CompactContextMarker: sentinel spliced into the planner prompt where the
// prior child-worker trail block should land. The planner builder embeds
// this exact string at a fixed offset so the prefix above it stays
// cacheable across iterations.
const CompactContextMarker = "## Prior Worker Trails"

// ChildTrailsMaxChars: hard character budget for the per-task child-trail
// digest, matching LessonsMaxChars so both injections stay comparably
// bounded.
const ChildTrailsMaxChars = 4000

// TrailsRoot: on-disk root for the per-loop trail ledger.
const TrailsRoot = ".selfbuild/trails"

// KnowledgeContextDir: repo-relative directory of always-on
// knowledge-context markdown files (FR-CTX-02).
const KnowledgeContextDir = ".devagent/context"

// KnowledgeContextHeader: section header the knowledge digest renders under
// when spliced at the marker.
const KnowledgeContextHeader = "## Knowledge Context"

// KgContextSubheader: sub-header marking the KG layer inside the digest
// (FR-CTX-01 layering).
const KgContextSubheader = "### Structural memory (leankg)"

// AgentsMdSubheader: sub-header marking the AGENTS.md layer inside the
// knowledge digest (Q11).
const AgentsMdSubheader = "### Repo instructions (.devagent/AGENTS.md)"

// LessonsMaxLines/LessonsMaxChars mirror src/lessons/guard.ts: the digest
// window is the newest 40 lines, hard character budget 4000 (PRD Q9).
const (
	LessonsMaxLines = 40
	LessonsMaxChars = 4000
)

// EventsFile: repo-relative ledger of orchestration events (lessons-eval,
// loop-result) backing the Q39 impact scores.
const EventsFile = ".devagent/runs/orchestration/events.jsonl"

func trailFile(cwd, loopID, taskID string) string {
	return filepath.Join(cwd, TrailsRoot, loopID, taskID+".jsonl")
}

// LoadLessons mirrors loadLessons(): load the curated durable lessons for
// prompt injection; "" when the file is absent so prompts stay unchanged by
// default. Ratchet-only content is assumed: callers keep the file
// append-only. maxChars 0 means the default budget (4000).
func LoadLessons(repoPath, lessonsFile string, maxChars int) string {
	return LoadLessonsDigest(repoPath, lessonsFile, maxChars)
}

// LessonScore is the per-excerptHash impact record (TS: LessonScore).
type LessonScore struct {
	Score                  float64
	AcceptRate             float64
	Delta                  float64
	EvalCount              int
	LessonLoopFailureRate  float64
	OverallLoopFailureRate float64
}

// LoadLessonsDigest mirrors loadLessonsDigest(): read the newest
// LessonsMaxLines lines, rank them by measured impact (Q39), then drop the
// lowest-ranked entries whole until the surviving block fits the budget.
// Ranking uses the per-lesson score from the orchestration ledger
// (loadLessonScores): lines whose excerpt hash has a score sort by score
// (descending, oldest-first tiebreak) ahead of unscored lines, which keep
// the existing newest-first order. Never splits a line and never strips
// content from a kept line.
func LoadLessonsDigest(repoPath, lessonsFile string, maxChars int) string {
	p := lessonsFile
	if p == "" {
		p = DefaultLessonsFile
	}
	p = filepath.Join(repoPath, p)
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	allLines := strings.Split(trimEnd(string(raw)), "\n")
	base := 0
	window := allLines
	if len(allLines) > LessonsMaxLines {
		base = len(allLines) - LessonsMaxLines
		window = allLines[base:]
	}
	scores := LoadLessonScores(repoPath)

	type indexed struct {
		line  string
		score float64
		idx   int
	}
	indexedWindow := make([]indexed, 0, len(window))
	for i, line := range window {
		score, ok := scores[LessonExcerptHash(line)]
		if !ok {
			score = math.Inf(-1)
		}
		indexedWindow = append(indexedWindow, indexed{line: line, score: score, idx: base + i})
	}
	// Score descending; scored ties break oldest-first, unscored keep the
	// existing newest-first order (so the oldest unscored lines are dropped
	// first when the budget bites — same recency semantics as before).
	sort.SliceStable(indexedWindow, func(a, b int) bool {
		x, y := indexedWindow[a], indexedWindow[b]
		if x.score != y.score {
			return x.score > y.score
		}
		if math.IsInf(x.score, -1) {
			return y.idx < x.idx
		}
		return x.idx < y.idx
	})
	if maxChars == 0 {
		maxChars = LessonsMaxChars
	}
	total := -1 // joining N lines adds N-1 newlines
	end := 0
	for _, entry := range indexedWindow {
		cost := utf16Len(entry.line) + 1
		if end > 0 && total+cost > maxChars {
			break
		}
		total += cost
		end++
	}
	kept := append([]indexed(nil), indexedWindow[:end]...)
	sort.SliceStable(kept, func(a, b int) bool { return kept[a].idx < kept[b].idx })
	lines := make([]string, 0, len(kept))
	for _, e := range kept {
		lines = append(lines, e.line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// trimEnd mirrors the JS trimEnd(): trailing whitespace (plus U+FEFF,
// which JS counts as whitespace) only.
func trimEnd(s string) string {
	for len(s) > 0 {
		r, size := utf8.DecodeLastRuneInString(s)
		if r == 0xFEFF || !unicode.IsSpace(r) {
			break
		}
		s = s[:len(s)-size]
	}
	return s
}

// utf16Len measures a string the way JS .length does: UTF-16 code units.
// Lesson lines are ASCII in every fixture and production path, so the rune
// count equals the UTF-16 length there; astral characters count double, as
// in JS.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// lessonsSection mirrors lessonsSection(): the rendered lessons block, or
// "" when there is nothing to inject.
func lessonsSection(lessons string) string {
	if strings.TrimSpace(lessons) == "" {
		return ""
	}
	return "\n\n## Lessons from previous runs\nApply these durable lessons; they exist because past attempts failed without them:\n" + strings.TrimSpace(lessons)
}

// SpliceOptions mirrors spliceCompactContext's opts bag.
type SpliceOptions struct {
	TaskID       string
	PriorTaskIDs []string
	Knowledge    string
}

// SpliceCompactContext mirrors spliceCompactContext(): splice
// prior-worker-trail and knowledge-context content into the marker slot of
// an assembled prompt. The marker sits at a fixed offset on every call path
// so the prefix above it stays cacheable across iterations. When the marker
// is absent the section is appended at the tail — same offset convention.
// With neither section present the prompt is returned byte-identical
// (noop).
//
// loopID "" means no loop context (the scout and repair call sites): the
// trail section is skipped. repoPath is the cwd the trail files resolve
// against.
func SpliceCompactContext(prompt, loopID, repoPath string, opts *SpliceOptions) string {
	if opts == nil {
		opts = &SpliceOptions{}
	}
	trailSection := ""
	if loopID != "" {
		trailSection = compactContext(loopID, repoPath, opts)
	}
	knowledgeSection := strings.TrimSpace(opts.Knowledge)
	sections := make([]string, 0, 2)
	for _, s := range []string{trailSection, knowledgeSection} {
		if s != "" {
			sections = append(sections, s)
		}
	}
	section := strings.Join(sections, "\n\n")
	if !strings.Contains(prompt, CompactContextMarker) {
		if section == "" {
			return prompt
		}
		return prompt + "\n\n" + trimEnd(section)
	}
	if section == "" {
		return prompt
	}
	// JS String.replace with a string pattern replaces the FIRST occurrence.
	return strings.Replace(prompt, CompactContextMarker, trimEnd(section), 1)
}

// compactContext mirrors the unexported compactContext(): read the
// per-(loopId, taskId) trail files back as one markdown block. "" when
// there is no trail yet.
func compactContext(loopID, cwd string, opts *SpliceOptions) string {
	ids := append([]string{}, opts.PriorTaskIDs...)
	if opts.TaskID != "" {
		ids = append(ids, opts.TaskID)
	}
	if len(ids) == 0 {
		return ""
	}
	var blocks []string
	for _, id := range ids {
		file := trailFile(cwd, loopID, id)
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var lines []string
		for _, l := range strings.Split(string(raw), "\n") {
			if len(l) > 0 {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			continue
		}
		blocks = append(blocks, "### Trail for "+id+"\n"+strings.Join(lines, "\n"))
	}
	if len(blocks) == 0 {
		return ""
	}
	return CompactContextMarker + "\n" + strings.Join(blocks, "\n\n") + "\n"
}

// RatchetResult mirrors ratchetToBudget's return shape.
type RatchetResult struct {
	Kept    []string
	Dropped int
}

// RatchetToBudget mirrors the shared ratchet (FR-CTX-01): drop the oldest
// entries whole until the joined block fits maxChars; lines are never
// split. Worst case is a single newest line that exceeds the cap on its own
// — it is surfaced whole and the rest are reported as dropped.
func RatchetToBudget(lines []string, maxChars int) RatchetResult {
	start := 0
	for start < len(lines)-1 {
		if utf16Len(strings.Join(lines[start:], "\n")) <= maxChars {
			break
		}
		start++
	}
	return RatchetResult{Kept: lines[start:], Dropped: start}
}

// BuildChildTrailsDigest mirrors buildChildTrailsDigest(): a
// character-bounded digest of the prior child-worker trail files listed in
// trailPaths. Oldest entries drop whole, lines are never split, and the
// surviving block fits maxChars (0 = the default ChildTrailsMaxChars).
// Missing or unreadable files are silently skipped. Returns the rendered
// digest plus dropped/total counts.
func BuildChildTrailsDigest(trailPaths []string, maxChars int) (string, int, int) {
	if maxChars == 0 {
		maxChars = ChildTrailsMaxChars
	}
	var lines []string
	for _, p := range trailPaths {
		if p == "" {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := trimEnd(line)
			if trimmed != "" {
				lines = append(lines, trimmed)
			}
		}
	}
	total := len(lines)
	if total == 0 {
		return "", 0, 0
	}
	r := RatchetToBudget(lines, maxChars)
	return strings.Join(r.Kept, "\n"), r.Dropped, total
}

// KnowledgeOptions mirrors buildKnowledgeContext's opts bag.
type KnowledgeOptions struct {
	MaxChars   *int
	Kg         string // "leankg" | "off"
	KgProvider func() string
	AgentsMd   trust.Mode
}

// BuildKnowledgeContext mirrors buildKnowledgeContext(): the always-on
// markdown baseline from .devagent/context/*.md, the trust-gated
// .devagent/AGENTS.md layer, plus the opt-in KG layer when kg is "leankg"
// and the provider yields content. The combined entry stream is
// ratchet-capped at the same character budget as lessonsMaxChars (default
// 4000): oldest entries drop whole, never split. The KG layer is
// orchestrator-side only (FR-CTX-04) and never blocks: an absent, throwing,
// or empty provider degrades the digest to baseline-only.
func BuildKnowledgeContext(repoPath string, opts KnowledgeOptions) string {
	budget := LessonsMaxChars
	if opts.MaxChars != nil {
		budget = *opts.MaxChars
	}
	var lines []string
	for _, p := range listKnowledgeFiles(repoPath) {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := trimEnd(line)
			if trimmed != "" {
				lines = append(lines, trimmed)
			}
		}
	}
	agentsMd := opts.AgentsMd
	if agentsMd == "" {
		agentsMd = trust.ModeAsk
	}
	agents := trust.LoadAgentsMd(repoPath, agentsMd)
	if agents != "" {
		lines = append(lines, AgentsMdSubheader)
		for _, l := range strings.Split(agents, "\n") {
			l = trimEnd(l)
			if l != "" {
				lines = append(lines, l)
			}
		}
	}
	if opts.Kg == "leankg" && opts.KgProvider != nil {
		// Unreachable provider: baseline-only digest, pipeline continues
		// (FR-CTX-03). A panicking provider is the JS throw case.
		kg := strings.TrimSpace(catchString(opts.KgProvider))
		if kg != "" {
			lines = append(lines, KgContextSubheader)
			for _, l := range strings.Split(kg, "\n") {
				l = trimEnd(l)
				if l != "" {
					lines = append(lines, l)
				}
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	kept := RatchetToBudget(lines, budget).Kept
	return KnowledgeContextHeader + "\n" + strings.Join(kept, "\n")
}

func catchString(f func() string) (s string) {
	defer func() {
		if recover() != nil {
			s = ""
		}
	}()
	return f()
}

// listKnowledgeFiles mirrors listKnowledgeFiles(): the baseline
// knowledge-context files under .devagent/context/, oldest first (mtime,
// name tiebreak) so the shared ratchet drops the oldest entries whole. A
// missing or unreadable directory yields nil — the digest degrades to
// noop.
func listKnowledgeFiles(repoPath string) []string {
	dir := filepath.Join(repoPath, KnowledgeContextDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	type entry struct {
		path    string
		mtimeMs int64
		name    string
	}
	var out []entry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		p := filepath.Join(dir, name)
		var mtimeMs int64
		if info, err := e.Info(); err == nil {
			mtimeMs = info.ModTime().UnixMilli()
		}
		out = append(out, entry{path: p, mtimeMs: mtimeMs, name: name})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].mtimeMs != out[b].mtimeMs {
			return out[a].mtimeMs < out[b].mtimeMs
		}
		// JS localeCompare on file names: byte comparison for the ASCII
		// names these files carry, deterministic here.
		return out[a].name < out[b].name
	})
	paths := make([]string, 0, len(out))
	for _, e := range out {
		paths = append(paths, e.path)
	}
	return paths
}

// PlannerSystemPrompt mirrors PLANNER_SYSTEM_PROMPT: the system prompt
// handed to the planner LLM, kept as a constant so the section header
// (CompactContextMarker) splices in at a fixed offset on every call.
const PlannerSystemPrompt = `You are a software planner. Decompose the given goal into 2-6 small, precise, independently testable implementation tasks for a coding agent.
Rules:
- Each task must be implementable in one focused session in an isolated worktree.
- "acceptanceCriteria" must be a list of machine-checkable completion signals (files that exist, tests that pass, exports present) — an independent auditor will verify each item separately against the environment.
- Optionally add "constraints" for things the executor must NOT do (e.g. touch unrelated modules, change public API).
- Order tasks so dependencies come first; use dependsOn with earlier task ids.
- Respond with ONLY a JSON array (no prose, no markdown fences):
[{"id":"T1","title":"...","prompt":"precise implementation instructions including which files/functions to touch","acceptanceCriteria":["src/x.ts exists and exports y","npm test passes"],"constraints":["do not modify src/other.ts"],"dependsOn":[]}]`

// PlannerPromptOptions mirrors buildPlannerPrompt's opts bag minus the
// seams owned by sibling packages (kgLog, real LeanKG provider resolution).
type PlannerPromptOptions struct {
	LoopID            string
	TaskID            string
	PriorTaskIDs      []string
	Kg                string
	AgentsMd          trust.Mode
	KnowledgeMaxChars *int
	// KgProvider overrides the structural-memory layer; nil + kg=leankg
	// degrades to baseline-only (the real client port is FR-CTX-05's).
	KgProvider func() string
}

// BuildPlannerPrompt mirrors buildPlannerPrompt(): the planner prompt with
// prior worker trails compacted into a fixed offset — the marker sits as
// the trailing section so the prefix above it stays cacheable. When
// loopID and taskID are supplied, ingestChildTrails drains the loop's
// child-worker worklogs into the per-(loopId, taskId) trail ledger first.
func BuildPlannerPrompt(goal, repoPath string, opts PlannerPromptOptions) string {
	if opts.LoopID != "" && opts.TaskID != "" {
		ingestChildTrails(opts.LoopID, opts.TaskID, nil, repoPath)
	}
	knowledgeOpts := KnowledgeOptions{
		Kg:         opts.Kg,
		AgentsMd:   opts.AgentsMd,
		KgProvider: opts.KgProvider,
	}
	if opts.KnowledgeMaxChars != nil {
		knowledgeOpts.MaxChars = opts.KnowledgeMaxChars
	}
	knowledge := BuildKnowledgeContext(repoPath, knowledgeOpts)
	return SpliceCompactContext(
		PlannerSystemPrompt+"\n\n## Goal\n"+goal+"\n\n"+CompactContextMarker,
		opts.LoopID,
		repoPath,
		&SpliceOptions{TaskID: opts.TaskID, PriorTaskIDs: opts.PriorTaskIDs, Knowledge: knowledge},
	)
}

// IngestChildTrails mirrors ingestChildTrails(): walk a loop's
// child-worker output directory and append every worklog.jsonl line into
// the per-(loopId, taskId) trail file. Returns the number of lines
// ingested (0 on no-op).
func IngestChildTrails(loopID, taskID string, sourceWorklogs []string, cwd string) int {
	return ingestChildTrails(loopID, taskID, sourceWorklogs, cwd)
}

func ingestChildTrails(loopID, taskID string, sourceWorklogs []string, cwd string) int {
	if loopID == "" || taskID == "" {
		return 0
	}
	sources := sourceWorklogs
	if len(sources) == 0 {
		sources = discoverChildWorklogs(cwd, loopID)
	}
	if len(sources) == 0 {
		return 0
	}
	dest := trailFile(cwd, loopID, taskID)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0
	}
	count := 0
	for _, src := range sources {
		raw, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		var lines []string
		for _, l := range strings.Split(string(raw), "\n") {
			l = trimEnd(l)
			if l != "" {
				lines = append(lines, l)
			}
		}
		if len(lines) == 0 {
			continue
		}
		f, err := os.OpenFile(dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_, werr := f.WriteString(strings.Join(lines, "\n") + "\n")
		_ = f.Close()
		if werr != nil {
			continue
		}
		count += len(lines)
	}
	return count
}

// discoverChildWorklogs mirrors discoverChildWorklogs(): best-effort
// discovery of child worker worklog.jsonl files for a loop. Honors
// whatever layout the selfbuild loop already produces — no new convention
// is invented here.
func discoverChildWorklogs(cwd, loopID string) []string {
	root := filepath.Join(cwd, ".selfbuild", "loops", loopID, "workers")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(root, e.Name(), "worklog.jsonl")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			out = append(out, candidate)
		}
	}
	return out
}

// --- lessons scoring (src/lessons/guard.ts subset needed by the digest) ---
// TODO(FR-GO-04 #197): the events ledger and its scoring belong to the
// ledger port; ReadEvents/ComputeLessonScores here are the minimal local
// surface the digest ranking needs and get replaced when that package
// lands.

// ReadEvents mirrors readEvents(): every parseable row from the
// orchestration events.jsonl ledger; corrupt lines are silently skipped
// (best-effort). Rows keep insertion order. (JS also keeps non-object JSON
// rows like arrays; they cannot match `event === "lessons-eval"` downstream,
// so dropping them here is behavior-identical.)
func ReadEvents(repoPath string) []map[string]any {
	raw, err := os.ReadFile(filepath.Join(repoPath, EventsFile))
	if err != nil {
		return nil
	}
	var out []map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue // skip corrupt line
		}
		out = append(out, m)
	}
	return out
}

// ComputeLessonScores mirrors computeLessonScores(): join `lessons-eval`
// rows with `loop-result` rows on the numeric `loop` field. A lessons-eval
// row without a `loop` field is matched to the nearest subsequent
// loop-result row by timestamp fallback. Pure function: no I/O.
//
// Impact formula:
//
//	score = acceptRate - (lessonLoopFailureRate - overallLoopFailureRate)
//
// When no loop-result data exists for a lesson, delta = 0 and score =
// acceptRate.
func ComputeLessonScores(events []map[string]any) map[string]LessonScore {
	var lessonsEvalRows, loopResultRows []map[string]any
	for _, r := range events {
		if r["event"] == "lessons-eval" {
			lessonsEvalRows = append(lessonsEvalRows, r)
		}
		if r["event"] == "loop-result" {
			loopResultRows = append(loopResultRows, r)
		}
	}
	if len(lessonsEvalRows) == 0 {
		return map[string]LessonScore{}
	}

	// Build loop-result lookup: numeric loop → status.
	loopResultMap := map[int]string{}
	for _, row := range loopResultRows {
		loop, ok := asNumber(row["loop"])
		if !ok {
			continue
		}
		status := "failed"
		if s, ok := row["status"]; ok && s != nil {
			status = tsString(s)
		}
		loopResultMap[int(loop)] = status
	}

	// For rows without a loop field, try timestamp-based matching: find the
	// nearest loop-result with ts >= this lessons-eval ts.
	type tsRow struct {
		ts   string
		loop int
	}
	var loopResultByTs []tsRow
	for _, row := range loopResultRows {
		ts, ok := row["ts"].(string)
		if !ok || ts == "" {
			continue
		}
		if loop, ok := asNumber(row["loop"]); ok {
			loopResultByTs = append(loopResultByTs, tsRow{ts: ts, loop: int(loop)})
		}
	}
	sort.SliceStable(loopResultByTs, func(a, b int) bool { return loopResultByTs[a].ts < loopResultByTs[b].ts })
	findLoopResultByTs := func(ts string) (int, bool) {
		for _, row := range loopResultByTs {
			if row.ts >= ts {
				return row.loop, true
			}
		}
		return 0, false
	}

	type evalAgg struct {
		evalCount     int
		acceptedCount int
		loopIDs       map[int]struct{}
	}
	lessonEvals := map[string]*evalAgg{}
	allLoopsWithEval := map[int]struct{}{}

	for _, row := range lessonsEvalRows {
		hash := tsString(row["excerptHash"])
		if hash == "" {
			continue
		}
		var loop int
		hasLoop := false
		if rawLoop := row["loop"]; rawLoop != nil {
			if v, ok := asNumber(rawLoop); ok {
				loop = int(v)
				hasLoop = true
			}
		}
		if !hasLoop {
			if ts, ok := row["ts"].(string); ok && ts != "" {
				if l, found := findLoopResultByTs(ts); found {
					loop = l
					hasLoop = true
				}
			}
		}
		entry := lessonEvals[hash]
		if entry == nil {
			entry = &evalAgg{loopIDs: map[int]struct{}{}}
			lessonEvals[hash] = entry
		}
		entry.evalCount++
		if b, ok := row["accepted"].(bool); ok && b {
			entry.acceptedCount++
		}
		if hasLoop {
			entry.loopIDs[loop] = struct{}{}
			allLoopsWithEval[loop] = struct{}{}
		}
	}

	// Compute overall failure rate.
	overallFailedCount, overallLoopCount := 0, 0
	for loop := range allLoopsWithEval {
		if status, ok := loopResultMap[loop]; ok {
			overallLoopCount++
			if status != "ok" {
				overallFailedCount++
			}
		}
	}
	overallLoopFailureRate := 0.0
	if overallLoopCount > 0 {
		overallLoopFailureRate = float64(overallFailedCount) / float64(overallLoopCount)
	}

	// Per-lesson scoring.
	scores := map[string]LessonScore{}
	for hash, entry := range lessonEvals {
		acceptRate := 0.0
		if entry.evalCount > 0 {
			acceptRate = float64(entry.acceptedCount) / float64(entry.evalCount)
		}
		lessonFailedCount, lessonLoopCount := 0, 0
		for loop := range entry.loopIDs {
			if status, ok := loopResultMap[loop]; ok {
				lessonLoopCount++
				if status != "ok" {
					lessonFailedCount++
				}
			}
		}
		lessonLoopFailureRate := 0.0
		if lessonLoopCount > 0 {
			lessonLoopFailureRate = float64(lessonFailedCount) / float64(lessonLoopCount)
		}
		delta := lessonLoopFailureRate - overallLoopFailureRate
		scores[hash] = LessonScore{
			Score:                  acceptRate - delta,
			AcceptRate:             acceptRate,
			Delta:                  delta,
			EvalCount:              entry.evalCount,
			LessonLoopFailureRate:  lessonLoopFailureRate,
			OverallLoopFailureRate: overallLoopFailureRate,
		}
	}
	return scores
}

// LoadLessonScores mirrors loadLessonScores(): Map<excerptHash, score> for
// the digest ranking. Empty map when no events exist (digest falls back to
// file-order).
func LoadLessonScores(repoPath string) map[string]float64 {
	scores := ComputeLessonScores(ReadEvents(repoPath))
	out := map[string]float64{}
	for hash, s := range scores {
		out[hash] = s.Score
	}
	return out
}

var (
	predictedImpactSuffixRe = regexp.MustCompile(`(?i)\s*\[predictedImpact:[^\]]*\]`)
	nonAlnumRe              = regexp.MustCompile(`[^a-z0-9]+`)
	collapseSpacesRe        = regexp.MustCompile(`\s+`)
)

// NormalizeLessonText mirrors normalizeLessonText(): strip the optional
// `[predictedImpact: ...]` metadata suffix, lowercase, replace every run of
// non-alphanumeric characters with a single space, then collapse whitespace.
// Punctuation-only rewordings normalize to the same token stream; word
// changes still register.
func NormalizeLessonText(text string) string {
	s := predictedImpactSuffixRe.ReplaceAllString(text, " ")
	s = strings.ToLower(s)
	s = nonAlnumRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(collapseSpacesRe.ReplaceAllString(s, " "))
}

// LessonExcerptHash mirrors lessonExcerptHash(): stable content hash of a
// lesson excerpt — sha256 over the normalized text, truncated to 16 hex
// chars. Keys the `lessons-eval` ledger row so replay can match a row to
// its entry without embedding the full line.
func LessonExcerptHash(entry string) string {
	sum := sha256.Sum256([]byte(NormalizeLessonText(entry)))
	return hex.EncodeToString(sum[:])[:16]
}

// asNumber mirrors Number(row.loop) for the JSON row values that can reach
// it: only real JSON numbers are finite; strings "1" coerce in JS but the
// ledger writer always emits numbers, so Go rejects them (documented
// divergence, unreachable with well-formed rows).
func asNumber(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

// tsString mirrors String(v) for the row values reaching it (excerptHash,
// status). nil is JS undefined/null → "undefined"; the callers treat
// "undefined" like an absent string.
func tsString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return strconv.FormatFloat(s, 'g', -1, 64)
	case bool:
		if s {
			return "true"
		}
		return "false"
	case nil:
		return "undefined"
	default:
		b, _ := json.Marshal(s)
		return string(b)
	}
}
