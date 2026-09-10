package tui

// FR-TUI (PRD §20.8) Go port (issue #198): the pure rendering half of the
// dashboard over the FR-CTRL daemon API — snapshot types, shape guards, view
// renderers (workers / sessions / log), overlays, the frame algebra and key
// decoding. The interactive raw-mode terminal loop stays behind the Loop
// seam for FR-GO-13; the HTTP/SSE client lives in transport.go.
import (
	"math"
	"strconv"
	"time"
)

// DefaultColumns/DefaultRows are the fallback terminal budgets (tui.ts
// termColumns/termRows: 100x40).
const (
	DefaultColumns = 100
	DefaultRows    = 40
)

// nowClock is the render clock (elapsed formatting); tests pin it via
// SetNow.
var nowClock = time.Now

// SetNow pins the render clock for deterministic tests; nil restores it.
func SetNow(fn func() time.Time) {
	if fn == nil {
		nowClock = time.Now
		return
	}
	nowClock = fn
}

// jsNum renders a float64 like JS String(number) for the integral values the
// header line interpolates (2p/1c/3d).
func jsNum(v float64) string {
	if v == float64(int64(v)) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// TuiOptions carries the daemon client options (the Go transport seam).
type TuiOptions struct {
	// URL is the daemon base URL; default http://127.0.0.1:7788.
	URL string
	// Token is the bearer token; default DEVAGENT_DAEMON_TOKEN or the
	// daemon-token file.
	Token string
	// UDSPath is a Unix-domain socket path; when set, requests go over it.
	UDSPath string
	// RepoPath is echoed into the kill (approve) call.
	RepoPath string
	// AttachOnly never starts an embedded daemon — attach to a running one
	// or degrade to the UNREACHABLE header (glances `-c` analog).
	AttachOnly bool
}

// TuiPane is the subset of the herdr pane roster the daemon exposes on
// /agents + /sessions.
type TuiPane struct {
	TaskID      string `json:"taskId"`
	Role        string `json:"role"`
	Worker      string `json:"worker"`
	PaneID      string `json:"paneId"`
	WorkspaceID string `json:"workspaceId"`
	Label       string `json:"label"`
	Cwd         string `json:"cwd"`
	AgentStatus string `json:"agentStatus"`
	State       string `json:"state"` // 'running' | 'idle' | 'stale'
	StartedAt   string `json:"startedAt"`
}

// TuiQueuedTask is the queue-row subset exposed on /agents.queued.
type TuiQueuedTask struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	CreatedAt string `json:"createdAt"`
}

// StatusPayload mirrors the daemon's /status envelope (field subsets used by
// the header renderer).
type StatusPayload struct {
	Now          string             `json:"now,omitempty"`
	UptimeS      *float64           `json:"uptime_s,omitempty"`
	Runs         *RunsPayload       `json:"runs,omitempty"`
	Queue        *StatusQueueCounts `json:"queue,omitempty"`
	Circuit      string             `json:"circuit,omitempty"`
	Herdr        *HerdrStatus       `json:"herdr,omitempty"`
	Spawn        *SpawnStatus       `json:"spawn,omitempty"`
	Loop         *LoopStatus        `json:"loop,omitempty"`
	Capabilities []string           `json:"capabilities,omitempty"`
	// Ask is the newest paused 'ask' task (FR-HAND-07) the approve sheet
	// can answer. The daemon's /status answers it directly; the loop's
	// pickPausedTask also falls back to the ledger tail 'ask' verdict.
	Ask *StatusAsk `json:"ask,omitempty"`
}

// LoopStatus is the loopdriver heartbeat block of /status (FR-VAL-03
// #291d): the header shows iteration · phase straight from the daemon
// instead of scraping the ledger tail.
type LoopStatus struct {
	Iteration *float64 `json:"iteration,omitempty"`
	Phase     string   `json:"phase,omitempty"`
	Pid       *float64 `json:"pid,omitempty"`
	UpdatedAt string   `json:"updatedAt,omitempty"`
}

