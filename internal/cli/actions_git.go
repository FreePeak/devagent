package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/FreePeak/devagent/internal/gates"
	"github.com/FreePeak/devagent/internal/git"
	"github.com/FreePeak/devagent/internal/spawn"
	"github.com/FreePeak/devagent/internal/tui"
	"github.com/spf13/cobra"
)

// spawnRunner adapts internal/spawn to the gates.Runner seam (the production
// implementation the gate package expects).
type spawnRunner struct{}

func (spawnRunner) RunCli(name string, args []string, opts spawn.Options) spawn.Result {
	return spawn.RunCli(name, args, opts)
}

// validateCommand mirrors the final `devagent validate` action (the
// FR-SIMPLE overlay replaced the base G1/G3 line printer): §20.8 gate cards
// by default, --json for scripts. Exit 1 when a gate fails (skipped gates do
// not fail).
func validateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Run all applicable gates against a repository or worktree (§20.8 card/chip default; --json for scripts)",
		RunE: func(cmd *cobra.Command, args []string) error {
			worktree, _ := cmd.Flags().GetString("worktree")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if worktree == "" {
				worktree, _ = os.Getwd()
			}

			probe := gates.RunMigrationStaticGate(gates.GateContext{
				RepoPath:       worktree,
				Classification: gates.TicketClassEndpointOnly,
			})
			hasMigrations := len(probe.Findings) > 0 || probe.Detail != "skipped: no migrations in this ticket"

			g1, err := gates.RunTestGate(spawnRunner{}, worktree, 15*60*1000)
			if err != nil {
				return err // detectTestCommand throw mirrors the TS await throw
			}
			rows := []tui.ValidateGateRow{{
				Label:        "G1",
				Gate:         g1.Gate,
				Passed:       g1.Passed,
				Skipped:      g1.Skipped,
				Detail:       g1.Detail,
				FindingsJSON: findingsJSON(g1.Findings),
			}}

			if hasMigrations {
				g3 := gates.RunMigrationStaticGate(gates.GateContext{
					RepoPath:       worktree,
					Classification: "migration-required",
				})
				rows = append(rows, tui.ValidateGateRow{
					Label:        "G3",
					Gate:         g3.Gate,
					Passed:       g3.Passed,
					Skipped:      g3.Skipped,
					Detail:       g3.Detail,
					FindingsJSON: findingsJSON(g3.Findings),
				})
			} else {
				rows = append(rows, tui.ValidateGateRow{
					Label:        "G3",
					Gate:         "G3-migration-static",
					Passed:       true,
					Skipped:      true,
					Detail:       "skipped: no migrations found",
					FindingsJSON: "[]",
				})
			}

			if jsonOut {
				fmt.Println(tui.ValidateJSON(rows))
			} else {
				fmt.Println(tui.RenderValidateCards(rows, cardWidth()))
			}

			for _, r := range rows {
				if !r.Passed && !r.Skipped {
					os.Exit(1)
				}
			}
			return nil
		},
	}
}

// cleanCommand mirrors `devagent clean`: remove .devagent-worktrees older
// than the --older-than cutoff (days).
func cleanCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "clean",
		Short: "Remove run worktrees older than the cutoff (default 7 days)",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			olderThan, _ := cmd.Flags().GetInt("older-than")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			stale := findStaleWorktrees(repo, int64(olderThan)*86_400_000)
			if len(stale) == 0 {
				fmt.Println("No stale worktrees.")
				return nil
			}
			failed := false
			for _, wt := range stale {
				parts := strings.Split(wt.path, "/")
				ticketKey := parts[len(parts)-1]
				// TS: `await removeWorktree(...)` inside try/catch; the Go
				// port is best-effort by construction — failure surfaces via
				// the git stderr only. The catch path never fires in TS
				// either (removeWorktree swallows its own errors), so the
				// exit-code-1 branch is unreachable in practice.
				git.RemoveWorktree(repo, ticketKey)
				fmt.Printf("removed %s (%dd old)\n", wt.path, int64(wt.ageMs)/86_400_000)
			}
			if failed {
				os.Exit(1)
			}
			return nil
		},
	}
}

