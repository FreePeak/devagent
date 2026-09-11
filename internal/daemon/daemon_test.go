package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"testing"
	"time"

	"github.com/FreePeak/devagent/internal/herdr"
	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/pipeline"
	"github.com/FreePeak/devagent/internal/platform"
	"github.com/FreePeak/devagent/internal/queue"
)

// fakeHerdrCli is the herdr CLI seam: it records invocations and returns a
// canned roster.
type fakeHerdrCli struct {
	stdout string
	code   int
	calls  [][]string
}

func (f *fakeHerdrCli) HerdrCli(args []string, timeoutMs int) herdr.CliResult {
	f.calls = append(f.calls, args)
	return herdr.CliResult{Code: f.code, Stdout: f.stdout}
}

// rosterJSON builds a herdr `agent list` stdout payload (parseAgentRows
// shape: result.agents with snake_case pointer fields).
func rosterJSON(rows ...string) string {
	return `{"result":{"agents":[` + strings.Join(rows, ",") + `]}}`
}

func agentRow(label, paneID, workspaceID, agentStatus, cwd string) string {
	return fmt.Sprintf(`{"name":"w-%s","label":%q,"agent":"omp","pane_id":%q,"workspace_id":%q,"agent_status":%q,"cwd":%q,"created_at":"2026-09-08T00:00:00Z"}`,
		label, label, paneID, workspaceID, agentStatus, cwd)
}

const worktreeBase = "/tmp/ta/.devagent-worktrees"

// runningTaskRoster: TASK-abc live (working), TASK-old stale (idle in a
// worktree checkout).
func runningTaskRoster() string {
	return rosterJSON(
		agentRow("TASK-abc-a1", "p1", "w1", "working", worktreeBase+"/TASK-abc-a1"),
		agentRow("TASK-old-a2", "p2", "w2", "idle", worktreeBase+"/TASK-old-a2"),
	)
}

// testDaemon wires a daemon against temp dirs with an ephemeral port and the
// default test token, then registers Stop on cleanup.
func testDaemon(t *testing.T, mutate func(*Options)) *Handle {
	t.Helper()
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	port := 0
	opts := Options{
		RepoPath: repo,
		Port:     &port,
		Token:    "test-token",
		HerdrCli: &fakeHerdrCli{stdout: runningTaskRoster()},
	}
	if mutate != nil {
		mutate(&opts)
	}
	h, err := Start(opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.Stop)
	return h
}

// daemonClient returns the base URL for the daemon's TCP listener.
func daemonBase(h *Handle) string {
	return fmt.Sprintf("http://127.0.0.1:%d", *h.Port)
}

// jsonRequest performs one request (auth optional) and decodes the JSON body.
func jsonRequest(t *testing.T, method, url, token string, body string) (int, http.Header, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, res.Header, out
}

func authedGet(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	code, _, body := jsonRequest(t, "GET", url, "test-token", "")
	return code, body
}

func authedPost(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	code, _, out := jsonRequest(t, "POST", url, "test-token", body)
	return code, out
}

