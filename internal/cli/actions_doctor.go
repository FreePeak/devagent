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
		Short: "One-command machine validation (FR-VAL-02): version stamp, config, DEVAGENT_HOME, git remote, gh auth (invalid env token flagged, not masked), herdr session, daemon /status, provider preflight, stale artifacts",
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