// staleWorktree mirrors src/maintenance.ts StaleWorktree.
type staleWorktree struct {
	path  string
	ageMs int64
}

// findStaleWorktrees mirrors findStaleWorktrees(): .devagent-worktrees dirs
// older than cutoffMs (missing dir -> empty).
func findStaleWorktrees(repoPath string, cutoffMs int64) []staleWorktree {
	wtRoot := repoPath + "/.devagent-worktrees"
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		return nil
	}
	now := timeNowMs()
	var stale []staleWorktree
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // raced removal
		}
		if !info.IsDir() {
			continue
		}
		ageMs := now - info.ModTime().UnixMilli()
		if ageMs >= cutoffMs {
			stale = append(stale, staleWorktree{path: wtRoot + "/" + e.Name(), ageMs: ageMs})
		}
	}
	return stale
}

// rebaseStackCommand mirrors `devagent rebase-stack <branches...>`: rebase
// stacked branches onto their updated parents; exit 1 when any outcome
// failed.
func rebaseStackCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rebase-stack",
		Short: "Rebase stacked branches onto their updated parents (merge-queue refresh)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			onto, _ := cmd.Flags().GetString("onto")
			push, _ := cmd.Flags().GetBool("push")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if onto == "" {
				onto = "main"
			}
			r := git.RebaseStack(repo, args, &git.RebaseStackOpts{Onto: onto, Push: push})
			for _, b := range r.Results {
				icon := "x"
				switch b.Outcome {
				case "up-to-date":
					icon = "="
				case "rebased", "pushed":
					icon = "+"
				}
				detail := ""
				if b.Detail != "" {
					detail = " — " + b.Detail
				}
				fmt.Printf("%s %s: %s%s\n", icon, b.Branch, b.Outcome, detail)
			}
			if !r.OK {
				os.Exit(1)
			}
			return nil
		},
	}
}

// syncDocsCommand mirrors `devagent sync-docs` (src/commands/sync-docs.ts):
// refresh the work-selection docs from origin; exit codes 0 ok, 1 failed,
// 2 dirty, 3 diverged.
func syncDocsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "sync-docs",
		Short: "Refresh work-selection docs (docs/PRD.md) from origin before doc-driven selection (PRD §17): fast-forward when linear, rebase with --autostash when diverged and clean, refuse on a dirty PRD. Exit codes: 0 ok/up-to-date, 1 failure (fetch/network), 2 dirty refusal, 3 diverged (conflict or diverged+dirty). --json emits {ok, upToDate, diverged, dirty, detail}",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			branch, _ := cmd.Flags().GetString("branch")
			jsonOut, _ := cmd.Flags().GetBool("json")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			if branch == "" {
				branch = "main"
			}
			result := git.SyncWorkSelectionDocs(repo, &git.DocSyncOpts{Branch: branch})
			payload := struct {
				OK       bool   `json:"ok"`
				UpToDate bool   `json:"upToDate"`
				Diverged bool   `json:"diverged"`
				Dirty    bool   `json:"dirty"`
				Detail   string `json:"detail"`
			}{
				OK:       result.OK,
				UpToDate: result.AlreadyUpToDate,
				Diverged: result.Diverged,
				Dirty:    result.Dirty,
				Detail:   result.Detail,
			}
			if jsonOut {
				fmt.Println(marshalIndent(payload))
			} else if payload.OK {
				fmt.Printf("[sync-docs] %s\n", result.Detail)
			} else {
				fmt.Fprintf(os.Stderr, "[sync-docs] %s\n", result.Detail)
			}
			if !payload.OK {
				switch {
				case payload.Diverged:
					os.Exit(3)
				case payload.Dirty:
					os.Exit(2)
				default:
					os.Exit(1)
				}
			}
			return nil
		},
	}
}
