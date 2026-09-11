package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/config"
)

// fakeCli serves herdr CLI replies from fixture JSON files, mirroring the
// vitest stub CLIs (test/herdr-sweep.test.ts): `pane list` from a pane-list
// fixture, `pane process-info` per pane, `agent list` from a roster fixture,
// and `workspace close` logged instead of executed.
type fakeCli struct {
	t *testing.T
	// paneList: testdata path (or inline JSON) for `pane list`.
	paneList string
	// agentList: testdata path (or inline JSON) for `agent list`.
	agentList string
	// procInfo: pane id -> testdata path (or inline JSON) for `pane
	// process-info`.
	procInfo map[string]string
	// failList makes pane list/agent list exit 1 (server_not_running).
	failList bool
	// closed records workspace close invocations.
	closed []string
	// calls records every argv (joined with spaces) in order.
	calls []string
}

func newFakeCli(t *testing.T) *fakeCli {
	t.Helper()
	return &fakeCli{t: t, procInfo: map[string]string{}}
}

// fixture reads a testdata file relative to this package.
func (f *fakeCli) fixture(rel string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", rel))
	if err != nil {
		f.t.Fatalf("read fixture %s: %v", rel, err)
	}
	return string(data)
}

func (f *fakeCli) HerdrCli(args []string, timeoutMs int) CliResult {
	if len(args) > 1 && args[0] == "--session" {
		args = args[2:]
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) >= 2 && args[0] == "pane" && args[1] == "list":
		if f.failList {
			return CliResult{Code: 1, Stderr: "server_not_running"}
		}
		body := f.paneList
		if strings.HasPrefix(body, "testdata/") {
			body = f.fixture(strings.TrimPrefix(body, "testdata/"))
		}
		return CliResult{Code: 0, Stdout: body}
	case len(args) >= 2 && args[0] == "agent" && args[1] == "list":
		if f.failList {
			return CliResult{Code: 1, Stderr: "server_not_running"}
		}
		body := f.agentList
		if strings.HasPrefix(body, "testdata/") {
			body = f.fixture(strings.TrimPrefix(body, "testdata/"))
		}
		return CliResult{Code: 0, Stdout: body}
	case len(args) >= 2 && args[0] == "pane" && args[1] == "process-info":
		pane := ""
		for i, a := range args {
			if a == "--pane" && i+1 < len(args) {
				pane = args[i+1]
			}
		}
		body := f.procInfo[pane]
		if strings.HasPrefix(body, "testdata/") {
			body = f.fixture(strings.TrimPrefix(body, "testdata/"))
		}
		return CliResult{Code: 0, Stdout: body}
	case len(args) >= 2 && args[0] == "workspace" && args[1] == "close":
		f.closed = append(f.closed, args[2])
		return CliResult{Code: 0, Stdout: `{"id":"x","result":{}}`}
	case len(args) >= 2 && args[0] == "workspace" && args[1] == "list":
		// The pane-list fixtures double as workspace_list answers so
		// EnsureHerdrServer short-circuits.
		return CliResult{Code: 0, Stdout: `{"id":"x","result":{"type":"workspace_list","workspaces":[]}}`}
	default:
		return CliResult{Code: 2, Stderr: "fakeCli: unsupported " + strings.Join(args, " ")}
	}
}

// sweepEnabled is the default HerdrSweepSettings (TS { enabled: true,
// denySessions: [] }).
func sweepEnabled() *config.HerdrSweepSettings {
	s := config.HerdrSweepSettings{Enabled: true}
	return &s
}

func boolPtr(b bool) *bool { return &b }

// orphanSeams installs the DEVAGENT_SWEEP_* test seams.
func orphanSeams(t *testing.T, ownerPids string, ancestry map[string][]string) {
	t.Helper()
	t.Setenv("DEVAGENT_SWEEP_OWNER_PIDS", ownerPids)
	if ancestry != nil {
		data, err := json.Marshal(ancestry)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("DEVAGENT_SWEEP_ANCESTRY_JSON", string(data))
	}
}

// ---------- Sweep safety (FR-VIS-07) ----------

func TestSweepNeverClosesLiveWorkerEvenWhenIdle(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SWEEP", "")
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty", got)
	}
}

