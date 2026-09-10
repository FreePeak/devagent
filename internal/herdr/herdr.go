// Package herdr ports the DevAgent herdr runtime integration
// (src/integrations/herdr.ts + src/workers/herdr-runtime.ts) to Go.
//
// Herdr (https://github.com/herdrdev/herdr) is a persistent terminal
// workspace manager for coding agents. When enabled, worker launches execute
// inside a pane of a dedicated named herdr session ("devagent" by default)
// instead of a direct child process: runs stay visible in a reattachable TUI,
// survive client disconnects, and leave per-run workspaces behind for
// inspection.
//
// Output contract: the worker command's stdout/stderr are redirected to temp
// files inside the pane and completion is signaled with an exit-code marker
// file. Devagent polls those files, so the exact stdout JSON parsing the
// adapters rely on is preserved (no pty scraping). Progress for the
// no-progress watchdog is derived from captured-file growth.
//
// Every herdr CLI invocation goes through the CliRunner seam so tests drive
// the full protocol (and the sweep decision matrix) with fixture JSON instead
// of a real herdr binary.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
	"github.com/FreePeak/devagent/internal/spawn"
)

// pollInterval mirrors POLL_MS: the captured-file poll cadence of a pane run.
const pollInterval = 250 * time.Millisecond

// paneWatchdogRowInterval is the FR-VAL-03 (#291c) periodic watchdog-health
// row cadence while a pane launch runs: a wedged worker must show up in the
// ledger during its budget burn, not only at teardown.
var paneWatchdogRowInterval = 30 * time.Second

// emitPaneWatchdogRow writes one watchdog-health row for a pane launch with
// a ledger context and an armed clock (Q34 teardown row + FR-VAL-03 #291c
// periodic rows). Best-effort; never throws.
func emitPaneWatchdogRow(ctx *WatchdogContext, noProgressMs, coldStartMs int, start, lastProgressAt time.Time, watchdogFired, coldStartFired bool, clockResets, lastBytes int) {
	if ctx == nil || (noProgressMs <= 0 && coldStartMs <= 0) {
		return
	}
	runtime := "herdr-pane"
	visible := true
	visibility := "herdr-pane"
	ledger.AppendWatchdogHealthRecord(ctx.RepoPath, ledger.WatchdogHealthRecord{
		TS:                  ledger.NowISO(),
		Kind:                "event",
		TaskID:              ctx.TaskID,
		Attempt:             ctx.Attempt,
		Event:               "watchdog-health",
		Site:                "herdr-pane",
		Worker:              ctx.Worker,
		NoProgressTimeoutMs: int64(noProgressMs),
		WatchdogFired:       watchdogFired,
		ColdStartFired:      coldStartFired,
		WallClockMs:         time.Since(start).Milliseconds(),
		ClockResets:         clockResets,
		MeaningfulBytes:     int64(lastBytes),
		IdleMs:              time.Since(lastProgressAt).Milliseconds(),
		// FR-VIS: pane launches are operator-visible by definition.
		Runtime:    &runtime,
		Visible:    &visible,
		Visibility: &visibility,
	})
}

// nestedPaneUnsets mirrors NESTED_PANE_UNSETS: vars unset in every pane
// before env.sh is sourced (mirrors spawn.NESTED_ENV_BLOCKLIST).
var nestedPaneUnsets = []string{
	"unset ANTHROPIC_MODEL",
	"unset ANTHROPIC_SMALL_FAST_MODEL",
	"unset CLAUDE_CODE_ENTRYPOINT",
	"unset CLAUDECODE",
}

// HerdrBin mirrors herdrBin(): the herdr binary override (tests inject a stub);
// defaults to "herdr".
func HerdrBin() string {
	if v := os.Getenv("DEVAGENT_HERDR_BIN"); v != "" {
		return v
	}
	return "herdr"
}

// ResolveSession mirrors resolveSession(): the named persistent session;
// defaults to DEVAGENT_HERDR_SESSION or "devagent".
func ResolveSession(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("DEVAGENT_HERDR_SESSION"); v != "" {
		return v
	}
	return "devagent"
}

// CliResult mirrors HerdrCliResult.
type CliResult struct {
	Code   int
	Stdout string
	Stderr string
}

