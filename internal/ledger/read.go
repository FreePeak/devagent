package ledger

import (
	"encoding/json"
	"math"
	"os"
	"sort"
	"strings"
)

// TailRecord is a permissive view of any ledger row (audit + event kinds).
// The TS read paths return the raw JSON.parse result, so unknown event kinds
// (loop-result, loop-phase, ...) flow through untouched. Reads never fail on
// schema drift: a known field with an unexpected JSON type is left zero
// (documented divergence: TS carries the raw value and only fails at use
// time; the row is kept either way).
type TailRecord struct {
	TS              string
	Kind            string
	TaskID          string
	Attempt         int
	Event           string
	FailureClass    string
	LastGateExcerpt string
}

// parseLine mirrors `JSON.parse(line)`: any valid JSON parses; invalid JSON
// returns ok=false (caller skips, "a ledger is data, not truth").
func parseLine(line string) (map[string]any, bool) {
	var row map[string]any
	if err := json.Unmarshal([]byte(line), &row); err != nil || row == nil {
		return nil, false
	}
	return row, true
}

func rowString(row map[string]any, key string) (string, bool) {
	if s, ok := row[key].(string); ok {
		return s, true
	}
	return "", false
}

// rowInt decodes a JSON number field; non-integral values truncate (TS keeps
// float64; ledger attempts/counts are written as integers).
func rowInt(row map[string]any, key string) (int, bool) {
	if f, ok := row[key].(float64); ok {
		return int(f), true
	}
	return 0, false
}

func newTailRecord(row map[string]any) TailRecord {
	var rec TailRecord
	rec.TS, _ = rowString(row, "ts")
	rec.Kind, _ = rowString(row, "kind")
	rec.TaskID, _ = rowString(row, "taskId")
	rec.Attempt, _ = rowInt(row, "attempt")
	rec.Event, _ = rowString(row, "event")
	rec.FailureClass, _ = rowString(row, "failureClass")
	rec.LastGateExcerpt, _ = rowString(row, "lastGateExcerpt")
	return rec
}