func TestSweepClosesIdleWorktreePaneWithIdleShell(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].PaneID != "wX:p1" || got[0].Reason != "agent-idle" {
		t.Fatalf("stale = %+v, want [wX:p1 agent-idle]", got)
	}
	if got[0].WorkspaceID != "wX" || got[0].Label != "TASK-abc-a1" || got[0].AgentStatus != "idle" {
		t.Fatalf("row fields = %+v", got[0])
	}
}

func TestSweepNeverListsOperatorScratchPanes(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-scratch.json"
	cli.procInfo["wV:p1"] = "testdata/sweep/process-info-zsh.json"
	cli.procInfo["wV:p2"] = "testdata/sweep/process-info-zsh.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty", got)
	}
}

func TestSweepClosesAgentlessLeftoverInWorktree(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-agentless.json"
	cli.procInfo["wT:p1"] = "testdata/sweep/process-info-zsh.json"
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "no-agent" {
		t.Fatalf("stale = %+v, want [no-agent]", got)
	}
}

// ---------- Orphaned-driver sweep (2026-09-07 orphan-pane class) ----------

func TestOrphanDefaultSweepLeavesLiveWorkerAlone(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		"4242": {"timeout 7200 devagent-go task"},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty (no --orphans)", got)
	}
}

func TestOrphanClosesLiveWorkerDetachedFromDriver(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		// Owner CLI alive but its ancestry has no loop driver (driver died).
		"4242": {"timeout 7200 devagent-go task", "launchd"},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" || got[0].PaneID != "wX:p1" {
		t.Fatalf("stale = %+v, want [wX:p1 orphaned-driver]", got)
	}
}

func TestOrphanSparesLiveWorkerWithLoopDriverAncestor(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		"4242": {
			"bash /repo/scripts/selfbuild-loop.sh",
			"timeout 7200 devagent-go task",
		},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty (live driver owns it)", got)
	}
}

func TestOrphanSparesLiveWorkerWithGoDriverAncestor(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "4242\n", map[string][]string{
		// The production driver after #205 is `devagent-go loop` (make
		// loop-start); a pane owned by it is live, not orphaned.
		"4242": {
			"./devagent-go loop",
			"timeout 7200 devagent-go task",
		},
	})
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 0 {
		t.Fatalf("stale = %v, want empty (live Go driver owns it)", got)
	}
}

func TestOrphanClosesLiveWorkerWithNoOwnerCLI(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil)
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true, Sweep: sweepEnabled()})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" {
		t.Fatalf("stale = %+v, want [orphaned-driver]", got)
	}
}

func TestSweepOrphansConfigArmsClassWithoutCallerFlag(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-live-worker.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil) // no pane-run owner at all
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: sweepEnabled()}); len(got) != 0 {
		t.Fatalf("default = %v, want empty", got)
	}
	sweep := config.HerdrSweepSettings{Enabled: true, Orphans: boolPtr(true)}
	got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep})
	if len(got) != 1 || got[0].Reason != "orphaned-driver" {
		t.Fatalf("config-armed = %+v, want [orphaned-driver]", got)
	}
}

// ---------- Deny toggle (FR-VIS-10 / Q23) ----------

func TestEnvSweepToggleStopsSweepBeforeListing(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	t.Setenv("DEVAGENT_HERDR_SWEEP", "0")
	got := FindStalePanes(cli, "devagent", SweepOptions{})
	if len(got) != 0 {
		t.Fatalf("find = %v, want empty", got)
	}
	if got := SweepStalePanes(cli, "devagent", SweepOptions{}, false); len(got) != 0 {
		t.Fatalf("sweep = %v, want empty", got)
	}
	if len(cli.calls) != 0 {
		t.Fatalf("sweep listed panes despite toggle: %v", cli.calls)
	}
}

func TestConfigSweepEnabledFalseIsSameSwitch(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	sweep := config.HerdrSweepSettings{Enabled: false}
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep}); len(got) != 0 {
		t.Fatalf("stale = %v, want empty", got)
	}
}