// CliRunner is the seam every herdr CLI invocation goes through. The default
// implementation (ExecRunner) spawns the herdr binary; tests inject a fake
// that answers from fixture JSON.
type CliRunner interface {
	// HerdrCli runs one herdr CLI command to completion. timeoutMs <= 0 means
	// the 5s default cap — these are local RPCs.
	HerdrCli(args []string, timeoutMs int) CliResult
}

// ExecRunner is the default CliRunner: exec the herdr binary (DEVAGENT_HERDR_BIN
// is read per call, exactly like the TS herdrBin()).
type ExecRunner struct{}

// HerdrCli implements CliRunner. Exit-code mapping mirrors Node's execFile:
// success 0, a real child exit code that code, anything else (spawn failure,
// signal, timeout kill) -1.
func (ExecRunner) HerdrCli(args []string, timeoutMs int) CliResult {
	if timeoutMs <= 0 {
		timeoutMs = 5_000
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, HerdrBin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		// Node kills on timeout with error.code undefined -> -1.
		return CliResult{Code: -1, Stdout: stdout.String(), Stderr: stderr.String()}
	}
	if err == nil {
		return CliResult{Code: 0, Stdout: stdout.String(), Stderr: stderr.String()}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return CliResult{Code: exitErr.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}
	}
	return CliResult{Code: -1, Stdout: stdout.String(), Stderr: stderr.String()}
}

// ParseCliJSON mirrors parseCliJson(): the herdr reply envelope, or nil when
// stdout is not a JSON object.
//
// Cross-runtime divergence: V8's JSON.parse accepts any JSON value (a scalar
// then yields undefined on `.result`); encoding/json rejects a non-object here.
// Both paths end in "no result", so callers observe identical behavior.
func ParseCliJSON(stdout string) map[string]any {
	var v map[string]any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		return nil
	}
	return v
}

// resultOf digs out the envelope's `result` object (nil when absent).
func resultOf(parsed map[string]any) map[string]any {
	r, _ := parsed["result"].(map[string]any)
	return r
}

// HerdrServerUp mirrors herdrServerUp(): true when the named session's server
// answers `workspace list`.
func HerdrServerUp(cli CliRunner, session string) bool {
	r := cli.HerdrCli([]string{"--session", session, "workspace", "list"}, 5_000)
	return r.Code == 0 && resultOf(ParseCliJSON(r.Stdout))["type"] == "workspace_list"
}

// ServerWait mirrors the ensureHerdrServer wait options (0 = TS defaults).
type ServerWait struct {
	Attempts int
	DelayMs  int
}

// EnsureHerdrServer mirrors ensureHerdrServer(): make sure the dedicated
// session's headless server is running, starting it detached when necessary.
// Returns false when herdr cannot serve at all. bin == "" resolves HerdrBin().
func EnsureHerdrServer(cli CliRunner, session, bin string, wait ServerWait) bool {
	if HerdrServerUp(cli, session) {
		return true
	}
	if bin == "" {
		bin = HerdrBin()
	}
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		return false
	}
	// Panes inherit the daemon's environment, so the daemon must start with a
	// scrubbed env too — a parent's ANTHROPIC_MODEL would otherwise leak into
	// every worker pane (env.sh only overrides, it never unsets).
	server := exec.Command(bin, "--session", session, "server")
	server.Env = spawnEnvSlice(spawn.BuildEnv(spawn.Options{Dir: cwd}))
	server.SysProcAttr = detachProcAttr()
	if err := server.Start(); err != nil {
		// TS defers this to the async 'error' event and returns false on the
		// first poll; the observable outcome (false, fast) is the same.
		return false
	}
	go func() { _ = server.Wait() }() // reap; the daemon outlives us either way
	attempts := wait.Attempts
	if attempts == 0 {
		attempts = 24
	}
	delay := wait.DelayMs
	if delay == 0 {
		delay = 500
	}
	for range attempts {
		time.Sleep(time.Duration(delay) * time.Millisecond)
		if HerdrServerUp(cli, session) {
			return true
		}
	}
	return false
}

func spawnEnvSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// shQuote mirrors shQuote(): single-quote a value for the pane shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// envIdentRe: keys zsh can export.
var envIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RenderEnvFile mirrors renderEnvFile(): a sourceable env file from an env
// map. Keys that are not valid shell identifiers (npm injects
// `npm_config_node_pre_gyp:cache`, and any var with a dash or colon) are
// skipped: `export KEY=val` for those aborts the whole `source` under zsh
// ("not valid in this context"), which left the worker pane without
// PATH/HOME and the process was killed. The pane shell already carries such
// vars in its own environment when they were set, so dropping them from the
// override file loses nothing.
//
// Cross-runtime divergence: TS emits keys in object insertion order; Go maps
// are unordered, so keys are sorted. The sourced result is identical.
func RenderEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		if envIdentRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("export %s=%s", k, shQuote(env[k])))
	}
	return strings.Join(lines, "\n") + "\n"
}

// PaneRefs mirrors PaneRefs.
type PaneRefs struct {
	WorkspaceID string
	PaneID      string
}

// PaneNameForCwd mirrors paneNameForCwd(): pane/workspace name is the cwd's
// basename. Worktree checkouts encode the task in their directory name
// (.devagent-worktrees/T1-a1), so the name identifies what is running without
// any per-call label plumbing.
func paneNameForCwd(cwd string) string {
	// TS basename("/") is "" (no final segment); Go's filepath.Base("/")
	// returns "/" — normalize so the "devagent worker" default applies.
	base := strings.TrimSuffix(strings.TrimSpace(filepath.Base(cwd)), "/")
	if base != "" {
		return base
	}
	return "devagent worker"
}

func openPane(cli CliRunner, cwd, session string) *PaneRefs {
	name := paneNameForCwd(cwd)
	r := cli.HerdrCli([]string{"--session", session, "workspace", "create", "--label", name, "--cwd", cwd}, 0)
	result := resultOf(ParseCliJSON(r.Stdout))
	ws, _ := result["workspace"].(map[string]any)
	root, _ := result["root_pane"].(map[string]any)
	workspaceID, _ := ws["workspace_id"].(string)
	paneID, _ := root["pane_id"].(string)
	if r.Code != 0 || workspaceID == "" || paneID == "" {
		return nil
	}
	// Best-effort name on the pane itself so it reads well in the tab bar.
	cli.HerdrCli([]string{"--session", session, "pane", "rename", paneID, name}, 0)
	return &PaneRefs{WorkspaceID: workspaceID, PaneID: paneID}
}

func closeWorkspace(cli CliRunner, workspaceID, session string) {
	cli.HerdrCli([]string{"--session", session, "workspace", "close", workspaceID}, 10_000)
}

// WatchdogContext mirrors WatchdogLedgerContext at the herdr call site: the
// ledger context a pane launch records its watchdog-health row against.
// nil disables the row (TS: no opts.watchdogLedger).
type WatchdogContext struct {
	RepoPath string
	TaskID   string
	Attempt  int
	Worker   string
}

// PaneRunOptions is SpawnCliOptions & HerdrRuntimeOptions for one pane launch.
type PaneRunOptions struct {
	// Dir is SpawnCliOptions.cwd — the pane's working directory.
	Dir       string
	TimeoutMs int
	// Env is merged over the base environment unless ReplaceEnv.
	Env        map[string]string
	ReplaceEnv bool
	// Session: named persistent session; "" resolves DEVAGENT_HERDR_SESSION
	// or "devagent".
	Session string
	// NoProgressTimeoutMs / ColdStartTimeoutMs arm the watchdog clocks; 0
	// leaves them disarmed (TS undefined).
	NoProgressTimeoutMs int
	ColdStartTimeoutMs  int
	// Watchdog: ledger context for the one watchdog-health row per launch.
	Watchdog *WatchdogContext
}

// Result mirrors SpawnCliResult (src/workers/spawn-utils.ts), including the
// optional coldStart flag the pane path sets on a cold-start fire.
type Result struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	TimedOut  bool
	ColdStart bool
}

