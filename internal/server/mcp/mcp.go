// Package mcp is the Go port of src/server/mcp.ts (#251): a minimal MCP
// (Model Context Protocol) stdio server exposing DevAgent as tools for
// MCP-capable hosts (Orca, Claude Desktop, any MCP client). Zero
// dependencies beyond the internal packages: JSON-RPC 2.0, one message per
// line on stdin/stdout.
//
//	devagent mcp
//
// Tools:
//
//	devagent_dispatch — run a prompt-driven task headlessly (task mode)
//	devagent_status   — list recent runs from the runs directory
//	devagent_log      — read one run's JSONL log tail
//	devagent_board    — read the durable orchestration board
//	devagent_ledger   — read the append-only audit ledger
//	devagent_answer   — answer a task paused for human input
package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/orchestrator"
	"github.com/FreePeak/devagent/internal/spawn"
	"github.com/FreePeak/devagent/internal/version"
)

// ProtocolVersion is the MCP protocol version this server answers
// initialize with (TS handleRpc literal).
const ProtocolVersion = "2024-11-05"

// ServerName is the serverInfo name (TS literal). serverInfo.version is the
// CLI version (see ServerVersionInfo): the TS hard-coded '0.3.0', which was
// already drift against package.json; the Go server reports the stamped
// internal/version value instead of baking a second stale constant.
const ServerName = "devagent"

// ServerVersionInfo is the version reported in initialize results.
func ServerVersionInfo() string { return version.Version }

// ---------------------------------------------------------------------------
// JSON-RPC envelopes
// ---------------------------------------------------------------------------

// jsonRPCRequest mirrors the TS JsonRpcRequest. ID is kept raw so it echoes
// back byte-for-byte (number or string); absent/empty means a notification.
type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is the JSON-RPC 2.0 envelope the server writes. Key order is
// jsonrpc, id, result — the TS object-literal order.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result"`
}

// ---------------------------------------------------------------------------
// Tool registry (TS TOOLS: names, descriptions, schemas, order)
// ---------------------------------------------------------------------------

// orderedObj is a JSON object that marshals in insertion order; Go's
// map[string]any would sort keys and JSON.stringify does not.
type orderedObj []orderedField

type orderedField struct {
	Key   string
	Value any
}

// ToolDef is one tools/list entry (TS ToolDef). The schema types marshal in
// the TS literal key order.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema toolInputSchema `json:"inputSchema"`
}

// obj builds an orderedObj from alternating key/value arguments.
func obj(kv ...any) orderedObj {
	out := make(orderedObj, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, orderedField{Key: kv[i].(string), Value: kv[i+1]})
	}
	return out
}

// MarshalJSON writes the fields in declaration order, compact, with the
// same no-HTML-escaping policy as JSON.stringify.
func (o orderedObj) MarshalJSON() ([]byte, error) {
	var buf strings.Builder
	buf.WriteByte('{')
	for i, f := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		key, err := json.Marshal(f.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(key)
		buf.WriteByte(':')
		val, err := marshalNoEscape(f.Value)
		if err != nil {
			return nil, err
		}
		buf.Write(val)
	}
	buf.WriteByte('}')
	return []byte(buf.String()), nil
}

// toolProp is one inputSchema property ({type, description?}).
type toolProp struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// toolInputSchema is the {type, properties, required?, additionalProperties?}
// prologue every TS tool schema shares.
type toolInputSchema struct {
	Type                 string     `json:"type"`
	Properties           orderedObj `json:"properties"`
	Required             []string   `json:"required,omitempty"`
	AdditionalProperties *bool      `json:"additionalProperties,omitempty"`
}