func TestDeniedSessionNeverListedOthersStillSweep(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	sweep := config.HerdrSweepSettings{Enabled: true, DenySessions: []string{"devagent"}}
	if got := FindStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep}); len(got) != 0 {
		t.Fatalf("devagent = %v, want empty", got)
	}
	if got := SweepStalePanes(cli, "devagent", SweepOptions{Sweep: &sweep}, false); len(got) != 0 {
		t.Fatalf("sweep devagent = %v, want empty", got)
	}
	if got := FindStalePanes(cli, "devagent-ci", SweepOptions{Sweep: &sweep}); len(got) != 1 || got[0].PaneID != "wX:p1" {
		t.Fatalf("devagent-ci = %+v, want [wX:p1]", got)
	}
}

func TestSweepDenyReasonNamesBound(t *testing.T) {
	cases := []struct {
		session string
		sweep   config.HerdrSweepSettings
		want    string
	}{
		{"devagent", config.HerdrSweepSettings{Enabled: true}, ""},
		{"devagent", config.HerdrSweepSettings{Enabled: false, DenySessions: []string{"devagent"}}, "disabled"},
		{"devagent", config.HerdrSweepSettings{Enabled: true, DenySessions: []string{"devagent", "other"}}, "session-denied"},
		{"other", config.HerdrSweepSettings{Enabled: true, DenySessions: []string{"devagent"}}, ""},
	}
	for _, tc := range cases {
		if got := SweepDenyReason(tc.session, tc.sweep); got != tc.want {
			t.Errorf("SweepDenyReason(%q) = %q, want %q", tc.session, got, tc.want)
		}
	}
}

// ---------- Operator-attach exemption (FR-VIS-10) ----------

func TestRosterRunningPaneSparedAndReported(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	found := FindStalePanes(cli, "devagent", SweepOptions{})
	if len(found) != 1 || found[0].Reason != SweepReasonOperatorAttached {
		t.Fatalf("found = %+v, want [operator-attached]", found)
	}
	swept := SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(swept) != 1 || swept[0].Reason != SweepReasonOperatorAttached {
		t.Fatalf("swept = %+v, want [operator-attached]", swept)
	}
	if len(cli.closed) != 0 {
		t.Fatalf("closed = %v, want none", cli.closed)
	}
}

func TestNonLiveRosterDoesNotExemptPane(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-idle.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	if got := FindStalePanes(cli, "devagent", SweepOptions{}); len(got) != 1 || got[0].Reason != "agent-idle" {
		t.Fatalf("found = %+v, want [agent-idle]", got)
	}
	SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(cli.closed) != 1 || cli.closed[0] != "wX" {
		t.Fatalf("closed = %v, want [wX]", cli.closed)
	}
}

func TestOperatorAttachedEnvSparesWholeSession(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-two-idle.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	cli.procInfo["wY:p1"] = "testdata/sweep/process-info-zsh.json"
	t.Setenv(PaneEnvOpAttach, "1")
	swept := SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(swept) != 2 {
		t.Fatalf("swept = %+v, want 2 spared rows", swept)
	}
	for _, s := range swept {
		if s.Reason != SweepReasonOperatorAttached {
			t.Fatalf("row reason = %q, want operator-attached", s.Reason)
		}
	}
	if len(cli.closed) != 0 {
		t.Fatalf("closed = %v, want none", cli.closed)
	}
	// A false-ish flag value is not an exemption.
	t.Setenv(PaneEnvOpAttach, "0")
	if got := FindStalePanes(cli, "devagent", SweepOptions{}); len(got) != 2 || got[0].Reason != "agent-idle" || got[1].Reason != "agent-idle" {
		t.Fatalf("found with =0 = %+v, want [agent-idle agent-idle]", got)
	}
}

func TestExemptionOutranksOrphanClass(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-omp.json"
	orphanSeams(t, "", nil) // owner gone -> orphan candidate
	got := FindStalePanes(cli, "devagent", SweepOptions{Orphans: true})
	if len(got) != 1 || got[0].Reason != SweepReasonOperatorAttached {
		t.Fatalf("stale = %+v, want [operator-attached]", got)
	}
}

// ---------- SweepStalePanes close behavior + dry-run ----------

