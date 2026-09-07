package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Monitoring: render a zero-dependency static HTML status board from the
// run-log JSONL (port of src/observe.ts). The dashboard must render the
// identical board model from identical JSONL input — the RunSummary
// extraction, kanban derivation, day/feature grouping and every HTML literal
// below mirror the TS generator.

const (
	// MaxInlineEvents is shown in the expandable row.
	MaxInlineEvents = 50
	// MaxEmbedEvents is embedded for the lazy new-tab detail view.
	MaxEmbedEvents = 500
)

// CardStatus is the four-column kanban state.
type CardStatus string

const (
	CardTodo       CardStatus = "todo"
	CardInProgress CardStatus = "inprogress"
	CardDone       CardStatus = "done"
	CardFailed     CardStatus = "failed"
)

// RunEvent is one JSONL event row of a run.
type RunEvent struct {
	Ts      string `json:"ts"`
	Stage   string `json:"stage"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// RunSummary is the per-run board model. Optional fields are nil/omitted,
// matching TS undefined; StartedAt/LastAt are null when absent (TS null).
type RunSummary struct {
	RunID       string     `json:"runId"`
	File        string     `json:"file"`
	StartedAt   *string    `json:"startedAt"`
	LastAt      *string    `json:"lastAt"`
	LastStage   string     `json:"lastStage"`
	LastLevel   string     `json:"lastLevel"`
	LastMessage string     `json:"lastMessage"`
	EventCount  int        `json:"eventCount"`
	Ok          bool       `json:"ok"`
	Title       *string    `json:"title,omitempty"`
	Repo        *string    `json:"repo,omitempty"`
	DurationMs  *float64   `json:"durationMs,omitempty"`
	ExitCode    *float64   `json:"exitCode,omitempty"`
	TimedOut    bool       `json:"timedOut,omitempty"`
	Ticket      *string    `json:"ticket,omitempty"`
	PrURL       *string    `json:"prUrl,omitempty"`
	Timeline    []RunEvent `json:"timeline"`
}

// BoardTask is one durable board task rendered in the board tab.
type BoardTask struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Status        string   `json:"status"`
	Attempts      *int     `json:"attempts,omitempty"`
	FailureDetail *string  `json:"failureDetail,omitempty"`
	EvidenceGaps  []string `json:"evidenceGaps,omitempty"`
	AuditStatus   *string  `json:"auditStatus,omitempty"`
}

// LoadedBoard is one scanned .devagent-project.json.
type LoadedBoard struct {
	Path  string      `json:"path"`
	Goal  string      `json:"goal"`
	Tasks []BoardTask `json:"tasks"`
}

// DeriveRunStatus picks the kanban column for a run log: triage-first, PR
// presence means shipped.
func DeriveRunStatus(s RunSummary) CardStatus {
	if !s.Ok || s.TimedOut {
		return CardFailed
	}
	if s.PrURL != nil {
		return CardDone
	}
	implemented := false
	for _, e := range s.Timeline {
		if e.Stage == "implement" || e.Stage == "validate" {
			implemented = true
			break
		}
	}
	if implemented {
		return CardInProgress
	}
	return CardTodo // plan-only runs are queued work
}

var (
	runStartingRe = regexp.MustCompile(`(?i)^run\s[0-9a-f-]+\sstarting$`)
	noiseRe       = regexp.MustCompile(`(?i)^(task starting|dispatching\s\w+:\s*\w+|\w+\sdone(\s\(audited\))?)$`)
	dispatchRe    = regexp.MustCompile(`(?i)^dispatching\s`)
	prRe          = regexp.MustCompile(`https://[^\s"']+(?:/pull/|/-/merge_requests/)[^\s"']*`)
)

// DeriveLabel is the human-readable one-liner for a run: structured title
// when the pipeline recorded one, then ticket reference, then the most
// informative-looking plain message, so cards never open as bare UUIDs.
func DeriveLabel(s RunSummary) string {
	if s.Title != nil && strings.TrimSpace(*s.Title) != "" {
		return strings.TrimSpace(*s.Title)
	}
	if s.Ticket != nil && strings.TrimSpace(*s.Ticket) != "" {
		return strings.TrimSpace(*s.Ticket)
	}
	for _, e := range s.Timeline {
		if !runStartingRe.MatchString(e.Message) && !noiseRe.MatchString(e.Message) {
			if len(e.Message) > 90 {
				return runeSlice(e.Message, 0, 87) + "..."
			}
			return e.Message
		}
	}
	for _, e := range s.Timeline {
		if dispatchRe.MatchString(e.Message) {
			return "Orchestrator dispatch loop"
		}
	}
	return runeSlice(s.RunID, 0, 8)
}

var boardToCard = map[string]CardStatus{
	"pending":    CardTodo,
	"ready":      CardTodo,
	"blocked":    CardTodo,
	"ask":        CardTodo,
	"dispatched": CardInProgress,
	"untrusted":  CardInProgress,
	"done":       CardDone,
	"failed":     CardFailed,
}

