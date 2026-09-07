// Package lessons is the Go port of src/lessons/guard.ts (FR-GO-07, issue
// #194): the lessons eval guard (PRD Phase 4 backlog, docs/PRD.md §17) — a
// content-level dedupe + propose→evaluate→accept gate for machine-appended
// lessons, plus the orchestration events.jsonl ledger (loop-result /
// lessons-eval rows) and the Q39 impact scoring that joins them.
//
// This file mirrors src/lessons/guard.ts (FR-GO-07, issue #194).
package lessons

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Repo-relative default lessons file (config `lessonsFile` overrides).
const LessonsPath = ".devagent/lessons.md"

// Repo-relative lessons file the self-build loop's machine appends land in.
const SelfbuildLessonsPath = ".selfbuild/lessons.md"

// Lessons are context, not the task: cap the digest to the newest 40 lines.
const LessonsMaxLines = 40

// Hard character budget for injected lessons (PRD Q9: distilled, not verbatim).
const LessonsMaxChars = 4000

// DefaultLessonsDedupeSimilarity is the default reject threshold for the
// lessons dedupe guard: a candidate lesson whose nearest existing entry has
// trigram-Jaccard similarity at or above this is rejected as a duplicate.
// 0.8 keeps formatting churn out while still admitting genuinely new
// lessons; the near-dup band (roughly 0.4-0.7) stays admitted on purpose —
// a v2 rewrite of a shipped lesson is allowed to land so its effect can be
// re-measured.
const DefaultLessonsDedupeSimilarity = 0.8

// DefaultLessonsSuiteTimeoutMs is the wall-clock budget for one evaluate-step
// suite run (default 10 minutes).
const DefaultLessonsSuiteTimeoutMs = 600_000

// LessonsEvalReason: why a gated append landed the way it did.
type LessonsEvalReason string

const (
	ReasonMissingPredictedImpact LessonsEvalReason = "missing-predictedImpact"
	ReasonDuplicate              LessonsEvalReason = "duplicate"
	ReasonSuiteRed               LessonsEvalReason = "suite-red"
	ReasonHeldOut                LessonsEvalReason = "held-out"
	ReasonAccepted               LessonsEvalReason = "accepted"
)

// LessonsSuiteOutcome: outcome of the evaluate step for one gated append.
type LessonsSuiteOutcome string

const (
	SuiteGreen   LessonsSuiteOutcome = "green"
	SuiteRed     LessonsSuiteOutcome = "red"
	SuiteSkipped LessonsSuiteOutcome = "skipped"
)

// LessonsMustBeatOutcome: result of the must-beat-best-so-far check at
// append time.
type LessonsMustBeatOutcome string

const (
	MustBeatNone  LessonsMustBeatOutcome = "none"
	MustBeatBeat  LessonsMustBeatOutcome = "beat"
	MustBeatBelow LessonsMustBeatOutcome = "below"
)

// LoopResultStatus: why a loop terminated (self-build loop outcomes; matches
// ledger statuses).
type LoopResultStatus string

const (
	LoopStatusOK               LoopResultStatus = "ok"
	LoopStatusFailed           LoopResultStatus = "failed"
	LoopStatusFailedTests      LoopResultStatus = "failed-tests"
	LoopStatusInvalid          LoopResultStatus = "invalid"
	LoopStatusSkipped          LoopResultStatus = "skipped"
	LoopStatusProviderDegraded LoopResultStatus = "provider-degraded"
	LoopStatusPushFailed       LoopResultStatus = "push-failed"
)

var loopResultStatuses = map[LoopResultStatus]bool{
	LoopStatusOK:               true,
	LoopStatusFailed:           true,
	LoopStatusFailedTests:      true,
	LoopStatusInvalid:          true,
	LoopStatusSkipped:          true,
	LoopStatusProviderDegraded: true,
	LoopStatusPushFailed:       true,
}

// LoopResultLedgerRecord is one `loop-result` ledger row per loop iteration:
// the deterministic outcome (status) the lessons-eval accept/reject rows from
// the same loop join against (Q39 impact telemetry). The self-build driver
// writes one row per iteration via RecordLoopResult; the join key is the
// numeric Loop.
type LoopResultLedgerRecord struct {
	TS     string `json:"ts"`
	Kind   string `json:"kind"` // event
	Event  string `json:"event"`
	Loop   int    `json:"loop"`
	Status string `json:"status"`
	Goal   string `json:"goal"`
}

// EventsFile is the repo-relative ledger of orchestration events
// (lessons-eval, loop-result).
const EventsFile = ".devagent/runs/orchestration/events.jsonl"

