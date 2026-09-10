package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/FreePeak/devagent/internal/commands"
	"github.com/spf13/cobra"
)

// newDoctorCmd wires `devagent doctor` (FR-VAL-02, issue #290): one-command
// machine validation with human output by default and --json for the
// TUI/Tauri app. Exit 0 = all checks pass, 1 = any fail.
func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "One-command machine validation (FR-VAL-02): version stamp, config, DEVAGENT_HOME, git remote, gh auth (invalid env token flagged, never masked), herdr session, daemon health, provider preflight, stale artifacts; PASS/FAIL per item with a remediation hint",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			asJSON, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			res := commands.RunDoctor(commands.DoctorOptions{RepoPath: repo})
			if asJSON {
				blob, err := json.MarshalIndent(res, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(blob))
			} else {
				commands.RenderDoctorReport(res, func(s string) { fmt.Println(s) })
			}
			if !res.OK {
				setExitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().String("repo", "", "repository whose config and .selfbuild state to validate")
	cmd.Flags().Bool("json", false, "print the machine-readable report (TUI/Tauri setup screens)")
	return cmd
}
