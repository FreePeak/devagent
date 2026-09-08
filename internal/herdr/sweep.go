package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/config"
)

// StalePane mirrors StalePane.
type StalePane struct {
	WorkspaceID string `json:"workspaceId"`
	PaneID      string `json:"paneId"`
	Label       string `json:"label"`
	AgentStatus string `json:"agentStatus"`
	Reason      string `json:"reason"`
}

// idleStatuses mirrors IDLE_STATUSES: agent statuses that mean "not doing work
// right now".
var idleStatuses = map[string]bool{"idle": true, "unknown": true, "done": true}

// SweepReasonOperatorAttached mirrors SWEEP_REASON_OPERATOR_ATTACHED: reason
// carried by a candidate the sweep reports but must never close — an operator
// is at the wheel of this pane (FR-VIS-10). Exported so the CLI renders it as
// a spared line instead of a closed one.
const SweepReasonOperatorAttached = "operator-attached"

// SweepDenyReason mirrors sweepDenyReason(): why the sweep must not touch
// `session` at all, or "" when it may. "disabled" = the master toggle
// (`herdr.sweep.enabled`, `DEVAGENT_HERDR_SWEEP=0`); "session-denied" = the
// session sits on the managed deny list (`herdr.sweep.denySessions`).
func SweepDenyReason(session string, sweep config.HerdrSweepSettings) string {
	if !sweep.Enabled {
		return "disabled"
	}
	for _, d := range sweep.DenySessions {
		if d == session {
			return "session-denied"
		}
	}
	return ""
}

// ResolveSweepSettings mirrors resolveSweepSettings(): resolve the sweep
// settings, failing closed — an unreadable/invalid devagent.json leaves the
// deny list unknown, and an unknown deny list is never swept. Loud, so the
// silence stays explainable.
func ResolveSweepSettings() config.HerdrSweepSettings {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[herdr-sweep] config invalid, sweep skipped: %v\n", err)
		return config.HerdrSweepSettings{Enabled: false}
	}
	cfg, err := config.Load(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[herdr-sweep] config invalid, sweep skipped: %v\n", err)
		return config.HerdrSweepSettings{Enabled: false}
	}
	return config.ResolveHerdrSweep(cfg)
}

// SweepOptions carries findStalePanes/sweepStalePanes options.
type SweepOptions struct {
	// Orphans arms the orphan-pane class (loop-driver use only).
	Orphans bool
	// Sweep overrides the resolved herdr.sweep settings (nil = resolve).
	Sweep *config.HerdrSweepSettings
}

// paneRow is one `pane list` entry; AgentStatus is a pointer so "field absent"
// (the no-agent leftover shape) stays distinguishable from an empty status.
type paneRow struct {
	PaneID      *string `json:"pane_id"`
	WorkspaceID *string `json:"workspace_id"`
	Label       *string `json:"label"`
	AgentStatus *string `json:"agent_status"`
	Cwd         *string `json:"cwd"`
}

func (p paneRow) paneID() string      { return derefOr(p.PaneID, "") }
func (p paneRow) workspaceID() string { return derefOr(p.WorkspaceID, "") }
func (p paneRow) label() string       { return derefOr(p.Label, "(no label)") }
func (p paneRow) cwd() string         { return derefOr(p.Cwd, "") }

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