func TestSweepStalePanesClosesAndDryRunDoesNot(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	got := SweepStalePanes(cli, "devagent", SweepOptions{}, true)
	if len(got) != 1 || got[0].Reason != "agent-idle" {
		t.Fatalf("dry-run = %+v", got)
	}
	if len(cli.closed) != 0 {
		t.Fatalf("dry-run closed %v", cli.closed)
	}
	SweepStalePanes(cli, "devagent", SweepOptions{}, false)
	if len(cli.closed) != 1 || cli.closed[0] != "wX" {
		t.Fatalf("closed = %v, want [wX]", cli.closed)
	}
}

// ---------- FR-VIS-02 roster / FR-VIS-03 attach ----------

func TestListSessionPanesMapsRosterRows(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	panes := ListSessionPanes(cli, "testsession")
	if len(panes) != 3 {
		t.Fatalf("panes = %d rows, want 3", len(panes))
	}
	var abc, old, shell *SessionPaneInfo
	for i := range panes {
		switch panes[i].PaneID {
		case "pane-abc":
			abc = &panes[i]
		case "pane-old":
			old = &panes[i]
		case "pane-shell":
			shell = &panes[i]
		}
	}
	if abc == nil || old == nil || shell == nil {
		t.Fatalf("missing rows: %+v", panes)
	}
	// Worker = label prefix before the FIRST '-a' ('TASK-abc-a1' -> 'TASK');
	// contract-literal parsing, even when the task id itself is hyphenated.
	if abc.TaskID != "TASK-abc" || abc.Worker != "TASK" || abc.Role != "worker" ||
		abc.State != "running" || abc.AgentStatus != "working" ||
		abc.StartedAt != "2026-09-04T10:00:00Z" || abc.WorkspaceID != "ws-abc" {
		t.Fatalf("abc = %+v", abc)
	}
	if !strings.Contains(abc.Cwd, ".devagent-worktrees") {
		t.Fatalf("abc cwd = %q", abc.Cwd)
	}
	if old.TaskID != "TASK-old" {
		t.Fatalf("old taskId = %q", old.TaskID)
	}
	// Scratch (non-worktree) pane: no task semantics -> empty taskId, worker
	// falls back to 'unknown' (label carries no attempt suffix).
	if shell.TaskID != "" || shell.Worker != "unknown" || shell.State != "idle" {
		t.Fatalf("shell = %+v", shell)
	}
}

// Regression (2026-09-11, #317): headless worker invocations (`omp -p --mode
// json` via pane-run) never advance herdr's agent_status past "idle", so the
// TUI rendered LIVE workers as "● idle". A pane whose foreground process is a
// worker binary must map to "running" regardless of agent_status; an idle pane
// at its shell keeps the status-derived state.
func TestListSessionPanesUpgradesLiveHeadlessWorker(t *testing.T) {
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-headless-worker.json"
	cli.procInfo["pane-live"] = "testdata/sweep/process-info-omp.json"
	cli.procInfo["pane-dead"] = "testdata/sweep/process-info-zsh.json"
	panes := ListSessionPanes(cli, "")
	if len(panes) != 2 {
		t.Fatalf("panes = %d rows, want 2", len(panes))
	}
	var live, dead *SessionPaneInfo
	for i := range panes {
		switch panes[i].PaneID {
		case "pane-live":
			live = &panes[i]
		case "pane-dead":
			dead = &panes[i]
		}
	}
	if live == nil || dead == nil {
		t.Fatalf("missing rows: %+v", panes)
	}
	// The exact false-idle shape: agent_status "idle", live omp foreground.
	if live.AgentStatus != "idle" || live.State != "running" {
		t.Fatalf("live = agentStatus %q state %q, want idle/running", live.AgentStatus, live.State)
	}
	// Same idle status, shell foreground: stays status-derived.
	if dead.State != "stale" {
		t.Fatalf("dead = state %q, want stale", dead.State)
	}
	// process-info is only paid for on idle/unknown rows, and the working
	// rows of the two-pane fixture never trigger a probe.
	probed := false
	for _, c := range cli.calls {
		if strings.Contains(c, "process-info") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("no process-info probe recorded: %v", cli.calls)
	}
}