// StatusAsk is the paused 'ask' task block of /status.
type StatusAsk struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

// RunsPayload is the runs block of /status.
type RunsPayload struct {
	Active       *float64 `json:"active,omitempty"`
	FailedRecent *float64 `json:"failed_recent,omitempty"`
}

// StatusQueueCounts is the queue block of /status (floats because the daemon
// serializes counters loosely; distinct from the integer QueueCounts the
// human cards use).
type StatusQueueCounts struct {
	Pending *float64 `json:"pending,omitempty"`
	Claimed *float64 `json:"claimed,omitempty"`
	Done    *float64 `json:"done,omitempty"`
}

// HerdrStatus is the herdr block of /status.
type HerdrStatus struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Session string `json:"session,omitempty"`
}

// SpawnStatus is the spawn block of /status.
type SpawnStatus struct {
	Visibility string `json:"visibility,omitempty"`
}

// AgentPayload is the /agents envelope: keep panes/queued only when they are
// actually arrays (NormalizeAgents).
type AgentPayload struct {
	Panes  []TuiPane       `json:"panes,omitempty"`
	Queued []TuiQueuedTask `json:"queued,omitempty"`
}

// HistoryRow is one ledger tail row of arbitrary producer shape.
type HistoryRow = map[string]any

// Snapshot is what one poll cycle produced; every field tolerates a partial
// failure.
type Snapshot struct {
	Status   *StatusPayload
	Agents   *AgentPayload
	History  []HistoryRow
	Sessions []TuiPane // nil when the payload shape was unusable
	// Reachable is false only when /status got no HTTP response at all.
	Reachable bool
	// AuthFailed is true when /status answered 401 — token present but wrong.
	AuthFailed bool
	FetchedAt  time.Time
}

// View is the three dashboard views; 1/2/3 switch, s and l toggle
// (htop-like tabs).
type View int

const (
	ViewWorkers View = iota
	ViewSessions
	ViewLog
)

// Overlay is a modal panel: per-item detail (Claude Code's expand), the
// upgrade hint, the dispatch sheet (FR-HAND-02 — one-line goal → POST
// /dispatch) or the approve sheet (FR-HAND-07 — answer a paused 'ask' via
// POST /approve). Exactly one of Pane/Queued/Upgrade/Input applies per Kind.
type Overlay struct {
	Kind    string // 'detail' | 'upgrade' | 'dispatch' | 'approve'
	Pane    *TuiPane
	Queued  *TuiQueuedTask
	Upgrade bool
	// Input is the typed one-line goal (dispatch) or free-text answer
	// (approve).
	Input string
	// TaskID is the taskId being answered (approve).
	TaskID string
}

// DispatchOverlay builds the `n` one-line goal dispatch sheet.
func DispatchOverlay() *Overlay {
	return &Overlay{Kind: "dispatch"}
}

// ApproveOverlay builds the `g` answer sheet for one paused task.
func ApproveOverlay(taskID string) *Overlay {
	return &Overlay{Kind: "approve", TaskID: taskID}
}

// DetailOverlay builds a detail overlay for one roster item.
func DetailOverlay(p *TuiPane, q *TuiQueuedTask) *Overlay {
	return &Overlay{Kind: "detail", Pane: p, Queued: q}
}

// UpgradeOverlay builds the `u` upgrade-recipe overlay.
func UpgradeOverlay() *Overlay {
	return &Overlay{Kind: "upgrade", Upgrade: true}
}

// MetricsState is the client-sampled activity series for the header
// sparkline (pilot cue).
type MetricsState struct {
	// Samples are active-worker counts, one per completed poll (newest last).
	Samples []float64
	// SampleMs is the interval between samples — for the window label.
	SampleMs float64
}

