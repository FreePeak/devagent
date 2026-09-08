package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/spawn"
)

// serveLines runs one Serve session over the given inbound lines and returns
// the response lines. Proof format: protocol behavior observed through the
// same pipe Serve reads in production.
func serveLines(t *testing.T, lines ...string) []string {
	t.Helper()
	var out strings.Builder
	in := strings.Join(lines, "\n") + "\n"
	if err := Serve(strings.NewReader(in), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	response := out.String()
	if response == "" {
		return nil
	}
	linesOut := strings.Split(strings.TrimSuffix(response, "\n"), "\n")
	return linesOut
}

// callToolRPC builds a tools/call request line.
func callToolRPC(id int, name string, args string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, args)
}

// toolCallText extracts the first text content of a tools/call response.
func toolCallText(t *testing.T, responseLine string) string {
	t.Helper()
	var res struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(responseLine), &res); err != nil {
		t.Fatalf("response %s: %v", responseLine, err)
	}
	if len(res.Result.Content) == 0 {
		t.Fatalf("response %s has no content", responseLine)
	}
	return res.Result.Content[0].Text
}

func TestInitializeHandshake(t *testing.T) {
	lines := serveLines(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"host","version":"1"}}}`)
	if len(lines) != 1 {
		t.Fatalf("want exactly one response line, got %d: %q", len(lines), lines)
	}
	var res struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &res); err != nil {
		t.Fatalf("initialize response not valid JSON: %v\n%s", err, lines[0])
	}
	if res.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", res.JSONRPC)
	}
	if string(res.ID) != "1" {
		t.Errorf("id = %s, want echoed 1", res.ID)
	}
	if res.Result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want 2024-11-05", res.Result.ProtocolVersion)
	}
	if len(res.Result.Capabilities.Tools) != 0 {
		t.Errorf("capabilities.tools = %v, want {}", res.Result.Capabilities.Tools)
	}
	if res.Result.ServerInfo.Name != "devagent" {
		t.Errorf("serverInfo.name = %q, want devagent", res.Result.ServerInfo.Name)
	}
	if res.Result.ServerInfo.Version == "" {
		t.Error("serverInfo.version empty")
	}
}

// TestInitializeResultKeyOrder pins the wire bytes of the initialize result:
// JSON.stringify order is protocolVersion, capabilities, serverInfo.
func TestInitializeResultKeyOrder(t *testing.T) {
	lines := serveLines(t, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	want := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{"tools":{}},"serverInfo":{"name":"devagent","version":%q}}}`, ServerVersionInfo())
	if lines[0] != want {
		t.Errorf("initialize line mismatch:\n got %s\nwant %s", lines[0], want)
	}
}

