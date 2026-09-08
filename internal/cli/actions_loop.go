package cli

// actions_loop.go wires the FR-GO-13 loop driver (internal/loopdriver) to
// the `loop` command — the production driver since the Node retirement
// (FR-GO-16, issue #205); the soak decision trail lives in
// internal/loopdriver/DECISION.md.

import (
	"os"
	"strconv"

	"github.com/FreePeak/devagent/internal/loopdriver"
	"github.com/spf13/cobra"
)

func newLoopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "loop",
		Short: "Run the self-build loop driver (FR-GO-13, the scripts/selfbuild-loop.sh port): one or more full iterations — state sync, issue-first pick, research/PO dispatch, extraction, task dispatch, ledger appends. SELFBUILD_* env knobs are honored exactly like the retired bash driver.",
		RunE: func(cmd *cobra.Command, args []string) error {
			dryRun, _ := cmd.Flags().GetBool("dry-run")
			maxIter := gatesFlagIntOpt(cmd, "max-iterations")
			if dryRun {
				_ = os.Setenv("SELFBUILD_DRY_RUN", "1")
			}
			if maxIter != nil {
				_ = os.Setenv("SELFBUILD_MAX_ITERATIONS", strconv.Itoa(*maxIter))
			}
			repo, _ := os.Getwd()
			code := loopdriver.RunLoop(loopdriver.ConfigFromEnv(repo))
			setExitCode(code)
			return nil
		},
	}
	cmd.Flags().Bool("dry-run", false, "execute phases 1-3 + record only (SELFBUILD_DRY_RUN=1)")
	cmd.Flags().Int("max-iterations", 0, "cap the iteration count (SELFBUILD_MAX_ITERATIONS; 0 = unbounded)")
	return cmd
}
