// Orca workspace awareness for post-run cleanup: the Go port of
// src/integrations/orca.ts. When a run's --repo is an Orca-managed worktree
// (e.g. ~/orca/workspaces/<name>), plain `git worktree remove` would leave
// a ghost card in the Orca app. These helpers detect that case and remove
// through orca-cli instead. Everything here is best-effort and gracefully
// degrades: missing binary, app not running, malformed output -> "not an
// Orca workspace" / no-op.
package integrations

import (
	"encoding/json"
	"strings"

	"github.com/FreePeak/devagent/internal/spawn"
)

// OrcaWorktreeRef mirrors OrcaWorktreeRef.
type OrcaWorktreeRef struct {
	// ID is the full Orca worktree id: `<repoId>::<worktreePath>`.
	ID string
	// Path is the worktree directory.
	Path string
}

// OrcaRunner is the CLI seam (mirrors the TS CliRunner injection);
// production uses spawn.RunCli.
type OrcaRunner func(cmd string, args []string, opts spawn.Options) spawn.Result

func defaultOrcaRunner(cmd string, args []string, opts spawn.Options) spawn.Result {
	return spawn.RunCli(cmd, args, opts)
}

// extractJSON mirrors extractJson: pull the first `{`-anchored JSON object
// out of CLI output that may carry non-JSON noise lines. ok=false when no
// object parses.
func extractJSON(text string) (json.RawMessage, bool) {
	start := strings.Index(text, "{")
	if start < 0 {
		return nil, false
	}
	trimmed := text[start:]
	// json.Unmarshal over the full tail fails on trailing noise, so probe
	// successive decodes the way TS JSON.parse(text.slice(start)) would
	// fail: find the longest prefix that parses as one object.
	dec := json.NewDecoder(strings.NewReader(trimmed))
	var out json.RawMessage
	if err := dec.Decode(&out); err != nil {
		return nil, false
	}
	return out, true
}

// MatchOrcaWorktree mirrors matchOrcaWorktree: pure matcher over parsed
// `orca worktree ps` output; exact path match with trailing slashes
// normalized. Returns "" when there is no match (TS null).
func MatchOrcaWorktree(psOutput any, repoPath string) string {
	if psOutput == nil {
		return ""
	}
	obj, ok := psOutput.(map[string]any)
	if !ok {
		return ""
	}
	result, ok := obj["result"].(map[string]any)
	if !ok {
		return ""
	}
	worktrees, ok := result["worktrees"].([]any)
	if !ok {
		return ""
	}
	normalize := func(p string) string { return strings.TrimRight(p, "/") }
	target := normalize(repoPath)
	for _, wt := range worktrees {
		m, ok := wt.(map[string]any)
		if !ok {
			continue
		}
		id, idOK := m["id"].(string)
		path, pathOK := m["path"].(string)
		if idOK && pathOK && normalize(path) == target {
			return id
		}
	}
	return ""
}

// FindOrcaWorktreeByPath mirrors findOrcaWorktreeByPath: resolve the Orca
// worktree id for repoPath, or "" when it is not Orca-managed.
func FindOrcaWorktreeByPath(repoPath string, runner OrcaRunner) string {
	if runner == nil {
		runner = defaultOrcaRunner
	}
	r := runner("orca", []string{"worktree", "ps", "--json"}, spawn.Options{Dir: repoPath, TimeoutMs: 15000})
	if r.TimedOut || r.ExitCode != 0 {
		return ""
	}
	raw, ok := extractJSON(r.Stdout)
	if !ok {
		return ""
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	return MatchOrcaWorktree(parsed, repoPath)
}

// DropOrcaWorkspace mirrors dropOrcaWorkspace: remove an Orca-managed
// workspace (card + directory). Reports success/failure without throwing.
func DropOrcaWorkspace(worktreeID, cwd string, runner OrcaRunner) bool {
	if runner == nil {
		runner = defaultOrcaRunner
	}
	r := runner("orca",
		[]string{"worktree", "rm", "--worktree", "id:" + worktreeID, "--force"},
		spawn.Options{Dir: cwd, TimeoutMs: 30000})
	return !r.TimedOut && r.ExitCode == 0
}

// EnsureOrcaRepo mirrors ensureOrcaRepo: best-effort Orca repo registration
// (`orca repo add --path <repoPath> --json`).
func EnsureOrcaRepo(repoPath string, runner OrcaRunner) bool {
	if runner == nil {
		runner = defaultOrcaRunner
	}
	r := runner("orca", []string{"repo", "add", "--path", repoPath, "--json"},
		spawn.Options{Dir: repoPath, TimeoutMs: 15000})
	return !r.TimedOut && r.ExitCode == 0
}

// CreateOrcaWorktree mirrors createOrcaWorktree: create an Orca worktree
// for a worker slot. Returns the worktree path or "" on any failure.
func CreateOrcaWorktree(repoPath, name string, runner OrcaRunner) string {
	if runner == nil {
		runner = defaultOrcaRunner
	}
	r := runner("orca",
		[]string{"worktree", "create", "--name", name, "--repo", "path:" + repoPath, "--json"},
		spawn.Options{Dir: repoPath, TimeoutMs: 30000})
	if r.TimedOut || r.ExitCode != 0 {
		return ""
	}
	raw, ok := extractJSON(r.Stdout)
	if !ok {
		return ""
	}
	var parsed struct {
		Result struct {
			Path     string `json:"path"`
			Worktree struct {
				Path string `json:"path"`
			} `json:"worktree"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	if parsed.Result.Path != "" {
		return parsed.Result.Path
	}
	return parsed.Result.Worktree.Path
}

// ListOrcaWorktrees mirrors listOrcaWorktrees: list Orca worktrees that
// belong to the given repo (path prefix match). Best-effort.
func ListOrcaWorktrees(repoPath string, runner OrcaRunner) []string {
	if runner == nil {
		runner = defaultOrcaRunner
	}
	r := runner("orca", []string{"worktree", "ps", "--json"}, spawn.Options{Dir: repoPath, TimeoutMs: 15000})
	if r.TimedOut || r.ExitCode != 0 {
		return []string{}
	}
	raw, ok := extractJSON(r.Stdout)
	if !ok {
		return []string{}
	}
	var parsed struct {
		Result struct {
			Worktrees []struct {
				Path string `json:"path"`
			} `json:"worktrees"`
		} `json:"result"`
	}
	out := []string{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out
	}
	for _, wt := range parsed.Result.Worktrees {
		if strings.HasPrefix(wt.Path, repoPath) {
			out = append(out, wt.Path)
		}
	}
	return out
}