// LogViewState is everything the log view needs; the interactive loop owns
// the buffer.
type LogViewState struct {
	Lines []LogLine
	// Scroll is lines scrolled back from the tail; 0 = following the newest.
	Scroll int
	Follow bool
	State  string // EventsState: 'connecting' | 'live' | 'down' | 'off'
	// Source is the runId of the newest structured line, when known.
	Source string
	// Search is the active case-insensitive log filter; "" = unfiltered.
	// Matching lines render dim-nonmatching (n/N jump between matches).
	Search string
	// SearchDraft is the in-progress query while SearchMode is true (the
	// prompt renders it); SearchMode false ignores it.
	SearchDraft string
	SearchMode  bool
}

// RenderOptions tunes one frame render. Zero fields mean defaults
// (TermColumns/TermRows equivalents live in the caller for testability —
// tests pass Width/Rows explicitly).
type RenderOptions struct {
	// ShowSessions is the legacy alias for the sessions view.
	ShowSessions bool
	// ShowHelp appends the help overlay above the cards.
	ShowHelp bool
	// Note is a one-line transient note (kill confirm, errors) in the footer.
	Note string
	// PendingKill is the taskId awaiting a y/n confirm for the kill flow.
	PendingKill string
	// View selects the tab (ViewWorkers default).
	View View
	// Selection is the cursor index into the current view's item list.
	Selection int
	// Overlay shows a modal panel; nil for none.
	Overlay *Overlay
	// Metrics feeds the header sparkline; nil for none.
	Metrics *MetricsState
	// Log feeds the log view; nil for none.
	Log *LogViewState
	// Rows is the terminal row budget; 0 = default 100.
	Rows int
	// Width is the terminal column budget; 0 = default 100.
	Width int
	// SpinnerFrame is the spinner animation frame (interactive only).
	SpinnerFrame int
	// DaemonMode says the dashboard embedded its own daemon ("embedded").
	DaemonMode string
}

// Poll cadence + view constants, mirrored from tui.ts.
const (
	PollMs      = 2000
	HistoryRows = 8
	// SparkSamples: one sample per completed poll → ~2 minutes of activity.
	SparkSamples = 60
	// LogCap is the live-log ring buffer bound (lines kept from /events).
	LogCap = 1000
	// Spinner glyphs (Claude Code cue) animate only while work is live.
	Spinner  = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"
	TickerMs = 150
	// DefaultDaemonURL is the resolved daemon target.
	DefaultDaemonURL = "http://127.0.0.1:7788"
)

// spinnerAt returns the glyph for one animation frame (wraps like JS %).
func spinnerAt(frame int) string {
	if frame < 0 {
		frame = 0
	}
	r := []rune(Spinner)
	return string(r[frame%len(r)])
}

// normalizeSessions mirrors normalizeSessions(): a `/sessions` payload of a
// bare array or {panes:[...]}; anything else → nil.
func normalizeSessions(value any) []TuiPane {
	switch v := value.(type) {
	case []any:
		return panesFromAny(v)
	case map[string]any:
		if raw, ok := v["panes"].([]any); ok {
			return panesFromAny(raw)
		}
	}
	return nil
}

func panesFromAny(raw []any) []TuiPane {
	panes := make([]TuiPane, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			panes = append(panes, paneFromMap(m))
		}
	}
	return panes
}

func paneFromMap(m map[string]any) TuiPane {
	str := func(k string) string {
		if s, ok := m[k].(string); ok {
			return s
		}
		return ""
	}
	return TuiPane{
		TaskID:      str("taskId"),
		Role:        str("role"),
		Worker:      str("worker"),
		PaneID:      str("paneId"),
		WorkspaceID: str("workspaceId"),
		Label:       str("label"),
		Cwd:         str("cwd"),
		AgentStatus: str("agentStatus"),
		State:       str("state"),
		StartedAt:   str("startedAt"),
	}
}

