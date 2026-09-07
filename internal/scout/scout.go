// scout.go is the deterministic core of src/scout.ts: task-id resolution,
// payload extraction, the golden replay harness, heartbeat persistence, the
// single-instance lock, and prompt assembly. Live worker dispatch
// (runScoutOnce/runScoutLoop) stays in TS until the queue (FR-GO-04) and
// doc-sync (FR-GO-05) ports land. See extract.go for the canonical package
// comment.

package scout

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/research/scantext"
)

// embeddedFixtures carries the golden scout-output fixtures (the six files
// + golden.json copied from src/scout/__fixtures__) so `scout --replay`
// works from a compiled binary exactly like replayScoutFixtures() does from
// a source checkout.
//
//go:embed testdata
var embeddedFixtures embed.FS

// HeartbeatPath mirrors heartbeatPath(): the scout liveness record
// `scout-status` reads.
func HeartbeatPath(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "scout.heartbeat.json")
}

var base36Alphabet = []byte("0123456789abcdefghijklmnopqrstuvwxyz")

// RandomTaskId mirrors randomTaskId(): SCOUT-<UTC yyyymmdd>-<4 base36
// chars>. JS derives the suffix from Math.random().toString(36).slice(2,6);
// Go draws the same 4-char alphabet uniformly (the value is unobservable to
// the dedup contract — only its per-cycle uniqueness matters).
func RandomTaskId(now time.Time) string {
	d := now.UTC()
	b := make([]byte, 4)
	for i := range b {
		b[i] = base36Alphabet[rand.IntN(36)]
	}
	return fmt.Sprintf("SCOUT-%04d%02d%02d-%s", d.Year(), int(d.Month()), d.Day(), string(b))
}

// FallbackTaskId mirrors fallbackTaskId(): the deterministic per-UTC-day id
// used when scout LLM output is unparseable or the worker failed. One id per
// day makes enqueueTask's exists-dedup catch repeats — a random id per cycle
// accumulated 12 same-goal duplicates in one day (2026-09-01). A new day
// legitimately gets a fresh fallback slot.
func FallbackTaskId(now time.Time) string {
	d := now.UTC()
	return fmt.Sprintf("SCOUT-%04d%02d%02d-fallback", d.Year(), int(d.Month()), d.Day())
}

var (
	sanitizeIllegalRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)
	sanitizeDashesRe  = regexp.MustCompile(`-+`)
)

// SanitizeForFile mirrors sanitizeForFile(). The 80-char cap counts JS
// UTF-16 code units; ids reaching this path are ASCII filenames, so the
// rune-count cap is equivalent for every real input.
func SanitizeForFile(s string) string {
	s = sanitizeIllegalRe.ReplaceAllString(s, "-")
	s = sanitizeDashesRe.ReplaceAllString(s, "-")
	return truncateRunes(s, 80)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// readTextIfExists mirrors readTextIfExists(path, maxChars): ” when
// missing/unreadable, truncated with the same marker when over budget.
func readTextIfExists(path string, maxChars int) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(raw)
	if len(s) > maxChars {
		return s[:maxChars] + "\n…[truncated]"
	}
	return s
}

// queueDir mirrors queue.ts queueDir().
// TODO(FR-GO-04 #194): replace with the queue package once it lands.
func queueDir(repoPath string) string { return filepath.Join(repoPath, ".devagent", "queue") }

// prdsDir mirrors queue.ts prdsDir().
// TODO(FR-GO-04 #194): replace with the queue package once it lands.
func prdsDir(repoPath string) string { return filepath.Join(repoPath, ".devagent", "prds") }

// queueCount mirrors queueCount(): the number of task JSON files queued.
func queueCount(repoPath string) int {
	entries, err := os.ReadDir(queueDir(repoPath))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// ledgerTail mirrors ledgerTail(): the last three ledger rows, or the same
// sentinel strings the TS prompt renders.
func ledgerTail(repoPath string) string {
	p := filepath.Join(repoPath, ".selfbuild", "ledger.jsonl")
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "(no ledger yet)"
		}
		return "(ledger unreadable)"
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return "(ledger empty)"
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, "\n")
}

