package daemon

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/queue"
	"github.com/FreePeak/devagent/internal/resilience"
)

// statusBody mirrors the /status response object in TS insertion order.
type statusBody struct {
	Now          string      `json:"now"`
	UptimeS      int64       `json:"uptime_s"`
	Runs         statusRuns  `json:"runs"`
	Queue        statusQueue `json:"queue"`
	Circuit      string      `json:"circuit"`
	Herdr        statusHerdr `json:"herdr"`
	Spawn        statusSpawn `json:"spawn"`
	Loop         *statusLoop `json:"loop,omitempty"`
	Capabilities []string    `json:"capabilities"`
}

// statusLoop is the loopdriver heartbeat (FR-VAL-03 #291d) read from
// .selfbuild/heartbeat.json: iteration N · phase X for the TUI header.
type statusLoop struct {
	Iteration int    `json:"iteration"`
	Phase     string `json:"phase"`
	Pid       int    `json:"pid"`
	UpdatedAt string `json:"updatedAt"`
}

// readLoopHeartbeat returns the loopdriver heartbeat, or nil when the
// driver has not written one (missing/unparseable file = no loop running).
func readLoopHeartbeat(repoPath string) *statusLoop {
	data, err := os.ReadFile(filepath.Join(repoPath, ".selfbuild", "heartbeat.json"))
	if err != nil {
		return nil
	}
	var hb statusLoop
	if err := json.Unmarshal(data, &hb); err != nil {
		return nil
	}
	return &hb
}

type statusRuns struct {
	Active       int `json:"active"`
	FailedRecent int `json:"failed_recent"`
}

type statusQueue struct {
	Pending int `json:"pending"`
	Claimed int `json:"claimed"`
	Done    int `json:"done"`
}

type statusHerdr struct {
	Enabled bool   `json:"enabled"`
	Session string `json:"session"`
}

type statusSpawn struct {
	Visibility string `json:"visibility"`
}

// statusEndpoint mirrors statusEndpoint: config + queue counts + proxy
// circuit + herdr/spawn blocks. Config load failure is a 500 via the route
// catch (note = error message), exactly like the TS throw.
func (d *daemon) statusEndpoint(res *response) error {
	cfg, err := config.Load(d.ctx.repoPath)
	if err != nil {
		return err
	}
	counts := queue.TaskCountOf(d.ctx.repoPath)
	proxy := resilience.ReadProxyState(d.ctx.repoPath)
	circuit := "closed"
	if proxy != nil {
		circuit = string(proxy.Circuit)
	}
	res.sendJSON(http.StatusOK, statusBody{
		Now:     ledger.NowISO(),
		UptimeS: int64(time.Since(d.ctx.startedAt).Seconds()),
		Runs: statusRuns{
			Active:       countActiveRuns(devagentHome()),
			FailedRecent: counts.Failed,
		},
		Queue:   statusQueue{Pending: counts.Pending, Claimed: counts.Claimed, Done: counts.Done},
		Circuit: circuit,
		Herdr: statusHerdr{
			Enabled: config.HerdrEnabled(cfg),
			Session: config.HerdrSessionName(cfg),
		},
		Spawn:        statusSpawn{Visibility: config.SpawnVisibility(cfg)},
		Loop:         readLoopHeartbeat(d.ctx.repoPath),
		Capabilities: devagentCapabilities,
	})
	return nil
}

// agentsBody mirrors the /agents response.
type agentsBody struct {
	Panes  []herdr.SessionPaneInfo `json:"panes"`
	Queued []*queue.QueuedTask     `json:"queued"`
}

// agentsEndpoint mirrors agentsEndpoint: herdr panes (failure → empty), then
// the pending + claimed queue rows concatenated.
func (d *daemon) agentsEndpoint(res *response) error {
	panes := d.listPanesLenient()
	queued := append(queue.ListTasks(d.ctx.repoPath, queue.StatusPending),
		queue.ListTasks(d.ctx.repoPath, queue.StatusClaimed)...)
	if queued == nil {
		queued = []*queue.QueuedTask{}
	}
	res.sendJSON(http.StatusOK, agentsBody{Panes: panes, Queued: queued})
	return nil
}

// agentBody is the /agents/<id> response.
type agentBody struct {
	Task *queue.QueuedTask `json:"task"`
}

