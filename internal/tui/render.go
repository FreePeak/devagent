package tui

import (
	"strings"

	versionpkg "github.com/FreePeak/devagent/internal/version"
)

// version matches internal/version.Version (release stamping binds it via
// -ldflags; a hand-rolled const went stale the moment release binaries
// reported a stamped tag in this overlay).
var version = versionpkg.Version

// Dashboard renderers: header strip, worker/session cards, history tail, log
// view, overlays and the frame fitting. Port of src/tui/tui.ts render half —
// every literal is byte-identical to the TS renderer.

// PadTo pads to n visible columns — ANSI color codes must not count toward
// width.
func PadTo(s string, n int) string {
	pad := n - VisibleLen(s)
	if pad < 1 {
		pad = 1
	}
	return s + strings.Repeat(" ", pad)
}

// BoxLines renders rounded-box panel lines for visible width w, CloddsBot
// box() style: the title sits CENTERED in the top rule (╭──── Title ────╮)
// and the whole frame draws in the Border color (CloddsBot's cyan-bordered
// boxes). VisibleLen measures title+body so ANSI colors never skew borders.
func BoxLines(title string, body []string, w int) string {
	tl := VisibleLen(title)
	gap := w - tl - 4 // 2 corners + one space each side of the title
	left := gap / 2
	if left < 1 {
		left = 1 // keep the box top starting with ╭─ at any width
	}
	right := gap - left
	if right < 0 {
		right = 0
	}
	var out []string
	head := Border + "╭" + strings.Repeat("─", left) + Reset + " " + title + " " +
		Border + strings.Repeat("─", right) + "╮" + Reset
	out = append(out, head)
	for _, b := range body {
		out = append(out, PadTo(b+" ", w-1)+Border+"│"+Reset)
	}
	foot := Border + "╰" + strings.Repeat("─", maxInt(1, w-2)) + "╯" + Reset
	return strings.Join(append(out, foot), "\n")
}

