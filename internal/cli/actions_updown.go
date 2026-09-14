package cli

// actions_updown.go wires `devagent up` / `devagent down` (issue #371): the
// lifecycle surface for the automated workflow driver. The logic — the
// prerequisite gate, the docs/PRD.md lane seeding, the detached start, the
// pid-recorded stop — lives in internal/commands; this file only turns flags
// into options and the report into output.

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/FreePeak/devagent/internal/commands"
	"github.com/spf13/cobra"
)

func newUpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Bring the factory up: check prerequisites, queue the operator's docs/PRD.md items, start the self-build driver (issue #371)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			foreground, _ := cmd.Flags().GetBool("foreground")
			skipChecks, _ := cmd.Flags().GetBool("skip-checks")
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			noDaemon, _ := cmd.Flags().GetBool("no-daemon")
			maxIntake, _ := cmd.Flags().GetInt("max-intake")
			waitSecs, _ := cmd.Flags().GetInt("wait")
			scoutLane, _ := cmd.Flags().GetBool("scout")
			scoutInterval, _ := cmd.Flags().GetInt("scout-interval")
			asJSON, _ := cmd.Flags().GetBool("json")

			daemon := !noDaemon
			res, err := commands.RunUp(commands.UpOptions{
				RepoPath:       repo,
				Daemon:         &daemon,
				Foreground:     foreground,
				SkipChecks:     skipChecks,
				DryRun:         dryRun,
				MaxIntakeItems: maxIntake,
				// --wait 0 = skip the health proof entirely (a negative
				// window is RunUp's "not asked to watch").
				HealthWindow:         time.Duration(waitSecs) * time.Second,
				Scout:                scoutLane,
				ScoutIntervalMinutes: scoutInterval,
				Stdout:               os.Stdout,
				Stderr:               os.Stderr,
			})
			if err != nil {
				return err
			}
			if asJSON {
				blob, jerr := json.MarshalIndent(res, "", "  ")
				if jerr != nil {
					return jerr
				}
				fmt.Println(string(blob))
			} else {
				commands.RenderUpReport(res, func(s string) { fmt.Println(s) })
			}
			if !res.OK {
				setExitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "repository to drive (default cwd)")
	cmd.Flags().Bool("foreground", false, "run the driver in this terminal instead of detaching it")
	cmd.Flags().Bool("skip-checks", false, "start even when a required prerequisite check fails")
	cmd.Flags().Bool("no-daemon", false, "do not bring up the control-plane daemon")
	cmd.Flags().Int("max-intake", 0, "cap how many docs/PRD.md items are queued by this run (0 = default)")
	cmd.Flags().Bool("scout", false, "also run the 24/7 researcher (devagent scout --interval) as a second child (FR-SCOUT-01)")
	cmd.Flags().Int("scout-interval", 0, "minutes between scout cycles (0 = config scout.intervalMinutes, else 30)")
	cmd.Flags().Int("wait", 15, "seconds to prove the driver is alive (loop lock + heartbeat) before up reports success; 0 = skip the proof")
	cmd.Flags().Bool("dry-run", false, "print the plan; write and start nothing")
	cmd.Flags().Bool("json", false, "machine-readable report")
	return cmd
}

func newDownCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop the self-build driver and daemon `devagent up` started (pid-recorded; never a pattern kill)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			asJSON, _ := cmd.Flags().GetBool("json")

			res, err := commands.RunDown(commands.UpOptions{RepoPath: repo, DryRun: dryRun})
			if err != nil {
				return err
			}
			if asJSON {
				blob, jerr := json.MarshalIndent(res, "", "  ")
				if jerr != nil {
					return jerr
				}
				fmt.Println(string(blob))
			} else {
				commands.RenderDownReport(res, func(s string) { fmt.Println(s) })
			}
			if !res.OK {
				setExitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "repository whose driver and daemon should stop (default cwd)")
	cmd.Flags().Bool("dry-run", false, "report what would be signalled")
	cmd.Flags().Bool("json", false, "machine-readable report")
	return cmd
}