// ScoutPromptOptions carries the seams buildScoutPrompt exposes to callers
// and tests (TS: opts.kgProvider / opts.kgLog).
type ScoutPromptOptions struct {
	// KgProvider overrides the structural-memory layer. With no override,
	// the KG layer is omitted: the real LeanKG client port
	// (src/leankg.ts, FR-CTX-05) is not part of this issue, so a
	// `context.kg: "leankg"` config degrades to baseline-only until it
	// lands — the same degradation the TS client applies when the service
	// is unreachable.
	KgProvider func() string
}

// BuildScoutPrompt mirrors buildScoutPrompt(): the SCOUT instruction block
// (repo path, PRD sections, queue depth, lessons, ledger tail, the GRADIENT
// adjacent-category scan text from internal/research/scantext) with the
// knowledge-context digest spliced through the shared
// spliceCompactContext seam (FR-CTX-01/02).
func BuildScoutPrompt(repoPath string, cfg config.Config, opts *ScoutPromptOptions) string {
	if opts == nil {
		opts = &ScoutPromptOptions{}
	}
	lessons := readTextIfExists(filepath.Join(repoPath, ".selfbuild", "lessons.md"), 2000)
	tail := ledgerTail(repoPath)
	qCount := queueCount(repoPath)
	recentPrds := recentPrdNames(repoPath)

	parts := []string{
		fmt.Sprintf("You are the DevAgent SCOUT. Repo: %s.", repoPath),
		"Read docs/PRD.md section 4 (competitive landscape) and section 17 (roadmap), plus .selfbuild/ledger.jsonl tail and lessons below.",
		fmt.Sprintf("Queue depth: %d task(s). Recent PRDs: %s.", qCount, recentPrds),
	}
	if lessons != "" {
		parts = append(parts, "Lessons (ratchet, do not re-derive):\n"+lessons)
	}
	parts = append(parts, "Ledger tail:\n"+tail)
	parts = append(parts, scantext.BuildAdjacentCategoryScanText())
	// The TS array carries an explicit '' entry here; filter(Boolean) drops
	// it, so no blank line separates the scan text from the instruction.
	parts = append(parts,
		"Select exactly ONE backlog item for a single iteration-sized improvement that is implementable + testable in one devagent task pass.",
		"Output STRICTLY in this format (no extra prose):",
		"---TASK---",
		"id: <short id, e.g. FEAT-123 or IMPROVE-foo>",
		"title: <max 80 chars>",
		"goal: Goal: <one sentence goal starting with \"Goal:\">",
		"criteria: <semicolon-separated acceptance criteria, or single line>",
		"---PRD---",
		"# <title>",
		"## Goal",
		"<2-3 sentences>",
		"## Acceptance criteria",
		"- <bullet>",
		"## Notes",
		"<optional notes>",
	)
	prompt := strings.Join(parts, "\n")

	knowledgeOpts := KnowledgeOptions{KgProvider: opts.KgProvider}
	if cfg.LessonsMaxChars != nil {
		m := int(*cfg.LessonsMaxChars)
		knowledgeOpts.MaxChars = &m
	}
	if cfg.Context != nil {
		if cfg.Context.Kg != "" {
			knowledgeOpts.Kg = cfg.Context.Kg
		}
		if cfg.Context.AgentsMd != "" {
			knowledgeOpts.AgentsMd = cfg.Context.AgentsMd
		}
	}
	knowledge := BuildKnowledgeContext(repoPath, knowledgeOpts)
	return SpliceCompactContext(prompt, "", repoPath, &SpliceOptions{Knowledge: knowledge})
}

// recentPrdNames mirrors the recentPrds IIFE: the last two .md files in the
// PRDs dir (os.ReadDir sorts by name, matching Node's sorted readdir on
// APFS/ext4), "(none)" when the dir is absent or empty.
func recentPrdNames(repoPath string) string {
	entries, err := os.ReadDir(prdsDir(repoPath))
	if err != nil {
		return "(none)"
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "(none)"
	}
	if len(names) > 2 {
		names = names[len(names)-2:]
	}
	return strings.Join(names, ", ")
}

// ScoutTask is the parsed scout output record (TS: parseScoutOutput return).
type ScoutTask struct {
	ID          string
	Title       string
	Goal        string
	Criteria    []string
	PRDMarkdown string
}

