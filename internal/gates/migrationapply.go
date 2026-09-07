package gates

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/devagent/internal/config"
	"github.com/FreePeak/devagent/internal/spawn"
)

// Gate G2 (FR-VALID-02): apply up-migration against a sandboxed database,
// then down-migration, verifying both succeed. Requires a compose file and
// configured migration commands; otherwise skips (never silently passes a
// migration-classified change without evidence).

var composeCandidates = []string{"docker-compose.devagent.yml", "docker-compose.yml", "compose.yml"}

// DetectComposeFile mirrors detectComposeFile().
func DetectComposeFile(repoPath string) string {
	for _, name := range composeCandidates {
		p := filepath.Join(repoPath, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// G2Config mirrors src/validation/migration-apply-gate.ts G2Config.
// MigrationDown is "" when unset (TS undefined).
type G2Config struct {
	// DBService is the service in the compose file that runs the database.
	DBService string
	// MigrationUp/MigrationDown are shell commands to apply migrations.
	MigrationUp   string
	MigrationDown string
}

// ExtractG2Config mirrors extractG2Config(): reads the raw `g2` section from
// a parsed devagent.json object (the Go config schema does not model g2; the
// TS reads it through a cast on the loaded config). Requires dbService and
// migrationUp; otherwise nil.
func ExtractG2Config(cfg map[string]any) *G2Config {
	g2, ok := cfg["g2"].(map[string]any)
	if !ok {
		return nil
	}
	dbService, _ := g2["dbService"].(string)
	migrationUp, _ := g2["migrationUp"].(string)
	if dbService == "" || migrationUp == "" {
		return nil
	}
	migrationDown, _ := g2["migrationDown"].(string)
	return &G2Config{DBService: dbService, MigrationUp: migrationUp, MigrationDown: migrationDown}
}

// loadG2Section mirrors the TS gate's extractG2Config(loadConfig(worktreePath))
// call: a config load error (invalid JSON, invalid values) is swallowed into
// a nil config, which skips the gate.
func loadG2Section(repoPath string) *G2Config {
	if _, err := config.Load(repoPath); err != nil {
		return nil
	}
	for _, name := range configFilenames {
		p := filepath.Join(repoPath, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(data, &raw) != nil {
			return nil
		}
		return ExtractG2Config(raw)
	}
	return ExtractG2Config(map[string]any{})
}

// RunMigrationApplyGate mirrors runMigrationApplyGate(). The docker/compose
// and migration commands go through the Runner seam so tests never touch a
// real container runtime.
func RunMigrationApplyGate(r Runner, worktreePath string, timeoutMs int) GateResult {
	composeFile := DetectComposeFile(worktreePath)
	if composeFile == "" {
		return GateResult{Gate: GateG2MigrationApply, Passed: true, Skipped: true, Findings: []Finding{}, Detail: "skipped: no compose file"}
	}
	// dockerAvailable
	da := runWith(r, "docker", []string{"info", "--format", "{{.ServerVersion}}"}, spawn.Options{Dir: worktreePath, TimeoutMs: 10_000})
	if da.TimedOut || da.ExitCode != 0 {
		return GateResult{Gate: GateG2MigrationApply, Passed: true, Skipped: true, Findings: []Finding{}, Detail: "skipped: docker not available"}
	}

	g2 := loadG2Section(worktreePath)
	if g2 == nil {
		return GateResult{
			Gate:     GateG2MigrationApply,
			Passed:   true,
			Skipped:  true,
			Findings: []Finding{},
			Detail:   "skipped: configure g2.dbService + g2.migrationUp in devagent.json",
		}
	}

	steps := []string{}
	fail := func(detail string) GateResult {
		return GateResult{Gate: GateG2MigrationApply, Passed: false, Findings: []Finding{}, Detail: detail}
	}
	failWithStderr := func(stderr string) GateResult {
		return fail(strings.Join(steps, "; ") + fmt.Sprintf("; stderr: %s", truncateStr(stderr, 400)))
	}

	// Bring up the database service only
	up := runWith(r, "docker", []string{"compose", "-f", composeFile, "up", "-d", g2.DBService}, spawn.Options{Dir: worktreePath, TimeoutMs: timeoutMs})
	steps = append(steps, fmt.Sprintf("compose up %s: exit %d", g2.DBService, up.ExitCode))
	if up.TimedOut || up.ExitCode != 0 {
		return failWithStderr(up.Stderr)
	}

	// Apply up-migration
	migrate := runWith(r, "sh", []string{"-c", g2.MigrationUp}, spawn.Options{Dir: worktreePath, TimeoutMs: timeoutMs})
	steps = append(steps, fmt.Sprintf("up-migration: exit %d", migrate.ExitCode))
	if migrate.TimedOut || migrate.ExitCode != 0 {
		teardownCompose(r, composeFile, worktreePath, timeoutMs)
		return failWithStderr(migrate.Stderr)
	}

	// Apply down-migration when configured
	if g2.MigrationDown != "" {
		down := runWith(r, "sh", []string{"-c", g2.MigrationDown}, spawn.Options{Dir: worktreePath, TimeoutMs: timeoutMs})
		steps = append(steps, fmt.Sprintf("down-migration: exit %d", down.ExitCode))
		if down.TimedOut || down.ExitCode != 0 {
			teardownCompose(r, composeFile, worktreePath, timeoutMs)
			return failWithStderr(down.Stderr)
		}
		// Rollback round-trip proof (FR-VALID-02): the up-migration must
		// still apply cleanly on a database the down-migration just rewound.
		// Catches non-idempotent or destructive migrations that pass a
		// single forward run.
		reUp := runWith(r, "sh", []string{"-c", g2.MigrationUp}, spawn.Options{Dir: worktreePath, TimeoutMs: timeoutMs})
		steps = append(steps, fmt.Sprintf("rollback round-trip re-up: exit %d", reUp.ExitCode))
		if reUp.TimedOut || reUp.ExitCode != 0 {
			teardownCompose(r, composeFile, worktreePath, timeoutMs)
			return failWithStderr(reUp.Stderr)
		}
	}

	teardownCompose(r, composeFile, worktreePath, timeoutMs)
	return GateResult{Gate: GateG2MigrationApply, Passed: true, Findings: []Finding{}, Detail: strings.Join(steps, "; ")}
}

func teardownCompose(r Runner, composeFile, cwd string, timeoutMs int) {
	runWith(r, "docker", []string{"compose", "-f", composeFile, "down", "-v"}, spawn.Options{Dir: cwd, TimeoutMs: timeoutMs})
}

// truncateStr mirrors TS string.slice(0, n) for log/detail excerpts.
func truncateStr(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