// agentEndpoint mirrors agentEndpoint.
func (d *daemon) agentEndpoint(res *response, id string) error {
	task := queue.ReadTask(d.ctx.repoPath, id)
	if task == nil {
		res.sendJSON(http.StatusNotFound, errorBody{OK: false, Note: fmt.Sprintf("no task %s", id)})
		return nil
	}
	res.sendJSON(http.StatusOK, agentBody{Task: task})
	return nil
}

// dispatchBody is the 202 /dispatch response; PID nil serializes as JSON
// null exactly like TS `pid: null` (no omitempty).
type dispatchBody struct {
	OK     bool   `json:"ok"`
	TaskID string `json:"taskId"`
	PID    *int   `json:"pid"`
}

// dispatchEndpoint mirrors dispatchEndpoint: read body, parse the spec,
// enqueue (conflict → 409), run the dispatch runner, respond 202.
func (d *daemon) dispatchEndpoint(res *response, req *http.Request) error {
	raw, err := readBody(req)
	if err != nil {
		return err
	}
	spec, taskID, errBody := parseDispatch(raw, d.ctx.repoPath)
	if errBody != nil {
		res.sendJSON(errBody.Status, errBody.Body)
		return nil
	}
	queued, err := enqueueFromSpec(d.ctx.repoPath, taskID, spec)
	if err != nil {
		res.sendJSON(http.StatusConflict, errorBody{OK: false, Note: err.Error()})
		return nil
	}
	result := d.ctx.dispatchRunner(spec)
	res.sendJSON(http.StatusAccepted, dispatchBody{OK: true, TaskID: queued.ID, PID: result.PID})
	return nil
}

// errorWithStatus carries a pre-built JSON error from the body parsers.
type errorWithStatus struct {
	Status int
	Body   errorBody
}

// parseDispatch mirrors parseDispatch: `JSON.parse(raw || "{}")` (400
// "invalid JSON body"), `parsed ?? {}` (scalar/null bodies behave as {}),
// prompt validation, budget coercion through JS Number, and a collision-safe
// random TASK-xxxxxxxx id checked against the queue.
func parseDispatch(raw string, defaultRepoPath string) (DispatchSpec, string, *errorWithStatus) {
	spec := DispatchSpec{}
	var parsed any
	if err := json.Unmarshal([]byte(rawOrEmptyObject(raw)), &parsed); err != nil {
		return spec, "", &errorWithStatus{Status: http.StatusBadRequest,
			Body: errorBody{OK: false, Note: "invalid JSON body"}}
	}
	p, ok := parsed.(map[string]any)
	if !ok {
		p = map[string]any{}
	}
	prompt, _ := p["prompt"].(string)
	if prompt == "" || strings.TrimSpace(prompt) == "" {
		return spec, "", &errorWithStatus{Status: http.StatusBadRequest,
			Body: errorBody{OK: false, Note: "prompt must be a nonempty string"}}
	}
	spec.RepoPath = defaultRepoPath
	if v, ok := p["repoPath"].(string); ok && v != "" {
		spec.RepoPath = v
	}
	spec.Prompt = strings.TrimSpace(prompt)
	if v, ok := p["worker"].(string); ok && v != "" {
		spec.Worker = v
	}
	if v, ok := p["role"].(string); ok && v != "" {
		spec.Role = v
	}
	// Mirrors `if (p.autoPr === true) spec.autoPr = true;` — only an explicit
	// JSON true enables headless publish; absent/false/string leave it off.
	if b, ok := p["autoPr"].(bool); ok && b {
		spec.AutoPr = true
	}
	if budget, present := p["budget"]; present && jsTruthy(budget) {
		// Number(budget.maxLoops): a truthy non-object budget (string,
		// number, array) has undefined properties → Number(undefined) is NaN
		// → not finite → unset. Only a JSON object contributes fields.
		if bm, isMap := budget.(map[string]any); isMap {
			if v, present := bm["maxLoops"]; present {
				if n, finite := jsNumber(v); finite {
					spec.MaxLoops = &n
				}
			}
			if v, present := bm["timeoutMinutes"]; present {
				if n, finite := jsNumber(v); finite {
					spec.TimeoutMinutes = &n
				}
			}
		}
	}
	// Prefix-collision-safe: 8 hex chars of entropy + a uniqueness check
	// against the queue.
	taskID := "TASK-" + randHex(4)
	for queue.ReadTask(defaultRepoPath, taskID) != nil {
		taskID = "TASK-" + randHex(4)
	}
	return spec, taskID, nil
}