// readLines returns the non-empty trimmed lines of the ledger file, oldest
// first. A missing file yields nil (TS: existsSync guard -> []).
func readLines(repoPath string) []string {
	data, err := os.ReadFile(ledgerPath(repoPath))
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// ReadLedger reads audit records, oldest first; taskID "" means no filter.
// The audit filter is load-bearing: it skips every row whose kind is not
// "audit" (which is exactly why taskInterrupt post-mortems are invisible to
// clusterFailures — see clusterFailureClasses) and skips corrupt lines.
// Returns nil when the ledger is absent.
func ReadLedger(repoPath string, taskID string) []AuditRecord {
	var out []AuditRecord
	for _, line := range readLines(repoPath) {
		row, ok := parseLine(line)
		if !ok {
			continue // skip corrupt lines; a ledger is data, not truth
		}
		kind, _ := rowString(row, "kind")
		if kind != "audit" {
			continue
		}
		var rec AuditRecord
		blob, _ := json.Marshal(row)
		if json.Unmarshal(blob, &rec) != nil {
			continue // known field with unusable JSON type: skip (TS would keep it)
		}
		if taskID != "" && rec.TaskID != taskID {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// ReadLedgerTail reads any ledger row (audit + event kinds), oldest first;
// taskID "" means no filter, limit <= 0 means unlimited. Backs
// dashboards/history views: most loops write loop-result/attach rows long
// before any audit verdict exists.
func ReadLedgerTail(repoPath string, taskID string, limit int) []TailRecord {
	var out []TailRecord
	for _, line := range readLines(repoPath) {
		row, ok := parseLine(line)
		if !ok {
			continue
		}
		rec := newTailRecord(row)
		if taskID != "" && rec.TaskID != taskID {
			continue
		}
		out = append(out, rec)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// TailEntry is the compact evidence-history row (ts, attempt, verdict,
// integrity — TS ledgerTailFor's object shape, in interface order).
type TailEntry struct {
	TS        string `json:"ts"`
	Attempt   int    `json:"attempt"`
	Verdict   string `json:"verdict"`
	Integrity string `json:"integrity"`
}

// LedgerTailFor reads the compact evidence history for one task, newest
// first, capped at n (n <= 0 defaults to 3).
func LedgerTailFor(repoPath string, taskID string, n int) []TailEntry {
	if n <= 0 {
		n = 3
	}
	records := ReadLedger(repoPath, taskID)
	if len(records) > n {
		records = records[len(records)-n:]
	}
	out := make([]TailEntry, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		out = append(out, TailEntry{TS: r.TS, Attempt: r.Attempt, Verdict: r.Verdict, Integrity: r.Integrity})
	}
	return out
}

// LedgerSummary is the outcome aggregation (tasks, audits, resolved,
// meanAttemptsToPass, unresolved — TS interface order). meanAttemptsToPass is
// null until a task resolves.
type LedgerSummary struct {
	Tasks              int      `json:"tasks"`
	Audits             int      `json:"audits"`
	Resolved           int      `json:"resolved"`
	MeanAttemptsToPass *float64 `json:"meanAttemptsToPass"`
	Unresolved         int      `json:"unresolved"`
}

// SummarizeLedger measures how often the evidence gate rejects first work —
// the auditor's real catch rate.
func SummarizeLedger(repoPath string) LedgerSummary {
	byTask := map[string][]AuditRecord{}
	var order []string
	for _, r := range ReadLedger(repoPath, "") {
		if _, seen := byTask[r.TaskID]; !seen {
			order = append(order, r.TaskID)
		}
		byTask[r.TaskID] = append(byTask[r.TaskID], r)
	}
	audits, resolved, attemptsSum := 0, 0, 0.0
	for _, taskID := range order {
		records := byTask[taskID]
		audits += len(records)
		for _, r := range records {
			if r.Verdict == "pass" && r.Integrity == "clean" {
				resolved++
				attemptsSum += float64(r.Attempt)
				break
			}
		}
	}
	sum := LedgerSummary{Tasks: len(byTask), Audits: audits, Resolved: resolved}
	if resolved > 0 {
		mean := math.Round(attemptsSum/float64(resolved)*100) / 100
		sum.MeanAttemptsToPass = &mean
	}
	sum.Unresolved = len(byTask) - resolved
	return sum
}

// FailureCluster is one recurring gap category across failed audits
// (criterion, occurrences, tasks, openTasks — TS interface order).
type FailureCluster struct {
	Criterion   string   `json:"criterion"`
	Occurrences int      `json:"occurrences"`
	Tasks       []string `json:"tasks"`
	OpenTasks   int      `json:"openTasks"`
}

// ClusterFailures groups recurring unmet acceptance criteria across failed
// audits into a ranked view. Grouping normalizes case and whitespace so
// trivial rewording does not fragment a cluster; semantic variants stay
// separate by design. Passing audits never contribute criteria.
func ClusterFailures(repoPath string) []FailureCluster {
	records := ReadLedger(repoPath, "")
	passedTasks := map[string]bool{}
	for _, r := range records {
		if r.Verdict == "pass" && r.Integrity == "clean" {
			passedTasks[r.TaskID] = true
		}
	}
	var keys []string
	clusters := map[string]*FailureCluster{}
	for _, r := range records {
		if r.Verdict == "pass" {
			continue
		}
		for _, criterion := range r.UnmetCriteria {
			key := normalizeCriterion(criterion)
			c, ok := clusters[key]
			if !ok {
				c = &FailureCluster{Criterion: criterion}
				clusters[key] = c
				keys = append(keys, key)
			}
			c.Occurrences++
			if !containsStr(c.Tasks, r.TaskID) {
				c.Tasks = append(c.Tasks, r.TaskID)
			}
		}
	}
	out := make([]FailureCluster, 0, len(keys))
	for _, key := range keys {
		c := clusters[key]
		for _, t := range c.Tasks {
			if !passedTasks[t] {
				c.OpenTasks++
			}
		}
		out = append(out, *c)
	}
	// Stable sort: TS Array#sort is stable and ties stay in first-seen order.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Occurrences != out[j].Occurrences {
			return out[i].Occurrences > out[j].Occurrences
		}
		return len(out[i].Tasks) > len(out[j].Tasks)
	})
	return out
}

// FailureClassCluster is one recurring executor failure class across
// taskInterrupt event rows (failureClass, occurrences, tasks, exemplar —
// TS interface order).
type FailureClassCluster struct {
	FailureClass string   `json:"failureClass"`
	Occurrences  int      `json:"occurrences"`
	Tasks        []string `json:"tasks"`
	Exemplar     string   `json:"exemplar"`
}

// ClusterFailureClasses groups the taskInterrupt post-mortems (kind "event"
// rows, so readLedger's audit filter never sees them and clusterFailures is
// criteria-only by construction) by executor failure class, ranked next to
// the recurring unmet criteria in `devagent ledger --clusters`. No
// normalization: failureClass is a machine-written taxonomy id, not operator
// wording. Rows with a missing or blank failureClass are skipped; the
// exemplar is the first row's trimmed lastGateExcerpt (TS:
// `typeof row.lastGateExcerpt === 'string' ? row.lastGateExcerpt.trim() : ”`).
func ClusterFailureClasses(repoPath string) []FailureClassCluster {
	var keys []string
	clusters := map[string]*FailureClassCluster{}
	for _, r := range ReadLedgerTail(repoPath, "", 0) {
		if r.Kind != "event" || r.Event != "taskInterrupt" {
			continue
		}
		if trimJS(r.FailureClass) == "" {
			continue
		}
		c, ok := clusters[r.FailureClass]
		if !ok {
			c = &FailureClassCluster{FailureClass: r.FailureClass, Exemplar: trimJS(r.LastGateExcerpt)}
			clusters[r.FailureClass] = c
			keys = append(keys, r.FailureClass)
		}
		c.Occurrences++
		if !containsStr(c.Tasks, r.TaskID) {
			c.Tasks = append(c.Tasks, r.TaskID)
		}
	}
	out := make([]FailureClassCluster, 0, len(keys))
	for _, key := range keys {
		out = append(out, *clusters[key])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Occurrences != out[j].Occurrences {
			return out[i].Occurrences > out[j].Occurrences
		}
		return len(out[i].Tasks) > len(out[j].Tasks)
	})
	return out
}

// EvalScoreEvent is the `event` discriminator of an FR-VAL-05 judge row.
const EvalScoreEvent = "eval-score"

// ReadEvalScores reads the FR-VAL-05 judge rows (`kind: "event"`,
// `event: "eval-score"`) oldest first — the same events.jsonl stream every
// other record family appends to. Rows that fail to decode (schema drift, a
// hand-edited ledger) are skipped rather than guessed at: a missing score must
// never masquerade as a zero.
func ReadEvalScores(repoPath string) []EvalScoreRecord {
	var out []EvalScoreRecord
	for _, line := range readLines(repoPath) {
		row, ok := parseLine(line)
		if !ok {
			continue
		}
		kind, _ := rowString(row, "kind")
		event, _ := rowString(row, "event")
		if kind != "event" || event != EvalScoreEvent {
			continue
		}
		var rec EvalScoreRecord
		blob, _ := json.Marshal(row)
		if json.Unmarshal(blob, &rec) != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// DriftWindow is the default trailing window the quality ratchet measures
// against: ten scores is enough history to hold a best-so-far without letting
// a month-old PR keep gating the loop forever.
const DriftWindow = 10

// QualityDrift is one ratchet violation: a shipped PR scored below the best
// score its own goal class achieved inside the trailing window (FR-VAL-05 #293).
// Criterion carries the largest per-criterion shortfall against that best, so
// the warning names what regressed instead of only "quality dropped". The
// criterion numbers are never omitempty: a 0/25 test-evidence score is exactly
// the finding a reviewer needs, and dropping it would hide the worst case.
type QualityDrift struct {
	GoalClass      string `json:"goalClass"`
	RubricVersion  string `json:"rubricVersion"`
	RubricDigest   string `json:"rubricDigest,omitempty"`
	TaskID         string `json:"taskId"`
	PR             int    `json:"pr"`
	Total          int    `json:"total"`
	Max            int    `json:"max"`
	Best           int    `json:"best"`
	BestPR         int    `json:"bestPr"`
	Drop           int    `json:"drop"`
	Criterion      string `json:"criterion,omitempty"`
	CriterionScore int    `json:"criterionScore"`
	CriterionMax   int    `json:"criterionMax"`
}

// QualityDriftFromScores is the pure ratchet: walk the scores in ledger order
// and report every score that fell below the best of the `window` scores that
// preceded it in its own bucket. The bucket is (goalClass, rubricVersion,
// rubricDigest): a class never competes against another class, and a rubric
// that measures something different — edited weights, edited ids, a typo that
// dropped a criterion — never shares a baseline with the scores it made before
// the edit. A row with no measurement scale (max 0) is malformed and excluded.
// No other-PR row in the window means no baseline, so the first artifact of a
// bucket is never drift, and a re-scored PR is never compared with its own
// earlier judge noise. Ranked by drop, largest first (ties keep ledger order).
func QualityDriftFromScores(records []EvalScoreRecord, window int) []QualityDrift {
	if window <= 0 {
		window = DriftWindow
	}
	var keys []string
	groups := map[string][]EvalScoreRecord{}
	for _, r := range records {
		if r.Max <= 0 {
			continue
		}
		key := r.GoalClass + "\x00" + r.RubricVersion + "\x00" + r.RubricDigest
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], r)
	}
	var out []QualityDrift
	for _, key := range keys {
		rows := groups[key]
		// Only the trailing `window` scores can still be reported as drift;
		// older violations are history, not a warning for the next iteration.
		for i := max(1, len(rows)-window); i < len(rows); i++ {
			cand := rows[i]
			bestIdx := -1
			for j := max(0, i-window); j < i; j++ {
				if rows[j].PR == cand.PR {
					continue
				}
				if bestIdx < 0 || rows[j].Total > rows[bestIdx].Total {
					bestIdx = j
				}
			}
			if bestIdx < 0 || cand.Total >= rows[bestIdx].Total {
				continue
			}
			best := rows[bestIdx]
			drift := QualityDrift{
				GoalClass:     cand.GoalClass,
				RubricVersion: cand.RubricVersion,
				RubricDigest:  cand.RubricDigest,
				TaskID:        cand.TaskID,
				PR:            cand.PR,
				Total:         cand.Total,
				Max:           cand.Max,
				Best:          best.Total,
				BestPR:        best.PR,
				Drop:          best.Total - cand.Total,
			}
			drift.Criterion, drift.CriterionScore, drift.CriterionMax = weakestCriterion(cand, best)
			out = append(out, drift)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Drop > out[j].Drop })
	return out
}

// ClusterQualityDrift reads the ledger and reports the ratchet violations.
func ClusterQualityDrift(repoPath string, window int) []QualityDrift {
	return QualityDriftFromScores(ReadEvalScores(repoPath), window)
}

// weakestCriterion names where the candidate lost the most ground against the
// best score: the criterion with the largest absolute point shortfall, i.e. the
// one that explains the biggest share of the reported drop. Points (not rates)
// because the warning, the drop and the gate budget are all in points. Criteria
// the best row does not carry cannot be compared and are skipped.
func weakestCriterion(cand, best EvalScoreRecord) (string, int, int) {
	var worstName string
	var worstScore, worstMax, worstGap int
	bests := map[string]EvalCriterionScore{}
	for _, c := range best.Criteria {
		bests[c.Criterion] = c
	}
	for _, c := range cand.Criteria {
		b, ok := bests[c.Criterion]
		if !ok || b.Score-c.Score <= worstGap {
			continue
		}
		worstGap = b.Score - c.Score
		worstName, worstScore, worstMax = c.Criterion, c.Score, c.Max
	}
	return worstName, worstScore, worstMax
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