func boxLinesLines(title string, body []string, w int) []string {
	return strings.Split(BoxLines(title, body, w), "\n")
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// paneCardLines renders a boxed worker card: title bar + status/cwd body.
func paneCardLines(p TuiPane, inner int, selected bool) []string {
	id := p.TaskID
	if id == "" {
		id = p.Label
	}
	if id == "" {
		id = "?"
	}
	el := ""
	if p.StartedAt != "" {
		el = fmtElapsed(p.StartedAt, nowClock())
	}
	worker := p.Worker
	if worker == "" {
		worker = "-"
	}
	body := []string{
		" " + ChipFor(p.State, orDefault(p.AgentStatus, p.State)) +
			" " + orEmpty(el != "", Dim+"· "+el+Reset) + " " +
			Dim + Truncate(worker, 12) + Reset,
		" " + Dim + "cwd " + Truncate(p.Cwd, maxInt(10, inner-8)) + Reset,
		" " + Cyan + "devagent attach " + Truncate(id, maxInt(8, inner-18)) + Reset,
	}
	title := Truncate(id, inner-6)
	if selected {
		title = Cyan + "▸" + Reset + " " + Truncate(id, inner-8)
	}
	return boxLinesLines(title, body, inner)
}

// queuedCardLines renders a boxed queued-task card.
func queuedCardLines(t TuiQueuedTask, inner int, selected bool) []string {
	body := []string{
		" " + ChipFor("queued", "queued") + "  " + Dim + Truncate(t.Title, maxInt(10, inner-14)) + Reset,
		" " + Dim + "waiting for a worker claim" + Reset,
	}
	title := Truncate(orDefault(t.ID, "?"), inner-6)
	if selected {
		title = Cyan + "▸" + Reset + " " + Truncate(orDefault(t.ID, "?"), inner-8)
	}
	return boxLinesLines(title, body, inner)
}

func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

func orEmpty(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

// headerLines renders the header strip + metrics line + iteration card.
func headerLines(snap *Snapshot, ropts RenderOptions) []string {
	width := ropts.Width
	if width == 0 {
		width = DefaultColumns
	}
	status := snap.Status
	if snap.AuthFailed {
		return []string{
			Bold + Red + " DevAgent — DAEMON AUTH REJECTED " + Reset +
				Dim + " token invalid (DEVAGENT_DAEMON_TOKEN / daemon-token file)" + Reset,
			"",
		}
	}
	if !snap.Reachable || status == nil {
		return []string{
			Bold + Red + " DevAgent — DAEMON UNREACHABLE " + Reset +
				Dim + " retrying every 2s · start the daemon" + Reset,
			"",
		}
	}
	panes := rosterPanes(snap)
	agg := AggregateStatus(status, panes)
	var pending, claimed, done float64
	if status.Queue != nil {
		pending = orNum(status.Queue.Pending)
		claimed = orNum(status.Queue.Claimed)
		done = orNum(status.Queue.Done)
	}
	// Spinner (CloddsBot dots cue) animates only while work is live. Steel =
	// the running color (FR-TUI-P-09).
	spin := ""
	if agg == "RUNNING" {
		spin = Steel + spinnerAt(ropts.SpinnerFrame) + Reset + " "
	}
	chip := StatusColor(agg) + "● " + agg + Reset
	// CloddsBot title composition: bold name + dim tagline, then the state
	// chip. Narrow terminals drop the tagline (the bar must never clamp).
	barBody := " DevAgent " + Dim + "· autonomous backend delivery agent" + Reset + "  " + spin + chip
	if VisibleLen(barBody)+2 > width {
		barBody = " DevAgent  " + spin + chip
	}
	// Embedded-daemon cue in the title bar (cyan = "the TUI started this one
	// for you") — the bar is short, so the marker survives narrow terminals.
	if ropts.DaemonMode == "embedded" {
		barBody += "  " + Cyan + "· daemon:embedded" + Reset
	}
	// SGR is not a stack: every Reset inside the body (tagline, spinner,
	// chip, embedded marker) would end the Bold+Inverse highlight early and
	// leave the bar plain from there on. Re-assert the bar attributes after
	// each internal Reset so the inverse strip runs edge to edge.
	barBody = strings.ReplaceAll(barBody, Reset, Reset+Bold+Inverse)
	// Metric strip (FR-TUI-P-06): ONE dense row — queue meter p/c/d, live
	// runs, activity sparkline. Uptime/herdr/vis demoted to the dim suffix;
	// circuit is highlighted only when not closed. Header stays ≤2 lines
	// before the hero card.
	openTasks := pending + claimed
	total := openTasks + done
	meter := Dim + "[" + Reset + MeterBar(openTasks, total, 10, Yellow+"█"+Reset, Dim+"░"+Reset) + Dim + "]" + Reset
	var samples []float64
	sampleMs := float64(PollMs)
	if ropts.Metrics != nil {
		samples = ropts.Metrics.Samples
		if ropts.Metrics.SampleMs > 0 {
			sampleMs = ropts.Metrics.SampleMs
		}
	}
	// Size the spark to the terminal: reserve room for the static prefix +
	// herdr/vis tail so the row never overflows into the clamp ellipsis.
	sparkBudget := 0
	if width >= 100 {
		sparkBudget = maxInt(8, minInt(24, width-108))
	}
	var sparkTail []float64
	if sparkBudget > 0 && len(samples) > sparkBudget {
		sparkTail = samples[len(samples)-sparkBudget:]
	} else if sparkBudget > 0 {
		sparkTail = samples
	}
	spark := ""
	if len(samples) > 0 && len(sparkTail) > 0 {
		uptime := fmtUptime(f64(len(samples) * int(sampleMs) / 1000))
		last := 0.0
		if len(samples) > 0 {
			last = samples[len(samples)-1]
		}
		spark = " · " + Dim + "activity(" + uptime + ") " + Cyan + Sparkline(sparkTail) + Reset +
			Dim + " " + jsNum(last) + Reset
	}
	circuit := ""
	if status.Circuit != "" && status.Circuit != "closed" {
		color := Yellow
		if status.Circuit == "open" {
			color = Red
		}
		circuit = " · " + color + "circuit:" + status.Circuit + Reset
	}
	strip := Dim + " queue " + meter + " " + jsNum(pending) + "p/" +
		jsNum(claimed) + "c/" + jsNum(done) + "d" + Reset +
		Dim + " · runs " + Reset + liveRunsField(status, panes) + spark + circuit +
		Dim + " · up " + fmtUptime(status.UptimeS) +
		" · herdr:" + herdrSession(status) + Reset
	if width >= 118 {
		strip += Dim + " · vis:" + spawnVisibility(status) + Reset
	}
	return append([]string{
		Bold + Inverse + PadTo(barBody, maxInt(width, VisibleLen(barBody)+1)) + Reset,
		strip,
		"",
	}, heroLines(snap, ropts, panes)...)
}

func orNum(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func f64(n int) *float64 {
	v := float64(n)
	return &v
}

func herdrSession(status *StatusPayload) string {
	if status.Herdr != nil && status.Herdr.Session != "" {
		return status.Herdr.Session
	}
	return "-"
}

func liveRunsField(status *StatusPayload, panes []TuiPane) string {
	// The daemon's runs.active (lock-derived, holder-liveness checked) is the
	// truthful live count: verified on the real surface 2026-09-11 — a task
	// was mid-flight (runs.active 1) while the pane-derived count painted
	// "0a", because workers are not herdr panes (issue #288/#317 lineage).
	// Pane state stays as the fallback for payloads without a runs block.
	active := 0.0
	if status.Runs != nil {
		active = orNum(status.Runs.Active)
	} else {
		for _, p := range panes {
			if p.State == "running" {
				active++
			}
		}
	}
	var failed float64
	if status.Runs != nil {
		// FR-TUI-P-03: failed_recent is a historical count — it must never
		// paint the aggregate FAILED (only circuit=="open" does). Render it
		// as a dim amber "Nf recent" suffix beside the live active count.
		failed = orNum(status.Runs.FailedRecent)
	}
	if failed > 0 {
		return jsNum(float64(active)) + "a " + Dim + "/ " + Reset + Yellow +
			jsNum(failed) + "f recent" + Reset
	}
	return jsNum(active) + "a"
}

// heroLines renders the FR-TUI-P-05 hero: one focused line for the running
// work — the selected (or first) running pane with phase, elapsed, worker
// and an indeterminate pulse bar (no fabricated percent: the snapshot
// carries no fraction field) — or a next-action cue when idle. Folded with
// iterationLines so the loop-phase row stays visible.
func heroLines(snap *Snapshot, ropts RenderOptions, panes []TuiPane) []string {
	var sel *TuiPane
	// Prefer the operator's selection if it is running; else first running.
	if ropts.Selection >= 0 && ropts.Selection < len(panes) &&
		panes[ropts.Selection].State == "running" {
		sel = &panes[ropts.Selection]
	}
	if sel == nil {
		for i := range panes {
			if panes[i].State == "running" {
				sel = &panes[i]
				break
			}
		}
	}
	// PAUSED banner (FR-TUI attention): a paused 'ask' gate needs the
	// operator's answer — the idle/running hero must never bury it. Amber
	// + the exact key, so the next action is one glance away (the
	// opencode/crush approval-attention convention).
	paused := ""
	if snap.Status != nil && snap.Status.Ask != nil && snap.Status.Ask.ID != "" {
		id := Truncate(snap.Status.Ask.ID, 24)
		paused = "  " + Yellow + Bold + "⏸ " + Reset + Yellow + "task " + id +
			" paused — [g] answer · [k] kill" + Reset
	}
	if sel == nil {
		// Idle: paused cue first, else one next-action line. "Idle" must not
		// be painted while the daemon reports live runs (pane-rostered or
		// not — workers currently bypass herdr, so the roster is empty even
		// mid-task): the count is the truth, the next-action cue is not.
		if paused != "" {
			return append(iterationLines(snap), paused, "")
		}
		if snap.Status != nil && snap.Status.Runs != nil && orNum(snap.Status.Runs.Active) > 0 {
			return append(iterationLines(snap),
				"  "+Steel+"● "+Reset+jsNum(orNum(snap.Status.Runs.Active))+
					" run(s) in flight "+Dim+"· pane roster empty (workers are not herdr panes)"+Reset, "")
		}
		next := "n goal · 1 workers · ? help"
		switch ropts.View {
		case ViewLog:
			next = "f follow · ? help"
		case ViewWorkers, ViewSessions:
			if ropts.Selection >= 0 && ropts.Selection < len(panes) {
				next = "⏎ detail · a attach · k kill · ? help"
			}
		}
		return append(iterationLines(snap),
			"  "+Dim+"▸ next: "+next+Reset, "")
	}
	// Running: id · phase · elapsed · worker + indeterminate pulse bar.
	id := sel.TaskID
	if id == "" {
		id = sel.Label
	}
	el := ""
	if sel.StartedAt != "" {
		el = fmtElapsed(sel.StartedAt, nowClock())
	}
	phase := orDefault(sel.AgentStatus, sel.State)
	line := "  " + Steel + "● " + Truncate(id, 28) + Reset + "  " +
		ChipFor(sel.State, phase)
	if el != "" {
		line += Dim + " · " + el + Reset
	}
	// Indeterminate pulse: a short amber segment traveling a dim track,
	// driven by the spinner frame — no fake percentage.
	track := maxInt(12, minInt(24, ropts.Width/4))
	seg := 3
	pos := 0
	if track > 0 {
		pos = ropts.SpinnerFrame % track
	}
	bar := ""
	for i := range track {
		rel := (i - pos + track) % track
		if rel < seg {
			bar += Yellow + "█" + Reset
		} else {
			bar += Dim + "▒" + Reset
		}
	}
	worker := sel.Worker
	if worker == "" {
		worker = "-"
	}
	hero := []string{
		line,
		"  " + Dim + "worker " + Reset + Truncate(worker, 14) +
			Dim + " · pulse " + Reset + bar,
	}
	if paused != "" {
		hero = append([]string{paused}, hero...)
	}
	return append(iterationLines(snap), hero...)
}

func spawnVisibility(status *StatusPayload) string {
	if status.Spawn != nil && status.Spawn.Visibility != "" {
		return status.Spawn.Visibility
	}
	return "visible"
}

// iterationLines renders the current loop progress (human jump-in cue,
// PR #140): iteration + phase. The driver heartbeat on /status is the
// source of truth (FR-VAL-03 #291d — a heartbeat present while the ledger
// tail lags is exactly the staleness defect it closes); the newest
// loop-phase history row is the fallback for daemons that predate it.
func iterationLines(snap *Snapshot) []string {
	if snap.Status != nil && snap.Status.Loop != nil && snap.Status.Loop.Phase != "" {
		return []string{
			Dim + "iteration " + jsNum(orNum(snap.Status.Loop.Iteration)) + " · phase: " +
				Reset + Cyan + snap.Status.Loop.Phase + Reset,
			"",
		}
	}
	var latest HistoryRow
	for _, r := range snap.History {
		if ev, ok := r["event"].(string); ok && ev == "loop-phase" {
			latest = r
		}
	}
	phase, ok := latest["phase"].(string)
	if !ok || latest == nil {
		return nil
	}
	det := ""
	if d, ok := latest["detail"].(string); ok && d != "" {
		det = " — " + d
	}
	loop := "?"
	if l, ok := latest["loop"].(float64); ok {
		loop = jsNum(l)
	}
	return []string{
		Dim + "iteration " + loop + " · phase: " + Reset + Cyan + phase + Reset + Dim + det + Reset,
		"",
	}
}

func helpLines() []string {
	return []string{
		Bold + "Keys" + Reset,
		"",
		Dim + "  — views —" + Reset,
		"  1 / 2 / 3  switch view: workers / sessions / live log   (s and l toggle back)",
		"",
		Dim + "  — act —" + Reset,
		"  n          dispatch sheet (FR-HAND-02): type a goal (Ctrl+N new line), Enter → POST /dispatch",
		"  g          answer a paused task (FR-HAND-07): y/n or free text → POST /approve",
		"             (no paused task: g jumps to the first item)",
		"  k  kill the running task via POST /approve (answer __kill__); daemon must advertise kill-via-answer",
		"  y  confirm the pending kill — any other key cancels",
		"  a         attach inline (FR-TUI-06): dashboard suspends, herdr owns the terminal; detach to return",
		"  u         upgrade hint (pilot-style self-update recipe)",
		"  r  refresh now",
		"",
		Dim + "  — move —" + Reset,
		"  ↑ ↓ / PgUp PgDn  move the selection (workers, sessions) · scroll (log)",
		"  g / G      jump to first / last item (log: oldest / newest)",
		"  Enter / o  expand the selected worker into a detail panel",
		"",
		Dim + "  — live log —" + Reset,
		"  /          search the log (case-insensitive filter; Enter applies, Esc cancels)",
		"  n / N      next / previous match while a search is active",
		"  f         toggle follow-tail in the log view",
		"",
		Dim + "  — general —" + Reset,
		"  ?  toggle this help",
		"  q or Ctrl+C  quit",
		"",
	}
}

// logViewLines renders the log view: dense structured tail (Claude Code
// transcript feel).
func logViewLines(ropts RenderOptions, width, bodyBudget int) []string {
	log := ropts.Log
	var titleState string
	if log == nil || log.State == "off" {
		titleState = DimText("● tail off")
	} else if log.State == "live" {
		titleState = Green + "● live" + Reset
	} else if log.State == "down" {
		titleState = Yellow + "● reconnecting…" + Reset
	} else {
		titleState = DimText("● connecting…")
	}
	src := ""
	if log != nil && log.Source != "" {
		src = DimText(" · run " + Truncate(log.Source, 8))
	}
	pos := DimText("  [following tail]")
	if log != nil && log.Scroll > 0 {
		pos = DimText("  [" + itoa(log.Scroll) + " older ↑ · f to follow]")
	}
	query := ""
	if log != nil && log.SearchMode {
		// The / prompt: live draft with an inverse-video cursor cell.
		query = "  " + Cyan + "/" + Truncate(log.SearchDraft, 24) + Reset + Inverse + " " + Reset
	} else if log != nil && log.Search != "" {
		query = DimText("  /" + Truncate(log.Search, 24) + " · n/N matches · Esc clear")
	}
	count := 0
	if log != nil {
		count = len(log.Lines)
	}
	shown := ""
	if log != nil && log.Search != "" {
		shown = " · " + itoa(logLinesMatching(log)) + " match(es)"
	}
	lines := []string{
		Bold + "▌Live log" + Reset + " " + titleState +
			DimText(" · "+itoa(count)+" line(s) buffered") + src + shown + pos + query,
		"",
	}
	if log == nil || len(log.Lines) == 0 {
		lines = append(lines, DimText("  no events yet — waiting for worker / daemon run-log output"))
		return lines
	}
	// Chrome inside the body: the title + blank above the rows. The title
	// must never be cut by fitting, so the viewport derives from the body
	// budget the caller computed (rows - page header - footer), not its own
	// guess.
	viewport := maxInt(3, bodyBudget-2)
	visible := logVisibleLines(log)
	start := len(visible) - viewport
	if !log.Follow {
		start -= log.Scroll
	}
	if start < 0 {
		start = 0
	}
	end := minInt(start+viewport, len(visible))
	for _, l := range visible[start:end] {
		lines = append(lines, FormatLogLine(l, width, Reset))
	}
	return lines
}

// logVisibleLines is the viewport's line source: every buffered line when
// unfiltered, only matches when a / filter is active.
func logVisibleLines(log *LogViewState) []LogLine {
	if log == nil || log.Search == "" {
		return log.Lines
	}
	out := make([]LogLine, 0, len(log.Lines))
	for _, l := range log.Lines {
		if logLineMatches(l, log.Search) {
			out = append(out, l)
		}
	}
	return out
}

// logLinesMatching counts filter hits (title indicator).
func logLinesMatching(log *LogViewState) int {
	return len(logVisibleLines(log))
}

// logLineMatches is the case-insensitive match over the rendered essence:
// timestamp + level + stage + message.
func logLineMatches(l LogLine, query string) bool {
	q := strings.ToLower(query)
	return strings.Contains(strings.ToLower(l.Message), q) ||
		strings.Contains(strings.ToLower(l.Stage), q) ||
		strings.Contains(strings.ToLower(l.Level), q) ||
		strings.Contains(strings.ToLower(l.RunID), q)
}

// detailOverlayLines renders the detail panel (Claude Code expand):
// everything known about one item.
func detailOverlayLines(item any, width int) []string {
	inner := maxInt(30, width-4)
	var body []string
	var titleID, title string
	switch it := item.(type) {
	case TuiPane:
		role := orDefault(it.Role, "-")
		worker := orDefault(it.Worker, "-")
		paneID := orDefault(it.PaneID, "-")
		wsID := orDefault(it.WorkspaceID, "-")
		up := ""
		if it.StartedAt != "" {
			up = fmtElapsed(it.StartedAt, nowClock())
		}
		since := orDefault(it.StartedAt, "-")
		id := orDefault(orDefault(it.TaskID, it.Label), "?")
		titleID = id
		body = []string{
			" " + ChipFor(it.State, orDefault(it.AgentStatus, it.State)) +
				"  " + Dim + "role " + role + " · engine " + worker + Reset,
			" " + Dim + "pane " + paneID + " · workspace " + wsID + Reset,
			" " + Dim + "up " + orDefault(up, "-") + " · since " + Truncate(since, inner-16) + Reset,
			" " + Dim + "cwd " + Truncate(it.Cwd, inner-6) + Reset,
			"",
			" " + Cyan + "devagent attach " + Truncate(id, maxInt(8, inner-18)) + Reset,
			DimText(" jump into this worker pane and steer it live"),
		}
	case TuiQueuedTask:
		titleID = orDefault(it.ID, "?")
		body = []string{
			" " + ChipFor("queued", "queued") + "  " + Dim + orDefault(it.Status, "pending") + Reset,
			" " + Dim + "created " + Truncate(orDefault(it.CreatedAt, "-"), inner-10) + Reset,
			"",
			" " + Truncate(it.Title, inner-2),
			DimText(" waiting for a worker claim"),
		}
	}
	title = Cyan + "▸" + Reset + " " + Truncate(titleID, inner-8)
	return boxLinesLines(title, body, inner)
}

// goalInputRows is the fixed input-viewport height: a slice of the terminal
// row budget (never content-sized, so the box does not jump while typing).
func goalInputRows(rows int) int {
	if rows <= 0 {
		rows = 100
	}
	n := rows / 6
	if n < 3 {
		n = 3
	}
	if n > 10 {
		n = 10
	}
	return n
}

// goalInputLines renders a fixed-height, multi-line input viewport: the text
// is split on newlines and the TAIL is shown (the cursor lives at the end), so
// growing the goal scrolls the box instead of resizing it. The first visible
// row carries the " > " prompt, continuations are indented, and the cursor
// cell sits on the last visible row.
func goalInputLines(text string, width, inputRows int) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > inputRows {
		lines = lines[len(lines)-inputRows:]
	}
	out := make([]string, 0, inputRows)
	for i := range inputRows {
		if i >= len(lines) {
			out = append(out, "")
			continue
		}
		prefix := "   "
		if i == 0 {
			prefix = " > "
		}
		line := Truncate(lines[i], maxInt(1, width-4))
		if i == len(lines)-1 {
			// Cursor cell on the line being typed.
			out = append(out, " "+prefix+line+Inverse+" "+Reset)
			continue
		}
		out = append(out, " "+prefix+line)
	}
	return out
}

// dispatchOverlayLines renders the dispatch sheet (FR-HAND-02 / FR-TUI-04): a
// multi-line goal input whose box is a fixed slice of the terminal (rows and
// width both derived from the window, never from the typed text). Defaults are
// the configured worker and the daemon's repo — the sheet asks nothing else, so
// a goal typed here is enough to start work (1+1 bar).
func dispatchOverlayLines(overlay *Overlay, width, rows int) []string {
	inner := maxInt(34, width-4)
	inputRows := goalInputRows(rows)
	body := []string{
		" " + Bold + "New goal" + Reset + " " + Dim + "(worker + repo come from your config)" + Reset,
		"",
	}
	body = append(body, goalInputLines(overlay.Input, inner, inputRows)...)
	body = append(body,
		"",
		" " + Cyan + "Enter" + Reset + Dim + " dispatch · " + Reset + Cyan + "Ctrl+N" + Reset + Dim + "/" + Reset + Cyan + "Alt+Enter" + Reset + Dim + " new line · Esc cancel" + Reset,
	)
	return boxLinesLines("Dispatch", body, inner)
}

// approveOverlayLines renders the approve sheet (FR-HAND-07): answer a
// paused 'ask' task. `y`/`n` submit approve/deny words; any other typing is
// a free-text answer for the worker (also multi-line, same fixed box).
func approveOverlayLines(overlay *Overlay, width, rows int) []string {
	inner := maxInt(34, width-4)
	inputRows := goalInputRows(rows)
	body := []string{
		" " + Dim + "task " + Reset + Truncate(orDefault(overlay.TaskID, "?"), 24),
		"",
	}
	body = append(body, goalInputLines(overlay.Input, inner, inputRows)...)
	body = append(body,
		"",
		" " + Cyan + "y" + Reset + Dim + "/Enter answer · " + Reset + Cyan + "Ctrl+N" + Reset + Dim + " new line · Esc cancel" + Reset,
	)
	return boxLinesLines("Answer task", body, inner)
}

// upgradeOverlayLines renders Pilot's `u` recipe (FR-TUI-05):
// self-hosted upgrade/rollback hint.
func upgradeOverlayLines(width int) []string {
	inner := maxInt(34, width-4)
	body := []string{
		" " + Bold + "devagent v" + version + Reset + " " + Dim + "(self-hosted checkout)" + Reset,
		"",
		" " + Dim + "upgrade — clean worktree only:" + Reset,
		"   " + Cyan + "git pull --ff-only" + Reset,
		"   " + Cyan + "make build" + Reset,
		"",
		" " + Dim + "rollback:" + Reset,
		"   " + Cyan + "git checkout <previous-commit> && make build" + Reset,
		"",
		" " + Dim + "the daemon dispatches its own binary — rebuild, then restart" + Reset,
		" " + Dim + "devagent tui so new tasks run the fresh build" + Reset,
	}
	return boxLinesLines("Upgrade", body, inner)
}

// fitLines trims so header+body+footer fit rows (htop always fits). Never
// emits more than rows lines total: a frame taller than the terminal scrolls
// the alternate screen and desyncs the incremental diff (pressing `?` on a
// short terminal garbled the whole dashboard). The header's tail is cut
// first — help lines are appended last — and at least one body row always
// survives.
func fitLines(header, body, footer []string, rows int, keep string) []string {
	head := header
	if len(header)+len(footer)+1 > rows {
		cut := maxInt(1, rows-len(footer)-1)
		if cut > len(header) {
			cut = len(header)
		}
		head = header[:cut]
	}
	budget := rows - len(head) - len(footer)
	if budget >= len(body) {
		return concat(head, body, footer)
	}
	cut := maxInt(1, budget)
	var trimmed []string
	if keep == "top" {
		trimmed = body[:minInt(cut, len(body))]
	} else {
		trimmed = body[maxInt(0, len(body)-cut):]
	}
	return concat(head, trimmed, footer)
}

func concat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// footerHint builds the htop function bar, width-tiered (FR-TUI-P-12): the
// hint and any transient note share ONE footer line, so each tier only
// renders when it fits its threshold — [q] quit is never clamped off (the
// full 116-cell hint used to lose its tail at the default 100 columns).
func footerHint(view View, width int) string {
	if width >= 116 {
		if view == ViewLog {
			return Inverse + " [1] workers [2] sessions [3] log · ↑↓ scroll · f follow · r refresh [?] help [q] quit " + Reset
		}
		return Inverse + " [n] goal [1] workers [2] sessions [3] log · ↑↓ select · ⏎ detail · a attach · k kill · r refresh [?] help [q] quit " + Reset
	}
	if width >= 96 {
		if view == ViewLog {
			return Inverse + " [1/2/3] views · ↑↓ scroll · f follow [?] help [q] quit " + Reset
		}
		return Inverse + " [n] goal [1] workers [2] sessions [3] log · ↑↓ · ⏎ detail · a attach · k kill [?] help [q] quit " + Reset
	}
	if width >= 62 {
		if view == ViewLog {
			return Inverse + " [1/2/3] views · f follow [?] help [q] quit " + Reset
		}
		return Inverse + " [n] goal [1/2/3] views · a attach · k kill [?] help [q] quit " + Reset
	}
	return Inverse + " [?] help [q] quit " + Reset
}

// RenderLines renders the full frame as lines (interactive diffs these;
// one-shot joins them).
func RenderLines(snap *Snapshot, ropts RenderOptions) []string {
	width := ropts.Width
	if width == 0 {
		width = DefaultColumns
	}
	rows := ropts.Rows
	if rows == 0 {
		rows = DefaultRows
	}
	view := ropts.View
	if ropts.ShowSessions && view == ViewWorkers {
		view = ViewSessions
	}
	panes := rosterPanes(snap)
	queued := queueRows(snap)

	header := headerLines(snap, ropts)
	// FR-TUI-P-12: the help overlay is kept bottom-first so its last row
	// ("q or Ctrl+C  quit") always survives a short terminal — cutting the
	// header tail instead would drop exactly the quit row the operator
	// needs.
	help := []string{}
	if ropts.ShowHelp {
		help = helpLines()
	}

	// Footer (htop function-bar cue): contextual keys + transient notes.
	var notes []string
	if ropts.PendingKill != "" {
		notes = append(notes, "kill "+ropts.PendingKill+": y confirm · other key cancels")
	}
	if ropts.Note != "" {
		notes = append(notes, ropts.Note)
	}
	keysHint := footerHint(view, width)
	noteSuffix := ""
	if len(notes) > 0 {
		noteSuffix = "  " + Yellow + Truncate(strings.Join(notes, " · "), maxInt(16, width-VisibleLen(keysHint)-6)) + Reset
	}
	footer := []string{keysHint + noteSuffix}
	if ropts.ShowHelp {
		return fitLines(header, append(help, ""), footer, rows, "bottom")
	}
	if ropts.Overlay != nil && ropts.Overlay.Kind == "dispatch" {
		return fitLines(header, append(dispatchOverlayLines(ropts.Overlay, width, rows), ""), footer, rows, "top")
	}
	if ropts.Overlay != nil && ropts.Overlay.Kind == "approve" {
		return fitLines(header, append(approveOverlayLines(ropts.Overlay, width, rows), ""), footer, rows, "top")
	}

	if ropts.Overlay != nil && ropts.Overlay.Kind == "upgrade" {
		return fitLines(header, append(upgradeOverlayLines(width), ""), footer, rows, "top")
	}
	if ropts.Overlay != nil && ropts.Overlay.Kind == "detail" && (ropts.Overlay.Pane != nil || ropts.Overlay.Queued != nil) {
		var item any
		if ropts.Overlay.Pane != nil {
			item = *ropts.Overlay.Pane
		} else {
			item = *ropts.Overlay.Queued
		}
		return fitLines(header, append(detailOverlayLines(item, width), ""), footer, rows, "top")
	}

	if view == ViewLog {
		bodyBudget := rows - len(header) - len(footer)
		return fitLines(header, logViewLines(ropts, width, bodyBudget), footer, rows, "bottom")
	}

	if view == ViewSessions {
		// Viewport follows the selection (the lazygit/htop rule): with more
		// panes than body rows the cursor must never slide off-screen, and
		// the cut must say so on the title line.
		capRows := rows - len(header) - len(footer) - 2 // title + blank
		if capRows < 1 {
			capRows = 1
		}
		title := Bold + "▌Sessions" + Reset + " " + Dim + "herdr panes" + Reset
		start := 0
		if len(panes) > capRows {
			if ropts.Selection >= capRows {
				start = minInt(ropts.Selection-capRows+1, len(panes)-capRows)
			}
			if start > 0 {
				title += Dim + " · ↑" + itoa(start) + " hidden" + Reset
			}
			if below := len(panes) - start - capRows; below > 0 {
				title += Dim + " · ↓" + itoa(below) + " hidden" + Reset
			}
		}
		body := []string{title, ""}
		window := panes
		if len(panes) > capRows {
			window = panes[start:minInt(start+capRows, len(panes))]
		}
		if len(window) == 0 {
			body = append(body, DimText("  no live sessions"))
		}
		for i, p := range window {
			el := ""
			if p.StartedAt != "" {
				el = fmtElapsed(p.StartedAt, nowClock())
			}
			mark := " "
			if i+start == ropts.Selection {
				mark = Cyan + "▸" + Reset
			}
			line := mark + " " + Cyan + Truncate(orDefault(p.PaneID, "-"), 18) + Reset + "  " +
				Bold + Truncate(orDefault(p.TaskID, "?"), 24) + Reset + "  " +
				ChipFor(p.State, orDefault(p.AgentStatus, p.State))
			if el != "" {
				line += " " + Dim + "· " + el + Reset
			}
			line += "  " + Dim + Truncate(p.Cwd, maxInt(20, width-74)) + Reset
			body = append(body, line)
		}
		return fitLines(header, body, footer, rows, "top")
	}

	// Workers view: cards + history tail. FR-TUI-P-08: below 80 columns the
	// 2-up cards would crush cwd/attach lines — stack them full-width.
	stackFull := width < 80
	half := maxInt(34, width/2)
	if stackFull {
		half = width
	}
	body := []string{Bold + "▌Workers" + Reset + " " + Dim + itoa(len(panes)) + " pane(s) · " +
		itoa(len(queued)) + " queued" + Reset, ""}
	var cards [][]string
	for i, p := range panes {
		cards = append(cards, paneCardLines(p, half-2, i == ropts.Selection))
	}
	for i, t := range queued {
		cards = append(cards, queuedCardLines(t, half-2, len(panes)+i == ropts.Selection))
	}
	if len(cards) == 0 {
		body = append(body, DimText("  no workers, queue empty"))
	}
	// Viewport follows the selection: each card costs its own height (+1
	// separator), so a roster taller than the body budget windows around
	// the selected card instead of letting fitLines silently cut it.
	cardCap := 4
	if budget := rows - len(header) - len(footer) - 4; budget > cardCap {
		cardCap = budget
	}
	cardStart, cardEnd := 0, len(cards)
	if len(cards) > cardCap {
		heights := make([]int, len(cards))
		for i, c := range cards {
			heights[i] = len(c) + 1
		}
		sel := ropts.Selection
		if sel < 0 || sel >= len(cards) {
			sel = 0
		}
		// Grow from the selected card until the budget is spent.
		lo, hi := sel, sel
		used := heights[sel]
		for used < cardCap {
			grew := false
			if lo > 0 {
				lo--
				used += heights[lo]
				grew = true
				if used >= cardCap {
					break
				}
			}
			if hi < len(cards)-1 {
				hi++
				used += heights[hi]
				grew = true
				if used >= cardCap {
					break
				}
			}
			if !grew {
				break
			}
		}
		cardStart, cardEnd = lo, hi+1
	}
	step := 2
	if stackFull {
		step = 1 // full-width stacked cards: no side-by-side pairing
	}
	for i := cardStart; i < cardEnd; i += step {
		a := cards[i]
		var b []string
		if step == 2 && i+1 < cardEnd {
			b = cards[i+1]
		}
		rws := maxInt(len(a), len(b))
		for r := range rws {
			left := ""
			if r < len(a) {
				left = a[r]
			}
			right := ""
			if b != nil && r < len(b) {
				right = b[r]
			}
			body = append(body, PadTo(left, half)+right)
		}
		body = append(body, "")
	}
	if cardEnd > cardStart {
		body = body[:len(body)-1] // single blank between cards and history
	}
	// Hidden-card indicators on the Workers title (visible == count).
	if cardStart > 0 || cardEnd < len(cards) {
		hidden := ""
		if cardStart > 0 {
			hidden += " · ↑" + itoa(cardStart) + " hidden"
		}
		if below := len(cards) - cardEnd; below > 0 {
			hidden += " · ↓" + itoa(below) + " hidden"
		}
		body[0] = Bold + "▌Workers" + Reset + " " + Dim + itoa(len(panes)) + " pane(s) · " +
			itoa(len(queued)) + " queued" + hidden + Reset
	}

	body = append(body, Bold+"▌History"+Reset+" "+Dim+"ledger tail"+Reset, "")
	history := snap.History
	if len(history) > HistoryRows {
		history = history[len(history)-HistoryRows:]
	}
	if len(history) == 0 {
		body = append(body, DimText("  no ledger rows"))
	} else {
		for _, row := range history {
			// Row shapes vary by producer: loop-result rows carry {event, loop,
			// status, goal}; watchdog-health rows carry {taskId, watchdogFired};
			// audit rows carry {taskId, verdict}. Columns: clock, kind, taskId
			// (loop-result rows show their loop number instead), short verdict,
			// goal prose — taskId must win over prose so every row is
			// identifiable.
			ev := historyEvent(row)
			task := historyTask(row)
			statusTxt := historyStatus(row, ev)
			goal := ""
			if g, ok := row["goal"].(string); ok {
				goal = g
			}
			goalW := minInt(60, maxInt(30, width-66))
			statusCell := "        "
			if statusTxt != "" {
				statusCell = StatusColor(statusTxt) + Truncate(statusTxt, 8) + Reset
			}
			body = append(body, "  "+Dim+fmtClock(row["ts"])+Reset+"  "+
				Cyan+Truncate(ev, 18)+Reset+"  "+Truncate(task, 18)+"  "+
				statusCell+"  "+Truncate(goal, goalW))
		}
	}
	return fitLines(header, body, footer, rows, "top")
}

func historyEvent(row HistoryRow) string {
	if ev, ok := row["event"].(string); ok && ev != "" {
		return ev
	}
	if st, ok := row["status"].(string); ok && st != "" {
		return st
	}
	if k, ok := row["kind"].(string); ok {
		return k
	}
	return ""
}

func historyTask(row HistoryRow) string {
	if id, ok := row["taskId"].(string); ok && id != "" {
		return id
	}
	if loop, ok := row["loop"].(float64); ok {
		return "loop:" + jsNum(loop)
	}
	return ""
}

func historyStatus(row HistoryRow, ev string) string {
	if st, ok := row["status"].(string); ok && st != "" && st != ev {
		return st
	}
	if v, ok := row["verdict"].(string); ok && v != "" {
		return v
	}
	if wf, ok := row["watchdogFired"].(bool); ok {
		if wf {
			return "fired"
		}
		return "pass"
	}
	return ""
}

// RenderDashboard renders the full frame (multi-line, no screen-control
// codes) for the current snapshot.
func RenderDashboard(snap *Snapshot, ropts RenderOptions) string {
	return strings.Join(RenderLines(snap, ropts), "\n")
}

// viewItems is the flat item list of the current view — what the selection
// cursor walks.
func viewItems(snap *Snapshot, view View) []any {
	if view == ViewLog {
		return nil
	}
	if view == ViewSessions {
		return panesToItems(rosterPanes(snap))
	}
	var items []any
	for _, p := range rosterPanes(snap) {
		items = append(items, p)
	}
	for _, q := range queueRows(snap) {
		items = append(items, q)
	}
	return items
}

func panesToItems(panes []TuiPane) []any {
	var items []any
	for _, p := range panes {
		items = append(items, p)
	}
	return items
}

// PickKillTarget resolves the kill target: the selection, else a running
// pane, else any pane, else first queued row. "" when nothing qualifies.
func PickKillTarget(snap *Snapshot, view View, selection int) string {
	items := viewItems(snap, view)
	if selection >= 0 && selection < len(items) {
		switch it := items[selection].(type) {
		case TuiPane:
			if it.TaskID != "" {
				return it.TaskID
			}
		case TuiQueuedTask:
			if it.ID != "" {
				return it.ID
			}
		}
	}
	for _, p := range rosterPanes(snap) {
		if p.State == "running" && p.TaskID != "" {
			return p.TaskID
		}
	}
	for _, p := range rosterPanes(snap) {
		if p.TaskID != "" {
			return p.TaskID
		}
	}
	for _, q := range queueRows(snap) {
		if q.Status == "pending" && q.ID != "" {
			return q.ID
		}
	}
	return ""
}
