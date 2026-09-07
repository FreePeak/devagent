// Code generated from internal/cli/testdata/commands.json (the live Node CLI
// surface, frozen 2026-09-07 for FR-GO-02). Do not hand-edit; regenerate with
// scripts/go/extract-commands.mjs and `npm run build` first.
package cli

// frozenCmd is one command row from the Node CLI help surface.
type frozenCmd struct {
	Flags       []string
	Subcommands []string
}

var frozenSurface = map[string]frozenCmd{
	"attach":              {Flags: []string{"--exec", "--repo"}, Subcommands: []string{}},
	"automerge":           {Flags: []string{"--base", "--dry-run", "--grace-hours", "--method", "--pr", "--repo", "--timeout"}, Subcommands: []string{}},
	"autosweep":           {Flags: []string{"--apply", "--grace-days", "--repo", "--stale-prs"}, Subcommands: []string{}},
	"backlog-check":       {Flags: []string{"--ledger", "--repo", "--strike"}, Subcommands: []string{}},
	"board-recovery":      {Flags: []string{"--max-total-attempts", "--parked-polls", "--poll-secs", "--repo", "--requeue-after"}, Subcommands: []string{}},
	"clean":               {Flags: []string{"--older-than", "--repo"}, Subcommands: []string{}},
	"config":              {Flags: []string{}, Subcommands: []string{}},
	"consume":             {Flags: []string{"--auto-merge", "--auto-pr", "--max-loops", "--once", "--repo"}, Subcommands: []string{}},
	"create":              {Flags: []string{"--auto-merge", "--builder", "--dry-run", "--interval", "--orchestrator", "--orchestrator-goal", "--repo", "--scout", "--scout-worker", "--self-update", "--track-interval", "--tracker", "--workers"}, Subcommands: []string{}},
	"daemon":              {Flags: []string{"--port", "--repo", "--token", "--uds-path"}, Subcommands: []string{}},
	"dashboard":           {Flags: []string{}, Subcommands: []string{}},
	"fleet":               {Flags: []string{"--auto-pr", "--cleanup", "--concurrency", "--drop-orca-workspace", "--max-loops", "--repo", "--ticket", "--worker"}, Subcommands: []string{}},
	"guard":               {Flags: []string{"--base-delay-ms", "--max-attempts", "--max-delay-ms", "--no-progress-timeout-ms", "--resume-prompt"}, Subcommands: []string{}},
	"guard-status":        {Flags: []string{"--project-dir", "--resume", "--resume-prompt"}, Subcommands: []string{}},
	"herdr-sweep":         {Flags: []string{"--dry-run", "--orphans", "--session"}, Subcommands: []string{}},
	"init":                {Flags: []string{"--model", "--repo", "--smoke", "--worker"}, Subcommands: []string{}},
	"ledger":              {Flags: []string{"--clusters", "--json", "--repo", "--summary", "--task"}, Subcommands: []string{}},
	"lessons":             {Flags: []string{"--dry-run", "--entry", "--lessons-file", "--loop", "--predicted-impact", "--repo", "--suite-timeout-ms", "--threshold"}, Subcommands: []string{}},
	"lessons scores":      {Flags: []string{"--json"}, Subcommands: []string{}},
	"log":                 {Flags: []string{"--run"}, Subcommands: []string{}},
	"mcp":                 {Flags: []string{}, Subcommands: []string{}},
	"orchestrate":         {Flags: []string{"--answer", "--auditor", "--concurrency", "--executor", "--goal", "--max-recoveries", "--max-task-retries", "--max-total-attempts", "--max-waves", "--no-audit", "--no-merge", "--plan-only", "--planner", "--repo", "--resume"}, Subcommands: []string{}},
	"page-degrade-breach": {Flags: []string{"--detail", "--model", "--repo", "--role", "--source", "--worker"}, Subcommands: []string{}},
	"pane-run":            {Flags: []string{"--cwd", "--done", "--err", "--out", "--session", "--timeout"}, Subcommands: []string{}},
	"pr-hygiene":          {Flags: []string{"--apply", "--auto-merge", "--grace-hours", "--repo"}, Subcommands: []string{}},
	"prd-audit":           {Flags: []string{"--json", "--repo"}, Subcommands: []string{}},
	"preflight":           {Flags: []string{"--model", "--repo", "--role", "--worker"}, Subcommands: []string{}},
	"project":             {Flags: []string{"--repo"}, Subcommands: []string{}},
	"queue":               {Flags: []string{}, Subcommands: []string{}},
	"queue bridge":        {Flags: []string{"--repo"}, Subcommands: []string{}},
	"queue list":          {Flags: []string{"--json", "--repo", "--status"}, Subcommands: []string{}},
	"queue show":          {Flags: []string{"--json", "--repo"}, Subcommands: []string{}},
	"reap-stale":          {Flags: []string{"--dry-run", "--older-than", "--repo"}, Subcommands: []string{}},
	"rebase-stack":        {Flags: []string{"--onto", "--push", "--repo"}, Subcommands: []string{}},
	"record":              {Flags: []string{}, Subcommands: []string{}},
	"record release":      {Flags: []string{"--repo", "--sha", "--source", "--tag"}, Subcommands: []string{}},
	"run":                 {Flags: []string{"--auto-merge", "--auto-pr", "--cleanup", "--drop-orca-workspace", "--dry-run", "--interactive", "--max-loops", "--model", "--repo", "--ticket", "--timeout", "--variant", "--worker"}, Subcommands: []string{}},
	"scan-text":           {Flags: []string{}, Subcommands: []string{}},
	"scout":               {Flags: []string{"--dry-run", "--interval", "--once", "--replay", "--repo", "--timeout", "--worker"}, Subcommands: []string{}},
	"scout-status":        {Flags: []string{"--json", "--repo"}, Subcommands: []string{}},
	"selfbuild-gate":      {Flags: []string{"--already-shipped", "--extra-productive", "--ledger", "--limit", "--repo", "--starved"}, Subcommands: []string{}},
	"serve":               {Flags: []string{"--port", "--repo"}, Subcommands: []string{}},
	"sessions":            {Flags: []string{"--json", "--repo"}, Subcommands: []string{}},
	"status":              {Flags: []string{"--degrade-threshold", "--json", "--limit", "--providers", "--repo"}, Subcommands: []string{}},
	"sync-docs":           {Flags: []string{"--branch", "--json", "--repo"}, Subcommands: []string{}},
	"task":                {Flags: []string{"--auto-merge", "--auto-pr", "--cleanup", "--drop-orca-workspace", "--dry-run", "--id", "--max-loops", "--model", "--pick", "--prompt", "--remote", "--repo", "--variant", "--worker"}, Subcommands: []string{}},
	"track":               {Flags: []string{"--interval", "--json", "--repo"}, Subcommands: []string{}},
	"trust":               {Flags: []string{}, Subcommands: []string{}},
	"trust agents-md":     {Flags: []string{"--repo"}, Subcommands: []string{}},
	"tui":                 {Flags: []string{"--attach-only", "--repo", "--token", "--uds-path", "--url"}, Subcommands: []string{}},
	"validate":            {Flags: []string{"--json", "--worktree"}, Subcommands: []string{}},
}