var goalPrefixRe = regexp.MustCompile(`(?i)^Goal:`)

// ParseScoutOutput mirrors parseScoutOutput(): the strict
// ---TASK---/---PRD--- contract. Returns nil for every rejected shape.
func ParseScoutOutput(text string) *ScoutTask {
	taskIdx := strings.Index(text, "---TASK---")
	prdIdx := strings.Index(text, "---PRD---")
	if taskIdx < 0 || prdIdx < 0 || prdIdx <= taskIdx {
		return nil
	}
	taskBlock := strings.TrimSpace(text[taskIdx+len("---TASK---") : prdIdx])
	prdMarkdown := strings.TrimSpace(text[prdIdx+len("---PRD---"):])
	if prdMarkdown == "" {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(taskBlock, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	get := func(key string) (string, bool) {
		prefix := key + ":"
		for _, l := range lines {
			if strings.HasPrefix(strings.ToLower(l), prefix) {
				return strings.TrimSpace(l[len(prefix):]), true
			}
		}
		return "", false
	}
	id, hasID := get("id")
	title, hasTitle := get("title")
	goal, hasGoal := get("goal")
	criteriaRaw, _ := get("criteria")
	if !hasID || id == "" || !hasTitle || title == "" || !hasGoal || goal == "" {
		return nil
	}
	if !goalPrefixRe.MatchString(goal) {
		return nil
	}
	var criteria []string
	if criteriaRaw != "" {
		for _, s := range strings.Split(criteriaRaw, ";") {
			s = strings.TrimSpace(s)
			if s != "" {
				criteria = append(criteria, s)
			}
		}
	}
	return &ScoutTask{
		ID:          SanitizeForFile(id),
		Title:       truncateRunes(title, 80),
		Goal:        goal,
		Criteria:    criteria,
		PRDMarkdown: prdMarkdown,
	}
}

// FallbackTask mirrors fallbackTaskId+fallbackTask(): the deterministic
// record enqueued when the LLM output is unparseable or the worker CLI is
// unavailable.
func FallbackTask(prompt string, now time.Time) ScoutTask {
	id := FallbackTaskId(now)
	title := "Scout fallback: improve devagent observability"
	goal := "Goal: Add a scout heartbeat status command so operators can verify the 24/7 scout is alive without reading files."
	prd := "# " + title + "\n\n## Goal\nExpose `devagent scout-status --repo <path>` that prints heartbeat, queue depth, and last task.\n\n## Acceptance criteria\n- scout-status prints JSON or human table\n- heartbeat age is reported\n- missing heartbeat is reported cleanly\n\n## Notes\nFallback PRD generated when scout LLM output was unparseable.\n\nPrompt excerpt (first 400 chars): " + truncateRunes(prompt, 400)
	return ScoutTask{
		ID:          id,
		Title:       title,
		Goal:        goal,
		Criteria:    []string{"scout-status prints heartbeat + queue depth", "missing heartbeat handled"},
		PRDMarkdown: prd,
	}
}

// ExtractScoutPayload mirrors extractScoutPayload(): pull the scout's
// ---TASK---/---PRD--- payload out of a worker output stream. Shapes
// handled: claude single-line JSON array of message objects, per-line
// NDJSON {text|result|part}, and raw marker-bearing text. `worker` is
// carried for signature parity with TS (the implementation is shape-based
// and never reads it).
func ExtractScoutPayload(raw, worker string) *string {
	_ = worker
	const marker = "---TASK---"
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "[") {
		var arr []any
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			for _, e := range arr {
				obj, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if s, ok := obj["result"].(string); ok && strings.Contains(s, marker) {
					return &s
				}
				if s, ok := obj["text"].(string); ok && strings.Contains(s, marker) {
					return &s
				}
			}
		}
		// not a JSON array; fall through to line scan
	}
	for _, line := range strings.Split(raw, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(t), &obj); err != nil {
			continue // not JSON
		}
		for _, key := range []string{"text", "result", "part"} {
			if s, ok := obj[key].(string); ok && strings.Contains(s, marker) {
				v := s
				return &v
			}
		}
	}
	// Fallback: raw itself contains the markers
	if strings.Contains(raw, "---TASK---") && strings.Contains(raw, "---PRD---") {
		return &raw
	}
	return nil
}