// NormalizeAgents mirrors normalizeAgents(): keep panes/queued only when they
// are actually arrays; nil for a non-object payload.
func NormalizeAgents(value any) *AgentPayload {
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	out := &AgentPayload{}
	if raw, ok := m["panes"].([]any); ok {
		out.Panes = panesFromAny(raw)
	} else {
		out.Panes = []TuiPane{}
	}
	if raw, ok := m["queued"].([]any); ok {
		out.Queued = make([]TuiQueuedTask, 0, len(raw))
		for _, item := range raw {
			if qm, ok := item.(map[string]any); ok {
				q := TuiQueuedTask{}
				if s, ok := qm["id"].(string); ok {
					q.ID = s
				}
				if s, ok := qm["title"].(string); ok {
					q.Title = s
				}
				if s, ok := qm["status"].(string); ok {
					q.Status = s
				}
				if s, ok := qm["createdAt"].(string); ok {
					q.CreatedAt = s
				}
				out.Queued = append(out.Queued, q)
			}
		}
	} else {
		out.Queued = []TuiQueuedTask{}
	}
	return out
}

// rosterPanes is the pane roster for rendering, status aggregation and
// selection: the agents payload first, the sessions roster as fallback.
// Runtime-array-checked — hand-built or future-shaped snapshots must degrade
// to empty, never throw.
func rosterPanes(snap *Snapshot) []TuiPane {
	if snap.Agents != nil && snap.Agents.Panes != nil {
		return snap.Agents.Panes
	}
	if snap.Sessions != nil {
		return snap.Sessions
	}
	return nil
}

// queueRows is the queued rows for rendering/selection; empty when the
// payload is not an array.
func queueRows(snap *Snapshot) []TuiQueuedTask {
	if snap.Agents != nil && snap.Agents.Queued != nil {
		return snap.Agents.Queued
	}
	return nil
}

// AggregateStatus computes the header aggregate. FAILED means live trouble
// (circuit open — the factory cannot dispatch), not "some task failed at
// some point": runs.failed_recent is a lifetime queue-failed count that never
// decays, so it pinned the header at FAILED permanently (2026-09-05 fix).
// PAUSED means a task waits on the operator (a paused 'ask' gate): the
// approval moment is the one state a dashboard must never render as idle
// (the opencode/crush convention — permission-needed is unmissable), so it
// outranks IDLE but not RUNNING.
func AggregateStatus(status *StatusPayload, panes []TuiPane) string {
	if status == nil {
		return "IDLE"
	}
	runningPanes := 0
	for _, p := range panes {
		if p.State == "running" {
			runningPanes++
		}
	}
	runsActive, queueClaimed := 0.0, 0.0
	if status.Runs != nil && status.Runs.Active != nil {
		runsActive = *status.Runs.Active
	}
	if status.Queue != nil && status.Queue.Claimed != nil {
		queueClaimed = *status.Queue.Claimed
	}
	if runsActive > 0 || runningPanes > 0 || queueClaimed > 0 {
		return "RUNNING"
	}
	if status.Circuit == "open" {
		return "FAILED"
	}
	if status.Ask != nil && status.Ask.ID != "" {
		return "PAUSED"
	}
	return "IDLE"
}
func fmtElapsed(startedAt string, now time.Time) string {
	if startedAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return ""
	}
	mins := int(now.Sub(t).Minutes())
	if mins < 0 {
		mins = 0
	}
	if mins < 60 {
		return itoa(mins) + "m"
	}
	hours := mins / 60
	if hours < 48 {
		return itoa(hours) + "h"
	}
	return itoa(hours/24) + "d"
}

// fmtClock renders an ISO ts → local HH:MM:SS for the history rows; blanks
// when unparseable.
func fmtClock(ts any) string {
	s, ok := ts.(string)
	if !ok || s == "" {
		return "         "
	}
	d, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return "         "
	}
	return d.Local().Format("15:04:05")
}

// fmtUptime renders uptime seconds → compact "3h12m" / "45s"; "-" for
// absent/negative.
func fmtUptime(s *float64) string {
	if s == nil || *s < 0 || !isFinite(*s) {
		return "-"
	}
	sec := int(*s)
	if sec < 60 {
		return itoa(sec) + "s"
	}
	m := sec / 60
	if m < 60 {
		return itoa(m) + "m"
	}
	h := m / 60
	return itoa(h) + "h" + itoa(m%60) + "m"
}

func itoa(n int) string { return strconv.Itoa(n) }
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