// MapBoardStatus maps a project-board status onto the four columns;
// unknown statuses fail safe to todo.
func MapBoardStatus(status string) CardStatus {
	if c, ok := boardToCard[status]; ok {
		return c
	}
	return CardTodo
}

// CollectRunSummaries scans a runs dir (DEVAGENT_HOME/runs) and builds the
// per-run summaries.
func CollectRunSummaries(runsDir string) []RunSummary {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil
	}
	var summaries []RunSummary
	for _, entry := range entries {
		f := entry.Name()
		if !strings.HasSuffix(f, ".jsonl") {
			continue
		}
		rawBytes, err := os.ReadFile(filepath.Join(runsDir, f))
		if err != nil {
			continue
		}
		raw := strings.TrimSpace(string(rawBytes))
		if raw == "" {
			continue
		}
		lines := strings.Split(raw, "\n")
		parsed := make([]map[string]any, 0, len(lines))
		for _, line := range lines {
			var v any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				continue
			}
			if m, ok := v.(map[string]any); ok {
				parsed = append(parsed, m)
			} else {
				parsed = append(parsed, nil) // JSON null row: first/last checks see it like TS
			}
		}
		if len(parsed) == 0 {
			continue
		}
		first, last := parsed[0], parsed[len(parsed)-1]
		if last == nil || first == nil {
			continue
		}
		level := jsStringOr(last["level"], "info")
		runID := jsStringOr(last["runId"], strings.TrimSuffix(f, ".jsonl"))
		startedAt := jsStringOrNull(first["ts"])
		lastAt := jsStringOrNull(last["ts"])
		s := RunSummary{
			RunID:       runID,
			File:        f,
			StartedAt:   startedAt,
			LastAt:      lastAt,
			LastStage:   jsStringOrEmpty(last["stage"]),
			LastLevel:   level,
			LastMessage: jsStringOrEmpty(last["message"]),
			EventCount:  len(lines),
			Ok:          level != "error",
		}
		// Enrich from run-level metadata carried in event payloads.
		for _, e := range parsed {
			if e == nil {
				continue // TS would throw on a null row; Go degrades instead
			}
			msg := jsStringOrEmpty(e["message"])
			if s.PrURL == nil {
				if loc := prRe.FindStringIndex(msg); loc != nil {
					trimmed := strings.TrimRight(msg[loc[0]:loc[1]], ".)")
					s.PrURL = &trimmed
				}
			}
			d, _ := e["data"].(map[string]any)
			if s.Title == nil {
				if v, ok := d["title"].(string); ok {
					s.Title = &v
				}
			}
			if s.Repo == nil {
				if v, ok := d["repo"].(string); ok {
					s.Repo = &v
				}
			}
			if s.Ticket == nil {
				if v, ok := d["ticket"].(string); ok {
					s.Ticket = &v
				}
			}
			if s.DurationMs == nil {
				if v, ok := d["durationMs"].(float64); ok {
					s.DurationMs = &v
				}
			}
			if s.ExitCode == nil {
				if v, ok := d["exitCode"].(float64); ok {
					s.ExitCode = &v
				}
			}
			if v, ok := d["timedOut"].(bool); ok && v {
				s.TimedOut = true
			}
		}
		events := make([]RunEvent, 0, len(parsed))
		for _, e := range parsed {
			if e == nil {
				continue
			}
			events = append(events, RunEvent{
				Ts:      jsStringOrEmpty(e["ts"]),
				Stage:   jsStringOrEmpty(e["stage"]),
				Level:   jsStringOr(e["level"], "info"),
				Message: jsStringOrEmpty(e["message"]),
			})
		}
		if len(events) > MaxInlineEvents {
			events = events[len(events)-MaxInlineEvents:]
		}
		s.Timeline = events
		summaries = append(summaries, s)
	}
	return summaries
}

func jsStringOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return def
	}
	return jsAnyString(v)
}

func jsStringOrEmpty(v any) string {
	return jsStringOr(v, "")
}

