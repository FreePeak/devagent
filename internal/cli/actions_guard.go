// actions_guard.go ports the `guard` and `guard-status` command bodies
// (src/cli.ts:1411-1448 and 1451-1505) over the internal/sessionguard
// package (the port of src/sessionguard/*: backoff, stream-json event
// classification, the resume-on-terminal-API-failure loop, the child
// runner and transcript inspection). Every output literal is byte-identical
// to the TypeScript original — stdout strings, the [cc-guard] stderr
// prefixes, and exit codes included (issue #251).

package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/FreePeak/devagent/internal/sessionguard"
	"github.com/spf13/cobra"
)

// guardCommand mirrors `devagent guard` (src/cli.ts:1411-1448): run the
// given claude invocation headlessly with auto-resume on terminal API
// failure. Flag registration happens in root.go via the frozen surface.
// --max-attempts stays nil when not passed — the TS opts.maxAttempts is
// undefined there and RunGuard falls back to DEVAGENT_API_MAX_ATTEMPTS then
// Infinity — while an explicit --max-attempts 0 passes through (the loop
// then never runs).
func guardCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "guard",
		Short: "Run a headless Claude Code session with auto-resume on API failure (args after --)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resumePrompt, _ := cmd.Flags().GetString("resume-prompt")
			var maxAttempts *int
			if cmd.Flags().Changed("max-attempts") {
				n, _ := cmd.Flags().GetInt("max-attempts")
				maxAttempts = &n
			}
			baseDelayMs, _ := cmd.Flags().GetInt("base-delay-ms")
			maxDelayMs, _ := cmd.Flags().GetInt("max-delay-ms")
			noProgressTimeoutMs, _ := cmd.Flags().GetInt("no-progress-timeout-ms")
			result, err := sessionguard.RunGuard(sessionguard.GuardOptions{
				Argv:         args,
				ResumePrompt: resumePrompt,
				MaxAttempts:  maxAttempts,
				Backoff: &sessionguard.BackoffOptions{
					BaseDelayMs: baseDelayMs,
					MaxDelayMs:  maxDelayMs,
					Factor:      2,
				},
				NoProgressTimeoutMs: noProgressTimeoutMs,
				Log:                 guardStderrLog,
				OnLine:              guardForwardLine,
				Runner:              sessionguard.SpawnClaude,
			})
			if err != nil {
				return err
			}
			if !result.OK {
				message := fmt.Sprintf("[cc-guard] gave up after %d attempt(s): %s", result.Attempts, result.Reason)
				if result.LastError != "" {
					message += fmt.Sprintf(" — %s", result.LastError)
				}
				fmt.Fprintln(os.Stderr, message)
				setExitCode(1)
			} else {
				fmt.Fprintf(os.Stderr, "[cc-guard] completed after %d attempt(s), resumed %d time(s)\n",
					result.Attempts, result.Resumed)
			}
			return nil
		},
	}
}

// guardStatusCommand mirrors `devagent guard-status` (src/cli.ts:1451-1505):
// inspect the newest Claude Code transcript for --project-dir's slug and
// report interrupted/OK; --resume relaunches the interrupted session
// headlessly through the same guard loop.
func guardStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "guard-status",
		Short: "Check whether the latest Claude Code session for a project ended on an API error",
		RunE: func(cmd *cobra.Command, args []string) error {
			projectDir, _ := cmd.Flags().GetString("project-dir")
			resumePrompt, _ := cmd.Flags().GetString("resume-prompt")
			resume, _ := cmd.Flags().GetBool("resume")
			if projectDir == "" {
				projectDir, _ = os.Getwd()
			}
			home := os.Getenv("HOME")
			if home == "" {
				home = "."
			}
			dir := filepath.Join(sessionguard.ClaudeProjectsDir(home), sessionguard.ProjectSlug(projectDir))
			// The TS latestTranscript throws when the slug dir is missing
			// (reported as "No transcript directory at"); a present-but-empty
			// dir reports "No transcripts found in". LatestTranscript
			// collapses both to "", so probe the dir first.
			if _, err := os.Stat(dir); err != nil {
				fmt.Fprintf(os.Stderr, "No transcript directory at %s\n", dir)
				setExitCode(1)
				return nil
			}
			file := sessionguard.LatestTranscript(dir)
			if file == "" {
				fmt.Fprintf(os.Stderr, "No transcripts found in %s\n", dir)
				setExitCode(1)
				return nil
			}
			status, err := sessionguard.InspectTranscript(file)
			if err != nil {
				return err
			}
			if !status.Interrupted {
				fmt.Printf("OK session %s (%s) last activity %s\n",
					guardOptSession(status.SessionID), status.File, guardUnknown(status.LastTimestamp))
				return nil
			}
			fmt.Printf("INTERRUPTED session %s (%s)\nlast error: %s\nresume with: claude --resume %s\n",
				guardOptSession(status.SessionID), status.File,
				guardUnknown(status.LastErrorText), guardOptSession(status.SessionID))
			if resume && status.SessionID != "" {
				result, err := sessionguard.RunGuard(sessionguard.GuardOptions{
					Argv:         []string{"claude", "--resume", status.SessionID, "-p", resumePrompt},
					ResumePrompt: resumePrompt,
					Log:          guardStderrLog,
					OnLine:       guardForwardLine,
					Runner:       sessionguard.SpawnClaude,
				})
				if err != nil {
					return err
				}
				if !result.OK {
					fmt.Fprintf(os.Stderr, "[cc-guard] resume failed after %d attempt(s): %s\n",
						result.Attempts, result.Reason)
					setExitCode(1)
				} else {
					fmt.Fprintf(os.Stderr, "[cc-guard] session %s resumed and completed\n", status.SessionID)
				}
			} else {
				setExitCode(1)
			}
			return nil
		},
	}
}

// guardStderrLog mirrors the TS `log: (message) => console.error(message)`.
func guardStderrLog(message string) {
	fmt.Fprintln(os.Stderr, message)
}

// guardForwardLine mirrors the TS onLine hook (cli.ts:1439): stdout lines
// are written to stdout, everything else to stderr.
func guardForwardLine(line string, stream string) {
	if stream == sessionguard.StreamStdout {
		fmt.Println(line)
	} else {
		fmt.Fprintln(os.Stderr, line)
	}
}

// guardUnknown renders a TS `x ?? 'unknown'` where the port models TS
// undefined as "".
func guardUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// guardOptSession renders status.sessionId the way a TS template literal
// does: an absent session id ("" in the port) prints "undefined".
func guardOptSession(sessionID string) string {
	if sessionID == "" {
		return "undefined"
	}
	return sessionID
}
