// Package cli is the Go port of the commander tree in src/cli.ts (FR-GO-02,
// issue #193, wave-2 wiring follow-through on tracker #207). Implemented
// commands: scan-text, config, init, trust agents-md, ledger, log,
// record release, status, dashboard, validate, clean, rebase-stack,
// herdr-sweep, sessions, attach, pane-run, sync-docs, scout (read-only
// --replay), scout-status, track, serve. Every other command is registered
// with its full flag surface (from the frozen parity fixture) but returns
// exit 3 with a clear not-ported message — its behavior lands with its
// owning FR-GO issue, and the Node CLI remains the production entrypoint
// until the FR-GO-15 cutover soak gate passes.
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
	"prd-audit": "#202", "run": "#194", "fleet": "#194",
	"task": "#194", "orchestrate": "#194", "project": "#194",
	"mcp": "#200", "preflight": "#194", "page-degrade-breach": "#194",
	"guard": "#190", "guard-status": "#190",
	"automerge": "#194", "autosweep": "#194", "pr-hygiene": "#194",
	"pane-run": "#201",
	"daemon":   "#200", "tui": "#198",
	"create": "#202", "lessons": "#194", "queue list": "#194",
	"queue show": "#194", "queue bridge": "#194", "consume": "#194",
	"reap-stale": "#190",
}