// FindStalePanes mirrors findStalePanes(): the sweep decision matrix.
//
// 2026-09-05: the loop's herdr-sweep closed an IN-FLIGHT worker pane (omp
// mid-run inside the task's worktree reported agent_status "idle" because the
// pane wrapper polls the done-marker file, not the agent state machine). Sweep
// must stay session-scoped but panes it may close must be BOTH (a) sitting in a
// .devagent-worktrees checkout — automation-spawned workers only, never the
// operator's scratch/interactive panes in the same session — and (b) not
// running a worker CLI right now. (b) uses herdr's own pane process-info: a
// LIVE pane's foreground process is the worker binary (omp/pi/claude/opencode);
// an idle pane sits at its shell.
func FindStalePanes(cli CliRunner, session string, opts SweepOptions) []StalePane {
	var sweep config.HerdrSweepSettings
	if opts.Sweep != nil {
		sweep = *opts.Sweep
	} else {
		sweep = ResolveSweepSettings()
	}
	if SweepDenyReason(session, sweep) != "" {
		return nil
	}
	// Config/env `orphans` outranks the caller's flag; unset defers to it, which
	// is today's behavior (only the loop driver passes --orphans).
	orphans := sweep.Orphans != nil && *sweep.Orphans
	if sweep.Orphans == nil {
		orphans = opts.Orphans
	}
	res := cli.HerdrCli([]string{"--session", session, "pane", "list"}, 10_000)
	if res.Code != 0 {
		return nil
	}
	var envelope struct {
		Result *struct {
			Panes []paneRow `json:"panes"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &envelope); err != nil {
		return nil
	}
	var rows []paneRow
	if envelope.Result != nil {
		rows = envelope.Result.Panes
	}
	stale := []StalePane{}
	// Operator at the wheel (FR-VIS-10): herdr-side attach detection sets
	// PANE_ENV_OP_ATTACH in the pane env, so a sweep launched from that
	// terminal inherits it — while it is set, nothing in the session is
	// sweepable.
	attachEnv := os.Getenv(PaneEnvOpAttach)
	envAttached := attachEnv != "" && attachEnv != "0" && !strings.EqualFold(attachEnv, "false")
	// Roster lookup is lazy: only paid for when a worktree candidate exists.
	var runningPanes map[string]bool
	rosterLoaded := false
	for _, p := range rows {
		status := derefOr(p.AgentStatus, "unknown")
		cwd := p.cwd()
		if !strings.Contains(cwd, ".devagent-worktrees") {
			continue
		}
		paneID := p.paneID()
		// FR-VIS-10 (Q23): an operator at the wheel outranks every stale
		// signal — including the orphan class. The pane is reported with
		// reason `operator-attached` so a dry-run explains why it survived,
		// and SweepStalePanes never closes it.
		if envAttached {
			stale = append(stale, StalePane{
				WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
				AgentStatus: status, Reason: SweepReasonOperatorAttached,
			})
			continue
		}
		if !rosterLoaded {
			runningPanes = rosteredRunningPaneIDs(cli, session)
			rosterLoaded = true
		}
		if runningPanes[paneID] {
			stale = append(stale, StalePane{
				WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
				AgentStatus: status, Reason: SweepReasonOperatorAttached,
			})
			continue
		}
		if PaneForegroundWorker(cli, paneID) {
			// 2026-09-07: a live foreground worker is normally "leave it
			// alone" — but when its OWNER (the `herdr pane run` CLI the
			// dispatching `devagent task` spawned) is gone or detached from
			// the loop driver, nobody polls the run or closes the pane: the
			// driver died mid-task (2026-09-07 v11 OOM-kill orphaned pane w68
			// + omp 45472 for hours, burning tokens with no collector). Only
			// the loop driver may assert orphanhood (opts.orphans) — at an
			// iteration head the driver is synchronous, so a live worker it
			// does not own is definitionally a dead driver's leftover.
			// Operator manual tasks keep their shell in the ancestry and are
			// spared.
			if orphans && PaneRunOwnerOrphaned(paneID) {
				stale = append(stale, StalePane{
					WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
					AgentStatus: status, Reason: "orphaned-driver",
				})
			}
			continue
		}
		// No agent at all (bare shell in a worktree) => leftover; known agent
		// => only idle ones.
		if p.AgentStatus == nil {
			stale = append(stale, StalePane{
				WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
				AgentStatus: status, Reason: "no-agent",
			})
		} else if idleStatuses[status] {
			stale = append(stale, StalePane{
				WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
				AgentStatus: status, Reason: "agent-" + status,
			})
		}
	}
	return stale
}

// rosteredRunningPaneIDs mirrors rosteredRunningPaneIds(): pane ids the
// FR-VIS-02 roster reports as live (`state: "running"`).
func rosteredRunningPaneIDs(cli CliRunner, session string) map[string]bool {
	ids := map[string]bool{}
	for _, p := range ListSessionPanes(cli, session) {
		if p.State == "running" && p.PaneID != "" {
			ids[p.PaneID] = true
		}
	}
	return ids
}

// workerBinNames mirrors WORKER_BIN_NAMES: foreground argv0s that prove a pane
// is mid-run, not idle.
var workerBinNames = map[string]bool{
	"omp":       true,
	"pi":        true,
	"claude":    true,
	"opencode":  true,
	"opencode2": true,
}

// PaneForegroundWorker mirrors paneForegroundWorker(): true when the pane's
// foreground process is a worker CLI (live dispatch). Best-effort: a
// process-info failure returns false so the status-based sweep still applies —
// a pane we cannot inspect is treated like before.
func PaneForegroundWorker(cli CliRunner, paneID string) bool {
	if paneID == "" {
		return false
	}
	r := cli.HerdrCli([]string{"pane", "process-info", "--pane", paneID}, 5_000)
	if r.Code != 0 {
		return false
	}
	var envelope struct {
		Result *struct {
			ProcessInfo *struct {
				ForegroundProcesses []struct {
					Name  *string `json:"name"`
					Argv0 *string `json:"argv0"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &envelope); err != nil || envelope.Result == nil || envelope.Result.ProcessInfo == nil {
		return false
	}
	for _, proc := range envelope.Result.ProcessInfo.ForegroundProcesses {
		key := derefOr(proc.Name, derefOr(proc.Argv0, ""))
		if workerBinNames[strings.ToLower(key)] {
			return true
		}
	}
	return false
}

// HasLoopDriverAncestor mirrors hasLoopDriverAncestor(): true when any
// ancestor command of the pane-run owner names the live selfbuild loop
// driver — the Go `devagent loop` command (argv[0] is the built binary, so
// both `devagent-go loop` and `devagent loop` shapes match), or the retired
// bash selfbuild-loop.sh (still matched so panes owned by a pre-retirement
// driver from the soak window are never swept as orphans).
func HasLoopDriverAncestor(ancestryCommands []string) bool {
	for _, cmd := range ancestryCommands {
		if strings.Contains(cmd, "selfbuild-loop.sh") ||
			strings.Contains(cmd, "devagent-go loop") ||
			strings.Contains(cmd, "devagent loop") {
			return true
		}
	}
	return false
}

// PaneRunOwnerOrphaned mirrors paneRunOwnerOrphaned(): orphan check for one
// pane's live worker — find the `herdr pane run <paneId>` owner CLI process
// (spawned by the dispatching `devagent task`), walk its ppid ancestry via ps,
// and require a live selfbuild loop driver somewhere in it. Missing owner
// CLI = the poller died = orphaned. ps failures are conservative: without
// evidence the pane is left alone.
func PaneRunOwnerOrphaned(paneID string) bool {
	if paneID == "" {
		return false
	}
	pids := psPidsMatching("herdr.*pane run .*" + paneID)
	if len(pids) == 0 {
		return true // no owner CLI at all -> nothing polls this run
	}
	for _, pid := range pids {
		if HasLoopDriverAncestor(pidAncestryCommands(pid)) {
			return false // live driver owns it
		}
	}
	return true // owner exists but detached from any live driver
}

// psPidsMatching mirrors psPidsMatching(): pgrep -f for the pane-run owner
// CLI; empty on pgrep absence/failure.
//
// Test seam: DEVAGENT_SWEEP_OWNER_PIDS stubs the process table (pgrep output
// format, one pid per line) so the orphan path is deterministic without
// spawning real pane-run CLIs.
func psPidsMatching(pattern string) []int {
	if stubPids, ok := os.LookupEnv("DEVAGENT_SWEEP_OWNER_PIDS"); ok {
		return parsePidList(stubPids)
	}
	out, code := runProc("pgrep", []string{"-f", pattern})
	_ = code // pgrep exits 1 on "no match" — that is an answer, not a failure
	if code != 0 && strings.TrimSpace(out) == "" {
		return nil
	}
	return parsePidList(out)
}

func parsePidList(out string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && n > 1 {
			pids = append(pids, n)
		}
	}
	return pids
}