func TestListSessionPanesStripsRecoverySuffix(t *testing.T) {
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-recovery-suffix.json"
	panes := ListSessionPanes(cli, "")
	if len(panes) != 1 || panes[0].TaskID != "TASK-x" {
		t.Fatalf("panes = %+v, want TASK-x", panes)
	}
}

func TestListSessionPanesFailsClosed(t *testing.T) {
	cli := newFakeCli(t)
	cli.failList = true
	if got := ListSessionPanes(cli, ""); got != nil {
		t.Fatalf("failed CLI = %v, want nil", got)
	}
	cli.failList = false
	cli.agentList = "not json at all"
	if got := ListSessionPanes(cli, ""); len(got) != 0 {
		t.Fatalf("junk stdout = %v, want empty", got)
	}
}

func TestAttachCommandForRosteredTask(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	if got := AttachCommandFor(cli, "TASK-abc", ""); got != "herdr --session testsession agent attach pane-abc" {
		t.Fatalf("attach = %q", got)
	}
	if got := AttachCommandFor(cli, "TASK-missing", ""); got != "" {
		t.Fatalf("missing = %q, want empty", got)
	}
}

func TestOperatorAttachTraceLiveOnly(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	if !OperatorAttachTrace(cli, "TASK-abc", "") {
		t.Error("TASK-abc should be live")
	}
	if OperatorAttachTrace(cli, "TASK-old", "") {
		t.Error("TASK-old is stale, not live")
	}
	if OperatorAttachTrace(cli, "TASK-missing", "") {
		t.Error("TASK-missing should be absent")
	}
}

// ---------- Sweep render (herdr-sweep stdout contract) ----------

func TestRunHerdrSweepRenderLines(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", true, false) })
	want := "[stale] wX:p1 (TASK-abc-a1) status=idle reason=agent-idle\n" +
		"1 pane(s) found in session \"devagent\"\n"
	if out != want {
		t.Fatalf("dry-run stdout =\n%q\nwant\n%q", out, want)
	}
	out = captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", false, false) })
	want = "[closed] wX:p1 (TASK-abc-a1) status=idle reason=agent-idle\n" +
		"1 pane(s) closed in session \"devagent\"\n"
	if out != want {
		t.Fatalf("sweep stdout =\n%q\nwant\n%q", out, want)
	}
}

func TestRunHerdrSweepDisabledRendersBound(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SWEEP", "0")
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", false, false) })
	want := "[devagent] sweep disabled (herdr.sweep.enabled / DEVAGENT_HERDR_SWEEP=0)\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunHerdrSweepSparedLines(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-idle-worktree.json"
	cli.agentList = "testdata/sweep/agent-list-running.json"
	cli.procInfo["wX:p1"] = "testdata/sweep/process-info-zsh.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", true, false) })
	want := "[spared] wX:p1 (TASK-abc-a1) status=idle reason=operator-attached\n" +
		"0 pane(s) found in session \"devagent\"; 1 spared (operator-attached)\n"
	if out != want {
		t.Fatalf("stdout =\n%q\nwant\n%q", out, want)
	}
}

func TestRunHerdrSweepNoStalePanes(t *testing.T) {
	cli := newFakeCli(t)
	cli.paneList = "testdata/sweep/pane-list-scratch.json"
	cli.procInfo["wV:p1"] = "testdata/sweep/process-info-zsh.json"
	cli.procInfo["wV:p2"] = "testdata/sweep/process-info-zsh.json"
	out := captureStdout(t, func() { _ = RunHerdrSweep(cli, "devagent", false, false) })
	want := "[devagent] no stale panes\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

// ---------- sessions / attach render (FR-VIS-02) ----------

func TestRunSessionsTableAndJSON(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	out := captureStdout(t, func() { _ = RunSessions(cli, false) })
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("table = %q", out)
	}
	if !strings.HasPrefix(lines[0], "PANE") || !strings.Contains(lines[0], "ATTACH") {
		t.Fatalf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "pane-abc") || !strings.Contains(lines[1], "herdr --session testsession agent attach pane-abc") {
		t.Fatalf("row1 = %q", lines[1])
	}
	if !strings.Contains(lines[3], "pane-shell") || !strings.Contains(lines[3], "-") {
		t.Fatalf("row3 = %q", lines[3])
	}
	jsonOut := captureStdout(t, func() { _ = RunSessions(cli, true) })
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil {
		t.Fatalf("sessions --json not valid JSON: %v\n%s", err, jsonOut)
	}
	if len(parsed) != 3 || parsed[0]["paneId"] != "pane-abc" || parsed[0]["taskId"] != "TASK-abc" {
		t.Fatalf("json rows = %+v", parsed)
	}
	// Key order is the declared TS contract.
	if !strings.Contains(jsonOut, `"taskId"`) || strings.Index(jsonOut, `"taskId"`) > strings.Index(jsonOut, `"paneId"`) {
		t.Fatalf("json key order wrong: %s", jsonOut)
	}
}

