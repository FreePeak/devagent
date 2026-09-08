// actions_tui.go ports the `tui` command body (src/cli.ts:1770-1787) and
// runTui/ensureDaemon (src/tui/tui.ts): resolve the daemon first (attach to
// a running one, or embed an ephemeral one for this session — see
// ensureDaemon), then either one-shot the snapshot (non-TTY stdin) or hand
// the terminal to the FR-TUI interactive loop. Errors surface as
// `tui: <msg>` on stderr and the exit code stays 0, exactly like the TS
// process.exitCode = 0 path. Wiring: FR-GO-13, issue #252.

package cli

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/FreePeak/devagent/internal/daemon"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/spf13/cobra"
)

// tuiCommand mirrors the commander registration (.option chain + the
// `start` alias): the dashboard attaches to a running daemon or embeds an
// ephemeral one for the session (--attach-only = never spawn; `devagent
// daemon` runs a long-lived shared one). Flags come from the frozen surface
// (surface_gen.go) via addFlags; the factory registers none itself.
func tuiCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "tui",
		Aliases: []string{"start"},
		Short:   "Full-screen terminal dashboard (FR-TUI): workers/sessions/live-log views, selection + detail panels, kill, upgrade hint. One command — attaches to a running daemon, or embeds an ephemeral one for the session (--attach-only = never spawn; `devagent daemon` runs a long-lived shared one)",
		RunE: func(cmd *cobra.Command, args []string) error {
			urlFlag, _ := cmd.Flags().GetString("url")
			tokenFlag, _ := cmd.Flags().GetString("token")
			udsFlag, _ := cmd.Flags().GetString("uds-path")
			repoFlag, _ := cmd.Flags().GetString("repo")
			attachOnly, _ := cmd.Flags().GetBool("attach-only")
			if repoFlag == "" {
				repoFlag, _ = os.Getwd() // TS .option('--repo <path>', ..., process.cwd())
			}
			opts := tui.TuiOptions{
				URL:        urlFlag,
				Token:      tokenFlag,
				UDSPath:    udsFlag,
				RepoPath:   repoFlag,
				AttachOnly: attachOnly,
			}
			return runTuiAction(opts)
		},
	}
}

// runTuiAction is the tui .action body: resolve the daemon session, then
// one-shot (non-TTY stdin) or run the interactive loop. Like the TS
// original it never fails the process — the alternate screen and cursor are
// restored on every exit path (loop.Run's defer covers panics), and an
// embedded daemon is stopped.
func runTuiAction(opts tui.TuiOptions) error {
	sessionOpts, mode, stop, err := ensureTuiDaemon(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		return nil
	}
	defer stop()
	if !tui.StdinIsTTY() {
		// Non-TTY stdin degrades to a one-shot snapshot on stdout + exit 0
		// — the smoke-testable path.
		tui.RunOneShot(sessionOpts, mode)
		return nil
	}

	l := tui.NewLoop(sessionOpts, tui.StdTransport{}, tui.NewTermEnv(), mode)
	if err := l.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
	}
	return nil
}

// ensureTuiDaemon mirrors ensureDaemon (the glances standalone pattern): an
// explicit target (url/token/udsPath — tests, remote, socket) or
// `--attach-only` are pure clients; otherwise probe the default daemon and
// attach when it answers, else embed one in-process on an ephemeral port —
// no port conflicts, stopped when the TUI exits.
//
// The embedded daemon gets an explicit in-memory token on purpose: without
// one, daemon.Start persists a fresh token into the shared daemon-token
// file and every later attach to the long-lived daemon 401s until its next
// boot (the 2026-09-05 AUTH REJECTED incident). An ephemeral daemon must
// not mutate shared on-disk auth state.
func ensureTuiDaemon(opts tui.TuiOptions) (tui.TuiOptions, string, func(), error) {
	explicitTarget := opts.URL != "" || opts.Token != "" || opts.UDSPath != ""
	if explicitTarget || tui.ProbeDaemon(opts) || opts.AttachOnly {
		return opts, "attach", func() {}, nil
	}
	repoPath := opts.RepoPath
	if repoPath == "" {
		repoPath, _ = os.Getwd()
	}
	port := 0
	handle, err := daemon.Start(daemon.Options{
		Port:     &port,
		RepoPath: repoPath,
		Token:    randomTuiToken(),
	})
	if err != nil {
		return opts, "", nil, err
	}
	embedded := opts
	embedded.URL = fmt.Sprintf("http://127.0.0.1:%d", *handle.Port)
	embedded.Token = handle.Token
	return embedded, "embedded", handle.Stop, nil
}

// randomTuiToken mirrors randomBytes(24).toString('base64url').
func randomTuiToken() string {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		// crypto/rand never fails on supported platforms; fall back to a
		// time-derived token like the daemon's own generator.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