// jsTruthy mirrors JS truthiness over the JSON value domain: null, false,
// 0, and "" are falsy; everything else (objects, arrays, non-empty strings,
// non-zero numbers, true) is truthy.
func jsTruthy(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	case float64:
		return n != 0
	case string:
		return n != ""
	default:
		return true
	}
}

// rawOrEmptyObject mirrors `raw || "{}"`.
func rawOrEmptyObject(raw string) string {
	if raw == "" {
		return "{}"
	}
	return raw
}

// randHex mirrors randomBytes(n).toString("hex").
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

// truncateUTF16 mirrors TS String.prototype.slice(0, n): n counts UTF-16
// code units. (Local copy; the ledger package's helper is unexported.)
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

// enqueueFromSpec mirrors enqueueFromSpec: write the queue row the pipeline
// consumers (consume.ts, selfbuild claim) expect.
func enqueueFromSpec(repoPath string, taskID string, spec DispatchSpec) (*queue.QueuedTask, error) {
	firstLine := spec.Prompt
	if i := strings.IndexByte(spec.Prompt, '\n'); i >= 0 {
		firstLine = spec.Prompt[:i]
	}
	return queue.EnqueueTask(repoPath, queue.EnqueueInput{
		ID:     taskID,
		Title:  truncateUTF16(firstLine, 120),
		Goal:   spec.Prompt,
		Source: "daemon",
	})
}

// countActiveRuns mirrors countActiveRuns: count live run locks under
// DEVAGENT_HOME/locks (the runregistry's on-disk state).
func countActiveRuns(home string) int {
	locksDir := filepath.Join(home, "locks")
	entries, err := os.ReadDir(locksDir)
	if err != nil {
		return 0 // no locks dir -> zero active runs
	}
	active := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		runBestEffort(func() {
			raw, err := os.ReadFile(filepath.Join(locksDir, name))
			if err != nil {
				return
			}
			var holder struct {
				StartedAt *float64 `json:"startedAt"`
			}
			if json.Unmarshal(raw, &holder) != nil {
				return // corrupt lock file does not count as an active run
			}
			startedAt := 0.0
			if holder.StartedAt != nil {
				startedAt = *holder.StartedAt
			}
			if float64(time.Now().UnixMilli())-startedAt <= float64(runLockTTLms) {
				active++
			}
		})
	}
	return active
}

// approveEndpoint mirrors approveEndpoint: parse the body (400 "invalid JSON
// body"), resolve repoPath/taskId/answer with the TS typeof-string rules,
// route the kill sentinel, else delegate to the answer applier and pass its
// status/body through.
func (d *daemon) approveEndpoint(res *response, req *http.Request) error {
	raw, err := readBody(req)
	if err != nil {
		return err
	}
	var parsed any
	if err := json.Unmarshal([]byte(rawOrEmptyObject(raw)), &parsed); err != nil {
		res.sendJSON(http.StatusBadRequest, errorBody{OK: false, Note: "invalid JSON body"})
		return nil
	}
	p, ok := parsed.(map[string]any)
	if !ok {
		// Scalar/null parse results behave like {} — every property lookup
		// is undefined in the TS boxing semantics.
		p = map[string]any{}
	}
	repoPath := d.ctx.repoPath
	if v, ok := p["repoPath"].(string); ok && v != "" {
		repoPath = v
	}
	taskID := stringOrEmpty(p["taskId"])
	answer := stringOrEmpty(p["answer"])
	if taskID == "" || answer == "" {
		res.sendJSON(http.StatusBadRequest, errorBody{OK: false, Note: "taskId and answer are required"})
		return nil
	}
	// The kill sentinel never reaches the answer pipeline: an injected human
	// answer would re-queue the task it is meant to stop (integration review).
	if answer == killSentinel {
		return d.killViaAnswerEndpoint(res, repoPath, taskID)
	}
	r := d.ctx.answerApplier(repoPath, taskID, answer)
	res.sendJSON(r.Status, r.Body)
	return nil
}

// stringOrEmpty mirrors `typeof p.x === "string" ? p.x : ""`.
func stringOrEmpty(v any) string {
	s, _ := v.(string)
	return s
}