// requiredFlags: flags the Node CLI declares with .requiredOption — the
// cobra tree must reject their absence with exit 1 exactly like commander.
var requiredFlags = map[string]map[string]bool{
	"run":         {"--ticket": true},
	"fleet":       {"--ticket": true, "--repo": true},
	"orchestrate": {"--goal": true},
	"create":      {"--repo": true},
	"pane-run":    {"--cwd": true, "--timeout": true, "--out": true, "--err": true, "--done": true},
	"log":         {"--run": true},
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
// matches; typed flags land with each command's real port. Commands wired to
// real behavior (wiredTypedFlags) get the exact TS types: commander's
// Number-parsing flags become ints, switches stay bool.
func addFlags(cmd *cobra.Command, flags []string, required map[string]bool) {
	typed, isTyped := wiredTypedFlags[strings.Join(append(parentNames(cmd), cmd.Name()), " ")]
	for _, f := range flags {
		if cmd.Flags().Lookup(f[2:]) != nil {
			continue
		}
		if isTyped {
			switch typed[f[2:]] {
			case flagInt:
				cmd.Flags().Int(f[2:], 0, "")
				continue
			case flagBool:
				cmd.Flags().Bool(f[2:], false, "")
				continue
			}
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

// flagKind is the TS-parsing type of one wired flag.
type flagKind int

const (
	flagString flagKind = iota
	flagInt
	flagBool
)

// wiredTypedFlags pins the exact commander option types for wired commands,
// keyed by dotted command path. Number-parsing flags (`.option('<n>', ...,
// Number, ...)`) are Int here; boolean switches are Bool; everything else is
// String. The `--clusters [n]` variadic-optional pattern stays a String flag
// (TS receives it as string|boolean) and re-parses in the action.
var wiredTypedFlags = map[string]map[string]flagKind{
	"ledger": {
		"json": flagBool, "summary": flagBool, "clusters": flagString, "repo": flagString, "task": flagString,
	},
	"log": {
		"run": flagString,
	},
	"status": {
		"limit": flagInt, "repo": flagString, "providers": flagBool,
		"degrade-threshold": flagInt, "json": flagBool,
	},
	"validate": {
		"worktree": flagString, "json": flagBool,
	},
	"clean": {
		"repo": flagString, "older-than": flagInt,
	},
	"rebase-stack": {
		"repo": flagString, "onto": flagString, "push": flagBool,
	},
	"herdr-sweep": {
		"session": flagString, "dry-run": flagBool, "orphans": flagBool,
	},
	"sessions": {
		"json": flagBool, "repo": flagString,
	},
	"attach": {
		"exec": flagBool, "repo": flagString,
	},
	"sync-docs": {
		"json": flagBool, "repo": flagString, "branch": flagString,
	},
	"scout": {
		"repo": flagString, "worker": flagString, "interval": flagInt,
		"timeout": flagInt, "once": flagBool, "dry-run": flagBool, "replay": flagBool,
	},
	"scout-status": {
		"repo": flagString, "json": flagBool,
	},
	"track": {
		"repo": flagString, "interval": flagInt, "json": flagBool,
	},
	"serve": {
		"port": flagInt, "repo": flagString,
	},
	"record release": {
		"tag": flagString, "sha": flagString, "repo": flagString, "source": flagString,
	},
	"dashboard": {},
}

// parentNames walks a command's ancestor chain (root first, immediate parent
// last) so a subcommand can key its typed flags by dotted path.
func parentNames(cmd *cobra.Command) []string {
	var names []string
	for p := cmd.Parent(); p != nil; p = p.Parent() {
		if p.Name() != "devagent" {
			names = append([]string{p.Name()}, names...)
		}
	}
	return names
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

	// Wave-1-backed commands (FR-GO-02 follow-through): register each wired
	// command body, then apply the frozen flag surface with the exact TS
	// option types and defaults. Flag factories set nothing themselves so
	// the surface stays driven by the fixture.
	wired := wiredCommands()
	dotted := make([]string, 0, len(wired))
	for name := range wired {
		dotted = append(dotted, name)
	}
	sort.Slice(dotted, func(i, j int) bool {
		return strings.Count(dotted[i], " ") < strings.Count(dotted[j], " ")
	})
	for _, name := range dotted {
		wiredCmd := wired[name]
		parent := root
		if i := strings.LastIndex(name, " "); i >= 0 {
			parent = findImplemented(root, name[:i])
		}
		already := false
		for _, existing := range parent.Commands() {
			if existing == wiredCmd {
				already = true // subcommand registered with its parent factory
				break
			}
		}
		if !already {
			parent.AddCommand(wiredCmd)
		}
		addFlags(wiredCmd, frozenSurface[name].Flags, requiredFlags[name])
		if name == "ledger" {
			// commander `.option('--clusters [n]')`: a bare flag yields
			// boolean true; pflag models that with a NoOptDefVal sentinel.
			// Set here because addFlags registers the flags after the
			// factory ran.
			if f := wiredCmd.Flags().Lookup("clusters"); f != nil {
				f.NoOptDefVal = "true"
			}
		}
		for flagName, def := range wiredFlagDefaults[name] {
			if f := wiredCmd.Flags().Lookup(flagName); f != nil {
				_ = f.Value.Set(def)
				f.DefValue = def
			}
		}
	}
}

// wiredCommands returns the wave-1-backed command bodies by dotted path.
// Factories live in actions_*.go; flag registration happens here so the
// frozen surface (types, defaults, required marks) is applied in one place.
func wiredCommands() map[string]*cobra.Command {
	wired := map[string]*cobra.Command{
		"ledger":       ledgerCommand(),
		"log":          logCommand(),
		"status":       statusCommand(),
		"dashboard":    dashboardCommand(),
		"validate":     validateCommand(),
		"clean":        cleanCommand(),
		"rebase-stack": rebaseStackCommand(),
		"herdr-sweep":  herdrSweepCommand(),
		"sessions":     sessionsCommand(),
		"attach":       attachCommand(),
		"pane-run":     paneRunCommand(),
		"sync-docs":    syncDocsCommand(),
		"scout":        scoutCommand(),
		"scout-status": scoutStatusCommand(),
		"track":        trackCommand(),
		"serve":        serveCommand(),
		"record":       recordCommand(),
	}
	for _, sub := range wired["record"].Commands() {
		if sub.Name() == "release" {
			wired["record release"] = sub
		}
	}
	return wired
}

// wiredFlagDefaults pins the TS option defaults for wired commands (the
// typed registration starts everything at zero values).
var wiredFlagDefaults = map[string]map[string]string{
	"clean":          {"older-than": "7"},
	"serve":          {"port": "8080"},
	"status":         {"limit": "10", "degrade-threshold": "3"},
	"rebase-stack":   {"onto": "main"},
	"sync-docs":      {"branch": "main"},
	"record release": {"source": "cli"},
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
	case "scan-text", "config", "init", "trust", "trust agents-md",
		"ledger", "log", "record release", "status", "dashboard",
		"validate", "clean", "rebase-stack", "herdr-sweep", "sessions",
		"attach", "sync-docs", "scout", "scout-status", "track", "serve",
		"record", "pane-run":
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