func jsStringOrNull(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// jsAnyString mirrors String(x) for the non-string JSON values the log rows
// can carry (numbers, booleans).
func jsAnyString(v any) string {
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return jsNum(x)
	default:
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
}

// LoadBoards scans directories for durable .devagent-project.json boards.
func LoadBoards(dirs []string) []LoadedBoard {
	var boards []LoadedBoard
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		file := filepath.Join(dir, ".devagent-project.json")
		if _, err := os.Stat(file); err != nil {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var b struct {
			Goal  *string          `json:"goal"`
			Tasks []map[string]any `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &b); err != nil || b.Tasks == nil {
			continue // corrupted board: skip, runs view still works
		}
		board := LoadedBoard{Path: file, Goal: derefStr(b.Goal)}
		for _, t := range b.Tasks {
			task := BoardTask{
				ID:     jsStringOr(t["id"], ""),
				Title:  jsStringOr(t["title"], jsStringOr(t["prompt"], "")),
				Status: jsStringOr(t["status"], "pending"),
			}
			if v, ok := t["attempts"].(float64); ok {
				n := int(v)
				task.Attempts = &n
			}
			if v, ok := t["failureDetail"].(string); ok {
				task.FailureDetail = &v
			}
			if gaps, ok := t["evidenceGaps"].([]any); ok {
				var gs []string
				for _, g := range gaps {
					if s, ok := g.(string); ok {
						gs = append(gs, s)
					}
				}
				task.EvidenceGaps = gs
			}
			if audit, ok := t["audit"].(map[string]any); ok {
				if st, ok := audit["status"].(string); ok {
					task.AuditStatus = &st
				}
			}
			board.Tasks = append(board.Tasks, task)
		}
		boards = append(boards, board)
	}
	return boards
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// esc escapes HTML text content exactly like the TS esc().
var escReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

func esc(s string) string { return escReplacer.Replace(s) }

// jsonEmbed embeds arbitrary data inside a <script type="application/json">
// safely. encoding/json escapes < > & to \u003c \u003e \u0026 by default —
// the same escapes the TS generator applies by hand.
func jsonEmbed(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// fmtDuration renders a duration: "Nms" under a second, then "MmSSs".
func fmtDuration(ms float64) string {
	if ms < 1000 {
		return jsNum(ms) + "ms"
	}
	m := int(ms / 60000)
	sec := int(mathRound((ms - float64(m*60000)) / 1000))
	if m > 0 {
		return strconv.Itoa(m) + "m" + strconv.Itoa(sec) + "s"
	}
	return strconv.Itoa(sec) + "s"
}

// maxDay is the newest YYYY-MM-DD across run starts (the heatmap anchor).
func maxDay(summaries []RunSummary) string {
	anchor := "1970-01-01"
	for _, s := range summaries {
		day := ""
		if s.StartedAt != nil {
			day = *s.StartedAt
		}
		if len(day) > 10 {
			day = day[:10]
		}
		if dayRe.MatchString(day) && day > anchor {
			anchor = day
		}
	}
	return anchor
}

var dayRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// renderHeatmap renders GitHub-style per-day activity squares for the
// trailing N days.
func renderHeatmap(summaries []RunSummary, days int) string {
	byDay := map[string][2]int{}
	for _, s := range summaries {
		day := ""
		if s.StartedAt != nil {
			day = *s.StartedAt
		}
		if len(day) > 10 {
			day = day[:10]
		}
		if !dayRe.MatchString(day) {
			continue
		}
		cur := byDay[day]
		cur[0]++
		if !s.Ok {
			cur[1]++
		}
		byDay[day] = cur
	}
	end, err := time.Parse(time.RFC3339, maxDay(summaries)+"T00:00:00Z")
	if err != nil {
		return ""
	}
	endMs := end.UnixMilli()
	var cells []string
	for i := days - 1; i >= 0; i-- {
		d := time.UnixMilli(endMs - int64(i)*86400000).UTC()
		key := d.Format("2006-01-02")
		v, ok := byDay[key]
		lvl := 0
		if ok {
			switch {
			case v[0] < 5:
				lvl = 1
			case v[0] < 20:
				lvl = 2
			case v[0] < 60:
				lvl = 3
			default:
				lvl = 4
			}
		}
		cls := "hm l" + strconv.Itoa(lvl)
		if ok && v[1] > 0 {
			cls = "hm err"
		}
		label := key + ": no runs"
		if ok {
			label = key + ": " + strconv.Itoa(v[0]) + " run(s)"
			if v[1] > 0 {
				label += ", " + strconv.Itoa(v[1]) + " failed"
			}
		}
		cells = append(cells, `<span class="`+cls+`" title="`+esc(label)+`"></span>`)
	}
	return `<div class="heatmap">` + strings.Join(cells, "") +
		`<span class="hmlabel">` + esc(maxDay(summaries)) + `, trailing ` + strconv.Itoa(days) + `d</span></div>`
}

func statTile(label, value, cls string) string {
	return `<div class="tile ` + cls + `"><div class="tv">` + esc(value) + `</div><div class="tl">` + esc(label) + `</div></div>`
}

func renderStats(summaries []RunSummary) string {
	failed := 0
	for _, s := range summaries {
		if !s.Ok {
			failed++
		}
	}
	today, err := time.Parse(time.RFC3339, maxDay(summaries)+"T00:00:00Z")
	if err != nil {
		today = time.Unix(0, 0)
	}
	runsToday := 0
	var durations []float64
	for _, s := range summaries {
		if s.StartedAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *s.StartedAt); err == nil && t.UnixMilli() >= today.UnixMilli() {
				runsToday++
			}
		}
		if s.DurationMs != nil {
			durations = append(durations, *s.DurationMs)
		}
	}
	sort.Float64s(durations)
	med := "-"
	if len(durations) > 0 {
		med = fmtDuration(durations[len(durations)/2])
	}
	cls := ""
	if failed > 0 {
		cls = "bad"
	}
	return strings.Join([]string{
		statTile("runs", strconv.Itoa(len(summaries)), ""),
		statTile("ok", strconv.Itoa(len(summaries)-failed), "good"),
		statTile("failed", strconv.Itoa(failed), cls),
		statTile("runs since "+maxDay(summaries), strconv.Itoa(runsToday), "good"),
		statTile("median duration", med, ""),
	}, "")
}

var statusLabel = map[CardStatus]string{
	CardTodo:       "todo",
	CardInProgress: "in progress",
	CardDone:       "done",
	CardFailed:     "failed",
}

func statusPill(st CardStatus) string {
	return `<span class="st st-` + string(st) + `">` + statusLabel[st] + `</span>`
}

func metaBits(s RunSummary) string {
	var bits []string
	if s.ExitCode != nil {
		bits = append(bits, "exit "+jsNum(*s.ExitCode))
	}
	if s.TimedOut {
		bits = append(bits, "timed out")
	}
	if s.DurationMs != nil {
		bits = append(bits, fmtDuration(*s.DurationMs))
	}
	if s.Repo != nil {
		parts := strings.Split(*s.Repo, "/")
		if len(parts) > 3 {
			parts = parts[len(parts)-3:]
		}
		bits = append(bits, strings.Join(parts, "/"))
	}
	if s.Ticket != nil {
		bits = append(bits, *s.Ticket)
	}
	if len(bits) == 0 {
		return ""
	}
	return `<span class="meta">` + esc(strings.Join(bits, " · ")) + `</span>`
}

var (
	tsTrailRe = regexp.MustCompile(`\.\d+Z$`)
)

func htmlWhen(ts string) string {
	return tsTrailRe.ReplaceAllString(strings.Replace(ts, "T", " ", 1), "")
}

func renderTimeline(timeline []RunEvent) string {
	var lis []string
	for _, e := range timeline {
		cls := ""
		if e.Level == "error" {
			cls = ` class="tl-err"`
		}
		lis = append(lis, `<li`+cls+`>
<span class="ts">`+esc(htmlWhen(e.Ts))+`</span>
<span class="pill">`+esc(e.Stage)+`</span>
<span>`+esc(e.Message)+`</span></li>`)
	}
	return `<ol class="timeline">` + strings.Join(lis, "") + `</ol>`
}

// embedSeqCounter numbers the lazy-detail embed blocks; guarded for tests.
var embedMu sync.Mutex
var embedSeq int

func nextEmbedID() string {
	embedMu.Lock()
	defer embedMu.Unlock()
	id := "ev" + strconv.Itoa(embedSeq)
	embedSeq++
	return id
}

// ResetEmbedSeq renumbers the embed blocks (tests run the generator repeatedly).
func ResetEmbedSeq() {
	embedMu.Lock()
	embedSeq = 0
	embedMu.Unlock()
}

func runRowHTML(s RunSummary) string {
	id8 := esc(runeSlice(s.RunID, 0, 8))
	status := DeriveRunStatus(s)
	heading := esc(DeriveLabel(s)) + ` <small class="rid-id">` + id8 + `</small>`
	prLink := ""
	if s.PrURL != nil {
		prLink = ` <a href="` + esc(*s.PrURL) + `" target="_blank" rel="noopener">PR ↗</a>`
	}
	embedID := nextEmbedID()
	openBtn := `<button class="openbtn" onclick="openDetail('` + embedID + `','` + esc(s.RunID) + `')">open</button>`
	searchParts := []string{s.RunID, s.LastMessage}
	if s.Title != nil {
		searchParts = append(searchParts, *s.Title)
	} else {
		searchParts = append(searchParts, "")
	}
	if s.Repo != nil {
		searchParts = append(searchParts, *s.Repo)
	} else {
		searchParts = append(searchParts, "")
	}
	searchParts = append(searchParts, s.LastStage)
	if s.Ticket != nil {
		searchParts = append(searchParts, *s.Ticket)
	} else {
		searchParts = append(searchParts, "")
	}
	dataSearch := strings.ToLower(esc(strings.Join(searchParts, " ")))
	when := ""
	if s.StartedAt != nil {
		when = htmlWhen(*s.StartedAt)
	}
	var sb strings.Builder
	sb.WriteString(`<details class="run ` + string(status) + `" data-status="` + string(status) + `" data-search="` + dataSearch + `">
<summary>
<span class="rid">` + heading + `</span>` + statusPill(status) + `
<span class="msg">` + esc(s.LastMessage) + prLink + `</span>
` + metaBits(s) + `
<span class="when">` + esc(when) + `</span>
` + openBtn + `
</summary>
`)
	if s.Timeline != nil {
		sb.WriteString(renderTimeline(s.Timeline))
	}
	type embedPayload struct {
		Run    RunSummary `json:"run"`
		Events []RunEvent `json:"events,omitempty"`
	}
	sb.WriteString(`<script type="application/json" id="` + embedID + `">` +
		jsonEmbed(embedPayload{Run: s, Events: s.Timeline}) + `</script>
</details>`)
	return sb.String()
}

func runCardHTML(s RunSummary) string {
	label := esc(runeSlice(DeriveLabel(s), 0, 100))
	prLink := ""
	if s.PrURL != nil {
		prLink = ` <a href="` + esc(*s.PrURL) + `" target="_blank" rel="noopener">PR ↗</a>`
	}
	fail := ""
	if !s.Ok {
		fail = `<div class="cfail">` + esc(runeSlice(s.LastMessage, 0, 120)) + `</div>`
	}
	when := ""
	if s.StartedAt != nil {
		when = *s.StartedAt
	}
	if len(when) > 16 {
		when = when[:16]
	}
	return `<div class="card runcard" title="` + esc(s.RunID) + `"><div class="cid">` + label + prLink +
		`</div>` + fail + `<div class="cmeta">` + esc(strings.Replace(when, "T", " ", 1)) + " · " +
		esc(s.LastStage) + `</div></div>`
}

func renderBoardTab(boards []LoadedBoard, summaries []RunSummary) string {
	cols := []CardStatus{CardTodo, CardInProgress, CardDone, CardFailed}
	cards := map[CardStatus][]string{
		CardTodo:       {},
		CardInProgress: {},
		CardDone:       {},
		CardFailed:     {},
	}
	for _, b := range boards {
		for _, t := range b.Tasks {
			var badges []string
			if t.Status == "ask" {
				badges = append(badges, `<span class="badge ask">needs input</span>`)
			}
			if t.Status == "blocked" {
				badges = append(badges, `<span class="badge blocked">blocked</span>`)
			}
			if t.Status == "untrusted" {
				badges = append(badges, `<span class="badge">unaudited</span>`)
			}
			if t.Attempts != nil && *t.Attempts > 1 {
				badges = append(badges, `<span class="badge">try `+strconv.Itoa(*t.Attempts)+`</span>`)
			}
			if len(t.EvidenceGaps) > 0 {
				badges = append(badges, `<span class="badge gap">`+strconv.Itoa(len(t.EvidenceGaps))+` gap(s)</span>`)
			}
			title := t.Title
			if len(title) > 140 {
				title = runeSlice(title, 0, 140)
			}
			cards[MapBoardStatus(t.Status)] = append(cards[MapBoardStatus(t.Status)],
				`<div class="card"><div class="cid">`+esc(t.ID)+`</div><div class="ctitle">`+esc(title)+`</div>`+
					strings.Join(badges, "")+`</div>`)
		}
	}
	for _, s := range summaries {
		cards[DeriveRunStatus(s)] = append(cards[DeriveRunStatus(s)], runCardHTML(s))
	}
	var colHTML []string
	for _, c := range cols {
		list := cards[c]
		inner := strings.Join(list, "")
		if inner == "" {
			inner = `<p class="empty">—</p>`
		}
		colHTML = append(colHTML, `<div class="col"><h3>`+statusLabel[c]+` <small>`+
			strconv.Itoa(len(list))+`</small></h3>`+inner+`</div>`)
	}
	note := ""
	if len(boards) == 0 {
		note = `<p class="note">No .devagent-project.json boards found — board shows run-derived state only. Pass board dirs via DEVAGENT_BOARD_DIRS.</p>`
	}
	return note + `<div class="boardcols">` + strings.Join(colHTML, "") + `</div>`
}

func renderRunsTab(summaries []RunSummary) string {
	// Group by calendar day of startedAt, newest first; insertion-ordered.
	var days []string
	byDay := map[string][]RunSummary{}
	for _, s := range summaries {
		day := "unknown"
		if s.StartedAt != nil && len(*s.StartedAt) >= 10 {
			day = (*s.StartedAt)[:10]
		}
		if _, seen := byDay[day]; !seen {
			days = append(days, day)
		}
		byDay[day] = append(byDay[day], s)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days)))
	var out []string
	for _, day := range days {
		list := byDay[day]
		rev := make([]RunSummary, len(list))
		for i, s := range list {
			rev[len(list)-1-i] = s
		}
		failed := 0
		for _, s := range list {
			if !s.Ok {
				failed++
			}
		}
		failedNote := ""
		if failed > 0 {
			failedNote = ", " + strconv.Itoa(failed) + " failed"
		}
		var rows []string
		for _, s := range rev {
			rows = append(rows, runRowHTML(s))
		}
		out = append(out, `<h2 class="dayhdr">`+esc(day)+` <small>`+strconv.Itoa(len(rev))+
			` run(s)`+failedNote+`</small></h2>
<div class="list">`+strings.Join(rows, "")+`</div>`)
	}
	return strings.Join(out, "")
}

func renderFeaturesTab(summaries []RunSummary) string {
	// A feature = distinct title or ticket; runs without either collapse onto
	// their repo+day. Insertion-ordered like the TS Map.
	var keys []string
	groups := map[string][]RunSummary{}
	for _, s := range summaries {
		key := ""
		if s.Title != nil {
			key = strings.TrimSpace(*s.Title)
		}
		if key == "" && s.Ticket != nil {
			key = strings.TrimSpace(*s.Ticket)
		}
		if key == "" {
			repo := "no-repo"
			if s.Repo != nil {
				repo = *s.Repo
			}
			day := ""
			if s.StartedAt != nil {
				day = *s.StartedAt
			}
			if len(day) > 10 {
				day = day[:10]
			}
			key = repo + " " + day
		}
		if _, seen := groups[key]; !seen {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], s)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return lastStarted(groups[keys[j]]) < lastStarted(groups[keys[i]])
	})
	var out []string
	for _, key := range keys {
		runs := groups[key]
		counts := map[CardStatus]int{}
		var order []CardStatus
		for _, r := range runs {
			st := DeriveRunStatus(r)
			if _, seen := counts[st]; !seen {
				order = append(order, st)
			}
			counts[st]++
		}
		var pills []string
		for _, st := range order {
			pills = append(pills, statusPill(st)+` <small>`+strconv.Itoa(counts[st])+`</small>`)
		}
		prLink := ""
		for _, r := range runs {
			if r.PrURL != nil {
				prLink = ` <a href="` + esc(*r.PrURL) + `" target="_blank" rel="noopener">PR ↗</a>`
				break
			}
		}
		rev := make([]RunSummary, len(runs))
		for i, s := range runs {
			rev[len(runs)-1-i] = s
		}
		var rows []string
		for _, s := range rev {
			rows = append(rows, runRowHTML(s))
		}
		out = append(out, `<details class="feature">
<summary><span class="rid">`+esc(key)+`</span>`+strings.Join(pills, " ")+prLink+
			`<span class="meta">`+strconv.Itoa(len(runs))+` run(s)</span></summary>
<div class="list">`+strings.Join(rows, "")+`</div>
</details>`)
	}
	joined := strings.Join(out, "")
	if joined == "" {
		return `<p class="empty">No features yet.</p>`
	}
	return joined
}

func lastStarted(runs []RunSummary) string {
	if len(runs) == 0 {
		return ""
	}
	s := runs[len(runs)-1].StartedAt
	if s == nil {
		return ""
	}
	return *s
}

// RenderDashboardHTML renders the static HTML board (src/observe.ts
// renderDashboard; named ...HTML here because the terminal renderer of the
// same TS name lives in this package too).
func RenderDashboardHTML(summaries []RunSummary, title string, boards []LoadedBoard) string {
	if title == "" {
		title = "DevAgent Runs"
	}
	// Newest-first display everywhere; callers may hand us either order.
	display := make([]RunSummary, len(summaries))
	copy(display, summaries)
	sort.SliceStable(display, func(i, j int) bool {
		a, b := "", ""
		if display[i].StartedAt != nil {
			a = *display[i].StartedAt
		}
		if display[j].StartedAt != nil {
			b = *display[j].StartedAt
		}
		return b < a
	})
	var sb strings.Builder
	sb.WriteString(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + esc(title) + `</title>
<style>
body{font-family:ui-sans-serif,system-ui,sans-serif;margin:1.5rem;background:#111;color:#eee}
h1 small,h2 small,h3 small{color:#888;font-weight:normal}
small{color:#888}
.tabs{display:flex;gap:.3rem;margin:1rem 0;border-bottom:1px solid #2a2a2a}
.tabbtn{background:none;border:none;color:#999;padding:.5rem .9rem;font-size:.9rem;cursor:pointer;border-bottom:2px solid transparent}
.tabbtn.on{color:#eee;border-bottom-color:#46a758}
.tabpane{display:none}.tabpane.on{display:block}
.tiles{display:flex;gap:.75rem;margin:1rem 0;flex-wrap:wrap}
.tile{background:#1a1a1a;border:1px solid #2a2a2a;border-radius:8px;padding:.6rem 1rem;min-width:7rem}
.tv{font-size:1.4rem;font-weight:600}.tl{font-size:.7rem;color:#999;text-transform:uppercase;letter-spacing:.05em}
.tile.good .tv{color:#46a758}.tile.bad .tv{color:#e5484d}
.heatmap{display:flex;align-items:center;gap:3px;margin:.5rem 0 1.5rem;flex-wrap:wrap}
.hm{width:14px;height:14px;border-radius:3px;background:#222;display:inline-block}
.hm.l1{background:#1b3a2a}.hm.l2{background:#256a43}.hm.l3{background:#37a06a}.hm.l4{background:#46a758}
.hm.err{background:#e5484d}
.hmlabel{font-size:.7rem;color:#888;margin-left:.5rem}
.boardcols{display:grid;grid-template-columns:repeat(4,minmax(15rem,1fr));gap:.75rem;align-items:start}
.col{background:#151515;border:1px solid #262626;border-radius:8px;padding:.6rem}
.col h3{margin:.2rem .2rem .6rem;font-size:.8rem;text-transform:uppercase;color:#aaa}
.card{background:#1d1d1d;border:1px solid #303030;border-radius:6px;padding:.5rem .6rem;margin-bottom:.4rem;font-size:.78rem}
.card .cid{font-family:ui-monospace,monospace;color:#9ecbff;font-size:.75rem;margin-bottom:.2rem}
.card .ctitle{color:#ddd}
.card .cfail{color:#e5484d;margin-top:.2rem}
.card .cmeta{color:#666;font-size:.68rem;margin-top:.3rem}
.st{padding:.05rem .45rem;border-radius:99px;font-size:.65rem;text-transform:uppercase}
.st-todo{background:#2a2a2a;color:#bbb}.st-inprogress{background:#14406b;color:#7cc0ff}
.st-done{background:#173b26;color:#46a758}.st-failed{background:#47191c;color:#ff7b81}
.badge{display:inline-block;background:#2a2a2a;color:#ccc;border-radius:4px;padding:.05rem .35rem;font-size:.62rem;margin-right:.25rem}
.badge.ask{background:#4a3a14;color:#ffd166}.badge.blocked{background:#47191c;color:#ff7b81}.badge.gap{background:#3a2a14;color:#ffb066}
.dayhdr{border-bottom:1px solid #262626;padding-bottom:.3rem}
input#f{background:#1a1a1a;border:1px solid #333;color:#eee;border-radius:6px;padding:.4rem .6rem;width:18rem;font-size:.85rem}
button.fbtn,.openbtn{background:#1a1a1a;border:1px solid #333;color:#ccc;border-radius:6px;padding:.3rem .7rem;font-size:.72rem;cursor:pointer;margin-left:.3rem}
button.fbtn.on{border-color:#46a758;color:#46a758}
.openbtn:hover{border-color:#7cc0ff;color:#7cc0ff}
.list{margin:.5rem 0 1.5rem}
details.run,details.feature{background:#161616;border:1px solid #262626;border-radius:6px;margin-bottom:.35rem}
details.run.err,details.feature:has(details.run.err){border-left:3px solid #e5484d}
details.run.done{border-left:3px solid #256a43}
details.run.todo,details.run.inprogress{border-left:3px solid #444}
summary{display:flex;align-items:center;gap:.6rem;padding:.45rem .7rem;cursor:pointer;flex-wrap:wrap}
details.feature>summary{font-weight:600}
summary:hover{background:#1c1c1c}
.rid{font-family:ui-monospace,monospace;font-size:.8rem;color:#9ecbff;min-width:14rem}
.rid-id{color:#666;font-size:.68rem;font-weight:normal;font-family:ui-monospace,monospace}
details.run .rid{min-width:auto;max-width:28rem}
.msg{font-size:.8rem;color:#ccc;flex:1;min-width:12rem}
.meta,.when{font-size:.7rem;color:#777}
.pill{background:#2a2a2a;color:#bbb;padding:.1rem .5rem;border-radius:99px;font-size:.7rem;text-transform:uppercase}
.timeline{list-style:none;margin:.4rem .8rem .8rem;padding:.4rem 0 0;border-top:1px dashed #2a2a2a}
.timeline li{font-size:.75rem;color:#aaa;padding:.15rem 0;display:flex;gap:.6rem;flex-wrap:wrap}
.timeline li.tl-err{color:#e5484d}
.ts{font-family:ui-monospace,monospace;color:#666}
.empty{color:#666;font-style:italic;padding:.5rem 0}
.note{color:#ffb066;font-size:.75rem;background:#241c10;border:1px solid #3a2a14;border-radius:6px;padding:.4rem .7rem}
@media print{body{background:#fff;color:#111}}
</style></head><body>
<h1>` + esc(title) + ` <small>(` + strconv.Itoa(len(summaries)) + ` runs)</small></h1>
<div class="tabs">
<button class="tabbtn on" id="tb-board" onclick="showTab('board')">Board</button>
<button class="tabbtn" id="tb-runs" onclick="showTab('runs')">Runs by date</button>
<button class="tabbtn" id="tb-features" onclick="showTab('features')">Features</button>
</div>
<div class="tiles">` + renderStats(display) + `</div>
<h2 style="margin-top:0">Activity</h2>
` + renderHeatmap(display, 14) + `
<div class="tabpane on" id="tp-board">` + renderBoardTab(boards, display) + `</div>
<div class="tabpane" id="tp-runs">
<p><input id="f" type="search" placeholder="filter runs..." oninput="applyFilter()">
<button class="fbtn on" id="b-all" onclick="setStatus('all')">all</button>
<button class="fbtn" id="b-ok" onclick="setStatus('ok')">ok</button>
<button class="fbtn" id="b-err" onclick="setStatus('err')">failed</button></p>
<div id="runs">` + renderRunsTab(display) + `</div>
</div>
<div class="tabpane" id="tp-features">` + renderFeaturesTab(display) + `</div>
<script>
function showTab(name){
  ['board','runs','features'].forEach(function(k){
    document.getElementById('tb-'+k).classList.toggle('on',k===name);
    document.getElementById('tp-'+k).classList.toggle('on',k===name);
  });
}
var statusFilter='all';
function applyFilter(){
  var q=document.getElementById('f').value.toLowerCase();
  document.querySelectorAll('#tp-runs details.run').forEach(function(el){
    var okStatus=statusFilter==='all'||el.dataset.status===statusFilter;
    var okText=!q||(el.dataset.search||'').indexOf(q)!==-1;
    el.style.display=okStatus&&okText?'':'none';
  });
  document.querySelectorAll('#tp-runs .dayhdr').forEach(function(h){
    var vis=0,nxt=h.nextElementSibling;
    if(nxt){nxt.querySelectorAll('details.run').forEach(function(el){if(el.style.display!=='none')vis++;});}
    h.style.display=vis?'':'none';
  });
}
function setStatus(s){
  statusFilter=s;
  ['all','ok','err'].forEach(function(k){
    document.getElementById('b-'+k).classList.toggle('on',k===s);
  });
  applyFilter();
}
/* Lazy detail view: renders the embedded run JSON into a fresh tab. */
function openDetail(embedId,runId){
  var node=document.getElementById(embedId);
  var payload=node?JSON.parse(node.textContent):null;
  var w=window.open('','_blank');
  if(!w)return;
  var evs=(payload&&payload.events)||[];
  var rows=evs.map(function(e){
    return '<tr class="'+(e.level==='error'?'err':'')+'"><td>'+escH(e.ts)+'</td><td>'+escH(e.stage)+'</td><td>'+escH(e.message)+'</td></tr>';
  }).join('');
  var raw='<pre>'+escH(JSON.stringify(payload&&(payload.run||payload),null,2))+'</pre>';
  w.document.write('<!doctype html><html><head><meta charset="utf-8"><title>'+escH(runId)+' — DevAgent run detail</title>'+
  '<style>body{font-family:ui-sans-serif,system-ui,sans-serif;margin:1.5rem;background:#111;color:#eee}'+
  'table{border-collapse:collapse;width:100%}th,td{text-align:left;padding:.35rem .5rem;border-bottom:1px solid #2a2a2a;font-size:.8rem}'+
  'th{color:#999;text-transform:uppercase;font-size:.65rem}tr.err td{color:#ff7b81}'+
  '.pill{background:#2a2a2a;padding:.1rem .5rem;border-radius:99px;font-size:.7rem}'+
  'pre{background:#181818;border:1px solid #2a2a2a;border-radius:6px;padding:.8rem;font-size:.72rem;overflow:auto;color:#bbb}'+
  'h1{font-size:1.1rem}a{color:#7cc0ff}</style></head><body>'+
  '<h1>'+escH(runId)+'</h1><p><a href="#" onclick="window.close()">close tab</a></p>'+
  '<h2>Timeline ('+evs.length+' events)</h2><table><thead><tr><th>time</th><th>stage</th><th>message</th></tr></thead><tbody>'+(rows||'<tr><td colspan=3>none</td></tr>')+'</tbody></table>'+
  '<h2>Raw summary</h2>'+raw+
  '</body></html>');
  w.document.close();
}
function escH(s){return String(s==null?'':s).replace(/[&<>"]/g,function(c){return{'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c];});}
</script>
</body></html>`)
	return sb.String()
}

// WriteDashboardResult reports what WriteDashboard produced.
type WriteDashboardResult struct {
	Path   string
	Runs   int
	Boards int
}

// WriteDashboard renders dashboard.html into homeDir from
// homeDir/runs + the given board dirs.
func WriteDashboard(homeDir string, boardDirs []string) WriteDashboardResult {
	summaries := CollectRunSummaries(filepath.Join(homeDir, "runs"))
	boards := LoadBoards(boardDirs)
	html := RenderDashboardHTML(summaries, "DevAgent Runs", boards)
	path := filepath.Join(homeDir, "dashboard.html")
	_ = os.WriteFile(path, []byte(html), 0o644)
	return WriteDashboardResult{Path: path, Runs: len(summaries), Boards: len(boards)}
}
