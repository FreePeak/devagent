package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/FreePeak/devagent/internal/commands"
	"github.com/spf13/cobra"
)

// newDoctorCmd wires `devagent doctor` (FR-VAL-02, issue #290): one-command
// machine validation with human + --json output. Doctor is read-only; a
// failing check sets exit 1 while warn rows (version stamp cosmetics) never
// do.
func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "One-command machine validation (FR-VAL-02): version stamp, config, DEVAGENT_HOME, git remote, gh auth (invalid env token flagged, not masked), herdr session, daemon /status, provider preflight, stale artifacts, loop supervision mode (issue #321)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			result, err := commands.RunDoctor(commands.DoctorOptions{RepoPath: repo})
			if err != nil {
				return err
			}
			if jsonOut {
				blob, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
			} else {
				commands.RenderDoctorReport(result, func(s string) { fmt.Println(s) })
			}
			if !result.OK {
				setExitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "repository (and devagent.json owner) to validate; default cwd")
	cmd.Flags().Bool("json", false, "machine-readable output for the TUI/Tauri setup screen (FR-SIMPLE-01)")
	return cmd
}

// newSupervisionCmd wires `devagent supervision` (issue #321): print the
// resolved supervision mode for `devagent-go loop` — the same verdict the
// doctor's supervision row reports, as a one-liner `make loop-status` can
// call. Read-only.
func newSupervisionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "supervision",
		Short: "Print the resolved supervision mode for `devagent-go loop` (issue #321): unit path + restart policy, or unsupervised (nohup)",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			m := commands.DetectSupervision(home)
			switch {
			case m.Unit == "":
				fmt.Println("unsupervised (nohup) — intentional halts leave the loop stopped")
			case m.Policy == "on-failure":
				fmt.Printf("%s: %s unit %s restarts on failure only — intentional exit-0 halts stay stopped\n", m.Policy, m.Source, m.Unit)
			case m.Policy == "none":
				fmt.Printf("none: %s unit %s has no restart policy — intentional halts stay stopped\n", m.Source, m.Unit)
			default:
				fmt.Printf("WARN %s: %s unit %s restarts on success — intentional exit-0 halts become hollow restarts (2026-09-10 incident)\n", m.Policy, m.Source, m.Unit)
			}
			return nil
		},
	}
}