// ReplayResult is one fixture's replay verdict (TS: ScoutReplayResult).
type ReplayResult struct {
	Name     string
	Pass     bool
	Expected *string
	Actual   *string
}

// ReplayScoutFixtures replays every captured scout output fixture through
// ExtractScoutPayload and compares against golden.json expectations — the
// `scout --replay` body. Pass fixturesDir "" to replay the fixtures
// embedded in this package (testdata/); pass a directory to replay an
// on-disk set (the Go test does both, and a stale expectation fails the
// replay exactly like a worker output-shape change would).
//
// Results come back in golden.json key order, like Object.entries in TS.
func ReplayScoutFixtures(fixturesDir string) ([]ReplayResult, error) {
	readFile := func(name string) ([]byte, error) {
		if fixturesDir == "" {
			return fs.ReadFile(embeddedFixtures, filepath.ToSlash(filepath.Join("testdata", name)))
		}
		return os.ReadFile(filepath.Join(fixturesDir, name))
	}
	goldenRaw, err := readFile("golden.json")
	if err != nil {
		return nil, fmt.Errorf("scout fixtures not found: %w", err)
	}
	entries, err := decodeGolden(goldenRaw)
	if err != nil {
		return nil, err
	}
	results := make([]ReplayResult, 0, len(entries))
	for _, e := range entries {
		content, err := readFile(e.Name)
		if err != nil {
			return nil, fmt.Errorf("scout fixture %s unreadable: %w", e.Name, err)
		}
		actual := ExtractScoutPayload(string(content), e.Worker)
		results = append(results, ReplayResult{
			Name:     e.Name,
			Pass:     sameStringPtr(actual, e.Expected),
			Expected: e.Expected,
			Actual:   actual,
		})
	}
	return results, nil
}

func sameStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type goldenEntry struct {
	Name     string
	Worker   string
	Expected *string
}

// decodeGolden preserves the JSON file's key order (Object.entries parity
// for the CLI's PASS/FAIL line order).
func decodeGolden(raw []byte) ([]goldenEntry, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("golden.json must be an object")
	}
	var out []goldenEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("golden.json key is not a string")
		}
		var entry struct {
			Worker   string  `json:"worker"`
			Expected *string `json:"expected"`
		}
		if err := dec.Decode(&entry); err != nil {
			return nil, err
		}
		out = append(out, goldenEntry{Name: name, Worker: entry.Worker, Expected: entry.Expected})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return out, nil
}

// FixtureNames lists the golden fixture files embedded in testdata
// (excluding golden.json itself), sorted — the replay suite's coverage
// guard.
func FixtureNames() ([]string, error) {
	entries, err := fs.ReadDir(embeddedFixtures, "testdata")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && e.Name() != "golden.json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// ScoutHeartbeat mirrors ScoutHeartbeat. Pointer fields reproduce
// JSON.stringify's drop-on-undefined so the on-disk row shape stays
// byte-identical to the Node writer's.
type ScoutHeartbeat struct {
	LastRunAt       string   `json:"lastRunAt"`
	LastTaskID      *string  `json:"lastTaskId,omitempty"`
	LastStatus      *string  `json:"lastStatus,omitempty"`
	LastDetail      *string  `json:"lastDetail,omitempty"`
	Worker          *string  `json:"worker,omitempty"`
	IntervalMinutes *float64 `json:"intervalMinutes,omitempty"`
}

// HeartbeatPatch is writeHeartbeat's argument: everything except the
// timestamp, which defaults to now.
type HeartbeatPatch struct {
	LastRunAt       *string
	LastTaskID      *string
	LastStatus      *string
	LastDetail      *string
	Worker          *string
	IntervalMinutes *float64
}

// ReadHeartbeat mirrors readHeartbeat(): a heartbeat whose timestamp cannot
// be parsed is as good as missing — scout-status reports an age from it, and
// a NaN age would mislead operators about whether the 24/7 scout is alive.
func ReadHeartbeat(repoPath string) *ScoutHeartbeat {
	raw, err := os.ReadFile(HeartbeatPath(repoPath))
	if err != nil {
		return nil
	}
	var hb ScoutHeartbeat
	if err := json.Unmarshal(raw, &hb); err != nil {
		return nil
	}
	if hb.LastRunAt == "" || parseJSDate(hb.LastRunAt).IsZero() {
		return nil
	}
	return &hb
}

// parseJSDate mirrors Date.parse for the timestamp shapes the heartbeat
// file carries (ISO 8601 with/without ms, date-only). Unparseable input
// yields the zero time, which callers treat as NaN.
func parseJSDate(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// WriteHeartbeat mirrors writeHeartbeat(): mkdir .devagent, write the
// 2-space-indented JSON row + newline, return the stored record.
func WriteHeartbeat(repoPath string, patch HeartbeatPatch) (ScoutHeartbeat, error) {
	lastRunAt := jsISOTimestamp(time.Now())
	if patch.LastRunAt != nil {
		lastRunAt = *patch.LastRunAt
	}
	hb := ScoutHeartbeat{
		LastRunAt:       lastRunAt,
		LastTaskID:      patch.LastTaskID,
		LastStatus:      patch.LastStatus,
		LastDetail:      patch.LastDetail,
		Worker:          patch.Worker,
		IntervalMinutes: patch.IntervalMinutes,
	}
	if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
		return hb, err
	}
	raw, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return hb, err
	}
	return hb, os.WriteFile(HeartbeatPath(repoPath), append(raw, '\n'), 0o644)
}