// killViaAnswerEndpoint mirrors killViaAnswerEndpoint: stop a live worker
// for taskId without touching the answer pipeline. Best-effort herdr pane
// stop (ctrl+c + workspace close), then a guarded headless-child reap scoped
// to this repo's .devagent-worktrees (same isDevagentWorkerCmd guard the
// reaper uses everywhere — never user sessions). No live target → 404.
func (d *daemon) killViaAnswerEndpoint(res *response, repoPath string, taskID string) error {
	// TS reads herdrSessionName() OUTSIDE any try: a config load failure is
	// a 500 via the route catch, not an empty roster.
	session, err := herdrSessionNameFromCwd()
	if err != nil {
		return err
	}
	paneStopped := false
	childKilled := false
	runBestEffort(func() {
		panes := d.listPanes(session)
		var pane *herdr.SessionPaneInfo
		for i := range panes {
			if panes[i].TaskID == taskID && panes[i].State == "running" {
				pane = &panes[i]
				break
			}
		}
		if pane == nil {
			return
		}
		keys := d.herdrCli.HerdrCli([]string{"--session", session, "pane", "send-keys", pane.PaneID, "ctrl+c"}, 5_000)
		if keys.Code == 0 {
			paneStopped = true
		}
		if pane.WorkspaceID != "" {
			closeRes := d.herdrCli.HerdrCli([]string{"--session", session, "workspace", "close", pane.WorkspaceID}, 10_000)
			if closeRes.Code == 0 {
				paneStopped = true
			}
		}
	})
	runBestEffort(func() {
		if d.reapStaleWorkers == nil {
			return // reaper seam not wired yet (FR-GO-05 #190)
		}
		stale := d.reapStaleWorkers(filepath.Join(repoPath, ".devagent-worktrees"), killMinChildAgeMs)
		for _, pid := range stale {
			if d.killProcessTree != nil {
				d.killProcessTree(pid, "operator-kill")
			}
		}
		childKilled = len(stale) > 0
	})
	if !paneStopped && !childKilled {
		res.sendJSON(http.StatusNotFound, errorBody{OK: false,
			Note: fmt.Sprintf("no live worker pane or child for %s", taskID)})
		return nil
	}
	// AuditLedgerRecord has no free-form event field; the operator kill is
	// recorded as a failed audit whose sole unmet criterion is the explicit
	// "operator-kill" token (filterable, visible to readLedger / /history).
	ledger.AppendAuditRecord(d.ctx.repoPath, ledger.MakeAuditRecord(taskID, 0, ledger.Verdict{
		Verdict:   "fail",
		Integrity: "clean",
		CriteriaResults: []ledger.CriterionResult{{
			Criterion: "operator-kill",
			Met:       false,
			Evidence:  "daemon /approve __kill__",
		}},
		Summary: "operator kill via daemon /approve __kill__ (kill-via-answer)",
	}, ""))
	res.sendJSON(http.StatusOK, killBody{OK: true, Killed: true, TaskID: taskID, Note: "operator kill requested"})
	return nil
}

// killBody is the /approve __kill__ 200 response.
type killBody struct {
	OK     bool   `json:"ok"`
	Killed bool   `json:"killed"`
	TaskID string `json:"taskId"`
	Note   string `json:"note"`
}

// sseSink is the daemon-side event sink: serialized writes (the follower's
// poll goroutine and the heartbeat tick write the same response), explicit
// flushes (net/http buffers, unlike Node's auto-flushed writes).
type sseSink struct {
	mu sync.Mutex
	w  http.ResponseWriter
}

func (s *sseSink) send(id int, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = fmt.Fprintf(s.w, "id: %d\ndata: %s\n\n", id, data)
	s.flush()
}

func (s *sseSink) writeHeartbeat() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write([]byte(": heartbeat\n\n"))
	s.flush()
}

func (s *sseSink) flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

// eventsEndpoint mirrors eventsEndpoint: SSE stream with the initial retry
// hint, replay tail, live fan-out, and heartbeats. The handler returns when
// the request context is canceled (client close or server shutdown via the
// registered BaseContext hook).
func (d *daemon) eventsEndpoint(res *response, req *http.Request) error {
	h := res.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	res.w.WriteHeader(http.StatusOK)
	_, _ = res.w.Write([]byte("retry: 3000\n\n"))
	res.ended = true // the stream owns the response now; panic recovery must not write a 500 over it

	sink := &sseSink{w: res.w}
	sink.flush()

	var lastEventID *int
	// TS: typeof raw === "string" && raw !== "" && Number.isFinite(Number(raw))
	// — a repeated header arrives as an array (typeof "object") → null.
	// textproto canonicalizes the key ("Last-Event-ID" → "Last-Event-Id").
	if values := req.Header["Last-Event-Id"]; len(values) == 1 && values[0] != "" {
		if n, finite := jsNumber(values[0]); finite {
			id := int(n)
			lastEventID = &id
		}
	}
	d.ctx.follower.replayTo(sink, lastEventID)
	d.ctx.follower.add(sink)
	defer d.ctx.follower.remove(sink)

	heartbeat := time.NewTicker(eventsHeartbeatInterval)
	defer heartbeat.Stop()
	done := req.Context().Done()
	for {
		select {
		case <-done:
			return nil
		case <-heartbeat.C:
			sink.writeHeartbeat()
		}
	}
}