// marshalLine encodes v as one JSON object followed by '\n', matching the TS
// appendFileSync(file, `${JSON.stringify(record)}\n`) shape and the
// internal/ledger marshaler conventions: HTML escaping is disabled because
// JSON.stringify does not escape <, > or &.
func marshalLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// appendEventFile appends one JSON line to the orchestration events ledger,
// creating the directory as needed. Every error is swallowed: best-effort
// observability only.
func appendEventFile(repoPath string, v any) {
	file := filepath.Join(repoPath, EventsFile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return
	}
	line, err := marshalLine(v)
	if err != nil {
		return
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(line)
}

// truncateUTF16 mirrors TS String.prototype.slice(0, n): n counts UTF-16 code
// units, not bytes or runes (same convention as internal/ledger).
func truncateUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// suffixUTF16 mirrors TS String.prototype.slice(-n): the LAST n UTF-16 code
// units of s.
func suffixUTF16(s string, n int) string {
	if n <= 0 {
		return ""
	}
	total := 0
	cut := len(s)
	for i := len(s); i > 0; {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if total+w > n {
			break
		}
		total += w
		cut = i - size
		i -= size
	}
	return s[cut:]
}

// nowISO mirrors `new Date().toISOString()`.
func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// RecordLoopResult is the best-effort, never-returns-error write of one
// loop-result ledger row (the deterministic loop outcome for impact
// scoring). Status must be a known self-build loop status (ok | failed |
// failed-tests | invalid | skipped | provider-degraded | push-failed);
// anything else is normalized to failed.
func RecordLoopResult(repoPath string, loop int, status string, goal string) {
	record := LoopResultLedgerRecord{
		TS:     nowISO(),
		Kind:   "event",
		Event:  "loop-result",
		Loop:   loop,
		Status: status,
		Goal:   truncateUTF16(collapseSpaces(strings.TrimSpace(goal)), 160),
	}
	if !loopResultStatuses[LoopResultStatus(status)] {
		record.Status = string(LoopStatusFailed)
	}
	appendEventFile(repoPath, record)
}

// ReadEvents reads all structured events from the orchestration events.jsonl
// ledger. Returns every parseable row; corrupt lines are silently skipped
// (best-effort). Cost: O(N) in rows, linear in the file size. Call once per
// scoring pass.
func ReadEvents(repoPath string) []map[string]any {
	file := filepath.Join(repoPath, EventsFile)
	raw, err := os.ReadFile(file)
	if err != nil {
		return []map[string]any{}
	}
	out := []map[string]any{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue // skip corrupt line
		}
		out = append(out, parsed)
	}
	return out
}

// LessonScore is the per-excerptHash impact score. Higher = more effective
// lesson. Score = acceptRate - repeatFailureDelta. AcceptRate =
// acceptedCount / evalCount (0 when no evals). RepeatFailureDelta =
// lessonLoopFailureRate - overallLoopFailureRate; a negative delta means the
// lesson correlates with fewer failures (good).
type LessonScore struct {
	Score                  float64 `json:"score"`
	AcceptRate             float64 `json:"acceptRate"`
	Delta                  float64 `json:"delta"`
	EvalCount              int     `json:"evalCount"`
	LessonLoopFailureRate  float64 `json:"lessonLoopFailureRate"`
	OverallLoopFailureRate float64 `json:"overallLoopFailureRate"`
}

// jsNumber mirrors TS Number(value) for the JSON-decoded row values the
// scorer touches: numbers pass through, numeric strings convert, anything
// else is NaN (reported as ok=false).
func jsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		t := strings.TrimSpace(n)
		if t == "" {
			return 0, true // Number("") === 0
		}
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	default:
		return 0, false // undefined / null / object → NaN
	}
}

// stringOf mirrors TS String(value ?? fallback) for row fields.
func stringOf(v any, fallback string) string {
	if v == nil {
		return fallback
	}
	if s, ok := v.(string); ok {
		return s
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	if b, ok := v.(bool); ok {
		return strconv.FormatBool(b)
	}
	return fallback
}

// truthy mirrors JS truthiness for the JSON-decoded values the guard reads.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return true
	}
}

