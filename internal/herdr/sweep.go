package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
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
//
// 2026-09-13 (a) stopped proving anything on its own and (b) was always false:
// the process-info probe went out unscoped (no --session), so herdr answered
// for its own default session and every live worker read as idle — while the
// roster, which shares that probe, kept claiming the same pane was stale. The
// matrix now probes once, in session, and orders the exemptions so the orphan
// class is reachable: scope (where the pane may be swept at all) → operator at
// the wheel → orphan evidence → roster spare → status classes.
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
	// Roster lookup is lazy: only paid for when a worktree candidate is left
	// standing after the liveness probe.
	var runningPanes map[string]bool
	rosterLoaded := false
	for _, p := range rows {
		status := derefOr(p.AgentStatus, "unknown")
		paneID := p.paneID()
		// FR-VIS-07 automation ownership. A `.devagent-worktrees` checkout is
		// itself proof devagent spawned the pane (the dispatcher cd's the
		// run into the task's worktree), so the status classes below reach it.
		// Anywhere else — the main checkout, where the research/PO phases run
		// their dispatches (FR-VIS-06) beside the operator's own panes — the
		// cwd proves nothing, so the pane only joins the sweep when the
		// orphan class is armed, and then only on positive dispatch evidence.
		automation := strings.Contains(p.cwd(), ".devagent-worktrees")
		if !automation && !orphans {
			continue
		}
		// Q23: an operator at the wheel outranks every stale signal, including
		// the orphan class. The pane is reported with reason
		// `operator-attached` so a dry-run explains why it survived, and
		// SweepStalePanes never closes it.
		if envAttached {
			stale = append(stale, StalePane{
				WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
				AgentStatus: status, Reason: SweepReasonOperatorAttached,
			})
			continue
		}
		info := paneProcessInfoCli(cli, session, paneID)
		if paneWorkerRunning(info) {
			// 2026-09-07: a live foreground worker is normally "leave it
			// alone" — but when the run has no live collector nobody polls
			// the done marker or closes the workspace: the driver died
			// mid-task (2026-09-07 v11 OOM-kill orphaned pane w68 + omp
			// 45472 for hours, burning tokens with no collector). Only the
			// loop driver may assert orphanhood (opts.orphans) — at an
			// iteration head the driver is synchronous, so a live worker it
			// does not own is definitionally a dead driver's leftover.
			//
			// This outranks the roster spare on purpose: the FR-VIS-02
			// "running" state is derived from this same probe (#317), so a
			// pane whose roster says running is exactly what an orphan looks
			// like — sparing there first left the class unreachable. An
			// operator really at the wheel is the env gate above, not the
			// roster.
			//
			// Collector evidence is process-wide: no pane id joins any
			// surviving argv, so ANY dispatcher still owned by a live loop
			// driver spares every live pane in the session. Dispatch evidence
			// (the capture contract on the worker's own fds) is per-pane and
			// narrows the non-worktree class: an operator's hand-run `omp`
			// never carries it, so scratch panes are structurally spared.
			if orphans && PaneRunOwnerOrphaned(paneID) &&
				(automation || paneDispatchEvidence(paneID, info)) {
				stale = append(stale, StalePane{
					WorkspaceID: p.workspaceID(), PaneID: paneID, Label: p.label(),
					AgentStatus: status, Reason: "orphaned-driver",
				})
			}
			continue
		}
		if !automation {
			// Outside the worktrees and not a live orphan: a pane at its own
			// shell in the main checkout is the operator's window, and the
			// sweep has no evidence it ever belonged to anyone else.
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

// paneRunPid: one foreground process of a pane, with the pid needed to check
// its file descriptors (the capture contract) for dispatch evidence.
type paneRunPid struct {
	Name  *string `json:"name"`
	Argv0 *string `json:"argv0"`
	Pid   *int    `json:"pid"`
}

func (p paneRunPid) key() string {
	if p.Name != nil && *p.Name != "" {
		return *p.Name
	}
	if p.Argv0 != nil {
		return *p.Argv0
	}
	return ""
}

// paneRunProcessInfo mirrors the `pane process-info` reply (the subset the
// sweep consumes).
type paneRunProcessInfo struct {
	ForegroundProcesses []paneRunPid `json:"foreground_processes"`
}

// paneProcessInfoCli fetches one pane's process-info in session, or nil when
// the probe fails (best-effort contract).
func paneProcessInfoCli(cli CliRunner, session, paneID string) *paneRunProcessInfo {
	if paneID == "" {
		return nil
	}
	r := cli.HerdrCli([]string{"--session", session, "pane", "process-info", "--pane", paneID}, 5_000)
	if r.Code != 0 {
		return nil
	}
	var envelope struct {
		Result *struct {
			ProcessInfo *paneRunProcessInfo `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &envelope); err != nil || envelope.Result == nil || envelope.Result.ProcessInfo == nil {
		return nil
	}
	return envelope.Result.ProcessInfo
}

// PaneForegroundWorker mirrors paneForegroundWorker(): true when the pane's
// foreground process is a worker CLI (live dispatch). Best-effort: a
// process-info failure returns false so the status-based sweep still applies —
// a pane we cannot inspect is treated like before.
//
// `session` is load-bearing: herdr scopes every subcommand, so an unscoped
// probe answers for whatever session the binary defaults to. For a devagent
// pane that is "no such pane", which made liveness read false for every pane
// (2026-09-13) and left both the busy-pane guard and the orphan class inert.
func PaneForegroundWorker(cli CliRunner, session, paneID string) bool {
	return paneWorkerRunning(paneProcessInfoCli(cli, session, paneID))
}

func paneWorkerRunning(info *paneRunProcessInfo) bool {
	if info == nil {
		return false
	}
	for _, proc := range info.ForegroundProcesses {
		if workerBinNames[strings.ToLower(proc.key())] {
			return true
		}
	}
	return false
}

// captureFileRe matches the capture-contract paths of a pane run:
// `<tmp>/devagent-herdr-<n>/{out,err,done}`, written by
// RunCommandInHerdrPane's script (herdr.go) and removed by its deferred
// cleanup — so the descriptor is open exactly while a dispatch owns the run.
var captureFileRe = regexp.MustCompile(`/devagent-herdr-\d+/(?:out|err|done)`)

// paneDispatchEvidence is the per-pane proof that devagent started this pane's
// worker: one of its foreground processes holds the capture contract on stdout
// or stderr. `pane run` types the script into the pane's interactive shell and
// exits, so no argv in the pane's tree names the contract (verified live:
// `omp -> -zsh -> herdr --session <s> server`), and the `herdr pane run`
// process the 2026-09-07 probe looked for is never in the table at all. A
// hand-run operator worker points at the pane tty and never matches — which is
// what keeps a pane outside an automation worktree sweepable only when devagent
// really dispatched it. Best-effort: a failed probe is no evidence.
//
// Test seam: DEVAGENT_SWEEP_PANE_CAPTURE_JSON = { "<pane id>": "<fd paths>" } —
// the same pane-keyed shape as the process-info fixtures, so dispatch evidence
// is deterministic in CI without real panes (the pid-keyed form would make a
// fixture's pid field load-bearing for two sweeps that disagree about it).
//
// ponytail: the ceiling is the file-name shape — if the capture contract's
// scratch prefix or names change, captureFileRe has to follow. A dispatch
// manifest (pane id -> capture dir -> dispatcher pid), written by openPane and
// read here, would retire both the regex and the fd probe.
func paneDispatchEvidence(paneID string, info *paneRunProcessInfo) bool {
	if info == nil {
		return false
	}
	if stub, ok := os.LookupEnv("DEVAGENT_SWEEP_PANE_CAPTURE_JSON"); ok {
		var m map[string]string
		if err := json.Unmarshal([]byte(stub), &m); err != nil {
			return false
		}
		return captureFileRe.MatchString(m[paneID])
	}
	for _, proc := range info.ForegroundProcesses {
		if proc.Pid == nil || *proc.Pid <= 1 {
			continue
		}
		if captureFileRe.MatchString(procFdsCapture(*proc.Pid)) {
			return true
		}
	}
	return false
}

// procFdsCapture runs the lsof fd probe for pid and returns the `n`-prefixed
// path lines of fd 1/2 joined — the descriptor targets that carry the capture
// contract. Empty on lsof absence/failure (conservative: no evidence).
func procFdsCapture(pid int) string {
	out, _ := runProc("lsof", []string{"-a", "-p", strconv.Itoa(pid), "-d", "1", "-d", "2", "-Fn"})
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "n") && line != "n" {
			paths = append(paths, line[1:])
		}
	}
	return strings.Join(paths, " ")
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

// paneRunOwnerPattern is the pgrep -f pattern for a pane run's OWNER: the
// dispatching devagent CLI (`devagent task --prompt …`, `devagent pane-run …`)
// with or without the driver's `timeout <secs>` wrapper.
//
// 2026-09-13: it used to match `herdr.*pane run .*<paneId>`. That process does
// not exist while a pane runs: herdr types the script into the pane's
// interactive shell and exits, and the pane tree reparents under the herdr
// server (verified against a live in-flight dispatch — empty pgrep, parent
// chain `omp -> -zsh -> herdr --session <s> server -> launchd`). So the probe
// matched nothing for every pane, PaneRunOwnerOrphaned always took its
// "no owner CLI -> orphaned" exit, and the HasLoopDriverAncestor spare below
// was unreachable. The CLI that survives the whole run — the one polling the
// done marker — is the dispatcher.
//
// Anchored at argv0 on purpose: an `omp -p <prompt>` command line quotes
// devagent commands (ticket text does), and an unanchored pattern would let a
// worker impersonate its own collector.
const paneRunOwnerPattern = `^(timeout [0-9]+ )?[^ ]*devagent(-go)? (task|pane-run)( |$)`

// PaneRunOwnerOrphaned mirrors paneRunOwnerOrphaned(): orphan check for a live
// pane worker — find the dispatching devagent CLI that owns the run, walk its
// ppid ancestry via ps, and require a live selfbuild loop driver somewhere in
// it. Missing owner CLI = the poller died with its driver = orphaned.
//
// Both probes have to ANSWER before anything is called an orphan. A pgrep that
// could not run (absent binary, the 5s cap, a rejected pattern) or an ancestry
// walk that never completed is no evidence, and asserting orphanhood on no
// evidence closes live workers — the exact failure class the 2026-09-05 and
// 2026-09-07 regressions were about. So the reaper fails toward leaving a pane
// running: an unanswered probe spares the session.
//
// The pane id cannot join the match: RunCommandInHerdrPane opens the pane
// inside the dispatching process, so no argv anywhere carries the pair.
// Ownership is therefore asserted process-wide — any dispatcher still hanging
// off a live driver spares every live pane in the session.
//
// ponytail: ceiling is per-session attribution, not correctness of the spare:
// the error direction is "leave a leftover running", never "close a run
// somebody is still collecting". Upgrading to per-pane ownership needs a
// dispatch manifest on disk (pane id -> capture dir -> dispatcher pid) written
// by openPane and read here.
func PaneRunOwnerOrphaned(paneID string) bool {
	if paneID == "" {
		return false
	}
	pids, answered := psPidsMatching(paneRunOwnerPattern)
	if !answered {
		return false // the process table never answered: nothing to assert
	}
	if len(pids) == 0 {
		return true // it answered "no collector": nothing polls this run
	}
	for _, pid := range pids {
		ancestry, walked := pidAncestryCommands(pid)
		if !walked {
			return false // an uninspectable collector is not a dead one
		}
		if HasLoopDriverAncestor(ancestry) {
			return false // live driver owns it
		}
	}
	return true // owner exists but detached from any live driver
}

// psPidsMatching mirrors psPidsMatching(): pgrep -f over the process table.
// The second result is whether the probe ANSWERED: exit 1 with no output is
// pgrep's definitive "no match" (answered, empty), while an absent binary, the
// 5s cap, or a rejected pattern is no answer — and a caller that read "no
// answer" as "no collector" would reap every live pane on a host that cannot
// inspect its own process table. Its one caller asks for the pane-run OWNER,
// which since 2026-09-13 is the dispatching devagent CLI rather than the
// transient `herdr pane run` client (see paneRunOwnerPattern).
//
// Test seam: DEVAGENT_SWEEP_OWNER_PIDS_JSON = { "<pattern>": "<pids>" } (pids
// in pgrep output format, one per line) stubs the process table so the
// collector path is deterministic without spawning real dispatchers. It is
// keyed BY the pattern asked for, so a probe retargeted away from
// paneRunOwnerPattern misses — and misses as an unanswered probe, never as a
// clean "no match" — and its test goes red instead of a global stub quietly
// handing out the old answer; per-pane dispatch evidence is stubbed separately,
// keyed by pane (DEVAGENT_SWEEP_PANE_CAPTURE_JSON).
func psPidsMatching(pattern string) ([]int, bool) {
	if stub, ok := os.LookupEnv("DEVAGENT_SWEEP_OWNER_PIDS_JSON"); ok {
		var byPattern map[string]string
		if err := json.Unmarshal([]byte(stub), &byPattern); err != nil {
			return nil, false
		}
		raw, answered := byPattern[pattern]
		if !answered {
			return nil, false
		}
		return parsePidList(raw), true
	}
	out, code := runProc("pgrep", []string{"-f", pattern})
	switch {
	case code == 0:
		return parsePidList(out), true
	case code == 1 && strings.TrimSpace(out) == "":
		return nil, true // "no match" is an answer, not a failure
	default:
		return nil, false // spawn failure, timeout, or pgrep usage error
	}
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
// `pid` toward init, collecting each ancestor's command line. Depth-capped.
// The second result is whether the walk ANSWERED: reaching init (ppid <= 1), or
// a ps reporting the process as already gone, completes it; a ps that could not
// run or output it cannot parse does not, and the caller leaves the pane alone.
//
// Test seam: DEVAGENT_SWEEP_ANCESTRY_JSON = { "<pid>": ["cmd", ...] } — the
// stubbed ppid walk for orphan tests (no real ps in CI). A pid key present with
// an empty list is a walk that completed with no driver above it; a missing key
// is an unanswered probe.
func pidAncestryCommands(pid int) ([]string, bool) {
	if stub, ok := os.LookupEnv("DEVAGENT_SWEEP_ANCESTRY_JSON"); ok {
		var m map[string][]string
		if err := json.Unmarshal([]byte(stub), &m); err != nil {
			return nil, false
		}
		commands, walked := m[strconv.Itoa(pid)]
		return commands, walked
	}
	var commands []string
	current := pid
	for range 24 {
		out, code := runProc("ps", []string{"-o", "ppid=,command=", "-p", strconv.Itoa(current)})
		if code != 0 {
			// ps exits 1 with no row for a pid that already died: that is an
			// answer (no driver above it). Anything else inspected nothing.
			if code == 1 && strings.TrimSpace(out) == "" {
				break
			}
			return commands, false
		}
		line := strings.TrimSpace(out)
		if line == "" {
			break // accepted query, no row: the process is gone
		}
		sep := strings.IndexByte(line, ' ')
		if sep <= 0 {
			return commands, false // unparseable row: no evidence either way
		}
		commands = append(commands, strings.TrimSpace(line[sep+1:]))
		ppid, err := strconv.Atoi(strings.TrimSpace(line[:sep]))
		if err != nil {
			return commands, false
		}
		if ppid <= 1 {
			break // reached init/launchd: the chain is complete
		}
		current = ppid
	}
	return commands, true
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