// jsISOTimestamp mirrors new Date().toISOString(): UTC, millisecond
// precision, trailing Z.
func jsISOTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func scoutLockPath(repoPath string) string {
	return filepath.Join(repoPath, ".devagent", "scout.lock")
}

// processAlive mirrors process.kill(pid, 0): true only when the signal is
// deliverable. TS treats EPERM as dead (its catch returns false), so the
// Go port returns false for every non-nil error — same verdict surface.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// AcquireScoutLock mirrors acquireScoutLock(): take the single-instance
// scout lock; false when a live holder exists. A corrupt or dead-holder
// lock file is stale and gets taken over.
func AcquireScoutLock(repoPath string, now time.Time) bool {
	if now.IsZero() {
		now = time.Now()
	}
	if err := os.MkdirAll(filepath.Join(repoPath, ".devagent"), 0o755); err != nil {
		return false
	}
	p := scoutLockPath(repoPath)
	if raw, err := os.ReadFile(p); err == nil {
		var current struct {
			Pid *int `json:"pid"`
		}
		if json.Unmarshal(raw, &current) == nil && current.Pid != nil && processAlive(*current.Pid) {
			// Liveness check passes for ANY live pid, including foreign
			// namespaces; combined with the mtime staleness window this is
			// best-effort by design.
			return false
		}
	}
	// {"pid":<n>,"at":<ms>} + newline, key order pinned by the TS writer.
	line := fmt.Sprintf(`{"pid":%d,"at":%d}`, os.Getpid(), now.UnixMilli())
	if err := os.WriteFile(p, []byte(line+"\n"), 0o644); err != nil {
		return false
	}
	return true
}

// ReleaseScoutLock mirrors releaseScoutLock(): remove the lock only when
// this process holds it.
func ReleaseScoutLock(repoPath string) {
	raw, err := os.ReadFile(scoutLockPath(repoPath))
	if err != nil {
		return // already gone
	}
	var current struct {
		Pid *int `json:"pid"`
	}
	if json.Unmarshal(raw, &current) != nil || current.Pid == nil || *current.Pid != os.Getpid() {
		return
	}
	_ = os.Remove(scoutLockPath(repoPath))
}

// ReadScoutLockPid mirrors readScoutLockPid(): the holder pid, or -1 when
// the lock is missing/corrupt (TS null).
func ReadScoutLockPid(repoPath string) int {
	raw, err := os.ReadFile(scoutLockPath(repoPath))
	if err != nil {
		return -1
	}
	var current struct {
		Pid *float64 `json:"pid"`
	}
	if json.Unmarshal(raw, &current) != nil || current.Pid == nil {
		return -1
	}
	return int(*current.Pid)
}
