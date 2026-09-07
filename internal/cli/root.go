// Package cli is the Go port of the commander tree in src/cli.ts (FR-GO-02,
// issue #193). Implemented commands: scan-text, config, init, trust
// agents-md. Every other command is registered with its full flag surface
// (from the frozen parity fixture) but returns exit 3 with a clear
// not-ported message — its behavior lands with its owning FR-GO issue, and
// the Node CLI remains the production entrypoint until the FR-GO-15 cutover
// soak gate passes.
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FreePeak/devagent/internal/commands"
	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/research/scantext"
	"github.com/FreePeak/devagent/internal/trust"
	"github.com/FreePeak/devagent/internal/version"
	"github.com/spf13/cobra"
)

// notPortedIssue maps each stubbed command to the FR-GO issue that owns its
// port, so the exit-3 message points at the tracker instead of dead-ending.
var notPortedIssue = map[string]string{
	"backlog-check": "#193", "selfbuild-gate": "#194", "board-recovery": "#194",
	"prd-audit": "#202", "run": "#194", "fleet": "#194", "serve": "#199",
	"validate": "#196", "log": "#194", "status": "#198", "dashboard": "#198",
	"task": "#194", "orchestrate": "#194", "project": "#194", "ledger": "#197",
	"mcp": "#200", "preflight": "#194", "page-degrade-breach": "#194",
	"clean": "#195", "guard": "#190", "guard-status": "#190",
	"automerge": "#194", "autosweep": "#194", "pr-hygiene": "#194",
	"rebase-stack": "#195", "herdr-sweep": "#201", "pane-run": "#201",
	"sessions": "#201", "attach": "#201", "sync-docs": "#195",
	"daemon": "#200", "tui": "#198", "scout": "#192", "scout-status": "#192",
	"track": "#202", "create": "#202", "lessons": "#194", "queue list": "#194",
	"queue show": "#194", "queue bridge": "#194", "consume": "#194",
	"reap-stale": "#190", "record release": "#197",
}

// requiredFlags: flags the Node CLI declares with .requiredOption — the
// cobra tree must reject their absence with exit 1 exactly like commander.
var requiredFlags = map[string]map[string]bool{
	"run":            {"--ticket": true},
	"fleet":          {"--ticket": true, "--repo": true},
	"orchestrate":    {"--goal": true},
	"pane-run":       {"--cwd": true, "--timeout": true},
	"create":         {"--repo": true},
	"record release": {"--tag": true, "--sha": true},
}

// notPortedError carries the exit-3 stub message through cobra.
type notPortedError struct{ msg string }

func (e *notPortedError) Error() string { return e.msg }

func stubRun(dotted string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		issue := notPortedIssue[dotted]
		if issue == "" {
			issue = "#194"
		}
		return &notPortedError{msg: fmt.Sprintf("devagent %s: not yet ported to Go (FR-GO %s) — the Node CLI remains the production entrypoint", dotted, issue)}
	}
}

// addFlags registers the fixture's long flags. Stubs keep every flag as a
// plain string (or bool where the flag is a switch) so the parsing surface
// matches; typed flags land with each command's real port.
func addFlags(cmd *cobra.Command, flags []string, required map[string]bool) {
	for _, f := range flags {
		if cmd.Flags().Lookup(f[2:]) != nil {
			continue
		}
		switch f {
		case "--dry-run", "--smoke", "--auto-pr", "--interactive", "--auto-merge",
			"--drop-orca-workspace", "--json", "--exec", "--stale-prs", "--strike",
			"--apply", "--autostash", "--no-sync-docs", "--once", "--skip-pr",
			"--daemon", "--attach-only", "--headless", "--visible", "--orphans",
			"--all", "--force":
			cmd.Flags().Bool(f[2:], false, "")
		default:
			cmd.Flags().String(f[2:], "", "")
		}
		if required[f] {
			_ = cmd.MarkFlagRequired(f[2:])
		}
	}
}

// NewRoot builds the full cobra tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:     "devagent",
		Short:   "Autonomous backend delivery agent: ticket to tested PR",
		Version: version.Version,
	}
	root.SetVersionTemplate("{{.Version}}\n")
	// commander prints usage on parse errors but not on action errors; cobra
	// cannot distinguish, and the action-error usage dump is the noisier
	// divergence — silence it (exit codes stay identical).
	root.SilenceUsage = true
	root.CompletionOptions.DisableDefaultCmd = true

	registerImplemented(root)
	registerStubs(root)
	return root
}

// Execute runs the CLI and maps errors to exit codes the way commander does
// (usage/parse errors: 1; stubbed commands: 3).
func Execute() {
	if err := NewRoot().Execute(); err != nil {
		if st, ok := err.(*notPortedError); ok {
			fmt.Fprintln(os.Stderr, st.msg)
			os.Exit(3)
		}
		os.Exit(1)
	}
}