func TestRunSessionsEmpty(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = `{"id":"x","result":{"agents":[]}}`
	out := captureStdout(t, func() { _ = RunSessions(cli, false) })
	want := "no worker panes in session \"testsession\"\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunAttachUnknownTask(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	repo := t.TempDir()
	code := RunAttach(cli, repo, "TASK-missing", false)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
}

func TestRunAttachRecordsLedgerRow(t *testing.T) {
	t.Setenv("DEVAGENT_HERDR_SESSION", "testsession")
	cli := newFakeCli(t)
	cli.agentList = "testdata/roster/agent-list-two-pane.json"
	repo := t.TempDir()
	code := RunAttach(cli, repo, "TASK-abc", false)
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".devagent", "runs", "orchestration", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &row); err != nil {
		t.Fatal(err)
	}
	if row["event"] != "operator-attached" || row["taskId"] != "TASK-abc" || row["paneId"] != "pane-abc" || row["session"] != "testsession" || row["kind"] != "event" || row["attempt"] != float64(1) {
		t.Fatalf("row = %v", row)
	}
}

// ---------- pane-run CLI contract ----------

func TestRunPaneRunUnavailableExits3(t *testing.T) {
	// Fast-fail the detached server spawn: a nonexistent DEVAGENT_HERDR_BIN
	// errors synchronously (the TS 'error'-event path) and EnsureHerdrServer
	// returns false without polling 24x500ms.
	t.Setenv("DEVAGENT_HERDR_BIN", filepath.Join(t.TempDir(), "no-such-herdr"))
	cli := newFakeCli(t)
	// EnsureHerdrServer fails: workspace list reports down.
	failing := &downCli{inner: cli}
	dir := t.TempDir()
	code := RunPaneRun(failing, dir, 5, filepath.Join(dir, "out"), filepath.Join(dir, "err"), filepath.Join(dir, "done"), "", "echo", []string{"hi"})
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "done")); !os.IsNotExist(err) {
		t.Fatalf("done marker should be absent (caller falls back), err = %v", err)
	}
}

// downCli fails every workspace list (server down) but records everything.
type downCli struct{ inner CliRunner }

func (d *downCli) HerdrCli(args []string, timeoutMs int) CliResult {
	if len(args) > 1 && args[0] == "--session" {
		args = args[2:]
	}
	if len(args) >= 2 && args[0] == "workspace" && args[1] == "list" {
		return CliResult{Code: 1, Stderr: "server_not_running"}
	}
	return d.inner.HerdrCli(args, timeoutMs)
}

func TestRunPaneRunWritesCaptureContract(t *testing.T) {
	cli := &scriptRunnerCli{t: t}
	dir := t.TempDir()
	outPath := filepath.Join(dir, "out")
	errPath := filepath.Join(dir, "err")
	donePath := filepath.Join(dir, "done")
	code := RunPaneRun(cli, dir, 10, outPath, errPath, donePath, "", "echo", []string{"hello"})
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	if got := readTrim(t, outPath); got != "hello" {
		t.Fatalf("stdout capture = %q, want hello", got)
	}
	if got := readTrim(t, donePath); got != "0" {
		t.Fatalf("done marker = %q, want 0", got)
	}
	// env.sh must be removed by the pane script (secrets never in scrollback).
	if cli.envFileKept {
		t.Fatal("env.sh survived the pane script")
	}
}

func readTrim(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// ---------- Test utils ----------

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	_ = w.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}

var _ = fmt.Sprintf  // silence fmt in builds without tests using it
var _ = strconv.Itoa // silence strconv
