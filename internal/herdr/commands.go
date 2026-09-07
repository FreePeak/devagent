// Operator-facing command surfaces ported from src/cli.ts herdr-sweep /
// pane-run and src/commands/sessions.ts: the exact stdout lines the loop
// driver and the operator parse. Command wiring (cobra) stays with the parent
// (internal/cli/root.go); these Run* functions are "ready to wire".
package herdr

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
)

// RunHerdrSweep mirrors the src/cli.ts `herdr-sweep` action: render the
// deny/summary lines and one `[stale]|[closed]|[spared]` line per pane.
// Returns the process exit code (always 0 today; the TS action never fails).
func RunHerdrSweep(cli CliRunner, session string, dryRun, orphans bool) int {
	s := ResolveSession(session)
	// An invalid config leaves the deny list unknown; the sweep's internal
	// ResolveSweepSettings() then refuses to run and says why on stderr
	// (TS: `sweep = undefined` on throw).
	var sweep *config.HerdrSweepSettings
	if cfg, err := loadConfigOrEmpty(cwdOrEmpty()); err == nil {
		resolved := config.ResolveHerdrSweep(cfg)
		sweep = &resolved
	}
	denied := ""
	if sweep != nil {
		denied = SweepDenyReason(s, *sweep)
	}
	if denied != "" {
		if denied == "disabled" {
			fmt.Printf("[%s] sweep disabled (herdr.sweep.enabled / DEVAGENT_HERDR_SWEEP=0)\n", s)
		} else {
			fmt.Printf("[%s] sweep denied for session %q (herdr.sweep.denySessions)\n", s, s)
		}
		return 0
	}
	stale := SweepStalePanes(cli, s, SweepOptions{Orphans: orphans, Sweep: sweep}, dryRun)
	if len(stale) == 0 {
		fmt.Printf("[%s] no stale panes\n", s)
		return 0
	}
	spared := 0
	for _, sp := range stale {
		if sp.Reason == SweepReasonOperatorAttached {
			spared++
		}
	}
	for _, sp := range stale {
		tag := "[closed]"
		if sp.Reason == SweepReasonOperatorAttached {
			tag = "[spared]"
		} else if dryRun {
			tag = "[stale]"
		}
		fmt.Printf("%s %s (%s) status=%s reason=%s\n", tag, sp.PaneID, sp.Label, sp.AgentStatus, sp.Reason)
	}
	verb := "closed"
	if dryRun {
		verb = "found"
	}
	extra := ""
	if spared > 0 {
		extra = fmt.Sprintf("; %d spared (operator-attached)", spared)
	}
	fmt.Printf("%d pane(s) %s in session %q%s\n", len(stale)-spared, verb, s, extra)
	return 0
}

// RunSessions mirrors src/commands/sessions.ts runSessions: list live worker
// panes as a table (or raw JSON with jsonOutput).
func RunSessions(cli CliRunner, jsonOutput bool) int {
	panes := ListSessionPanes(cli, "")
	if jsonOutput {
		fmt.Print(RenderSessionsJSON(panes))
		return 0
	}
	if len(panes) == 0 {
		fmt.Printf("no worker panes in session %q\n", ResolveSession(""))
		return 0
	}
	rows := make([][]string, 0, len(panes))
	for _, p := range panes {
		attach := AttachCommandFor(cli, p.TaskID, "")
		if attach == "" {
			attach = "-"
		}
		label := p.Label
		if label == "" {
			label = p.Worker
		}
		cwd := p.Cwd
		if cwd == "" {
			cwd = "-"
		}
		rows = append(rows, []string{p.PaneID, label, p.State, cwd, attach})
	}
	header := []string{"PANE", "AGENT", "STATUS", "CWD", "ATTACH"}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	printRow := func(cells []string) {
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = c + strings.Repeat(" ", widths[i]-len(c))
		}
		fmt.Println(strings.Join(parts, "  "))
	}
	printRow(header)
	for _, r := range rows {
		printRow(r)
	}
	return 0
}