// runProc runs a process-table probe with a hard 5s cap and reports its stdout
// plus exit code (-1 on spawn failure/timeout).
func runProc(name string, args []string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.String(), -1
	}
	if err == nil {
		return stdout.String(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), exitErr.ExitCode()
	}
	return stdout.String(), -1
}

// pidAncestryCommands mirrors pidAncestryCommands(): walk the ppid chain from
// `pid` toward init, collecting each ancestor's command line. Depth-capped;
// ps failure yields nil (callers treat that as "no evidence" and stay
// conservative).
//
// Test seam: DEVAGENT_SWEEP_ANCESTRY_JSON = { "<pid>": ["cmd", ...] } — the
// stubbed ppid walk for orphan tests (no real ps in CI).
func pidAncestryCommands(pid int) []string {
	if stub, ok := os.LookupEnv("DEVAGENT_SWEEP_ANCESTRY_JSON"); ok {
		var m map[string][]string
		if err := json.Unmarshal([]byte(stub), &m); err != nil {
			return nil
		}
		return m[strconv.Itoa(pid)]
	}
	var commands []string
	current := pid
	for range 24 {
		out, code := runProc("ps", []string{"-o", "ppid=,command=", "-p", strconv.Itoa(current)})
		if code != 0 {
			break
		}
		line := strings.TrimSpace(out)
		if line == "" {
			break
		}
		sep := strings.IndexByte(line, ' ')
		if sep <= 0 {
			break
		}
		commands = append(commands, strings.TrimSpace(line[sep+1:]))
		ppid, err := strconv.Atoi(strings.TrimSpace(line[:sep]))
		if err != nil || ppid <= 1 {
			break
		}
		current = ppid
	}
	return commands
}

// SweepStalePanes mirrors sweepStalePanes(): close every stale pane workspace
// in `session`. Returns the panes it closed beside the candidates it
// deliberately spared (reason SweepReasonOperatorAttached, never closed). A
// disabled or session-denied sweep returns nothing.
func SweepStalePanes(cli CliRunner, session string, opts SweepOptions, dryRun bool) []StalePane {
	stale := FindStalePanes(cli, session, opts)
	if dryRun {
		return stale
	}
	for _, s := range stale {
		if s.WorkspaceID == "" {
			continue
		}
		if s.Reason == SweepReasonOperatorAttached {
			continue
		}
		closeWorkspace(cli, s.WorkspaceID, session)
	}
	return stale
}