func TestHealthzNoAuth(t *testing.T) {
	h := testDaemon(t, nil)
	code, _, body := jsonRequest(t, "GET", daemonBase(h)+"/healthz", "", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if body["ok"] != true {
		t.Fatalf("body = %v, want {ok:true}", body)
	}
}

func TestAuthRequired(t *testing.T) {
	h := testDaemon(t, nil)
	base := daemonBase(h)

	code, hdr, body := jsonRequest(t, "GET", base+"/status", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("missing bearer: status = %d, want 401", code)
	}
	if got := hdr.Get("WWW-Authenticate"); got != `Bearer realm="devagent-daemon"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if body["note"] != "unauthorized" {
		t.Fatalf("note = %v", body["note"])
	}

	code, _, _ = jsonRequest(t, "GET", base+"/status", "wrong-token", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: status = %d, want 401", code)
	}

	code, _, _ = jsonRequest(t, "GET", base+"/status", "test-token", "")
	if code != http.StatusOK {
		t.Fatalf("good bearer: status = %d, want 200", code)
	}
}

func TestHostAndOriginGuards(t *testing.T) {
	h := testDaemon(t, nil)
	base := daemonBase(h)

	// Foreign Host -> 403.
	req, _ := http.NewRequest("GET", base+"/status", nil)
	req.Host = "evil.example.com:7788"
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden || body["note"] != "forbidden host" {
		t.Fatalf("foreign host: %d %v", res.StatusCode, body)
	}

	// Loopback hosts pass the guard (land on the endpoint).
	for _, host := range []string{"localhost:7788", "[::1]:7788"} {
		req, _ := http.NewRequest("GET", base+"/status", nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer test-token")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("host %q: status = %d, want 200", host, res.StatusCode)
		}
	}

	// Evil Origin -> 403.
	req, _ = http.NewRequest("GET", base+"/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "https://evil.example.com")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden || body["note"] != "forbidden origin" {
		t.Fatalf("evil origin: %d %v", res.StatusCode, body)
	}

	// IPv6 loopback Origin passes.
	req, _ = http.NewRequest("GET", base+"/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Origin", "http://[::1]:1")
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("ipv6 origin: status = %d, want 200", res.StatusCode)
	}
}

func TestStatusShape(t *testing.T) {
	repo := t.TempDir()
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	base := daemonBase(h)

	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "TASK-a", Title: "a", Goal: "g", Source: "scout"}); err != nil {
		t.Fatal(err)
	}
	// Circuit open via proxy state file.
	devDir := filepath.Join(repo, ".devagent")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	proxy := `{"circuit":"open","circuitChangedAt":"2026-09-08T00:00:00.000Z","updatedAt":"2026-09-08T00:00:00.000Z"}`
	if err := os.WriteFile(filepath.Join(devDir, "proxy-state.json"), []byte(proxy), 0o644); err != nil {
		t.Fatal(err)
	}
	// herdr config block.
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte(`{"herdr":{"enabled":true,"session":"ops"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// spawn visibility env override.
	t.Setenv("DEVAGENT_VISIBILITY", "headless")

	code, body := authedGet(t, base+"/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, body)
	}
	caps, _ := body["capabilities"].([]any)
	var capStrs []string
	for _, c := range caps {
		capStrs = append(capStrs, c.(string))
	}
	if len(capStrs) != 4 || capStrs[0] != "approve" || capStrs[1] != "dispatch" || capStrs[2] != "attach" || capStrs[3] != "kill-via-answer" {
		t.Fatalf("capabilities = %v", capStrs)
	}
	runs, _ := body["runs"].(map[string]any)
	if runs["failed_recent"] != float64(0) {
		t.Fatalf("failed_recent = %v", runs["failed_recent"])
	}
	if u, ok := body["uptime_s"].(float64); !ok || u < 0 {
		t.Fatalf("uptime_s = %v", body["uptime_s"])
	}
	q, _ := body["queue"].(map[string]any)
	if q["pending"] != float64(1) || q["claimed"] != float64(0) || q["done"] != float64(0) {
		t.Fatalf("queue = %v", q)
	}
	if body["circuit"] != "open" {
		t.Fatalf("circuit = %v, want open", body["circuit"])
	}
	herdrBlock, _ := body["herdr"].(map[string]any)
	if herdrBlock["enabled"] != true || herdrBlock["session"] != "ops" {
		t.Fatalf("herdr = %v", herdrBlock)
	}
	spawn, _ := body["spawn"].(map[string]any)
	if spawn["visibility"] != "headless" {
		t.Fatalf("spawn = %v", spawn)
	}
	if _, ok := body["now"].(string); !ok {
		t.Fatalf("now = %v, want string", body["now"])
	}
}

func TestStatusIncludesLoopHeartbeat(t *testing.T) {
	// FR-VAL-03 #291d: the TUI header reads iteration · phase from /status,
	// sourced from the loopdriver's .selfbuild/heartbeat.json — never by
	// scraping the ledger. No heartbeat file → the loop field is absent.
	repo := t.TempDir()
	sb := filepath.Join(repo, ".selfbuild")
	if err := os.MkdirAll(sb, 0o755); err != nil {
		t.Fatal(err)
	}
	hb := `{"iteration":7,"phase":"task","pid":4242,"updatedAt":"2026-09-11T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(sb, "heartbeat.json"), []byte(hb), 0o644); err != nil {
		t.Fatal(err)
	}
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	code, body := authedGet(t, daemonBase(h)+"/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, body)
	}
	loop, ok := body["loop"].(map[string]any)
	if !ok {
		t.Fatalf("loop = %v, want object", body["loop"])
	}
	if loop["iteration"] != float64(7) || loop["phase"] != "task" || loop["pid"] != float64(4242) {
		t.Fatalf("loop = %v", loop)
	}
	if _, ok := loop["updatedAt"].(string); !ok {
		t.Fatalf("loop.updatedAt = %v, want string", loop["updatedAt"])
	}
}

func TestStatusOmitsLoopWhenNoHeartbeat(t *testing.T) {
	repo := t.TempDir()
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	code, body := authedGet(t, daemonBase(h)+"/status")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, body)
	}
	if _, present := body["loop"]; present {
		t.Fatalf("loop = %v, want absent without heartbeat file", body["loop"])
	}
}

func TestStatusConfigErrorIs500(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "devagent.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	code, body := authedGet(t, daemonBase(h)+"/status")
	if code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	note, _ := body["note"].(string)
	if !strings.HasPrefix(note, "Invalid JSON in ") {
		t.Fatalf("note = %q", note)
	}
}

func TestSessionsPanes(t *testing.T) {
	h := testDaemon(t, nil)
	code, body := authedGet(t, daemonBase(h)+"/sessions")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	panes, _ := body["panes"].([]any)
	if len(panes) != 2 {
		t.Fatalf("panes = %v", panes)
	}
	p0, _ := panes[0].(map[string]any)
	p1, _ := panes[1].(map[string]any)
	if p0["taskId"] != "TASK-abc" || p0["state"] != "running" || p0["paneId"] != "p1" {
		t.Fatalf("pane0 = %v", p0)
	}
	if p1["taskId"] != "TASK-old" || p1["state"] != "stale" {
		t.Fatalf("pane1 = %v", p1)
	}
}

func TestAgentsListAndRead(t *testing.T) {
	repo := t.TempDir()
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	base := daemonBase(h)
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "TASK-a", Title: "a", Goal: "g"}); err != nil {
		t.Fatal(err)
	}

	code, body := authedGet(t, base+"/agents")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	panes, _ := body["panes"].([]any)
	queued, _ := body["queued"].([]any)
	if len(panes) != 2 {
		t.Fatalf("panes = %v", panes)
	}
	if len(queued) != 1 {
		t.Fatalf("queued = %v", queued)
	}
	q0, _ := queued[0].(map[string]any)
	if q0["id"] != "TASK-a" || q0["status"] != "pending" {
		t.Fatalf("queued[0] = %v", q0)
	}

	code, body = authedGet(t, base+"/agents/TASK-a")
	if code != http.StatusOK {
		t.Fatalf("agent read: status = %d", code)
	}
	task, _ := body["task"].(map[string]any)
	if task["id"] != "TASK-a" {
		t.Fatalf("task = %v", task)
	}

	code, body = authedGet(t, base+"/agents/TASK-none")
	if code != http.StatusNotFound || body["note"] != "no task TASK-none" {
		t.Fatalf("agent 404: %d %v", code, body)
	}
}

func TestHistoryRawRows(t *testing.T) {
	repo := t.TempDir()
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	base := daemonBase(h)
	ledger.AppendAuditRecord(repo, ledger.MakeAuditRecord("TASK-a", 1, ledger.Verdict{Verdict: "pass", Integrity: "clean"}, ""))
	ledger.AppendAuditRecord(repo, ledger.MakeAuditRecord("TASK-b", 1, ledger.Verdict{Verdict: "fail", Integrity: "clean"}, ""))

	code, body := authedGet(t, base+"/history")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	records, _ := body["records"].([]any)
	if len(records) != 2 {
		t.Fatalf("records = %v", records)
	}
	r0, _ := records[0].(map[string]any)
	if r0["taskId"] != "TASK-a" || r0["kind"] != "audit" || r0["verdict"] != "pass" {
		t.Fatalf("records[0] = %v", r0)
	}

	// taskId filter.
	code, body = authedGet(t, base+"/history?taskId=TASK-b")
	records, _ = body["records"].([]any)
	if code != http.StatusOK || len(records) != 1 {
		t.Fatalf("filtered = %d %v", code, records)
	}
	rb, _ := records[0].(map[string]any)
	if rb["taskId"] != "TASK-b" {
		t.Fatalf("filtered row = %v", rb)
	}

	// limit=1 keeps the LAST row.
	code, body = authedGet(t, base+"/history?limit=1")
	records, _ = body["records"].([]any)
	if code != http.StatusOK || len(records) != 1 {
		t.Fatalf("limited = %d %v", code, records)
	}
	rl, _ := records[0].(map[string]any)
	if rl["taskId"] != "TASK-b" {
		t.Fatalf("limit row = %v (want the newest row)", rl)
	}
}

func TestDispatchHappyPath(t *testing.T) {
	repo := t.TempDir()
	var captured DispatchSpec
	h := testDaemon(t, func(o *Options) {
		o.RepoPath = repo
		o.DispatchRunner = func(spec DispatchSpec) DispatchResult {
			captured = spec
			pid := 4242
			return DispatchResult{PID: &pid}
		}
	})
	base := daemonBase(h)

	body := `{"prompt":"  do the thing\nsecond line  ","worker":"omp","budget":{"maxLoops":"5","timeoutMinutes":"abc"}}`
	code, out := authedPost(t, base+"/dispatch", body)
	if code != http.StatusAccepted {
		t.Fatalf("status = %d (%v)", code, out)
	}
	if out["ok"] != true || out["pid"] != float64(4242) {
		t.Fatalf("body = %v", out)
	}
	taskID, _ := out["taskId"].(string)
	if !regexp.MustCompile(`^TASK-[0-9a-f]{8}$`).MatchString(taskID) {
		t.Fatalf("taskId = %q", taskID)
	}

	// Runner captured the parsed spec.
	if captured.Prompt != "do the thing\nsecond line" {
		t.Fatalf("spec.prompt = %q", captured.Prompt)
	}
	if captured.RepoPath != repo {
		t.Fatalf("spec.repoPath = %q", captured.RepoPath)
	}
	if captured.Worker != "omp" {
		t.Fatalf("spec.worker = %q", captured.Worker)
	}
	if captured.MaxLoops == nil || *captured.MaxLoops != 5 {
		t.Fatalf("spec.maxLoops = %v", captured.MaxLoops)
	}
	if captured.TimeoutMinutes != nil {
		t.Fatalf("spec.timeoutMinutes = %v, want unset", captured.TimeoutMinutes)
	}
	if captured.TaskID != taskID {
		t.Fatalf("spec.taskId = %q, want %q", captured.TaskID, taskID)
	}

	// autoPr: explicit true threads through; absent defaults to false.
	code, out = authedPost(t, base+"/dispatch", `{"prompt":"headless","autoPr":true}`)
	if code != http.StatusAccepted {
		t.Fatalf("autoPr dispatch status = %d (%v)", code, out)
	}
	if !captured.AutoPr {
		t.Fatalf("spec.autoPr = false, want true")
	}
	code, _ = authedPost(t, base+"/dispatch", `{"prompt":"interactive"}`)
	if code != http.StatusAccepted {
		t.Fatalf("plain dispatch status = %d", code)
	}
	if captured.AutoPr {
		t.Fatalf("spec.autoPr = true, want false when the field is absent")
	}
	// Only a JSON true enables it (mirrors `p.autoPr === true`).
	if spec, _, _ := parseDispatch(`{"prompt":"p","autoPr":"true"}`, repo); spec.AutoPr {
		t.Fatalf("string autoPr enabled the flag")
	}
	if spec, _, _ := parseDispatch(`{"prompt":"p"}`, repo); spec.AutoPr {
		t.Fatalf("absent autoPr enabled the flag")
	}

	// Queue row: source daemon, title = first line, goal = full prompt.
	row := queue.ReadTask(repo, taskID)
	if row == nil {
		t.Fatal("queue row missing")
	}
	if row.Source == nil || *row.Source != "daemon" {
		t.Fatalf("source = %v", row.Source)
	}
	if row.Title != "do the thing" {
		t.Fatalf("title = %q", row.Title)
	}
	if row.Goal != "do the thing\nsecond line" {
		t.Fatalf("goal = %q", row.Goal)
	}

	// Issue #315: the row is claimed by the daemon before the worker spawns,
	// with a lease the selfbuild loop must respect.
	if row.Status != queue.StatusClaimed {
		t.Fatalf("status = %q, want claimed", row.Status)
	}
	if row.ClaimedBy == nil || *row.ClaimedBy != pipeline.DispatchClaimOwner {
		t.Fatalf("claimedBy = %v, want %s", row.ClaimedBy, pipeline.DispatchClaimOwner)
	}
	if row.LeaseExpiresAt == nil || *row.LeaseExpiresAt == "" {
		t.Fatalf("claimed row carries no lease: %+v", row)
	}
	if next := queue.ClaimNextPending(repo, "selfbuild-loop-1", nil); next != nil {
		t.Fatalf("selfbuild loop claimed a dispatched row: %+v", next)
	}
}

func TestDispatchValidation(t *testing.T) {
	h := testDaemon(t, nil)
	base := daemonBase(h)

	code, out := authedPost(t, base+"/dispatch", `{"prompt":"   "}`)
	if code != http.StatusBadRequest || out["note"] != "prompt must be a nonempty string" {
		t.Fatalf("blank prompt: %d %v", code, out)
	}

	code, out = authedPost(t, base+"/dispatch", "{not json")
	if code != http.StatusBadRequest || out["note"] != "invalid JSON body" {
		t.Fatalf("bad json: %d %v", code, out)
	}

	// Scalar body behaves like {} (parsed ?? {}).
	code, out = authedPost(t, base+"/dispatch", "123")
	if code != http.StatusBadRequest || out["note"] != "prompt must be a nonempty string" {
		t.Fatalf("scalar body: %d %v", code, out)
	}
}

func TestDispatchEnqueueCollision409(t *testing.T) {
	repo := t.TempDir()
	spec := DispatchSpec{RepoPath: repo, Prompt: "p"}
	if _, err := enqueueFromSpec(repo, "TASK-x", spec); err != nil {
		t.Fatal(err)
	}
	_, err := enqueueFromSpec(repo, "TASK-x", spec)
	if err == nil || err.Error() != "Task TASK-x already queued" {
		t.Fatalf("err = %v", err)
	}
}

func TestApprovePassthrough(t *testing.T) {
	var gotRepo, gotTask, gotAnswer string
	var called int
	h := testDaemon(t, func(o *Options) {
		o.AnswerApplier = func(repoPath, taskID, answer string) orchestrator.AnswerEndpointResult {
			called++
			gotRepo, gotTask, gotAnswer = repoPath, taskID, answer
			return orchestrator.AnswerEndpointResult{Status: http.StatusCreated, Body: map[string]any{"ok": true, "custom": "x"}}
		}
	})
	base := daemonBase(h)

	code, out := authedPost(t, base+"/approve", `{"taskId":"TASK-a","answer":"yes"}`)
	if code != http.StatusCreated || out["custom"] != "x" {
		t.Fatalf("passthrough: %d %v", code, out)
	}
	if called != 1 || gotTask != "TASK-a" || gotAnswer != "yes" {
		t.Fatalf("applier args: %q %q called=%d", gotTask, gotAnswer, called)
	}
	defaultRepo := gotRepo

	// Body repoPath override flows to the applier.
	code, _ = authedPost(t, base+"/approve", `{"taskId":"TASK-a","answer":"yes","repoPath":"/other/repo"}`)
	if code != http.StatusCreated {
		t.Fatalf("override status = %d", code)
	}
	if gotRepo != "/other/repo" || gotRepo == defaultRepo {
		t.Fatalf("override repoPath = %q (default %q)", gotRepo, defaultRepo)
	}
}

func TestApproveValidation(t *testing.T) {
	h := testDaemon(t, nil)
	base := daemonBase(h)

	code, out := authedPost(t, base+"/approve", "{not json")
	if code != http.StatusBadRequest || out["note"] != "invalid JSON body" {
		t.Fatalf("bad json: %d %v", code, out)
	}
	code, out = authedPost(t, base+"/approve", `{"taskId":"TASK-a"}`)
	if code != http.StatusBadRequest || out["note"] != "taskId and answer are required" {
		t.Fatalf("missing answer: %d %v", code, out)
	}
	code, out = authedPost(t, base+"/approve", `{"answer":"yes"}`)
	if code != http.StatusBadRequest || out["note"] != "taskId and answer are required" {
		t.Fatalf("missing taskId: %d %v", code, out)
	}
}

func TestKillViaAnswer(t *testing.T) {
	repo := t.TempDir()
	var reaperCwd string
	var reaperIdle int
	var killedPIDs []int
	var killReason string
	fake := &fakeHerdrCli{stdout: runningTaskRoster()}
	var applierCalled bool
	h := testDaemon(t, func(o *Options) {
		o.RepoPath = repo
		o.HerdrCli = fake
		o.ReapStaleWorkers = func(cwdPrefix string, idleMs int) []int {
			reaperCwd = cwdPrefix
			reaperIdle = idleMs
			return []int{123}
		}
		o.KillProcessTree = func(pid int, reason string) {
			killedPIDs = append(killedPIDs, pid)
			killReason = reason
		}
		o.AnswerApplier = func(string, string, string) orchestrator.AnswerEndpointResult {
			applierCalled = true
			return orchestrator.AnswerEndpointResult{Status: 200, Body: map[string]any{}}
		}
	})
	t.Setenv("DEVAGENT_HERDR_SESSION", "sess")
	base := daemonBase(h)

	code, out := authedPost(t, base+"/approve", `{"taskId":"TASK-abc","answer":"__kill__"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, out)
	}
	if out["ok"] != true || out["killed"] != true || out["taskId"] != "TASK-abc" || out["note"] != "operator kill requested" {
		t.Fatalf("body = %v", out)
	}

	// herdr calls: send-keys p1 ctrl+c then workspace close w1.
	// Call 0 is the roster probe (agent list); 1 is the #317 process-info
	// probe for the idle TASK-old pane (best-effort live-worker upgrade,
	// returns no process_info here); 2 = send-keys, 3 = close.
	if len(fake.calls) != 4 {
		t.Fatalf("herdr calls = %v", fake.calls)
	}
	wantProbe := []string{"pane", "process-info", "--pane", "p2"}
	wantKeys := []string{"--session", "sess", "pane", "send-keys", "p1", "ctrl+c"}
	wantClose := []string{"--session", "sess", "workspace", "close", "w1"}
	if strings.Join(fake.calls[1], " ") != strings.Join(wantProbe, " ") {
		t.Fatalf("call1 = %v", fake.calls[1])
	}
	if strings.Join(fake.calls[2], " ") != strings.Join(wantKeys, " ") {
		t.Fatalf("call2 = %v", fake.calls[2])
	}
	if strings.Join(fake.calls[3], " ") != strings.Join(wantClose, " ") {
		t.Fatalf("call3 = %v", fake.calls[3])
	}
	// Reaper seam scoped to this repo's worktrees.
	if filepath.Base(reaperCwd) != ".devagent-worktrees" || filepath.Dir(reaperCwd) != repo {
		t.Fatalf("reaper cwdPrefix = %q (repo %q)", reaperCwd, repo)
	}
	if reaperIdle != 60_000 {
		t.Fatalf("reaper idleMs = %d", reaperIdle)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != 123 || killReason != "operator-kill" {
		t.Fatalf("killProcessTree = %v %q", killedPIDs, killReason)
	}
	if applierCalled {
		t.Fatal("answer applier must not run for the kill sentinel")
	}

	// Audit row recorded on the daemon's repoPath with the operator-kill token.
	raw, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"operator-kill"`) {
		t.Fatalf("ledger = %q", string(raw))
	}
}

func TestKillNoTarget404(t *testing.T) {
	var applierCalled bool
	h := testDaemon(t, func(o *Options) {
		o.HerdrCli = &fakeHerdrCli{stdout: runningTaskRoster()}
		o.AnswerApplier = func(string, string, string) orchestrator.AnswerEndpointResult {
			applierCalled = true
			return orchestrator.AnswerEndpointResult{Status: 200, Body: map[string]any{}}
		}
		// No reaper seams: nil.
	})
	base := daemonBase(h)
	code, out := authedPost(t, base+"/approve", `{"taskId":"TASK-none","answer":"__kill__"}`)
	if code != http.StatusNotFound || out["note"] != "no live worker pane or child for TASK-none" {
		t.Fatalf("%d %v", code, out)
	}
	if applierCalled {
		t.Fatal("applier must not run")
	}
}

func TestAttachEndpoints(t *testing.T) {
	repo := t.TempDir()
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	base := daemonBase(h)

	code, out := authedPost(t, base+"/attach/TASK-abc", "")
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("attach: %d %v", code, out)
	}
	// TS attachCommandFor passes no session: ResolveSession("") falls back to
	// DEVAGENT_HERDR_SESSION (unset here) then "devagent".
	if out["command"] != "herdr --session devagent agent attach p1" {
		t.Fatalf("command = %v", out["command"])
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"operator-attached"`) || !strings.Contains(string(raw), `"paneId":"p1"`) {
		t.Fatalf("ledger = %q", string(raw))
	}

	code, out = authedPost(t, base+"/attach/TASK-none", "")
	if code != http.StatusNotFound || out["note"] != "no live pane for TASK-none" {
		t.Fatalf("attach 404: %d %v", code, out)
	}
}

func TestMethodRouting(t *testing.T) {
	h := testDaemon(t, nil)
	base := daemonBase(h)

	code, out := authedGet(t, base+"/dispatch")
	if code != http.StatusMethodNotAllowed || out["note"] != "method not allowed" {
		t.Fatalf("GET /dispatch: %d %v", code, out)
	}
	code, out = authedGet(t, base+"/nope")
	if code != http.StatusNotFound || out["note"] != "not found" {
		t.Fatalf("GET /nope: %d %v", code, out)
	}

	// POST /healthz is 404 (healthz is not in the known-method set).
	code, out = authedPost(t, base+"/healthz", "")
	if code != http.StatusNotFound || out["note"] != "not found" {
		t.Fatalf("POST /healthz: %d %v", code, out)
	}

	// DELETE /status is 405 (known path, wrong method).
	req, _ := http.NewRequest("DELETE", base+"/status", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed || body["note"] != "method not allowed" {
		t.Fatalf("DELETE /status: %d %v", res.StatusCode, body)
	}
}

// --- SSE ---

type sseEvent struct{ id, data string }

// readSSEEvent reads one event block ("id:"/"data:" lines + blank
// separator).
func readSSEEvent(t *testing.T, r *bufio.Reader) sseEvent {
	t.Helper()
	ev := sseEvent{}
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("sse read: %v (have %+v)", err, ev)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if ev.id != "" || ev.data != "" {
				return ev
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestSSEStream(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	// The run log exists BEFORE Start so the follower seeds it.
	runsDir := filepath.Join(home, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(runsDir, "live.jsonl")
	if err := os.WriteFile(logPath, []byte("line0\nline1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	port := 0
	h, err := Start(Options{RepoPath: repo, Port: &port, Token: "test-token", HerdrCli: &fakeHerdrCli{stdout: runningTaskRoster()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	base := fmt.Sprintf("http://127.0.0.1:%d", *h.Port)

	// Connect: retry hint then replay ids 0..2.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/events", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	r := bufio.NewReader(res.Body)
	first, err := r.ReadString('\n')
	if err != nil || strings.TrimRight(first, "\r\n") != "retry: 3000" {
		t.Fatalf("first line = %q err=%v", first, err)
	}
	want := []sseEvent{{"0", "line0"}, {"1", "line1"}, {"2", "line2"}}
	for _, w := range want {
		ev := readSSEEvent(t, r)
		if ev != w {
			t.Fatalf("event = %+v, want %+v", ev, w)
		}
	}

	// Append a 4th line: poll picks it up as id 3.
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line3\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	ev := readSSEEvent(t, r)
	if ev.id != "3" || ev.data != "line3" {
		t.Fatalf("live event = %+v", ev)
	}
	_ = res.Body.Close()

	// Reconnect with Last-Event-ID: 1 -> only ids 2,3.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	req2, _ := http.NewRequestWithContext(ctx2, "GET", base+"/events", nil)
	req2.Header.Set("Authorization", "Bearer test-token")
	req2.Header.Set("Last-Event-ID", "1")
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	r2 := bufio.NewReader(res2.Body)
	_, _ = r2.ReadString('\n') // retry hint
	evA := readSSEEvent(t, r2)
	evB := readSSEEvent(t, r2)
	if evA.id != "2" || evA.data != "line2" || evB.id != "3" || evB.data != "line3" {
		t.Fatalf("replay = %+v %+v", evA, evB)
	}
	_ = res2.Body.Close()

	// Client close leaves the daemon healthy.
	code, body := authedGet(t, base+"/healthz")
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("post-close healthz: %d %v", code, body)
	}
}

// --- Transports & token ---

func TestUnixSocket(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	sock := filepath.Join(t.TempDir(), "d.sock")
	port := 0
	h, err := Start(Options{RepoPath: repo, Port: &port, UDSPath: sock, Token: "test-token", HerdrCli: &fakeHerdrCli{stdout: runningTaskRoster()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	if h.Port != nil {
		t.Fatalf("port = %v, want nil on UDS", *h.Port)
	}
	if h.UDSPath != sock {
		t.Fatalf("udsPath = %q", h.UDSPath)
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: platform.DialContext(sock),
		},
	}
	res, err := client.Get("http://daemon/healthz")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("uds healthz: %d %v", res.StatusCode, body)
	}
}

func TestTokenGenerationAndPersistence(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	port := 0
	h, err := Start(Options{RepoPath: repo, Port: &port, HerdrCli: &fakeHerdrCli{stdout: runningTaskRoster()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	if len(h.Token) != 32 {
		t.Fatalf("token length = %d, want 32 base64url chars", len(h.Token))
	}
	raw, err := os.ReadFile(filepath.Join(home, "daemon-token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != h.Token+"\n" {
		t.Fatalf("persisted token file = %q", string(raw))
	}
	info, err := os.Stat(filepath.Join(home, "daemon-token"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	// The generated token authenticates.
	code, _, _ := jsonRequest(t, "GET", fmt.Sprintf("http://127.0.0.1:%d/status", *h.Port), h.Token, "")
	if code != http.StatusOK {
		t.Fatalf("generated token status = %d", code)
	}
}

func TestTokenExplicitNoFile(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "")
	port := 0
	h, err := Start(Options{RepoPath: repo, Port: &port, Token: "explicit", HerdrCli: &fakeHerdrCli{stdout: runningTaskRoster()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	if h.Token != "explicit" {
		t.Fatalf("token = %q", h.Token)
	}
	if _, err := os.Stat(filepath.Join(home, "daemon-token")); !os.IsNotExist(err) {
		t.Fatalf("daemon-token must not be written for an explicit token (err = %v)", err)
	}
}

func TestTokenEnvFallback(t *testing.T) {
	home := t.TempDir()
	repo := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	t.Setenv("DEVAGENT_DAEMON_TOKEN", "env-token")
	port := 0
	h, err := Start(Options{RepoPath: repo, Port: &port, HerdrCli: &fakeHerdrCli{stdout: runningTaskRoster()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	if h.Token != "env-token" {
		t.Fatalf("token = %q", h.Token)
	}
}

// --- Follower unit tests ---

func TestFollowerReplayCap(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "runs", "big.jsonl")
	if err := os.MkdirAll(filepath.Dir(log), 0o755); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for i := range 250 {
		fmt.Fprintf(&sb, "l%d\n", i)
	}
	if err := os.WriteFile(log, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newRunLogFollower(dir, nil)
	var got []sseEvent
	f.replayTo(recorderSink(func(id int, data string) { got = append(got, sseEvent{fmt.Sprint(id), data}) }), nil)
	if len(got) != 200 {
		t.Fatalf("replay count = %d, want 200", len(got))
	}
	if got[0].id != "50" || got[0].data != "l50" || got[199].id != "249" || got[199].data != "l249" {
		t.Fatalf("ends = %v ... %v", got[0], got[199])
	}
}

func TestFollowerTwoSourcesSharedIDs(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "runs", "a.jsonl")
	b := filepath.Join(dir, "extra.jsonl")
	if err := os.MkdirAll(filepath.Dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte("a0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b0\nb1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newRunLogFollower(dir, []string{b})
	var got []sseEvent
	f.replayTo(recorderSink(func(id int, data string) { got = append(got, sseEvent{fmt.Sprint(id), data}) }), nil)
	want := []sseEvent{{"0", "a0"}, {"1", "b0"}, {"2", "b1"}}
	if len(got) != 3 {
		t.Fatalf("got = %v", got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("got[%d] = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestFollowerDedupsExtraSource(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "runs", "a.jsonl")
	if err := os.MkdirAll(filepath.Dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a, []byte("a0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newRunLogFollower(dir, []string{a})
	if len(f.sources) != 1 {
		t.Fatalf("sources = %d, want 1 (extra == runLog deduped)", len(f.sources))
	}
}

func TestNewestRunLog(t *testing.T) {
	dir := t.TempDir()
	if newestRunLog(dir) != "" {
		t.Fatal("no runs dir must yield empty")
	}
	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(runsDir, "old.jsonl")
	newP := filepath.Join(runsDir, "new.jsonl")
	if err := os.WriteFile(old, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newP, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(newP, future, future); err != nil {
		t.Fatal(err)
	}
	if got := newestRunLog(dir); got != newP {
		t.Fatalf("newest = %q, want %q", got, newP)
	}
	// Non-.jsonl files are ignored.
	if err := os.WriteFile(filepath.Join(runsDir, "note.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := newestRunLog(dir); got != newP {
		t.Fatalf("newest with txt = %q", got)
	}
}

// recorderSink adapts a func into an eventSink for unit tests.
type recorderSink func(id int, data string)

func (r recorderSink) send(id int, data string) { r(id, data) }

// --- parseDispatch / jsNumber unit edges ---

func TestParseDispatchBudgetEdges(t *testing.T) {
	repo := t.TempDir()
	// String "5" coerces; "abc" is NaN -> unset.
	spec, _, errBody := parseDispatch(`{"prompt":"p","budget":{"maxLoops":"5","timeoutMinutes":"abc"}}`, repo)
	if errBody != nil {
		t.Fatalf("errBody = %v", errBody)
	}
	if spec.MaxLoops == nil || *spec.MaxLoops != 5 {
		t.Fatalf("maxLoops = %v", spec.MaxLoops)
	}
	if spec.TimeoutMinutes != nil {
		t.Fatalf("timeoutMinutes = %v, want unset", spec.TimeoutMinutes)
	}
	// Falsy budget is skipped entirely.
	spec, _, errBody = parseDispatch(`{"prompt":"p","budget":0}`, repo)
	if errBody != nil || spec.MaxLoops != nil || spec.TimeoutMinutes != nil {
		t.Fatalf("falsy budget: %+v %v", spec, errBody)
	}
	// Non-object truthy budget contributes nothing.
	spec, _, errBody = parseDispatch(`{"prompt":"p","budget":"yes"}`, repo)
	if errBody != nil || spec.MaxLoops != nil || spec.TimeoutMinutes != nil {
		t.Fatalf("string budget: %+v %v", spec, errBody)
	}
}

func TestDispatchArgvAutoPr(t *testing.T) {
	// --auto-pr is appended iff spec.AutoPr is set; budget/worker threading
	// stays untouched.
	base := DispatchSpec{RepoPath: "/repo", Prompt: "p"}
	got := dispatchArgv(base)
	if slices.Contains(got, "--auto-pr") {
		t.Fatalf("absent autoPr produced %v", got)
	}
	got = dispatchArgv(DispatchSpec{RepoPath: "/repo", Prompt: "p", AutoPr: true})
	if !slices.Contains(got, "--auto-pr") || got[len(got)-1] != "--auto-pr" {
		t.Fatalf("autoPr argv = %v", got)
	}
	loops, minutes := 5.0, 45.0
	full := dispatchArgv(DispatchSpec{RepoPath: "/repo", Prompt: "p", Worker: "omp",
		MaxLoops: &loops, TimeoutMinutes: &minutes, AutoPr: true})
	for _, flag := range []string{"--worker", "--max-loops", "--timeout", "--auto-pr"} {
		if !slices.Contains(full, flag) {
			t.Fatalf("argv missing %s: %v", flag, full)
		}
	}
}

func TestJsNumberEdges(t *testing.T) {
	cases := []struct {
		in     any
		val    float64
		finite bool
	}{
		{nil, 0, true},
		{true, 1, true},
		{false, 0, true},
		{float64(7.5), 7.5, true},
		{"", 0, true},
		{"  42 ", 42, true},
		{"0x10", 16, true},
		{"-0x10", -16, true},
		{"abc", 0, false},
		{"Infinity", 0, false},
		{"1e999", 0, false},
		{[]any{1}, 0, false},
	}
	for _, c := range cases {
		val, finite := jsNumber(c.in)
		if finite != c.finite || (finite && val != c.val) {
			t.Fatalf("jsNumber(%v) = %v,%v want %v,%v", c.in, val, finite, c.val, c.finite)
		}
	}
}

func TestTruncateUTF16(t *testing.T) {
	// 120-unit cap counts UTF-16 units: an astral char is 2 units.
	s := strings.Repeat("😀", 80) // 160 units
	if got := truncateUTF16(s, 120); got != strings.Repeat("😀", 60) {
		t.Fatalf("astral truncate wrong: %d runes", len([]rune(got)))
	}
	if got := truncateUTF16("hello", 120); got != "hello" {
		t.Fatalf("short = %q", got)
	}
}

func TestCountActiveRuns(t *testing.T) {
	home := t.TempDir()
	locks := filepath.Join(home, "locks")
	if err := os.MkdirAll(locks, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if err := os.WriteFile(filepath.Join(locks, "a.lock"), []byte(fmt.Sprintf(`{"startedAt":%d}`, now)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locks, "b.lock"), []byte(`{"startedAt":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locks, "c.lock"), []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locks, "d.txt"), []byte(`{"startedAt":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := countActiveRuns(home); got != 1 {
		t.Fatalf("active = %d, want 1 (fresh lock only; stale/corrupt/non-lock skipped)", got)
	}
}

func TestDispatchArgvTaskID(t *testing.T) {
	// Issue #315: the queue row's id reaches the child as --id, so its run
	// lock, worktree and branch all name the row the card shows.
	withID := dispatchArgv(DispatchSpec{RepoPath: "/repo", Prompt: "p", TaskID: "TASK-1234abcd"})
	i := slices.Index(withID, "--id")
	if i < 0 || i+1 >= len(withID) || withID[i+1] != "TASK-1234abcd" {
		t.Fatalf("argv = %v, want --id TASK-1234abcd", withID)
	}
	if slices.Contains(dispatchArgv(DispatchSpec{RepoPath: "/repo", Prompt: "p"}), "--id") {
		t.Fatalf("empty task id emitted --id")
	}
	// An empty worker must not produce a bare --worker "" (the flag pair is
	// emitted only when a worker is set).
	if slices.Contains(dispatchArgv(DispatchSpec{RepoPath: "/repo", Prompt: "p"}), "--worker") {
		t.Fatalf("empty worker emitted --worker")
	}
}

// TestDispatchLogWriter pins the issue #316 observability fix: the detached
// child's output lands in <home>/runs/dispatch-<taskID>.log (appended across
// calls, not clobbered), best-effort under a resolved DEVAGENT_HOME.
func TestDispatchLogWriter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	f := dispatchLogWriter("TASK-1234abcd")
	if f == nil {
		t.Fatal("dispatch log writer must open under a writable home")
	}
	if _, err := f.WriteString("first spawn\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	// A later dispatch to the same id appends: a daemon restart must not
	// wipe the refused-spawn evidence.
	f2 := dispatchLogWriter("TASK-1234abcd")
	if f2 == nil {
		t.Fatal("second open must succeed")
	}
	if _, err := f2.WriteString("second spawn\n"); err != nil {
		t.Fatal(err)
	}
	_ = f2.Close()
	raw, err := os.ReadFile(filepath.Join(home, "runs", "dispatch-TASK-1234abcd.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "first spawn\nsecond spawn\n" {
		t.Fatalf("log = %q, want appended content", raw)
	}
	// An empty task id still gets a file (never nil just for a missing id).
	if f3 := dispatchLogWriter(""); f3 == nil {
		t.Fatal("empty task id must fall back to the unscoped log")
	} else {
		_ = f3.Close()
	}
}

func TestStatusFailedRecentWindow(t *testing.T) {
	repo := t.TempDir()
	// The issue #315 field case: a row failed 17 days ago must not read as
	// "1f recent" on the dashboard banner or pin the desktop tray at failed.
	queue.EnsureQueueDirs(repo)
	old := `{"id":"SCOUT-20260825-lvvj","title":"old","goal":"g","acceptanceCriteria":[],` +
		`"status":"failed","createdAt":"2026-08-25T00:00:00.000Z","updatedAt":"2026-08-25T00:00:00.000Z"}`
	if err := os.WriteFile(filepath.Join(queue.QueueDir(repo), "SCOUT-20260825-lvvj.json"),
		[]byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	h := testDaemon(t, func(o *Options) { o.RepoPath = repo })
	base := daemonBase(h)
	runs := func() int {
		t.Helper()
		code, body := authedGet(t, base+"/status")
		if code != http.StatusOK {
			t.Fatalf("status = %d (%v)", code, body)
		}
		r, _ := body["runs"].(map[string]any)
		n, _ := r["failed_recent"].(float64)
		return int(n)
	}
	if got := runs(); got != 0 {
		t.Fatalf("failed_recent = %d, want 0 (17-day-old failure)", got)
	}

	// A failure inside the window counts, and decays out of it.
	if _, err := queue.EnqueueTask(repo, queue.EnqueueInput{ID: "TASK-new", Title: "n", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	claimed := queue.ClaimTask(repo, "TASK-new", "w", nil)
	if claimed == nil {
		t.Fatal("claim TASK-new failed")
	}
	if queue.FailTask(repo, "TASK-new", int64(*claimed.LeaseGeneration), "boom", nil) == nil {
		t.Fatal("fail TASK-new refused")
	}
	if got := runs(); got != 1 {
		t.Fatalf("failed_recent = %d, want 1 (fresh failure)", got)
	}
	at, err := queue.ParseISO(queue.ReadTask(repo, "TASK-new").UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := countRecentFailed(repo, at+failedRecentWindowMs+1); got != 0 {
		t.Fatalf("failed_recent = %d one window later, want 0", got)
	}
}