// ComputeLessonScores computes lesson impact scores from the orchestration
// events.jsonl ledger. Joins `lessons-eval` rows with `loop-result` rows on
// the numeric Loop field. A lessons-eval row without a Loop field is matched
// to the nearest subsequent loop-result row by timestamp fallback (existing
// rows from before the `--loop` flag). Pure function: no I/O, operates on
// the parsed events array.
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
		if r == nil {
			continue
		}
		switch stringOf(r["event"], "") {
		case "lessons-eval":
			lessonsEvalRows = append(lessonsEvalRows, r)
		case "loop-result":
			loopResultRows = append(loopResultRows, r)
		}
	}

	scores := map[string]LessonScore{}
	if len(lessonsEvalRows) == 0 {
		return scores
	}

	// Build loop-result lookup: numeric loop → status.
	loopResultMap := map[float64]string{}
	for _, row := range loopResultRows {
		loop, ok := jsNumber(row["loop"])
		if !ok {
			continue
		}
		loopResultMap[loop] = stringOf(row["status"], "failed")
	}

	// For rows without a loop field, try timestamp-based matching: find the
	// nearest loop-result with ts >= this lessons-eval ts.
	loopResultByTs := make([]map[string]any, 0, len(loopResultRows))
	for _, row := range loopResultRows {
		if ts, ok := row["ts"].(string); ok && ts != "" {
			loopResultByTs = append(loopResultByTs, row)
		}
	}
	sort.Slice(loopResultByTs, func(i, j int) bool {
		return stringOf(loopResultByTs[i]["ts"], "") < stringOf(loopResultByTs[j]["ts"], "")
	})
	findLoopResultByTs := func(ts string) (float64, bool) {
		for _, row := range loopResultByTs {
			if stringOf(row["ts"], "") >= ts {
				return jsNumber(row["loop"])
			}
		}
		return 0, false
	}

	// Per-excerptHash aggregation.
	type lessonEval struct {
		evalCount     int
		acceptedCount int
		loopIDs       []float64
		loopSeen      map[float64]bool
	}
	lessonEvals := map[string]*lessonEval{}
	allLoopsWithEval := map[float64]bool{}

	for _, row := range lessonsEvalRows {
		hash := stringOf(row["excerptHash"], "")
		if hash == "" {
			continue
		}
		loop, loopOK := jsNumber(row["loop"])
		if !loopOK {
			if ts, isStr := row["ts"].(string); isStr {
				loop, loopOK = findLoopResultByTs(ts)
			}
		}
		entry := lessonEvals[hash]
		if entry == nil {
			entry = &lessonEval{loopSeen: map[float64]bool{}}
			lessonEvals[hash] = entry
		}
		entry.evalCount++
		if truthy(row["accepted"]) {
			entry.acceptedCount++
		}
		if loopOK {
			if !entry.loopSeen[loop] {
				entry.loopSeen[loop] = true
				entry.loopIDs = append(entry.loopIDs, loop)
			}
			allLoopsWithEval[loop] = true
		}
	}

	// Compute overall failure rate.
	overallFailedCount, overallLoopCount := 0, 0
	for loop := range allLoopsWithEval {
		if status, ok := loopResultMap[loop]; ok {
			overallLoopCount++
			if status != string(LoopStatusOK) {
				overallFailedCount++
			}
		}
	}
	overallLoopFailureRate := 0.0
	if overallLoopCount > 0 {
		overallLoopFailureRate = float64(overallFailedCount) / float64(overallLoopCount)
	}

	// Per-lesson scoring.
	for hash, entry := range lessonEvals {
		acceptRate := 0.0
		if entry.evalCount > 0 {
			acceptRate = float64(entry.acceptedCount) / float64(entry.evalCount)
		}
		lessonFailedCount, lessonLoopCount := 0, 0
		for _, loop := range entry.loopIDs {
			if status, ok := loopResultMap[loop]; ok {
				lessonLoopCount++
				if status != string(LoopStatusOK) {
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

// LoadLessonScores loads lesson scores from the repo's events.jsonl ledger
// for digest ranking. Returns a map<excerptHash, score>. Reads the events
// file and computes scores via ComputeLessonScores. When no events exist,
// returns an empty map (digest falls back to file-order).
func LoadLessonScores(repoPath string) map[string]float64 {
	events := ReadEvents(repoPath)
	computed := ComputeLessonScores(events)
	out := map[string]float64{}
	for hash, s := range computed {
		out[hash] = s.Score
	}
	return out
}

// HeldOutFraction is the fraction of the newest machine-appended lessons
// held out of digest scoring.
const HeldOutFraction = 0.2

// HeldOutMin is the minimum number of held-out lessons (always holds out at
// least one).
const HeldOutMin = 1

// HeldOutMax is the maximum number of held-out lessons (the digest window is
// 40 lines).
const HeldOutMax = 3

// PredictedImpactSuffix is the suffix that marks a machine-appended lesson
// line (see AppendPredictedImpact).
const PredictedImpactSuffix = "predictedImpact:"

// HeldOutLessonSlice is the held-out slice of a lessons file: content lines
// plus the excerpt hashes that digest scoring must exclude.
type HeldOutLessonSlice struct {
	Lines  []string
	Hashes map[string]bool
}

var (
	heldOutBulletRe = regexp.MustCompile(`^[-*]\s*$`)
	heldOutHeaderRe = regexp.MustCompile(`^#{1,6}\s`)
	heldOutFenceRe  = regexp.MustCompile(`^---`)
)

// HeldOutLessonHashes returns the held-out slice of a lessons file: the
// newest 20% (min 1, max 3) of machine-appended lesson lines, by append
// order. Append order is file order (the ratchet is append-only). Only
// machine-appended lines (those carrying the `predictedImpact:` suffix) are
// eligible: lines that never went through the propose→evaluate→accept gate
// have no measured effect to hold out, and the leading front-matter / date
// headings / prose are not lessons. An empty lessonsFile selects
// SelfbuildLessonsPath.
func HeldOutLessonHashes(repoPath string, lessonsFile string) HeldOutLessonSlice {
	if lessonsFile == "" {
		lessonsFile = SelfbuildLessonsPath
	}
	file := filepath.Join(repoPath, lessonsFile)
	raw, err := os.ReadFile(file)
	if err != nil {
		return HeldOutLessonSlice{Lines: []string{}, Hashes: map[string]bool{}}
	}
	machine := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || heldOutBulletRe.MatchString(trimmed) || heldOutHeaderRe.MatchString(trimmed) || heldOutFenceRe.MatchString(trimmed) {
			continue
		}
		if strings.Contains(line, PredictedImpactSuffix) {
			machine = append(machine, line)
		}
	}
	keep := int(float64(len(machine)) * HeldOutFraction)
	if keep < HeldOutMin {
		keep = HeldOutMin
	}
	if keep > HeldOutMax {
		keep = HeldOutMax
	}
	if keep > len(machine) {
		keep = len(machine)
	}
	slice := machine[len(machine)-keep:]
	hashes := map[string]bool{}
	for _, l := range slice {
		hashes[LessonExcerptHash(l)] = true
	}
	return HeldOutLessonSlice{Lines: slice, Hashes: hashes}
}

var (
	// Direction 1 (word before the number, ≤ 80 non-digit chars between):
	// "cuts failures by 25%", "reduces re-picks of shipped items by 50%",
	// "drops 30 percent".
	gradePercentRe = regexp.MustCompile(`(?:fewer|less|lower|reduc\w*|reduction|drop|down|cut\w*)\D{0,80}?((?:\d+(?:\.\d+)?%|\d+(?:\.\d+)?\s*(?:percent|percentage|per\s+cent)))`)
	// Leading numeric prefix of a JS parseFloat candidate.
	parseFloatPrefixRe = regexp.MustCompile(`^\s*[+-]?(\d+(\.\d*)?|\.\d+)`)
)

// parseJSFloat mirrors parseFloat on an already-anchored numeric candidate:
// parse the longest leading numeric prefix, ignore any trailing prose.
func parseJSFloat(s string) (float64, bool) {
	m := parseFloatPrefixRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(m[0], 64)
	return f, err == nil
}

// PredictedImpactGrade is the numeric grade for a `predictedImpact` text,
// used by the must-beat gate. A higher grade means the lesson predicts a
// bigger measured improvement. Mirrors the Q39 digest formula's acceptRate −
// repeatFailureDelta shape. When the text names no number, the grade is 0 so
// unquantified predictions lose to quantified ones that name real reductions.
func PredictedImpactGrade(impact string) float64 {
	text := strings.ToLower(impact)
	reductions := 0.0
	for _, m := range gradePercentRe.FindAllStringSubmatch(text, -1) {
		if n, ok := parseJSFloat(m[1]); ok {
			reductions += n
		}
	}
	if reductions > 0 {
		return min(reductions/100, 1)
	}
	return 0
}

// LoadBestMeasuredScore loads the current digest best measured score,
// excluding the held-out slice: the best score among non-held-out lessons
// that have ledger evidence. Used by the append-time must-beat gate — a
// candidate must beat the best score on loops the held-out slice did not
// inform, so the newest 20% cannot be used to chase a self-set bar. Returns
// nil when no eligible lesson has a measured score (nothing to beat yet).
func LoadBestMeasuredScore(repoPath string, lessonsFile string) *float64 {
	heldOut := HeldOutLessonHashes(repoPath, lessonsFile)
	var best *float64
	for hash, score := range LoadLessonScores(repoPath) {
		if heldOut.Hashes[hash] {
			continue // held-out lessons never set the bar
		}
		if best == nil || score > *best {
			v := score
			best = &v
		}
	}
	return best
}

// CheckMustBeat is the must-beat-best-so-far check at append time (held-out
// tier): the candidate's `predictedImpact` grade must strictly beat the
// digest's current best measured score on loops the held-out slice did not
// inform. Returns MustBeatNone when no applicable baseline exists — no
// measured scores among non-held-out lessons (cold start), or the best is
// saturated (>= 1, the unavoidable score of any accepted lesson on an ok
// loop; PredictedImpactGrade caps at 1, so a saturated bar would reject
// every future append and lock the ratchet). MustBeatBeat when the grade
// strictly exceeds the best; MustBeatBelow otherwise (rejected). Callers run
// this only after the evaluate step is green so a proposal that regresses
// the suite never reaches the gate.
func CheckMustBeat(repoPath string, predictedImpact string, lessonsFile string) LessonsMustBeatOutcome {
	grade := PredictedImpactGrade(predictedImpact)
	best := LoadBestMeasuredScore(repoPath, lessonsFile)
	// No baseline, or a saturated one the grade scale cannot beat: accept.
	if best == nil || *best >= 1 {
		return MustBeatNone
	}
	if grade > *best {
		return MustBeatBeat
	}
	return MustBeatBelow
}

var (
	normalizeImpactRe = regexp.MustCompile(`(?i)\s*\[predictedImpact:[^\]]*\]`)
	normalizeNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

// NormalizeLessonText normalizes lesson text for comparison: strip the
// optional `[predictedImpact: ...]` metadata suffix (it is captured
// separately in the lessons-eval ledger row and must neither dilute nor
// bypass dedupe), lowercase, replace every run of non-alphanumeric
// characters with a single space, then collapse whitespace.
// Punctuation-only rewordings ("Lessons eval guard" vs "Lessons-eval-guard")
// normalize to the same token stream; word changes still register.
func NormalizeLessonText(text string) string {
	t := normalizeImpactRe.ReplaceAllString(text, " ")
	t = strings.ToLower(t)
	t = normalizeNonAlnum.ReplaceAllString(t, " ")
	return strings.TrimSpace(t)
}

// LessonShingles is the word-level trigram (3-shingle) set of normalized
// lesson text. Word shingles — not character n-grams — stay robust to
// markdown emphasis, URL churn, and line-wrap differences while remaining
// sensitive to real content changes.
func LessonShingles(text string) map[string]bool {
	words := []string{}
	for _, w := range strings.Split(NormalizeLessonText(text), " ") {
		if len(w) > 0 {
			words = append(words, w)
		}
	}
	out := map[string]bool{}
	for i := 0; i+3 <= len(words); i++ {
		out[words[i]+" "+words[i+1]+" "+words[i+2]] = true
	}
	return out
}

// LessonSimilarity is the trigram-Jaccard similarity between two lesson
// texts in [0, 1]: intersection of the normalized word-trigram sets over
// their union. Two texts sharing no trigram score 0; identical texts score
// 1; two texts that are both empty (or too short to carry a trigram) score 1
// so an empty candidate can never bypass the guard by containing nothing to
// compare.
func LessonSimilarity(a string, b string) float64 {
	sa := LessonShingles(a)
	sb := LessonShingles(b)
	if len(sa) == 0 && len(sb) == 0 {
		return 1
	}
	inter := 0
	for s := range sa {
		if sb[s] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

// LessonsDedupeResult is the result of a lessons dedupe check (and, with
// the gated-append fields set, of AppendLessonGuarded).
type LessonsDedupeResult struct {
	// True when no existing entry meets the similarity threshold.
	OK bool `json:"ok"`
	// Trigram-Jaccard similarity of the nearest existing entry (0 when none).
	Similarity float64 `json:"similarity"`
	// The nearest existing entry, '' when the file has no entries at all.
	MatchedEntry string `json:"matchedEntry"`
	// The threshold that was applied.
	Threshold float64 `json:"threshold"`
	// Why the gated append landed this way (set by AppendLessonGuarded).
	Reason LessonsEvalReason `json:"reason,omitempty"`
	// Evaluate-step outcome for the gated append (set by AppendLessonGuarded).
	Suite LessonsSuiteOutcome `json:"suite,omitempty"`
	// Held-out slice size the must-beat check judged against (when it ran).
	HeldOut *int `json:"heldOut,omitempty"`
	// Must-beat-best-so-far outcome (when the check ran).
	MustBeat *LessonsMustBeatOutcome `json:"mustBeat,omitempty"`
	// Best measured non-held-out score the must-beat check compared against.
	MustBeatScore *float64 `json:"mustBeatScore,omitempty"`
	// True when the append ran in dry-run mode (validated, nothing written).
	DryRun bool `json:"dryRun,omitempty"`
	// Bounded tail of the failing suite output when the suite ran and failed.
	SuiteDetail *string `json:"suiteDetail,omitempty"`
}

var lessonHeaderRe = regexp.MustCompile(`^#{1,6}\s`)

// ReadLessonEntries reads the candidate-comparison surface of a lessons
// file: its non-blank content lines. Blank lines and structural lines are
// not lesson content, so the guard would waste the digest budget comparing a
// candidate against a dated `## <date>` header or a `---` front-matter
// fence; only actual content lines (the granularity the ratchet appends and
// the digest slices) count. The argument is a full file path.
func ReadLessonEntries(lessonsPath string) []string {
	raw, err := os.ReadFile(lessonsPath)
	if err != nil {
		return []string{}
	}
	out := []string{}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if trimmed == "---" {
			continue
		}
		if lessonHeaderRe.MatchString(trimmed) {
			continue
		}
		out = append(out, strings.TrimRight(line, " \t\r\n"))
	}
	return out
}

// LessonExcerptHash is the stable content hash of a lesson excerpt: sha256
// over the normalized text, truncated to 16 hex chars (same shape as the
// executor trail signature). It keys the `lessons-eval` ledger row so replay
// can match a row to its entry without embedding the full line.
func LessonExcerptHash(entry string) string {
	sum := sha256.Sum256([]byte(NormalizeLessonText(entry)))
	return hex.EncodeToString(sum[:])[:16]
}

// LessonsSuiteResult is the result of one evaluate-step suite run.
type LessonsSuiteResult struct {
	// True when the suite exited 0.
	OK bool
	// Bounded output tail (or failure detail) for the ledger row.
	Detail string
}

// SuiteRunner is the seam RunLessonsSuite shells out through: run the test
// command in dir with a wall-clock cap and return its output plus exit code.
// The default implementation runs os/exec; tests inject fakes so they stay
// hermetic. The default implementation joins stdout and stderr into the
// output string (the TS detail is `${stdout}\n${stderr}`, trimmed by the
// caller). An unstartable command returns output "" and a negative exit
// code.
type SuiteRunner func(cmd string, args []string, dir string, timeoutMs int) (output string, exitCode int)

// defaultSuiteRunner is the real os/exec implementation of SuiteRunner.
func defaultSuiteRunner(cmd string, args []string, dir string, timeoutMs int) (string, int) {
	if timeoutMs <= 0 {
		timeoutMs = DefaultLessonsSuiteTimeoutMs
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	c.Dir = dir
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	runErr := c.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String() + "\n" + stderr.String(), -1
	}
	if runErr != nil {
		if _, ok := runErr.(*exec.ExitError); !ok {
			// Command could not start (ENOENT etc.).
			return stdout.String() + "\n" + stderr.String(), -1
		}
	}
	if c.ProcessState == nil {
		// Command could not start (ENOENT etc.).
		return stdout.String() + "\n" + stderr.String(), -1
	}
	return stdout.String() + "\n" + stderr.String(), c.ProcessState.ExitCode()
}

// RunLessonsSuiteOpts carries the optional arguments of RunLessonsSuite.
type RunLessonsSuiteOpts struct {
	// Wall-clock budget for the suite run; 0 = DefaultLessonsSuiteTimeoutMs.
	TimeoutMs int
	// Runner seam; nil = default os/exec-based runner (`npm test`).
	Runner SuiteRunner
}

// RunLessonsSuite is the evaluate step of the propose→evaluate→accept gate:
// run the repo regression suite (`npm test` — vitest in this repo) against
// the proposed lessons-file state. Never panics and never returns an error:
// a suite that cannot even start is a red result, not a crash path — a bad
// lesson must never land because the runner broke.
func RunLessonsSuite(repoPath string, opts *RunLessonsSuiteOpts) LessonsSuiteResult {
	timeoutMs := DefaultLessonsSuiteTimeoutMs
	runner := defaultSuiteRunner
	if opts != nil {
		if opts.TimeoutMs > 0 {
			timeoutMs = opts.TimeoutMs
		}
		if opts.Runner != nil {
			runner = opts.Runner
		}
	}
	output, exitCode := runner("npm", []string{"test"}, repoPath, timeoutMs)
	detail := strings.TrimSpace(output)
	if exitCode == 0 {
		return LessonsSuiteResult{OK: true, Detail: suffixUTF16(detail, 300)}
	}
	if strings.TrimSpace(detail) == "" {
		detail = fmt.Sprintf("npm test exited %d", exitCode)
	}
	return LessonsSuiteResult{OK: false, Detail: suffixUTF16(detail, 300)}
}

// CheckLessonsDedupeOpts carries the optional arguments of CheckLessonsDedupe.
type CheckLessonsDedupeOpts struct {
	// Lessons file override (repo-relative); "" = SelfbuildLessonsPath.
	LessonsFile string
	// Reject threshold; nil = DefaultLessonsDedupeSimilarity.
	Threshold *float64
}

// CheckLessonsDedupe is the pure content-similarity gate run before a
// candidate lesson is appended: compare the candidate (normalized word
// trigrams) against every existing content line of the lessons file and
// reject when the nearest match is at or above Threshold. Returns the
// decision plus what it was based on so callers can surface / record the
// rejection.
func CheckLessonsDedupe(repoPath string, entry string, opts *CheckLessonsDedupeOpts) LessonsDedupeResult {
	threshold := DefaultLessonsDedupeSimilarity
	lessonsFile := SelfbuildLessonsPath
	if opts != nil {
		if opts.Threshold != nil {
			threshold = *opts.Threshold
		}
		if opts.LessonsFile != "" {
			lessonsFile = opts.LessonsFile
		}
	}
	entries := ReadLessonEntries(filepath.Join(repoPath, lessonsFile))
	best := 0.0
	bestEntry := ""
	for _, line := range entries {
		s := LessonSimilarity(entry, line)
		if bestEntry == "" || s > best {
			best = s
			bestEntry = line
		}
	}
	return LessonsDedupeResult{OK: best < threshold, Similarity: best, MatchedEntry: bestEntry, Threshold: threshold}
}

// AppendLessonGuardedOpts carries the optional arguments of
// AppendLessonGuarded.
type AppendLessonGuardedOpts struct {
	// Lessons file override (repo-relative); "" = SelfbuildLessonsPath.
	LessonsFile string
	// Dedupe threshold override; nil = DefaultLessonsDedupeSimilarity.
	Threshold *float64
	// Required predictedImpact for machine appends.
	PredictedImpact string
	// Wall-clock budget for the evaluate-step suite run.
	SuiteTimeoutMs int
	// Loop join key recorded on the lessons-eval ledger row (Q39); nil omits it.
	Loop *int
	// Run the held-out must-beat-best-so-far check after a green suite
	// (default true); nil = true.
	MustBeat *bool
	// Validate without writing: dedupe + held-out checks run, no file/ledger
	// write, no suite spawn.
	DryRun bool
	// SuiteRunner seam (tests); nil = default os/exec-based runner.
	SuiteRunner SuiteRunner
}

// lessonsEvalRecord is one accept/reject ledger row per gated append
// (best-effort, never fails): carries the lesson excerpt hash, similarity
// score, predictedImpact, suite result, and the held-out-tier fields
// (`heldOut` slice size + `mustBeat` outcome / best score) so replay can
// answer "why did this lesson land or not". Field order = TS object literal
// order = JSON key order for Go-written rows.
type lessonsEvalRecord struct {
	TS              string   `json:"ts"`
	Kind            string   `json:"kind"`
	Event           string   `json:"event"`
	ExcerptHash     string   `json:"excerptHash"`
	Similarity      float64  `json:"similarity"`
	Threshold       float64  `json:"threshold"`
	PredictedImpact string   `json:"predictedImpact"`
	Suite           string   `json:"suite"`
	Accepted        bool     `json:"accepted"`
	Reason          string   `json:"reason"`
	Entry           string   `json:"entry"`
	Loop            *int     `json:"loop,omitempty"`
	HeldOut         *int     `json:"heldOut,omitempty"`
	MustBeat        *string  `json:"mustBeat,omitempty"`
	MustBeatScore   *float64 `json:"mustBeatScore,omitempty"`
	SuiteDetail     *string  `json:"suiteDetail,omitempty"`
}

func recordLessonsEval(repoPath string, rec lessonsEvalRecord) {
	appendEventFile(repoPath, rec)
}

// round3Exact mirrors Math.round(x*1000)/1000 (Math.round rounds half away
// from zero for positive values; negative values round half up toward +inf,
// matching Math.round).
func round3Exact(x float64) float64 {
	return float64(int64(x*1000+0.5)) / 1000
}

// AppendLessonGuarded is the eval-gated append (PRD §17 "Lessons eval
// guard", evaluate→accept slice).
//
// A candidate lesson is accepted only when ALL of the following hold:
//  1. it carries a non-empty predictedImpact;
//  2. the dedupe gate passes (trigram-Jaccard below the threshold);
//  3. the evaluate step is green: the repo regression suite passes against
//     the PROPOSED lessons-file state. The entry is staged by writing it to
//     the file first, the suite runs, and on failure the file is restored
//     byte-for-byte to its pre-append state — a lesson that regresses
//     anything (including the suite itself) never lands;
//  4. when MustBeat is set (default), the must-beat-best-so-far check
//     passes (held-out tier): the candidate's predictedImpact must beat the
//     digest's current best measured score on loops the held-out slice did
//     not inform. The check runs strictly AFTER a green suite so a proposal
//     that regresses the repo is rejected before the comparison matters,
//     and a failed comparison reverts the staged file exactly like a red
//     suite.
//
// Exactly one `lessons-eval` ledger row is written per gated append, accept
// or reject, carrying the lesson excerpt hash, similarity score,
// predictedImpact, suite result, and (when the must-beat check ran) the
// held-out slice size and the must-beat outcome + best score it was judged
// against.
func AppendLessonGuarded(repoPath string, entry string, opts *AppendLessonGuardedOpts) LessonsDedupeResult {
	if opts == nil {
		opts = &AppendLessonGuardedOpts{}
	}
	lessonsFile := opts.LessonsFile
	if lessonsFile == "" {
		lessonsFile = SelfbuildLessonsPath
	}
	excerptHash := LessonExcerptHash(entry)
	runMustBeat := opts.MustBeat == nil || *opts.MustBeat
	dryRun := opts.DryRun
	// Captured before the entry is staged so the ledger records the slice
	// the check judged against (the candidate is not part of it yet).
	heldOutLines := 0
	if runMustBeat {
		heldOutLines = len(HeldOutLessonHashes(repoPath, lessonsFile).Lines)
	}
	suiteRunner := opts.SuiteRunner

	dedupe := CheckLessonsDedupe(repoPath, entry, &CheckLessonsDedupeOpts{
		LessonsFile: lessonsFile,
		Threshold:   opts.Threshold,
	})

	// suiteLedger records the row and returns the merged result.
	suiteLedger := func(suite LessonsSuiteOutcome, reason LessonsEvalReason, base LessonsDedupeResult, suiteDetail string, heldOut *int, mustBeat *LessonsMustBeatOutcome, mustBeatScore *float64) LessonsDedupeResult {
		var loop *int
		if opts.Loop != nil {
			l := *opts.Loop
			loop = &l
		}
		rec := lessonsEvalRecord{
			TS:              nowISO(),
			Kind:            "event",
			Event:           "lessons-eval",
			ExcerptHash:     excerptHash,
			Similarity:      round3Exact(base.Similarity),
			Threshold:       base.Threshold,
			PredictedImpact: "",
			Suite:           string(suite),
			Accepted:        reason == ReasonAccepted,
			Reason:          string(reason),
			Entry:           truncateUTF16(entry, 300),
		}
		if pi := strings.TrimSpace(opts.PredictedImpact); pi != "" {
			rec.PredictedImpact = truncateUTF16(collapseSpaces(pi), 300)
		}
		if loop != nil {
			rec.Loop = loop
		}
		if heldOut != nil {
			rec.HeldOut = heldOut
		}
		if mustBeat != nil {
			mb := string(*mustBeat)
			rec.MustBeat = &mb
		}
		if mustBeatScore != nil {
			v := round3Exact(*mustBeatScore)
			rec.MustBeatScore = &v
		}
		if suiteDetail != "" {
			d := suiteDetail
			rec.SuiteDetail = &d
		}
		recordLessonsEval(repoPath, rec)
		out := base
		out.Reason = reason
		out.Suite = suite
		if suiteDetail != "" {
			out.SuiteDetail = &suiteDetail
		}
		if heldOut != nil {
			out.HeldOut = heldOut
		}
		if mustBeat != nil {
			out.MustBeat = mustBeat
		}
		if mustBeatScore != nil {
			out.MustBeatScore = mustBeatScore
		}
		return out
	}

	// Gate 1: predictedImpact is required for machine appends.
	if strings.TrimSpace(opts.PredictedImpact) == "" {
		return suiteLedger(SuiteSkipped, ReasonMissingPredictedImpact, dedupe, "", nil, nil, nil)
	}

	// Gate 2: dedupe (similarity below threshold).
	if !dedupe.OK {
		return suiteLedger(SuiteSkipped, ReasonDuplicate, dedupe, "", nil, nil, nil)
	}

	// Gate 2b (dry-run): dedupe is a pure read and always runs; everything
	// from the evaluate step onward is skipped so the validation never
	// stages the lessons file, spawns the suite, or appends a ledger row.
	if dryRun {
		heldOut := len(HeldOutLessonHashes(repoPath, lessonsFile).Lines)
		var mustBeat *LessonsMustBeatOutcome
		var mustBeatScore *float64
		if runMustBeat {
			mb := CheckMustBeat(repoPath, opts.PredictedImpact, lessonsFile)
			mustBeat = &mb
			mustBeatScore = LoadBestMeasuredScore(repoPath, lessonsFile)
		}
		reason := ReasonAccepted
		if mustBeat != nil && *mustBeat == MustBeatBelow {
			reason = ReasonHeldOut
		}
		return LessonsDedupeResult{
			OK:            dedupe.OK,
			Similarity:    dedupe.Similarity,
			MatchedEntry:  dedupe.MatchedEntry,
			Threshold:     dedupe.Threshold,
			Reason:        reason,
			Suite:         SuiteSkipped,
			HeldOut:       &heldOut,
			MustBeat:      mustBeat,
			MustBeatScore: mustBeatScore,
			DryRun:        true,
		}
	}

	// Gate 3: evaluate step — stage the proposed state, run the suite,
	// revert on red.
	p := filepath.Join(repoPath, lessonsFile)
	entryText := AppendPredictedImpact(entry, opts.PredictedImpact)
	var existing []byte
	fileExisted := false
	if data, err := os.ReadFile(p); err == nil {
		existing = data
		fileExisted = true
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return suiteLedger(SuiteRed, ReasonSuiteRed, dedupe, "failed to stage lessons file", nil, nil, nil)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return suiteLedger(SuiteRed, ReasonSuiteRed, dedupe, "failed to stage lessons file", nil, nil, nil)
	}
	payload := entryText + "\n"
	if fileExisted && len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		payload = "\n" + payload
	}
	_, werr := f.WriteString(payload)
	_ = f.Close()
	if werr != nil {
		return suiteLedger(SuiteRed, ReasonSuiteRed, dedupe, "failed to stage lessons file", nil, nil, nil)
	}

	revert := func() {
		// Restore the pre-append bytes exactly (including file absence).
		if !fileExisted {
			_ = os.Remove(p)
			return
		}
		_ = os.WriteFile(p, existing, 0o644)
	}

	suite := RunLessonsSuite(repoPath, &RunLessonsSuiteOpts{TimeoutMs: opts.SuiteTimeoutMs, Runner: suiteRunner})
	if !suite.OK {
		revert()
		return suiteLedger(SuiteRed, ReasonSuiteRed, dedupe, suite.Detail, nil, nil, nil)
	}

	// Gate 4 (held-out tier): must-beat-best-so-far — only after the suite
	// is green, so the staged state is the proposal being compared. A
	// failed comparison rejects and reverts exactly like a red suite.
	if runMustBeat {
		mustBeat := CheckMustBeat(repoPath, opts.PredictedImpact, lessonsFile)
		best := LoadBestMeasuredScore(repoPath, lessonsFile)
		if mustBeat != MustBeatNone && mustBeat != MustBeatBeat {
			revert()
			return suiteLedger(SuiteGreen, ReasonHeldOut, dedupe, "", &heldOutLines, &mustBeat, best)
		}
		// Accepted (or no baseline to beat): record what the check saw.
		return suiteLedger(SuiteGreen, ReasonAccepted, dedupe, "", &heldOutLines, &mustBeat, best)
	}

	return suiteLedger(SuiteGreen, ReasonAccepted, dedupe, "", nil, nil, nil)
}

var predictedImpactPresentRe = regexp.MustCompile(`\bpredictedImpact:\s*\S`)

// collapseSpaces mirrors TS text.replace(/\s+/g, ' ').
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// AppendPredictedImpact appends the optional AHE/Meta-Harness
// `predictedImpact` field: a short, free-form prediction of which future
// outcome the lesson should flip (e.g. "avoids re-picking already-shipped
// backlog items"). When present it is appended to the lesson line on a
// distinct `predictedImpact:` suffix so it round-trips through the digest
// verbatim (the digest is a text cursor, not a parser — it slices lines
// whole). Absent, the entry is written exactly as given.
func AppendPredictedImpact(entry string, predictedImpact string) string {
	if strings.TrimSpace(predictedImpact) == "" {
		return strings.TrimRightFunc(entry, unicode.IsSpace)
	}
	text := strings.TrimRightFunc(entry, unicode.IsSpace)
	impact := truncateUTF16(collapseSpaces(strings.TrimSpace(predictedImpact)), 300)
	if strings.HasSuffix(text, PredictedImpactSuffix) || predictedImpactPresentRe.MatchString(text) {
		return text
	}
	return text + " [predictedImpact: " + impact + "]"
}