// Tools is the server's tool registry — the six tools the TS server
// registers, in the same order, with the same schemas.
var Tools = []ToolDef{
	{
		Name: "devagent_dispatch",
		Description: "Run a prompt-driven implementation task through the DevAgent pipeline in an isolated git worktree " +
			"with a test gate. Returns the worktree path and result note.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: obj(
				"prompt", toolProp{Type: "string", Description: "Task description; first line becomes the title"},
				"repoPath", toolProp{Type: "string", Description: "Absolute path to the target git repository"},
				"autoPr", toolProp{Type: "boolean", Description: "Push branch and open a PR when tests pass (default false)"},
			),
			Required: []string{"prompt", "repoPath"},
		},
	},
	{
		Name:        "devagent_status",
		Description: "List recent DevAgent runs (run id, timestamp, first event) from the local runs directory.",
		InputSchema: toolInputSchema{
			Type:                 "object",
			Properties:           orderedObj{},
			AdditionalProperties: boolPtr(false),
		},
	},
	{
		Name:        "devagent_log",
		Description: "Read the last N entries of a run JSONL log.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: obj(
				"runId", toolProp{Type: "string"},
				"tail", toolProp{Type: "number", Description: "Number of trailing entries (default 10)"},
			),
			Required: []string{"runId"},
		},
	},
	{
		Name: "devagent_board",
		Description: "Read the durable orchestration board (.devagent-project.json): goal, planner/executor roles, and every " +
			"task with status, dependencies, attempts, and failure detail. Tasks paused for human input (status \"ask\") are also " +
			"listed under pendingQuestions.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: obj(
				"repoPath", toolProp{Type: "string", Description: "Absolute path to the git repository holding the board file"},
			),
			Required: []string{"repoPath"},
		},
	},
	{
		Name: "devagent_ledger",
		Description: "Read the append-only audit ledger (.devagent/runs/orchestration/events.jsonl): one record per independent " +
			"audit with verdict, integrity, unmet criteria, and summary. History that survives worktree cleanup; complements the " +
			"live devagent_board view.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: obj(
				"repoPath", toolProp{Type: "string", Description: "Absolute path to the git repository holding the ledger"},
				"taskId", toolProp{Type: "string", Description: "Optional filter to one task id"},
			),
			Required: []string{"repoPath"},
		},
	},
	{
		Name: "devagent_answer",
		Description: "Answer a task an auditor paused for human input (board status \"ask\"). The answer folds into the task " +
			"contract and the task re-enters the queue; use devagent_board to discover pending questions.",
		InputSchema: toolInputSchema{
			Type: "object",
			Properties: obj(
				"repoPath", toolProp{Type: "string", Description: "Absolute path to the git repository holding the board file"},
				"taskId", toolProp{Type: "string", Description: "Task id currently in status \"ask\""},
				"answer", toolProp{Type: "string", Description: "The human decision or missing information"},
			),
			Required: []string{"repoPath", "taskId", "answer"},
		},
	},
}

func boolPtr(b bool) *bool { return &b }

// ---------------------------------------------------------------------------
// runs directory (status/log backends)
// ---------------------------------------------------------------------------

// RunsDir mirrors the TS runsDir(): DEVAGENT_HOME wins; otherwise
// $HOME/.devagent (with the TS '.' fallback when HOME is unset). Exported so
// the CLI and tests resolve the same directory the tools read.
func RunsDir() string {
	if home := os.Getenv("DEVAGENT_HOME"); home != "" {
		return home
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".devagent")
}

// runEntry is the TS per-run object {runId, startedAt: string|null}.
type runEntry struct {
	RunID     string  `json:"runId"`
	StartedAt *string `json:"startedAt"`
}

// ListRuns mirrors listRuns(): the last 20 *.jsonl files under
// <RunsDir()>/runs sorted by name (JS sort + slice(-20) order: oldest kept
// twenty first), each reduced to its id and first-line ts. A missing runs
// directory is an empty list, never an error.
func ListRuns() string {
	runs := listRunsIn(filepath.Join(RunsDir(), "runs"))
	return mustJSON(obj("runs", runs))
}

func listRunsIn(dir string) []runEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []runEntry{}
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if len(names) > 20 {
		names = names[len(names)-20:]
	}
	runs := make([]runEntry, 0, len(names))
	for _, name := range names {
		runs = append(runs, runEntry{
			RunID:     strings.TrimSuffix(name, ".jsonl"),
			StartedAt: firstLineTs(filepath.Join(dir, name)),
		})
	}
	return runs
}