// RunCommandInHerdrPane mirrors runCommandInHerdrPane(): execute
// `cmd args...` inside a new herdr pane of the dedicated session.
//
// The (result, err) triple maps the TS shapes exactly:
//   - (*Result, nil)  — the run completed (or timed out; Result.TimedOut).
//   - (nil, nil)      — herdr unavailable/misbehaving: callers fall back to
//     direct execution.
//   - (nil, err)      — the TS throw path (scratch dir/env file I/O failed);
//     runWorkerCli turns it into a loud per-site fallback warning.
func RunCommandInHerdrPane(cli CliRunner, cmd string, args []string, opts PaneRunOptions) (*Result, error) {
	session := ResolveSession(opts.Session)
	if !EnsureHerdrServer(cli, session, "", ServerWait{}) {
		return nil, nil
	}
	refs := openPane(cli, opts.Dir, session)
	if refs == nil {
		return nil, nil
	}
	dir, err := os.MkdirTemp("", "devagent-herdr-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	outFile := filepath.Join(dir, "out")
	errFile := filepath.Join(dir, "err")
	doneFile := filepath.Join(dir, "done")

	// Panes inherit the server daemon's environment, not this process', so the
	// full computed child env is materialized into a source-only env file.
	// It is removed by the pane script immediately after sourcing and never
	// appears on the command line (nothing leaks into pane scrollback).
	env := spawn.BuildEnv(spawn.Options{Dir: opts.Dir, Env: opts.Env, ReplaceEnv: opts.ReplaceEnv})
	envFile := filepath.Join(dir, "env.sh")
	if err := os.WriteFile(envFile, []byte(RenderEnvFile(env)), 0o600); err != nil {
		return nil, err
	}

	quotedArgs := make([]string, 0, len(args))
	for _, a := range args {
		quotedArgs = append(quotedArgs, shQuote(a))
	}
	scriptParts := append([]string{}, nestedPaneUnsets...)
	scriptParts = append(scriptParts,
		// Unset harness-injected vars the daemon may have leaked before env.sh
		// overrides — claude rejects a nested ANTHROPIC_MODEL outright.
		"set -a; . "+shQuote(envFile)+"; set +a",
		"rm -f "+shQuote(envFile),
		"cd "+shQuote(opts.Dir)+" || exit 125",
		shQuote(cmd)+" "+strings.Join(quotedArgs, " ")+" > "+shQuote(outFile)+" 2> "+shQuote(errFile),
		"echo $? > "+shQuote(doneFile),
	)
	script := strings.Join(scriptParts, "; ")
	run := cli.HerdrCli([]string{"--session", session, "pane", "run", refs.PaneID, script}, 30_000)
	if run.Code != 0 {
		return nil, nil
	}

	noProgressMs := opts.NoProgressTimeoutMs
	coldStartMs := opts.ColdStartTimeoutMs
	start := time.Now()
	// lastBytes is seeded below, before the poll loop (the TS `= -1`
	// initializer is immediately overwritten by the seed — dead store, no
	// observable difference).
	var lastBytes int
	lastProgressAt := time.Now()
	timedOut := false
	watchdogFired := false
	// Q31: set only when the cold-start (first-progress) deadline fired.
	coldStartFired := false
	clockResets := 0

	// Progress = NEW TOOLCALL OR TEXT output, not raw byte growth. glm-style
	// models stream thinking_delta continuously (2026-08-31 live evidence:
	// 60k+ thinking deltas, 8-11 MB, while making zero tool calls for the
	// full hour), so byte-counting treats deliberation as progress and the
	// no-progress watchdog never fires. Strip thinking_delta lines before
	// counting: a run that only thinks is a hang in headless mode.
	// PRD Q33: progress via the shared classifier — only lines evidencing new
	// work (tool calls, answer text) count; thinking-only lines never do.
	seededBytes := meaningfulBytes(outFile) + meaningfulBytes(errFile)
	lastBytes = seededBytes
	// Q34: the pre-loop seed counts as one clock reset when the pane already
	// carried meaningful output.
	if seededBytes > 0 {
		clockResets++
	}
	graceMs := noProgressMs
	if graceMs > 60_000 {
		graceMs = 60_000
	}

	// FR-VAL-03 (#291c): periodic watchdog-health rows while the pane runs.
	lastRow := start
	for {
		// Sample output BEFORE the done-check: a fast pane (echo → exit) can
		// finish between polls, and checking the marker first would skip the
		// final progress snapshot — the row would claim zero clock resets and
		// zero meaningful bytes for a perfectly healthy run.
		now := time.Now()
		bytes := meaningfulBytes(outFile) + meaningfulBytes(errFile)
		if bytes != lastBytes {
			lastBytes = bytes
			lastProgressAt = now
			clockResets++
		}
		if fileExists(doneFile) {
			break
		}
		// TS `now >= start + opts.timeoutMs` is never true for an undefined
		// timeout (NaN); TimeoutMs <= 0 keeps that semantics: no wall cap.
		if opts.TimeoutMs > 0 && now.Sub(start) >= time.Duration(opts.TimeoutMs)*time.Millisecond {
			timedOut = true
			break
		}
		if coldStartMs > 0 && clockResets == 0 && now.Sub(start) >= time.Duration(coldStartMs)*time.Millisecond {
			// Q31: first-progress deadline. Until the classifier has seen one
			// meaningful line (clockResets === 0 — the seed counts),
			// coldStartMs is the binding budget: startup chatter that never
			// evidences new work must not keep a wedged plugin/MCP init alive
			// to the silence clock. Distinct from the no-progress fire below,
			// like Q34 separates it from wall-clock expiry.
			coldStartFired = true
			timedOut = true
			break
		}
		if noProgressMs > 0 &&
			now.Sub(lastProgressAt) >= time.Duration(noProgressMs)*time.Millisecond &&
			now.Sub(start) >= time.Duration(graceMs)*time.Millisecond {
			// Q34: watchdogFired is recorded only for the no-progress branch —
			// wall-clock expiry is a different outcome and the row must not
			// conflate them.
			watchdogFired = true
			timedOut = true
			break
		}
		if now.Sub(lastRow) >= paneWatchdogRowInterval {
			lastRow = now
			emitPaneWatchdogRow(opts.Watchdog, noProgressMs, coldStartMs, start, lastProgressAt, watchdogFired, coldStartFired, clockResets, lastBytes)
		}
		time.Sleep(pollInterval)
	}
	// Q34: the firing teardown row for a pane launch with a clock armed
	// (periodic rows were already emitted by the poll loop above).
	emitPaneWatchdogRow(opts.Watchdog, noProgressMs, coldStartMs, start, lastProgressAt, watchdogFired, coldStartFired, clockResets, lastBytes)
	if timedOut {
		// Match spawnCli's SIGKILL semantics: interrupt the foreground process
		// hard, then tear the workspace down.
		cli.HerdrCli([]string{"--session", session, "pane", "send-keys", refs.PaneID, "ctrl+c"}, 0)
		cli.HerdrCli([]string{"--session", session, "pane", "send-keys", refs.PaneID, "ctrl+c"}, 3_000)
		closeWorkspace(cli, refs.WorkspaceID, session)
		return &Result{
			ExitCode:  -1,
			Stdout:    readIfExists(outFile),
			Stderr:    readIfExists(errFile),
			TimedOut:  true,
			ColdStart: coldStartFired,
		}, nil
	}

	exitCode := -1
	if data, err := os.ReadFile(doneFile); err == nil {
		// marker vanished between existsSync and read -> treat as spawn failure
		if n, ok := parseJsInt(string(data)); ok {
			exitCode = n
		}
	}

	result := &Result{
		ExitCode: exitCode,
		Stdout:   readIfExists(outFile),
		Stderr:   readIfExists(errFile),
		TimedOut: false,
	}

	// Hygiene default: tear the run workspace down once its output is
	// captured. Set DEVAGENT_HERDR_KEEP_PANES=1 to leave completed runs open
	// in the session for inspection.
	if os.Getenv("DEVAGENT_HERDR_KEEP_PANES") != "1" {
		closeWorkspace(cli, refs.WorkspaceID, session)
	}
	return result, nil
}

// meaningfulBytes counts captured output that evidences new work: the sum of
// progress-classified line lengths (plus their newline) in one file. A missing
// or unreadable file has zero meaningful bytes.
func meaningfulBytes(p string) int {
	data, err := os.ReadFile(p)
	if err != nil {
		// File not there yet / mid-rename: zero meaningful bytes.
		return 0
	}
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if !IsNdjsonProgressLine(line) {
			continue
		}
		n += len(line) + 1
	}
	return n
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func readIfExists(p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(data)
}

// parseJsInt mirrors Number.parseInt(s.trim(), 10) + Number.isFinite: a base-10
func parseJsInt(s string) (int, bool) {
	t := strings.TrimSpace(s)
	i := 0
	if i < len(t) && (t[i] == '+' || t[i] == '-') {
		i++
	}
	digits := i
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if digits == i {
		return 0, false
	}
	n, err := strconv.Atoi(t[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}
