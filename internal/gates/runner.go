package gates

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/spawn"
)

// Gate orchestration (src/validation/runner.ts). This package implements G3
// (static migration analysis) fully; G1/G2 are seamed through Runner for
// hermetic tests.

// GateContext mirrors src/validation/runner.ts GateContext.
type GateContext struct {
	RepoPath       string
	Classification string
	// MigrationDirs overrides the default scan dirs, relative to RepoPath.
	MigrationDirs []string
}

var migrationDirDefaults = []string{"migrations", "db/migrations", "prisma/migrations", "drizzle"}

var migrationExt = regexp.MustCompile(`(?i)\.(sql|up\.sql|down\.sql)$`)

// collectMigrationFiles mirrors collectMigrationFiles(): recursive readdir
// of each dir; unreadable dirs are skipped (not present in this repo) and
// unreadable files are skipped (never fail the gate on collection errors).
// Paths are the dir-relative paths the TS readdirSync({recursive:true})
// yields; nested dirs are prefixed.
func collectMigrationFiles(repoPath string, dirs []string) []MigrationFile {
	files := []MigrationFile{}
	for _, dir := range dirs {
		abs := filepath.Join(repoPath, dir)
		collectDirEntries(abs, "", &files)
	}
	return files
}

func collectDirEntries(absDir, relPrefix string, files *[]MigrationFile) {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return // dir not present in this repo (or unreadable): skip
	}
	for _, e := range entries {
		rel := e.Name()
		if relPrefix != "" {
			rel = relPrefix + string(filepath.Separator) + e.Name()
		}
		if e.IsDir() {
			collectDirEntries(filepath.Join(absDir, e.Name()), rel, files)
			continue
		}
		if !migrationExt.MatchString(filepath.Base(e.Name())) {
			continue
		}
		sql, err := os.ReadFile(filepath.Join(absDir, e.Name()))
		if err != nil {
			continue // unreadable file: skip, do not fail the gate on collection errors
		}
		*files = append(*files, MigrationFile{Path: rel, SQL: string(sql)})
	}
}

// FindUnpairedUpMigrations mirrors findUnpairedUpMigrations(): an up file
// `NNN_name.up.sql` pairs with `NNN_name.down.sql`.
func FindUnpairedUpMigrations(files []MigrationFile) []string {
	paths := map[string]bool{}
	for _, f := range files {
		paths[f.Path] = true
	}
	var upDotSQL = regexp.MustCompile(`(?i)\.up\.sql$`)
	out := []string{}
	for _, f := range files {
		if !upDotSQL.MatchString(f.Path) {
			continue
		}
		down := upDotSQL.ReplaceAllString(f.Path, ".down.sql")
		if !paths[down] {
			out = append(out, f.Path)
		}
	}
	return out
}

// RunMigrationStaticGate mirrors runMigrationStaticGate(): gate G3 static
// migration analysis over the repo's migration directories.
func RunMigrationStaticGate(ctx GateContext) GateResult {
	if ctx.Classification != TicketClassMigrationRequired {
		return GateResult{Gate: GateG3MigrationStatic, Passed: true, Skipped: true, Findings: []Finding{}, Detail: "skipped: no migrations in this ticket"}
	}

	dirs := ctx.MigrationDirs
	if dirs == nil {
		dirs = []string{}
		for _, d := range migrationDirDefaults {
			if existsInRepo(ctx.RepoPath, d) {
				dirs = append(dirs, d)
			}
		}
	}
	files := collectMigrationFiles(ctx.RepoPath, dirs)

	if len(files) == 0 {
		return GateResult{
			Gate:     GateG3MigrationStatic,
			Passed:   false,
			Findings: []Finding{},
			Detail:   "classified as migration-required but no migration files found",
		}
	}

	unpaired := FindUnpairedUpMigrations(files)
	findings := AnalyzeMigrations(files, AnalyzeOptions{})
	for _, path := range unpaired {
		findings = append(findings, Finding{
			RuleID:   "DA006",
			Severity: SeverityMedium,
			Message:  "up-migration has no matching down-migration",
			File:     path,
		})
	}

	blocking := false
	for _, f := range findings {
		if f.Severity == SeverityCritical {
			blocking = true
			break
		}
	}
	return GateResult{
		Gate:     GateG3MigrationStatic,
		Passed:   !blocking,
		Findings: findings,
		Detail:   fmt.Sprintf("%d migration file(s) analyzed, %d finding(s)", len(files), len(findings)),
	}
}