// firstLineTs mirrors readFileSync(...).split('\n')[0] -> safeTs. The TS reads
// the whole file; reading only the leading line observes the same value while
// keeping a large run log cheap.
func firstLineTs(file string) *string {
	f, err := os.Open(file)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	if !sc.Scan() {
		return nil
	}
	return safeTs(sc.Text())
}

// safeTs mirrors the TS helper: JSON.parse the line, take a string ts, else null.
func safeTs(line string) *string {
	var row struct {
		TS *string `json:"ts"`
	}
	if json.Unmarshal([]byte(line), &row) != nil {
		return nil
	}
	return row.TS
}

// runIDPattern is the TS path-safety guard: a runId must be a bare filename
// component.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// ReadRunLog mirrors readRunLog: validate the id, then return the last
// max(1, tail) lines of <RunsDir()>/runs/<runId>.jsonl. A missing file is the
// TS throw path, surfaced as an error (the caller turns it into isError).
func ReadRunLog(runID string, tail int) (string, error) {
	if !runIDPattern.MatchString(runID) {
		return "", fmt.Errorf("invalid runId: %s", runID)
	}
	data, err := os.ReadFile(filepath.Join(RunsDir(), "runs", runID+".jsonl"))
	if err != nil {
		return "", err
	}
	return tailLines(strings.TrimSpace(string(data)), tail), nil
}