// historyBody is the /history response; Records is a []json.RawMessage slice
// so every ledger row round-trips byte-identically.
type historyBody struct {
	Records []json.RawMessage `json:"records"`
}

// historyEndpoint mirrors historyEndpoint: JS Number limit parsing (default
// 100, capped 1000) over the RAW ledger rows (the daemon must not drop
// fields the typed Go tail view omits).
func (d *daemon) historyEndpoint(res *response, req *http.Request) error {
	limit := historyDefaultLimit
	if n, finite := jsNumber(req.URL.Query().Get("limit")); finite && n > 0 {
		limit = int(math.Min(math.Floor(n), historyMaxLimit))
	}
	taskID := req.URL.Query().Get("taskId")
	records := readLedgerTailRaw(d.ctx.repoPath, taskID, historyMaxLimit)
	// TS records.slice(-limit): limit 0 (a fractional 0<x<1 ?limit) keeps
	// everything, limit ≥ len keeps everything, else the last `limit` rows.
	if limit > 0 && len(records) > limit {
		records = records[len(records)-limit:]
	}
	res.sendJSON(http.StatusOK, historyBody{Records: records})
	return nil
}

// readLedgerTailRaw mirrors TS readLedgerTail: raw rows (every field
// preserved), blank lines skipped, corrupt JSON skipped, strict taskId
// equality, `opts.limit ? out.slice(-limit) : out` tail semantics. The Go
// ledger package's ReadLedgerTail returns a typed subset view, so the daemon
// reads the stream directly (a raw tail reader in ledger would remove this
// local copy).
func readLedgerTailRaw(repoPath string, taskID string, limit int) []json.RawMessage {
	file := filepath.Join(repoPath, ledger.LedgerDir, "events.jsonl")
	raw, err := os.ReadFile(file)
	if err != nil {
		return []json.RawMessage{}
	}
	out := []json.RawMessage{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var probe any
		if json.Unmarshal([]byte(line), &probe) != nil {
			continue // skip corrupt lines; a ledger is data, not truth
		}
		if taskID != "" {
			var row struct {
				TaskID string `json:"taskId"`
			}
			if json.Unmarshal([]byte(line), &row) != nil || row.TaskID != taskID {
				continue
			}
		}
		out = append(out, json.RawMessage(line))
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// sessionsBody is the /sessions response.
type sessionsBody struct {
	Panes []herdr.SessionPaneInfo `json:"panes"`
}

// sessionsEndpoint mirrors sessionsEndpoint: herdr panes, failure → empty.
func (d *daemon) sessionsEndpoint(res *response) error {
	res.sendJSON(http.StatusOK, sessionsBody{Panes: d.listPanesLenient()})
	return nil
}

// attachBody is the /attach/<id> response.
type attachBody struct {
	OK      bool   `json:"ok"`
	Command string `json:"command"`
}

// attachEndpoint mirrors attachEndpoint: find the task's live pane, build
// the attach command, record the operator-attached event, respond 200; no
// pane → 404.
func (d *daemon) attachEndpoint(res *response, taskID string) error {
	command := ""
	paneID := ""
	runBestEffort(func() {
		session, err := herdrSessionNameFromCwd()
		if err != nil {
			return // inside the TS try: roster resolution failure → command stays null
		}
		panes := d.listPanes(session)
		var pane *herdr.SessionPaneInfo
		for i := range panes {
			if panes[i].TaskID == taskID {
				pane = &panes[i]
				break
			}
		}
		if pane == nil {
			return
		}
		// TS attachCommandFor(taskId) passes NO session: the command string
		// resolves DEVAGENT_HERDR_SESSION env (or "devagent"), not the
		// cwd-config session — mirrored bug-for-bug via ResolveSession("").
		command = herdr.AttachCommandFor(d.herdrCli, taskID, "")
		paneID = pane.PaneID
	})
	if command == "" {
		res.sendJSON(http.StatusNotFound, errorBody{OK: false,
			Note: fmt.Sprintf("no live pane for %s", taskID)})
		return nil
	}
	// The TS handler calls herdrSessionName() a second time OUTSIDE the try:
	// a config load failure here is a 500, not a skipped audit row.
	session, err := herdrSessionNameFromCwd()
	if err != nil {
		return err
	}
	ledger.AppendOperatorAttachRecord(d.ctx.repoPath, ledger.OperatorAttachRecord{
		TS:      ledger.NowISO(),
		Kind:    "event",
		Event:   "operator-attached",
		TaskID:  taskID,
		Attempt: 0,
		PaneID:  paneID,
		Session: session,
	})
	res.sendJSON(http.StatusOK, attachBody{OK: true, Command: command})
	return nil
}

// listPanesLenient mirrors the endpoints' `try { panes = await
// listSessionPanes(herdrSessionName()) } catch { panes = [] }` pattern,
// including the session-name resolution INSIDE the lenient block.
func (d *daemon) listPanesLenient() []herdr.SessionPaneInfo {
	panes := []herdr.SessionPaneInfo{}
	runBestEffort(func() {
		session, err := herdrSessionNameFromCwd()
		if err != nil {
			return
		}
		panes = d.listPanes(session)
	})
	return panes
}

// listPanes is herdr.ListSessionPanes normalized to a non-nil slice (JSON
// must be [] not null, like the TS array).
func (d *daemon) listPanes(session string) []herdr.SessionPaneInfo {
	panes := herdr.ListSessionPanes(d.herdrCli, session)
	if panes == nil {
		return []herdr.SessionPaneInfo{}
	}
	return panes
}

// herdrSessionNameFromCwd mirrors the TS no-arg herdrSessionName() default:
// the config loads from process.cwd() BEFORE the env check, so an invalid
// config in the daemon's cwd fails even when DEVAGENT_HERDR_SESSION is set.
func herdrSessionNameFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		return "", err
	}
	return config.HerdrSessionName(cfg), nil
}