// RunAttach mirrors src/commands/sessions.ts runAttach: resolve the jump-in
// command for a task's pane, print it, record the attach in the ledger, and
// with execMode run `herdr --session <s> agent attach <pane>` with inherited
// stdio (exit code follows the child). Returns the process exit code (1 when
// no live pane exists or the child failed).
func RunAttach(cli CliRunner, repoPath, taskID string, execMode bool) int {
	if repoPath == "" {
		repoPath = cwdOrEmpty()
	}
	session := ResolveSession("")
	panes := ListSessionPanes(cli, session)
	cmd := AttachCommandFor(cli, taskID, session)
	var pane *SessionPaneInfo
	for i := range panes {
		if panes[i].TaskID == taskID {
			pane = &panes[i]
			break
		}
	}
	if cmd == "" || pane == nil {
		fmt.Fprintf(os.Stderr, "no live pane found for task %s in session %q\n", taskID, session)
		return 1
	}
	fmt.Println(cmd)
	AppendOperatorAttachRecord(repoPath, OperatorAttachRecord{
		Ts:      ledgerNow(),
		Kind:    "event",
		Event:   "operator-attached",
		TaskID:  taskID,
		Attempt: 1,
		PaneID:  pane.PaneID,
		Session: session,
	})
	if !execMode {
		return 0
	}
	child := exec.Command(HerdrBin(), "--session", session, "agent", "attach", pane.PaneID)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

// RunPaneRun mirrors the src/cli.ts `pane-run` action: run one command inside
// a herdr pane and write the capture contract (stdout -> out, stderr -> err,
// exit code -> done marker). Exit code 3 means the herdr pane runtime was
// unavailable; callers fall back to their own direct dispatch so visibility
// never becomes a hard dependency. Timed-out runs exit 124.
func RunPaneRun(cli CliRunner, cwd string, timeoutSecs int, outPath, errPath, donePath, session, cmd string, args []string) int {
	res, err := RunCommandInHerdrPane(cli, cmd, args, PaneRunOptions{
		Dir:       cwd,
		TimeoutMs: timeoutSecs * 1000,
		Session:   session,
	})
	if err != nil || res == nil {
		// Caller (loop driver) inspects the missing done-marker and falls back
		// to its own direct dispatch; keep the exit code distinct for triage.
		fmt.Fprintln(os.Stderr, "[pane-run] herdr pane unavailable")
		return 3
	}
	// Capture contract matches the driver's own direct dispatch: stdout ->
	// out, stderr -> err, exit code -> done marker.
	if res.Stdout != "" {
		_ = os.WriteFile(outPath, []byte(res.Stdout), 0o644)
	}
	if res.Stderr != "" {
		_ = os.WriteFile(errPath, []byte(res.Stderr), 0o644)
	}
	_ = os.WriteFile(donePath, []byte(fmt.Sprintf("%d", res.ExitCode)), 0o644)
	if res.TimedOut {
		return 124
	}
	return 0
}

// RenderSessionsJSON mirrors `JSON.stringify(panes, null, 2)` + console.log's
// trailing newline for the SessionPaneInfo shape: two-space indent, object key
// order as declared in the TS literal.
func RenderSessionsJSON(panes []SessionPaneInfo) string {
	var b strings.Builder
	b.WriteString("[")
	if len(panes) > 0 {
		b.WriteString("\n")
	}
	for i, p := range panes {
		b.WriteString("  {\n")
		fields := []struct {
			key string
			val string
		}{
			{"taskId", p.TaskID},
			{"role", p.Role},
			{"worker", p.Worker},
			{"paneId", p.PaneID},
			{"workspaceId", p.WorkspaceID},
			{"label", p.Label},
			{"cwd", p.Cwd},
			{"agentStatus", p.AgentStatus},
			{"state", p.State},
			{"startedAt", p.StartedAt},
		}
		for j, f := range fields {
			comma := ","
			if j == len(fields)-1 {
				comma = ""
			}
			b.WriteString("    " + jsonString(f.key) + ": " + jsonString(f.val) + comma + "\n")
		}
		b.WriteString("  }")
		if i < len(panes)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("]")
	return b.String() + "\n"
}

// jsonString renders one JSON string value with encoding/json escaping
// (identical to JSON.stringify for strings).
func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(data)
}

func cwdOrEmpty() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return cwd
}