// tailLines is `lines.slice(-Math.max(1, tail)).join('\n')`.
func tailLines(content string, tail int) string {
	lines := strings.Split(content, "\n")
	n := tail
	if n < 1 {
		n = 1
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// board / ledger / answer / dispatch backends
// ---------------------------------------------------------------------------

// boardTask is one entry of the devagent_board task list (TS map shape:
// optional audit/evidenceGaps/failureDetail keys are omitted when absent).
type boardTask struct {
	ID            string      `json:"id"`
	Title         string      `json:"title"`
	Status        string      `json:"status"`
	DependsOn     []string    `json:"dependsOn"`
	Attempts      int         `json:"attempts"`
	Audit         *boardAudit `json:"audit,omitempty"`
	EvidenceGaps  []string    `json:"evidenceGaps,omitempty"`
	FailureDetail string      `json:"failureDetail,omitempty"`
}

type boardAudit struct {
	Verdict       string   `json:"verdict"`
	Integrity     string   `json:"integrity"`
	UnmetCriteria []string `json:"unmetCriteria"`
}

type pendingQuestion struct {
	TaskID   string `json:"taskId"`
	Title    string `json:"title"`
	Question string `json:"question"`
}

// boardTool mirrors callTool('devagent_board'): loadBoard, then goal/roles/
// per-status counts/pendingQuestions/tasks. No board -> {"exists":false}.
func boardTool(repoPath string) string {
	board := orchestrator.LoadBoard(repoPath)
	if board == nil {
		return `{"exists":false}`
	}
	counts := map[string]int{}
	for _, t := range board.Tasks {
		counts[string(t.Status)]++
	}
	tasks := make([]boardTask, 0, len(board.Tasks))
	pending := []pendingQuestion{}
	for _, t := range board.Tasks {
		bt := boardTask{
			ID: t.ID, Title: t.Title, Status: string(t.Status),
			DependsOn: t.DependsOn, Attempts: t.Attempts,
			EvidenceGaps: t.EvidenceGaps, FailureDetail: t.FailureDetail,
		}
		if bt.DependsOn == nil {
			bt.DependsOn = []string{}
		}
		if bt.EvidenceGaps == nil {
			bt.EvidenceGaps = []string{}
		}
		if t.Audit != nil {
			unmet := []string{}
			for _, c := range t.Audit.CriteriaResults {
				if !c.Met {
					unmet = append(unmet, c.Criterion)
				}
			}
			bt.Audit = &boardAudit{Verdict: t.Audit.Verdict, Integrity: t.Audit.Integrity, UnmetCriteria: unmet}
		}
		if t.Status == orchestrator.TaskStatusAsk {
			// TS: .replace(/^needs human input:\s*/, '')
			pending = append(pending, pendingQuestion{
				TaskID:   t.ID,
				Title:    t.Title,
				Question: stripNeedsHumanInput(t.FailureDetail),
			})
		}
		tasks = append(tasks, bt)
	}
	return mustJSON(obj(
		"exists", true,
		"goal", board.Goal,
		"updatedAt", board.UpdatedAt,
		"roles", board.Roles,
		"counts", counts,
		"pendingQuestions", pending,
		"tasks", tasks,
	))
}

// needsHumanPrefix is the TS /^needs human input:\s*/ anchor.
var needsHumanPrefix = regexp.MustCompile(`^needs human input:\s*`)

func stripNeedsHumanInput(s string) string {
	return needsHumanPrefix.ReplaceAllString(s, "")
}

func ledgerTool(repoPath, taskID string) string {
	records := ledger.ReadLedger(repoPath, taskID)
	if records == nil {
		records = []ledger.AuditRecord{} // TS: [] when the ledger is absent
	}
	return mustJSON(obj(
		"summary", ledger.SummarizeLedger(repoPath),
		"records", records,
	))
}

// answerTool mirrors callTool('devagent_answer'): fold the answer into the
// board and persist only on success.
func answerTool(repoPath, taskID, answer string) string {
	board := orchestrator.LoadBoard(repoPath)
	if board == nil {
		return `{"ok":false,"note":"no project board for this repo"}`
	}
	res := orchestrator.ApplyHumanAnswer(board, taskID, answer)
	if res.OK {
		orchestrator.SaveBoard(repoPath, board)
	}
	return mustJSON(obj("ok", res.OK, "note", res.Note))
}

// runCLI is the spawn seam; tests swap it so a tools/call round-trip never
// launches a real pipeline.
var runCLI = func(name string, args []string, opts spawn.Options) spawn.Result {
	return spawn.RunCli(name, args, opts)
}

// dispatchCliPath resolves the executable the dispatch tool re-invokes: this
// binary when the OS can name it, else the PATH command (the TS child command
// was the Node entrypoint, which retires with FR-GO-16).
func dispatchCliPath() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	return "devagent"
}

// dispatchTool mirrors callTool('devagent_dispatch'): run `devagent task
// --prompt ... --repo ... [--auto-pr]` with the TS 30-minute wall and report
// {exitCode, timedOut, note} where note is the tail of stdout-or-stderr.
func dispatchTool(prompt, repoPath string, autoPr bool) string {
	argv := []string{"task", "--prompt", prompt, "--repo", repoPath}
	if autoPr {
		argv = append(argv, "--auto-pr")
	}
	r := runCLI(dispatchCliPath(), argv, spawn.Options{Dir: ".", TimeoutMs: 30 * 60 * 1000})
	return mustJSON(obj(
		"exitCode", r.ExitCode,
		"timedOut", r.TimedOut,
		"note", tailNote(r.Stdout, r.Stderr),
	))
}

// tailNote is TS `(stdout || stderr).trim().slice(-2000)`: stdout wins while
// non-empty, whitespace is trimmed off both ends, the last 2000 code units
// survive (UTF-16 in TS; runes approximate it for prose-shaped output).
func tailNote(stdout, stderr string) string {
	out := stdout
	if out == "" {
		out = stderr
	}
	note := strings.TrimSpace(out)
	runes := []rune(note)
	if len(runes) > 2000 {
		note = string(runes[len(runes)-2000:])
	}
	return note
}

// ---------------------------------------------------------------------------
// tool dispatch
// ---------------------------------------------------------------------------

func callTool(name string, args map[string]any) (string, error) {
	switch name {
	case "devagent_status":
		return ListRuns(), nil
	case "devagent_log":
		return ReadRunLog(argString(args["runId"]), argTail(args["tail"]))
	case "devagent_board":
		return boardTool(argString(args["repoPath"])), nil
	case "devagent_ledger":
		return ledgerTool(argString(args["repoPath"]), argString(args["taskId"])), nil
	case "devagent_answer":
		return answerTool(argString(args["repoPath"]), argString(args["taskId"]), argString(args["answer"])), nil
	case "devagent_dispatch":
		return dispatchTool(argString(args["prompt"]), argString(args["repoPath"]), argBool(args["autoPr"])), nil
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// argString mirrors String(args.x): strings pass through, other JSON values
// render the way JS stringification would (numbers bare, null as "null").
// A missing key reads as "" — String(undefined) would be "undefined" in TS,
// but every consuming schema marks the field required, so the distinction is
// unreachable through a conforming client; the safe "" keeps a misbehaving
// caller from materializing "undefined" into a path argument.
func argString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	default:
		return mustJSON(t)
	}
}

// argTail mirrors `Number(args.tail) || 10`: NaN, 0 and non-numeric values
// take the default.
func argTail(v any) int {
	switch t := v.(type) {
	case float64:
		if n := int(t); n != 0 {
			return n
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			if f != 0 {
				return int(f)
			}
			return 10
		}
	case bool:
		if t {
			return 1
		}
	}
	return 10
}

// argBool mirrors `if (args.autoPr)`: JSON null/false/0/"" are falsy.
func argBool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	default:
		return true
	}
}