func existsInRepo(repoPath, rel string) bool {
	_, err := os.ReadDir(filepath.Join(repoPath, rel))
	return err == nil
}

// ---------- G1 test gate (src/validation/test-gate.ts) ----------

var configFilenames = []string{"devagent.json", ".devagent.json"}

// DetectTestCommand mirrors detectTestCommand(): declarative devagent.json
// override first, then conventional files (package.json, go.mod,
// pyproject.toml). Returns nil when nothing is detectable. An invalid
// testCommand type errors like the TS throw (message byte-identical).
func DetectTestCommand(repoPath string) (*TestCommand, error) {
	for _, name := range configFilenames {
		p := filepath.Join(repoPath, name)
		if _, err := os.Stat(p); err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			// unreadable config: fall through to convention detection
			break
		}
		var raw map[string]any
		if err := json.Unmarshal(data, &raw); err != nil {
			// malformed config JSON: fall through to convention detection
			break
		}
		// TS: JSON.parse(...)?.testCommand — a present key (even null) is
		// "defined" and validated; only a missing key falls through.
		tc, present := raw["testCommand"]
		if present {
			s, ok := tc.(string)
			if !ok || len(s) == 0 {
				return nil, fmt.Errorf("Invalid testCommand %q in %s: expected a non-empty string", jsString(tc), p)
			}
			parts := strings.Fields(strings.TrimSpace(s))
			cmd := ""
			var args []string
			if len(parts) > 0 {
				cmd = parts[0]
				args = parts[1:]
			}
			return &TestCommand{Cmd: cmd, Args: args}, nil
		}
		break
	}
	pkg := filepath.Join(repoPath, "package.json")
	if data, err := os.ReadFile(pkg); err == nil {
		var parsed map[string]any
		if json.Unmarshal(data, &parsed) == nil {
			if scripts, ok := parsed["scripts"].(map[string]any); ok {
				if test, ok := scripts["test"].(string); ok && test != "" {
					return &TestCommand{Cmd: "npm", Args: []string{"test"}}, nil
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(repoPath, "go.mod")); err == nil {
		return &TestCommand{Cmd: "go", Args: []string{"test", "./..."}}, nil
	}
	if _, err := os.Stat(filepath.Join(repoPath, "pyproject.toml")); err == nil {
		return &TestCommand{Cmd: "python3", Args: []string{"-m", "pytest"}}, nil
	}
	return nil, nil
}

// jsString mirrors JS String() for the testCommand error message.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		return fmt.Sprintf("%t", t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// RunTestGate mirrors runTestGate(): gate G1 (lightweight variant) — run the
// target repo's own test suite in the worktree. Docker-based sandbox
// execution arrives with loop 5; this variant covers repos where tests are
// runnable directly (npm/go conventions). The detectTestCommand error
// surfaces like the TS throw.
func RunTestGate(r Runner, worktreePath string, timeoutMs int) (GateResult, error) {
	detected, err := DetectTestCommand(worktreePath)
	if err != nil {
		return GateResult{}, err
	}
	if detected == nil {
		return GateResult{
			Gate:     GateG1Tests,
			Passed:   true,
			Skipped:  true,
			Findings: []Finding{},
			Detail:   "skipped: no runnable test command detected (Docker sandbox not yet configured)",
		}, nil
	}

	result := runWith(r, detected.Cmd, detected.Args, spawn.Options{Dir: worktreePath, TimeoutMs: timeoutMs})
	lines := strings.Split(result.Stdout, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	tail := strings.Join(lines, "\n")
	timedOut := ""
	if result.TimedOut {
		timedOut = " (TIMED OUT)"
	}
	return GateResult{
		Gate:     GateG1Tests,
		Passed:   !result.TimedOut && result.ExitCode == 0,
		Findings: []Finding{},
		Detail:   fmt.Sprintf("%s %s -> exit %d%s\n%s", detected.Cmd, strings.Join(detected.Args, " "), result.ExitCode, timedOut, tail),
	}, nil
}