// TestToolsListMatchesTSRegistry pins the full tools/list payload — the six
// tools in TS order with the TS names/descriptions/schemas, byte for byte.
func TestToolsListMatchesTSRegistry(t *testing.T) {
	lines := serveLines(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)
	if len(lines) != 1 {
		t.Fatalf("want one response, got %q", lines)
	}
	// The envelope (jsonrpc/id/result key order) and the exact registry below
	// both mirror src/server/mcp.ts JSON.stringify output.
	want := `{"jsonrpc":"2.0","id":7,"result":{"tools":[` +
		`{"name":"devagent_dispatch","description":"Run a prompt-driven implementation task through the DevAgent pipeline in an isolated git worktree with a test gate. Returns the worktree path and result note.",` +
		`"inputSchema":{"type":"object","properties":{"prompt":{"type":"string","description":"Task description; first line becomes the title"},"repoPath":{"type":"string","description":"Absolute path to the target git repository"},"autoPr":{"type":"boolean","description":"Push branch and open a PR when tests pass (default false)"}},"required":["prompt","repoPath"]}},` +
		`{"name":"devagent_status","description":"List recent DevAgent runs (run id, timestamp, first event) from the local runs directory.",` +
		`"inputSchema":{"type":"object","properties":{},"additionalProperties":false}},` +
		`{"name":"devagent_log","description":"Read the last N entries of a run JSONL log.",` +
		`"inputSchema":{"type":"object","properties":{"runId":{"type":"string"},"tail":{"type":"number","description":"Number of trailing entries (default 10)"}},"required":["runId"]}},` +
		`{"name":"devagent_board","description":"Read the durable orchestration board (.devagent-project.json): goal, planner/executor roles, and every task with status, dependencies, attempts, and failure detail. Tasks paused for human input (status \"ask\") are also listed under pendingQuestions.",` +
		`"inputSchema":{"type":"object","properties":{"repoPath":{"type":"string","description":"Absolute path to the git repository holding the board file"}},"required":["repoPath"]}},` +
		`{"name":"devagent_ledger","description":"Read the append-only audit ledger (.devagent/runs/orchestration/events.jsonl): one record per independent audit with verdict, integrity, unmet criteria, and summary. History that survives worktree cleanup; complements the live devagent_board view.",` +
		`"inputSchema":{"type":"object","properties":{"repoPath":{"type":"string","description":"Absolute path to the git repository holding the ledger"},"taskId":{"type":"string","description":"Optional filter to one task id"}},"required":["repoPath"]}},` +
		`{"name":"devagent_answer","description":"Answer a task an auditor paused for human input (board status \"ask\"). The answer folds into the task contract and the task re-enters the queue; use devagent_board to discover pending questions.",` +
		`"inputSchema":{"type":"object","properties":{"repoPath":{"type":"string","description":"Absolute path to the git repository holding the board file"},"taskId":{"type":"string","description":"Task id currently in status \"ask\""},"answer":{"type":"string","description":"The human decision or missing information"}},"required":["repoPath","taskId","answer"]}}` +
		`]}}`
	if lines[0] != want {
		t.Errorf("tools/list mismatch\n got %s\nwant %s", lines[0], want)
	}
}

func TestPingAndNotifications(t *testing.T) {
	lines := serveLines(t,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":4,"method":"no/such/method"}`,
		`not json at all`,
		``,
	)
	if len(lines) != 1 {
		t.Fatalf("want exactly the ping answer, got %q", lines)
	}
	want := `{"jsonrpc":"2.0","id":3,"result":{}}`
	if lines[0] != want {
		t.Errorf("ping = %s, want %s", lines[0], want)
	}
}

func TestPingNullIDEchoesNull(t *testing.T) {
	lines := serveLines(t, `{"jsonrpc":"2.0","id":null,"method":"ping"}`)
	want := `{"jsonrpc":"2.0","id":null,"result":{}}`
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("got %q, want %q", lines, want)
	}
}

func TestStatusTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	runsDir := filepath.Join(home, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 22 runs: only the last 20 survive, oldest dropped.
	for i := 1; i <= 22; i++ {
		name := filepath.Join(runsDir, fmt.Sprintf("run-%02d.jsonl", i))
		body := fmt.Sprintf("{\"ts\":\"2026-09-08T0%d:00:00Z\",\"event\":\"start\"}\n{\"event\":\"next\"}\n", i%10)
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(runsDir, "notes.txt"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines := serveLines(t, callToolRPC(11, "devagent_status", `{}`))
	text := toolCallText(t, lines[0])
	var payload struct {
		Runs []struct {
			RunID     string  `json:"runId"`
			StartedAt *string `json:"startedAt"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("status text not JSON: %v\n%s", err, text)
	}
	if len(payload.Runs) != 20 {
		t.Fatalf("got %d runs, want last 20", len(payload.Runs))
	}
	if payload.Runs[0].RunID != "run-03" || payload.Runs[19].RunID != "run-22" {
		t.Errorf("runs window = %q..%q, want run-03..run-22", payload.Runs[0].RunID, payload.Runs[19].RunID)
	}
	if payload.Runs[0].StartedAt == nil || *payload.Runs[0].StartedAt != "2026-09-08T03:00:00Z" {
		t.Errorf("startedAt = %v, want 2026-09-08T03:00:00Z", payload.Runs[0].StartedAt)
	}

	// Missing runs directory: empty list, never an error.
	t.Setenv("DEVAGENT_HOME", filepath.Join(home, "absent"))
	lines = serveLines(t, callToolRPC(12, "devagent_status", `{}`))
	if text := toolCallText(t, lines[0]); text != `{"runs":[]}` {
		t.Errorf("absent runs dir text = %s, want {\"runs\":[]}", text)
	}
}

func TestLogTool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVAGENT_HOME", home)
	runsDir := filepath.Join(home, "runs")
	if err := os.MkdirAll(runsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "l1\nl2\nl3\nl4\nl5\n"
	if err := os.WriteFile(filepath.Join(runsDir, "abc123.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	lines := serveLines(t, callToolRPC(21, "devagent_log", `{"runId":"abc123","tail":2}`))
	if text := toolCallText(t, lines[0]); text != "l4\nl5" {
		t.Errorf("tail=2 text = %q, want \"l4\\nl5\"", text)
	}

	// Default tail (absent -> 10, clamped to the 5 available lines).
	lines = serveLines(t, callToolRPC(22, "devagent_log", `{"runId":"abc123"}`))
	if text := toolCallText(t, lines[0]); text != "l1\nl2\nl3\nl4\nl5" {
		t.Errorf("default tail text = %q", text)
	}

	// Path safety: traversal and separators are rejected in-band.
	for _, bad := range []string{"../etc/passwd", "a/b", "a b", ""} {
		lines = serveLines(t, callToolRPC(23, "devagent_log", fmt.Sprintf(`{"runId":%q}`, bad)))
		text := toolCallText(t, lines[0])
		if !strings.HasPrefix(text, "error: invalid runId: ") {
			t.Errorf("runId %q -> %q, want invalid-runId error", bad, text)
		}
	}

	// Unknown but well-formed run id: the TS readFileSync throw path.
	lines = serveLines(t, callToolRPC(24, "devagent_log", `{"runId":"missing"}`))
	if text := toolCallText(t, lines[0]); !strings.HasPrefix(text, "error: ") {
		t.Errorf("missing run -> %q, want error text", text)
	}
}

// boardFixture writes a board with one ask task (audit + gaps + failure
// detail), one done task (clean audit), and one pending task (no dependsOn).
func boardFixture(t *testing.T, dir string) {
	t.Helper()
	board := `{
  "goal": "Ship the MCP server",
  "createdAt": "2026-09-08T00:00:00Z",
  "updatedAt": "2026-09-08T01:00:00Z",
  "roles": {"planner": "omp", "executor": "omp", "auditor": "claude-code"},
  "tasks": [
    {
      "id": "T1", "title": "Ask task", "prompt": "do it",
      "dependsOn": [], "status": "ask", "attempts": 2,
      "evidenceGaps": ["gap-1"],
      "failureDetail": "needs human input: which provider?"
    },
    {
      "id": "T2", "title": "Done task", "prompt": "did it",
      "dependsOn": ["T1"], "status": "done", "attempts": 1,
      "audit": {
        "verdict": "pass", "integrity": "clean", "summary": "ok",
        "criteriaResults": [
          {"criterion": "tests pass", "met": true, "evidence": "go test"},
          {"criterion": "lint clean", "met": false, "evidence": "2 findings"}
        ]
      }
    },
    {
      "id": "T3", "title": "Pending task", "prompt": "wait",
      "status": "pending", "attempts": 0
    }
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, orchestrator.BoardFile), []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBoardTool(t *testing.T) {
	repo := t.TempDir()
	boardFixture(t, repo)

	lines := serveLines(t, callToolRPC(31, "devagent_board", fmt.Sprintf(`{"repoPath":%q}`, repo)))
	text := toolCallText(t, lines[0])

	// Shape assertions on the parsed payload...
	var payload struct {
		Exists           bool              `json:"exists"`
		Goal             string            `json:"goal"`
		UpdatedAt        string            `json:"updatedAt"`
		Roles            map[string]string `json:"roles"`
		Counts           map[string]int    `json:"counts"`
		PendingQuestions []struct {
			TaskID   string `json:"taskId"`
			Title    string `json:"title"`
			Question string `json:"question"`
		} `json:"pendingQuestions"`
		Tasks []struct {
			ID        string    `json:"id"`
			Status    string    `json:"status"`
			DependsOn *[]string `json:"dependsOn"`
			Attempts  int       `json:"attempts"`
			Audit     *struct {
				Verdict       string   `json:"verdict"`
				Integrity     string   `json:"integrity"`
				UnmetCriteria []string `json:"unmetCriteria"`
			} `json:"audit"`
			EvidenceGaps  []string `json:"evidenceGaps"`
			FailureDetail string   `json:"failureDetail"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("board text not JSON: %v\n%s", err, text)
	}
	if !payload.Exists || payload.Goal != "Ship the MCP server" {
		t.Errorf("exists/goal = %v/%q", payload.Exists, payload.Goal)
	}
	if payload.Roles["planner"] != "omp" || payload.Roles["executor"] != "omp" || payload.Roles["auditor"] != "claude-code" {
		t.Errorf("roles = %v", payload.Roles)
	}
	if !reflect.DeepEqual(payload.Counts, map[string]int{"ask": 1, "done": 1, "pending": 1}) {
		t.Errorf("counts = %v", payload.Counts)
	}
	if len(payload.PendingQuestions) != 1 || payload.PendingQuestions[0].TaskID != "T1" ||
		payload.PendingQuestions[0].Question != "which provider?" {
		t.Errorf("pendingQuestions = %+v", payload.PendingQuestions)
	}
	byID := map[string]int{}
	for i, task := range payload.Tasks {
		byID[task.ID] = i
	}
	t1 := payload.Tasks[byID["T1"]]
	if len(t1.EvidenceGaps) != 1 || t1.EvidenceGaps[0] != "gap-1" || t1.FailureDetail != "needs human input: which provider?" {
		t.Errorf("T1 = %+v", t1)
	}
	t2 := payload.Tasks[byID["T2"]]
	if t2.Audit == nil || t2.Audit.Verdict != "pass" || t2.Audit.Integrity != "clean" ||
		!reflect.DeepEqual(t2.Audit.UnmetCriteria, []string{"lint clean"}) {
		t.Errorf("T2 audit = %+v", t2.Audit)
	}
	t3 := payload.Tasks[byID["T3"]]
	if t3.DependsOn == nil || len(*t3.DependsOn) != 0 {
		t.Errorf("T3 dependsOn = %v, want []", t3.DependsOn)
	}

	// ...and key order on the wire (TS object-literal order).
	if !strings.Contains(text, `{"exists":true,"goal":"Ship the MCP server","updatedAt":"2026-09-08T01:00:00Z","roles":{"planner":"omp","executor":"omp","auditor":"claude-code"},"counts":`) {
		t.Errorf("board prologue key order drifted: %s", text[:min(160, len(text))])
	}

	// No board: {"exists":false}.
	empty := t.TempDir()
	lines = serveLines(t, callToolRPC(32, "devagent_board", fmt.Sprintf(`{"repoPath":%q}`, empty)))
	if text := toolCallText(t, lines[0]); text != `{"exists":false}` {
		t.Errorf("absent board text = %s", text)
	}
}

func TestLedgerTool(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, ledger.LedgerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rows := strings.Join([]string{
		`{"ts":"2026-09-08T02:00:00Z","kind":"audit","taskId":"T1","attempt":1,"verdict":"fail","integrity":"clean","unmetCriteria":["tests pass"],"summary":"not done"}`,
		`{"ts":"2026-09-08T03:00:00Z","kind":"event","event":"taskInterrupt","taskId":"T1","attempt":1,"failureClass":"test-fail","lastGateExcerpt":"boom"}`,
		`{"ts":"2026-09-08T04:00:00Z","kind":"audit","taskId":"T1","attempt":2,"verdict":"pass","integrity":"clean","unmetCriteria":[],"summary":"done"}`,
		`{"ts":"2026-09-08T05:00:00Z","kind":"audit","taskId":"T2","attempt":1,"verdict":"fail","integrity":"suspect","unmetCriteria":["e2e green"],"summary":"broken"}`,
		"", // corrupt/blank line: skipped
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}

	lines := serveLines(t, callToolRPC(41, "devagent_ledger", fmt.Sprintf(`{"repoPath":%q}`, repo)))
	text := toolCallText(t, lines[0])
	var payload struct {
		Summary struct {
			Tasks              int      `json:"tasks"`
			Audits             int      `json:"audits"`
			Resolved           int      `json:"resolved"`
			MeanAttemptsToPass *float64 `json:"meanAttemptsToPass"`
			Unresolved         int      `json:"unresolved"`
		} `json:"summary"`
		Records []map[string]any `json:"records"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("ledger text not JSON: %v\n%s", err, text)
	}
	// Only audit rows: the event row and the blank line never surface.
	if payload.Summary.Audits != 3 || len(payload.Records) != 3 {
		t.Fatalf("summary = %+v, records = %d", payload.Summary, len(payload.Records))
	}
	if payload.Summary.Resolved != 1 || payload.Summary.MeanAttemptsToPass == nil || *payload.Summary.MeanAttemptsToPass != 2 {
		t.Errorf("summary = %+v, want resolved 1 mean 2", payload.Summary)
	}

	// Task filter (taskId is optional in the schema).
	lines = serveLines(t, callToolRPC(42, "devagent_ledger", fmt.Sprintf(`{"repoPath":%q,"taskId":"T2"}`, repo)))
	text = toolCallText(t, lines[0])
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Records) != 1 || payload.Records[0]["taskId"] != "T2" {
		t.Errorf("filtered records = %+v", payload.Records)
	}

	// Absent ledger: empty records, zero summary (never an error).
	lines = serveLines(t, callToolRPC(43, "devagent_ledger", fmt.Sprintf(`{"repoPath":%q}`, t.TempDir())))
	if text := toolCallText(t, lines[0]); text != `{"summary":{"tasks":0,"audits":0,"resolved":0,"meanAttemptsToPass":null,"unresolved":0},"records":[]}` {
		t.Errorf("absent ledger text = %s", text)
	}
}

func TestAnswerTool(t *testing.T) {
	repo := t.TempDir()
	boardFixture(t, repo)

	// Round-trip: the answer folds in, the board persists, the task requeues.
	lines := serveLines(t, callToolRPC(51, "devagent_answer",
		fmt.Sprintf(`{"repoPath":%q,"taskId":"T1","answer":"use omp with fallback"}`, repo)))
	if text := toolCallText(t, lines[0]); text != `{"ok":true,"note":"Answered T1; task back in queue."}` {
		t.Fatalf("answer text = %s", text)
	}
	var board struct {
		Tasks []struct {
			ID           string   `json:"id"`
			Status       string   `json:"status"`
			Prompt       string   `json:"prompt"`
			EvidenceGaps []string `json:"evidenceGaps"`
		} `json:"tasks"`
	}
	blob, err := os.ReadFile(filepath.Join(repo, orchestrator.BoardFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(blob, &board); err != nil {
		t.Fatal(err)
	}
	if board.Tasks[0].Status != "ready" {
		t.Errorf("T1 status after answer = %q, want ready (pending re-promoted by saveBoard's readiness recompute, TS parity)", board.Tasks[0].Status)
	}
	if !strings.Contains(board.Tasks[0].Prompt, `Human answer to "needs human input: which provider?": use omp with fallback`) {
		t.Errorf("T1 prompt after answer = %q", board.Tasks[0].Prompt)
	}

	// A failed apply must not persist (TS saves only on success).
	lines = serveLines(t, callToolRPC(52, "devagent_answer",
		fmt.Sprintf(`{"repoPath":%q,"taskId":"nope","answer":"x"}`, repo)))
	if text := toolCallText(t, lines[0]); !strings.HasPrefix(text, `{"ok":false`) {
		t.Errorf("failed answer text = %s", text)
	}

	// No board at all.
	lines = serveLines(t, callToolRPC(53, "devagent_answer",
		fmt.Sprintf(`{"repoPath":%q,"taskId":"T1","answer":"x"}`, t.TempDir())))
	if text := toolCallText(t, lines[0]); text != `{"ok":false,"note":"no project board for this repo"}` {
		t.Errorf("no-board answer text = %s", text)
	}
}

func TestDispatchTool(t *testing.T) {
	var gotName string
	var gotArgs []string
	var gotOpts spawn.Options
	saved := runCLI
	runCLI = func(name string, args []string, opts spawn.Options) spawn.Result {
		gotName, gotArgs, gotOpts = name, args, opts
		return spawn.Result{ExitCode: 0, Stdout: "worktree .worktrees/x ready\nnote line\n"}
	}
	t.Cleanup(func() { runCLI = saved })

	lines := serveLines(t, callToolRPC(61, "devagent_dispatch",
		`{"prompt":"Port the MCP server","repoPath":"/tmp/repo","autoPr":true}`))
	text := toolCallText(t, lines[0])
	if gotName != dispatchCliPath() {
		t.Errorf("spawned %q, want the devagent executable", gotName)
	}
	wantArgs := []string{"task", "--prompt", "Port the MCP server", "--repo", "/tmp/repo", "--auto-pr"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("argv = %q, want %q", gotArgs, wantArgs)
	}
	if gotOpts.Dir != "." || gotOpts.TimeoutMs != 30*60*1000 {
		t.Errorf("opts = %+v, want Dir . and 30-minute wall", gotOpts)
	}
	var payload struct {
		ExitCode int    `json:"exitCode"`
		TimedOut bool   `json:"timedOut"`
		Note     string `json:"note"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("dispatch text not JSON: %v\n%s", err, text)
	}
	if payload.ExitCode != 0 || payload.TimedOut || payload.Note != "worktree .worktrees/x ready\nnote line" {
		t.Errorf("dispatch payload = %+v", payload)
	}

	// autoPr absent/0 stays falsy; timeout surface becomes timedOut=true.
	runCLI = func(name string, args []string, opts spawn.Options) spawn.Result {
		gotArgs = args
		return spawn.Result{ExitCode: -1, TimedOut: true, Stderr: "wall hit"}
	}
	t.Cleanup(func() { runCLI = saved })
	lines = serveLines(t, callToolRPC(62, "devagent_dispatch", `{"prompt":"p","repoPath":"/tmp/repo"}`))
	text = toolCallText(t, lines[0])
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TimedOut != true || payload.ExitCode != -1 || payload.Note != "wall hit" {
		t.Errorf("timeout payload = %+v", payload)
	}
	if reflect.DeepEqual(gotArgs, wantArgs) {
		t.Error("autoPr absent but --auto-pr still passed")
	}
}

func TestUnknownToolIsInBandError(t *testing.T) {
	lines := serveLines(t, callToolRPC(71, "devagent_nope", `{}`))
	text := toolCallText(t, lines[0])
	if text != "error: unknown tool: devagent_nope" {
		t.Errorf("text = %q", text)
	}
	if !strings.Contains(lines[0], `"isError":true`) {
		t.Errorf("response missing isError: %s", lines[0])
	}
}

// TestHTMLNotEscaped pins the JSON.stringify parity: <, > and & must survive
// unescaped through tool bodies and envelopes.
func TestHTMLNotEscaped(t *testing.T) {
	repo := t.TempDir()
	board := `{"goal":"a<b&c","createdAt":"x","updatedAt":"y","roles":{"planner":"p","executor":"e"},"tasks":[]}`
	if err := os.WriteFile(filepath.Join(repo, orchestrator.BoardFile), []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}
	lines := serveLines(t, callToolRPC(81, "devagent_board", fmt.Sprintf(`{"repoPath":%q}`, repo)))
	text := toolCallText(t, lines[0])
	if !strings.Contains(text, `"a<b&c"`) {
		t.Errorf("HTML got escaped: %s", text)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