// dispatchArgv mirrors the TS dispatchArgv: the spawned `devagent task`
// arguments for one dispatched run, pure so tests can pin flag threading.
func dispatchArgv(spec DispatchSpec) []string {
	argv := []string{"task",
		"--prompt", spec.Prompt,
		"--repo", spec.RepoPath}
	if spec.Worker != "" {
		argv = append(argv, "--worker", spec.Worker)
	}
	if spec.MaxLoops != nil {
		argv = append(argv, "--max-loops", formatJSNumber(*spec.MaxLoops))
	}
	if spec.TimeoutMinutes != nil {
		argv = append(argv, "--timeout", formatJSNumber(*spec.TimeoutMinutes))
	}
	if spec.AutoPr {
		argv = append(argv, "--auto-pr")
	}
	return argv
}

// DefaultDispatchRunner mirrors defaultDispatchRunner: a detached spawn of
// the real `devagent task` pipeline (FR-CTRL-03). In a compiled binary the
// executable itself is the CLI.
func DefaultDispatchRunner(spec DispatchSpec) DispatchResult {
	cwd, err := os.Getwd()
	if err != nil {
		return DispatchResult{PID: nil}
	}
	exe, err := os.Executable()
	if err != nil {
		return DispatchResult{PID: nil}
	}
	argv := dispatchArgv(spec)
	cmd := exec.Command(exe, argv...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), "DEVAGENT_VISIBILITY="+visibilityEnv())
	setDetach(cmd)
	if err := cmd.Start(); err != nil {
		return DispatchResult{PID: nil}
	}
	pid := cmd.Process.Pid
	return DispatchResult{PID: &pid}
}

// visibilityEnv mirrors `process.env.DEVAGENT_VISIBILITY ?? "visible"`: an
// existing env value (even "") wins; only an unset variable defaults.
func visibilityEnv() string {
	if v, ok := os.LookupEnv("DEVAGENT_VISIBILITY"); ok {
		return v
	}
	return "visible"
}

// formatJSNumber mirrors JS String(number) for the finite floats the budget
// fields can hold (shortest 'g' formatting is the closest Go match).
func formatJSNumber(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