// recv decodes one inbound JSON-RPC line the way the TS readline loop does
// (trim first, ignore blank and non-JSON lines); ok=false means skip.
func recv(line string) (jsonRPCRequest, bool) {
	var req jsonRPCRequest
	if json.Unmarshal([]byte(line), &req) != nil {
		return jsonRPCRequest{}, false
	}
	return req, true
}

// ---------------------------------------------------------------------------
// JSON-RPC handling
// ---------------------------------------------------------------------------

// toolCallParams is the tools/call params object.
type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// contentBlock is one MCP text content part.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolCallResult is the tools/call result: {content, isError?} — the TS adds
// isError only on the failure branch.
type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

// HandleRPC answers one decoded JSON-RPC request; nil means "nothing to
// write" (notifications, unknown methods, malformed shape). Tool failures are
// in-band (isError result), never JSON-RPC errors — identical to the TS.
func HandleRPC(req jsonRPCRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: obj(
			"protocolVersion", ProtocolVersion,
			"capabilities", obj("tools", orderedObj{}),
			"serverInfo", obj("name", ServerName, "version", ServerVersionInfo()),
		)}
	case "tools/list":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: obj("tools", Tools)}
	case "tools/call":
		var params toolCallParams
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		text, err := callTool(params.Name, params.Arguments)
		if err != nil {
			return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "error: " + err.Error()}},
				IsError: true,
			}}
		}
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: toolCallResult{
			Content: []contentBlock{{Type: "text", Text: text}},
		}}
	case "ping":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: orderedObj{}}
	}
	return nil
}

// maxLineBytes bounds one inbound JSON-RPC line (a tools/call carries a full
// task prompt): 8 MiB, well past any real request and small enough that a
// runaway writer cannot exhaust the server.
const maxLineBytes = 8 * 1024 * 1024

// Serve reads JSON-RPC lines from input and writes one compact response line
// per answerable request to output; blank and non-JSON lines are ignored, like
// the TS readline handler. The TS answered asynchronously (responses could in
// principle interleave); these handlers are synchronous, so ordering follows
// request order. Serve returns at EOF or on the first write error.
func Serve(input io.Reader, output io.Writer) error {
	sc := bufio.NewScanner(input)
	sc.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
	w := bufio.NewWriter(output)
	defer func() { _ = w.Flush() }()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		req, ok := recv(line)
		if !ok {
			continue // not JSON: ignore line
		}
		res := HandleRPC(req)
		if res == nil {
			continue
		}
		blob, err := marshalNoEscape(res)
		if err != nil {
			continue
		}
		if _, err := w.Write(blob); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Run wires Serve to the process streams.
func Run() error { return Serve(os.Stdin, os.Stdout) }

// marshalNoEscape is JSON.stringify's contract: compact, and never escaping
// <, > or & (Go's default encoder does).
func marshalNoEscape(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

// mustJSON stringifies a value the way JSON.stringify would; a value that
// cannot marshal (impossible for these hand-built shapes) yields "null"
// instead of unwinding the request handler.
func mustJSON(v any) string {
	blob, err := marshalNoEscape(v)
	if err != nil {
		return "null"
	}
	return string(blob)
}