func registerImplemented(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use:   "scan-text",
		Short: "Print the canonical GRADIENT adjacent-category scan text (src/research/scan-text.ts). Machine-readable: scripts/selfbuild-loop.sh embeds it in RESEARCH_PROMPT/PO_PROMPT verbatim so the prompts cannot drift from the module.",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println(scantext.BuildAdjacentCategoryScanText())
			return nil
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "config",
		Short: "Show effective configuration and credential presence (never values)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, _ := os.Getwd()
			cfg, err := config.Load(cwd)
			if err != nil {
				return err
			}
			out := struct {
				Config      config.Config   `json:"config"`
				Credentials map[string]bool `json:"credentials"`
			}{cfg, config.CredentialStatus(config.LoadCredentials())}
			blob, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(blob))
			return nil
		},
	})

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Guided setup (§21 FR-SIMPLE-01): check prerequisites, write devagent.json with sane defaults, print a plain-language checklist; optional --smoke hermetic fixture",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			worker, _ := cmd.Flags().GetString("worker")
			model, _ := cmd.Flags().GetString("model")
			smoke, _ := cmd.Flags().GetBool("smoke")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			res, err := commands.RunInit(commands.InitOptions{RepoPath: repo, Worker: worker, Model: model, Smoke: smoke})
			if err != nil {
				return err
			}
			commands.RenderInitReport(res, func(s string) { fmt.Println(s) })
			if res.Smoke != nil {
				commands.RenderSmokeReport(*res.Smoke, func(s string) { fmt.Println(s) })
			}
			if !res.OK {
				os.Exit(1)
			}
			return nil
		},
	}
	initCmd.Flags().String("repo", "", "repository to set up")
	initCmd.Flags().String("worker", "", "worker CLI to check and record (default omp; claude-code | opencode | omp | pi | grok)")
	initCmd.Flags().String("model", "", "model id to record (provider/model)")
	initCmd.Flags().Bool("smoke", false, "after checklist write, run a hermetic fixture smoke (stub → done); default off")
	root.AddCommand(initCmd)

	trustCmd := &cobra.Command{
		Use:   "trust",
		Short: "One-time per-repo trust confirms (PRD §18 Q11)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	trustAgentsMd := &cobra.Command{
		Use:   "agents-md",
		Short: "Confirm auto-loading <repo>/.devagent/AGENTS.md once (the `ask` default of config `context.agentsMd`, PRD §18 Q11): writes the approval to <repo>/.devagent/trust.json; until this confirm the file is never injected into worker/planner prompts. `on` bypasses the gate; `off` disables loading.",
		RunE: func(cmd *cobra.Command, args []string) error {
			repo, _ := cmd.Flags().GetString("repo")
			if repo == "" {
				repo, _ = os.Getwd()
			}
			written, err := trust.RecordAgentsMd(repo)
			if err != nil {
				return err
			}
			present := ""
			if _, statErr := os.Stat(filepath.Join(repo, trust.AgentsMdFile)); statErr != nil {
				present = " (file not present yet; it will load once written)"
			}
			fmt.Printf("Trusted %s for %s%s. Trust record: %s\n", trust.AgentsMdFile, repo, present, written)
			return nil
		},
	}
	trustAgentsMd.Flags().String("repo", "", "repository to trust")
	trustCmd.AddCommand(trustAgentsMd)
	root.AddCommand(trustCmd)
}

// registerStubs registers every remaining command from the frozen surface
// with its full flag set; running one exits 3 with a pointer to its owning
// FR-GO issue.
func registerStubs(root *cobra.Command) {
	byParent := map[string]*cobra.Command{"": root}
	get := func(parent string) *cobra.Command {
		if c, ok := byParent[parent]; ok {
			return c
		}
		return root
	}
	// Parents first so subcommands attach to their parent, not the root.
	names := make([]string, 0, len(frozenSurface))
	for dotted := range frozenSurface {
		names = append(names, dotted)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.Count(names[i], " ") < strings.Count(names[j], " ")
	})
	for _, dotted := range names {
		frozen := frozenSurface[dotted]
		if handled(dotted) {
			byParent[dotted] = findImplemented(root, dotted)
			continue
		}
		name, parent := dotted, ""
		for i := len(dotted) - 1; i >= 0; i-- {
			if dotted[i] == ' ' {
				name, parent = dotted[i+1:], dotted[:i]
				break
			}
		}
		cmd := &cobra.Command{
			Use:   name,
			Short: "Not yet ported to Go (PRD §22 migration) — behavior arrives with the owning FR-GO issue; the Node CLI remains the production entrypoint.",
		}
		if len(frozen.Subcommands) == 0 || stubRuns(dotted) {
			// A leaf stub (or an action-bearing parent like `lessons`) exits
			// 3; pure grouping parents (queue, record) fall through to
			// cobra's automatic help, matching the Node outputHelp actions.
			cmd.RunE = stubRun(dotted)
		}
		addFlags(cmd, frozen.Flags, requiredFlags[dotted])
		get(parent).AddCommand(cmd)
		byParent[dotted] = cmd
	}
}

// stubRuns: parents that carry a real action in Node (not just help), so the
// Go stub must also run (exit 3) rather than print help.
func stubRuns(dotted string) bool {
	return dotted == "lessons"
}

// handled lists the dotted names implemented in registerImplemented.
func handled(dotted string) bool {
	switch dotted {
	case "scan-text", "config", "init", "trust", "trust agents-md":
		return true
	}
	return false
}

func findImplemented(root *cobra.Command, dotted string) *cobra.Command {
	cur := root
	for _, p := range strings.Split(dotted, " ") {
		var next *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == p {
				next = c
				break
			}
		}
		if next == nil {
			return cur
		}
		cur = next
	}
	return cur
}
