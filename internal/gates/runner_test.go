package gates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FreePeak/devagent/internal/spawn"
)

// writeRepo materializes files under a fresh temp dir (TS tempRepo helper).
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunMigrationStaticGateSkipsNonMigrationClassification(t *testing.T) {
	r := RunMigrationStaticGate(GateContext{RepoPath: t.TempDir(), Classification: TicketClassEndpointOnly})
	if !r.Passed || !r.Skipped {
		t.Errorf("expected skipped pass, got %+v", r)
	}
	if r.Detail != "skipped: no migrations in this ticket" {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestRunMigrationStaticGateFailsWithoutMigrationFiles(t *testing.T) {
	dir := writeRepo(t, map[string]string{"migrations/keep.txt": "not sql"})
	r := RunMigrationStaticGate(GateContext{RepoPath: dir, Classification: TicketClassMigrationRequired})
	if r.Passed {
		t.Errorf("expected failure, got %+v", r)
	}
	if r.Detail != "classified as migration-required but no migration files found" {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestRunMigrationStaticGatePassesOnCleanMigrations(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"migrations/001_add_email_idx.up.sql":   "CREATE INDEX CONCURRENTLY idx_users_email ON users (email);\n",
		"migrations/001_add_email_idx.down.sql": "DROP INDEX idx_users_email;\n",
	})
	r := RunMigrationStaticGate(GateContext{RepoPath: dir, Classification: TicketClassMigrationRequired})
	if !r.Passed {
		t.Errorf("expected pass, got %+v", r)
	}
	if r.Detail != "2 migration file(s) analyzed, 0 finding(s)" {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestRunMigrationStaticGateBlocksOnCriticalDropTable(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"migrations/001_add_email_idx.up.sql":   "CREATE INDEX CONCURRENTLY idx_users_email ON users (email);\n",
		"migrations/001_add_email_idx.down.sql": "DROP INDEX idx_users_email;\n",
		"migrations/002_drop_users.sql":         "DROP TABLE users;\n",
	})
	r := RunMigrationStaticGate(GateContext{RepoPath: dir, Classification: TicketClassMigrationRequired})
	if r.Passed {
		t.Errorf("critical DA001 must block, got %+v", r)
	}
	if !hasRule(r.Findings, "DA001") {
		t.Errorf("expected DA001, got %v", ruleIDs(r.Findings))
	}
	if r.Detail != "3 migration file(s) analyzed, 1 finding(s)" {
		t.Errorf("detail = %q", r.Detail)
	}
}

func TestRunMigrationStaticGateDA006UnpairedAndDefaultDirs(t *testing.T) {
	// Unpaired up file in a NON-default dir (custom migrationDirs) plus one
	// in db/migrations found via the defaults filter.
	dir := writeRepo(t, map[string]string{
		"migrations/003_audit_events.up.sql": "CREATE TABLE audit_events (id bigint PRIMARY KEY, payload text);\n",
	})
	r := RunMigrationStaticGate(GateContext{RepoPath: dir, Classification: TicketClassMigrationRequired})
	// Medium findings are advisory: the gate still passes (blocking is
	// critical-only), but the DA006 finding is reported.
	if !r.Passed {
		t.Errorf("medium DA006 must not block: %+v", r)
	}
	if !hasRule(r.Findings, "DA006") {
		t.Errorf("expected DA006 unpaired finding, got %v", ruleIDs(r.Findings))
	}
	for _, f := range r.Findings {
		if f.RuleID == "DA006" {
			if f.Severity != SeverityMedium || f.Message != "up-migration has no matching down-migration" {
				t.Errorf("DA006 = %+v", f)
			}
		}
	}
}

// fakeRunner scripts CLI calls by executable name (and first arg for sh -c).
type fakeRunner struct {
	calls    []fakeCall
	response func(name string, args []string, opts spawn.Options) spawn.Result
}

type fakeCall struct {
	name string
	args []string
}

func (f *fakeRunner) RunCli(name string, args []string, opts spawn.Options) spawn.Result {
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	if f.response != nil {
		return f.response(name, args, opts)
	}
	return spawn.Result{}
}

func (f *fakeRunner) called(name string, arg any) bool {
	for _, c := range f.calls {
		if c.name != name {
			continue
		}
		switch a := arg.(type) {
		case nil:
			return true
		case string:
			for _, x := range c.args {
				if x == a {
					return true
				}
			}
		}
	}
	return false
}

func TestDetectTestCommandConventions(t *testing.T) {
	npm := writeRepo(t, map[string]string{"package.json": `{"scripts": {"test": "vitest run"}}`})
	tc, err := DetectTestCommand(npm)
	if err != nil || tc == nil || tc.Cmd != "npm" || len(tc.Args) != 1 || tc.Args[0] != "test" {
		t.Errorf("npm detect = %+v err=%v", tc, err)
	}

	gomod := writeRepo(t, map[string]string{"go.mod": "module x\n"})
	tc, err = DetectTestCommand(gomod)
	if err != nil || tc == nil || tc.Cmd != "go" || strings.Join(tc.Args, " ") != "test ./..." {
		t.Errorf("go detect = %+v err=%v", tc, err)
	}

	py := writeRepo(t, map[string]string{"pyproject.toml": "[tool.pytest]\n"})
	tc, err = DetectTestCommand(py)
	if err != nil || tc == nil || tc.Cmd != "python3" || strings.Join(tc.Args, " ") != "-m pytest" {
		t.Errorf("python detect = %+v err=%v", tc, err)
	}

	none, err := DetectTestCommand(t.TempDir())
	if err != nil || none != nil {
		t.Errorf("empty repo detect = %+v err=%v", none, err)
	}
}

func TestDetectTestCommandOverride(t *testing.T) {
	override := writeRepo(t, map[string]string{"devagent.json": `{"testCommand": "python3 -m pytest -q"}`})
	tc, err := DetectTestCommand(override)
	if err != nil || tc == nil || tc.Cmd != "python3" || strings.Join(tc.Args, " ") != "-m pytest -q" {
		t.Errorf("override = %+v err=%v", tc, err)
	}

	// Override beats package.json and go.mod conventions.
	both := writeRepo(t, map[string]string{
		"package.json":  `{"scripts": {"test": "vitest run"}}`,
		"devagent.json": `{"testCommand": "make test"}`,
	})
	tc, _ = DetectTestCommand(both)
	if tc == nil || tc.Cmd != "make" || strings.Join(tc.Args, " ") != "test" {
		t.Errorf("override over npm = %+v", tc)
	}

	// Ranks overrides above npm, go, and python conventions.
	all := writeRepo(t, map[string]string{
		"package.json":   `{"scripts": {"test": "x"}}`,
		"go.mod":         "module x\n",
		"pyproject.toml": "[tool.pytest]\n",
	})
	goPy := writeRepo(t, map[string]string{
		"go.mod":         "module x\n",
		"pyproject.toml": "[tool.pytest]\n",
	})
	tc, _ = DetectTestCommand(goPy)
	if tc == nil || tc.Cmd != "go" {
		t.Errorf("go over python = %+v", tc)
	}
	tc, _ = DetectTestCommand(all)
	if tc == nil || tc.Cmd != "npm" {
		t.Errorf("npm over python = %+v", tc)
	}
	if err := os.WriteFile(filepath.Join(all, "devagent.json"), []byte(`{"testCommand": "yarn test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tc, _ = DetectTestCommand(all)
	if tc == nil || tc.Cmd != "yarn" || strings.Join(tc.Args, " ") != "test" {
		t.Errorf("yarn override = %+v", tc)
	}
	if err := os.Remove(filepath.Join(all, "devagent.json")); err != nil {
		t.Fatal(err)
	}
	tc, _ = DetectTestCommand(all)
	if tc == nil || tc.Cmd != "npm" {
		t.Errorf("back to npm = %+v", tc)
	}
}

func TestDetectTestCommandMalformedAndInvalid(t *testing.T) {
	// Malformed devagent.json falls through to conventions.
	malformed := writeRepo(t, map[string]string{
		"package.json":  `{"scripts": {"test": "x"}}`,
		"devagent.json": `{oops`,
	})
	tc, err := DetectTestCommand(malformed)
	if err != nil || tc == nil || tc.Cmd != "npm" {
		t.Errorf("malformed fallthrough = %+v err=%v", tc, err)
	}

	// Invalid type throws instead of falling through.
	invalid := writeRepo(t, map[string]string{
		"package.json":  `{"scripts": {"test": "vitest run"}}`,
		"devagent.json": `{"testCommand": 42}`,
	})
	_, err = DetectTestCommand(invalid)
	if err == nil || !strings.Contains(err.Error(), "Invalid testCommand") {
		t.Errorf("invalid override err = %v", err)
	}
}

func TestRunTestGatePassFailSkip(t *testing.T) {
	npm := writeRepo(t, map[string]string{"package.json": `{"scripts": {"test": "vitest run"}}`})

	okRunner := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: "all good\n"}
	}}
	r, err := RunTestGate(okRunner, npm, 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || r.Gate != GateG1Tests {
		t.Errorf("pass = %+v err=%v", r, err)
	}
	if len(okRunner.calls) != 1 || okRunner.calls[0].name != "npm" || strings.Join(okRunner.calls[0].args, " ") != "test" {
		t.Errorf("expected `npm test` with cwd, got %+v", okRunner.calls)
	}

	failRunner := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 1, Stdout: "a\nb\nc\nFAILED\n"}
	}}
	r, err = RunTestGate(failRunner, writeRepo(t, map[string]string{"go.mod": "module x\n"}), 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed {
		t.Errorf("non-zero exit must fail: %+v", r)
	}
	if !strings.Contains(r.Detail, "FAILED") {
		t.Errorf("detail should carry output tail: %q", r.Detail)
	}

	timeoutRunner := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: -1, TimedOut: true}
	}}
	r, err = RunTestGate(timeoutRunner, npm, 1)
	if err != nil {
		t.Fatal(err)
	}
	if r.Passed {
		t.Errorf("timeout must fail: %+v", r)
	}

	emptyRunner := &fakeRunner{}
	r, err = RunTestGate(emptyRunner, t.TempDir(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Passed || !r.Skipped || !strings.Contains(r.Detail, "skipped") {
		t.Errorf("skip = %+v", r)
	}
	if len(emptyRunner.calls) != 0 {
		t.Errorf("skipped gate must not spawn: %+v", emptyRunner.calls)
	}
}

func TestRunTestGateTailIsLastFifteenLines(t *testing.T) {
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "line-"+itoa(i))
	}
	lines = append(lines, "final-error-line")
	out := strings.Join(lines, "\n") + "\n"
	r := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 1, Stdout: out}
	}}
	res, err := RunTestGate(r, writeRepo(t, map[string]string{"go.mod": "module x\n"}), 60_000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed {
		t.Fatalf("expected failure: %+v", res)
	}
	if !strings.Contains(res.Detail, "final-error-line") {
		t.Errorf("tail should include the final line: %q", res.Detail)
	}
	// The detail body carries at most the last 15 stdout lines (plus the
	// `go test ./... -> exit 1` header and one newline).
	if got := strings.Count(res.Detail, "line-"); got > 15 {
		t.Errorf("tail should be capped at 15 lines, found %d line- markers", got)
	}
}

// ---------- G2 migration apply gate (interface-seamed: no docker in CI) ----------

const composeFixture = "services:\n  db:\n    image: postgres:16\n"

func g2Repo(t *testing.T, extra map[string]string) string {
	files := map[string]string{"docker-compose.yml": composeFixture}
	for k, v := range extra {
		files[k] = v
	}
	return writeRepo(t, files)
}

func TestDetectComposeFile(t *testing.T) {
	pref := writeRepo(t, map[string]string{
		"docker-compose.devagent.yml": composeFixture,
		"docker-compose.yml":          composeFixture,
	})
	if got := DetectComposeFile(pref); !strings.HasSuffix(got, "docker-compose.devagent.yml") {
		t.Errorf("devagent compose must win, got %q", got)
	}
	if got := DetectComposeFile(t.TempDir()); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractG2Config(t *testing.T) {
	if c := ExtractG2Config(map[string]any{}); c != nil {
		t.Errorf("empty config should give nil, got %+v", c)
	}
	c := ExtractG2Config(map[string]any{"g2": map[string]any{"dbService": "db", "migrationUp": "npm run migrate:up"}})
	if c == nil || c.DBService != "db" || c.MigrationUp != "npm run migrate:up" || c.MigrationDown != "" {
		t.Errorf("g2 extraction = %+v", c)
	}
}

func TestRunMigrationApplyGateSkips(t *testing.T) {
	// No compose file: skip without spawning anything.
	noComposeRunner := &fakeRunner{}
	r := RunMigrationApplyGate(noComposeRunner, t.TempDir(), 1000)
	if !r.Passed || !r.Skipped || !strings.Contains(r.Detail, "skipped") {
		t.Errorf("no compose = %+v", r)
	}
	if len(noComposeRunner.calls) != 0 {
		t.Errorf("skip must not spawn: %+v", noComposeRunner.calls)
	}

	// Docker daemon unavailable: skip.
	unavail := g2Repo(t, nil)
	dockerDown := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 1, Stderr: "cannot connect"}
	}}
	r = RunMigrationApplyGate(dockerDown, unavail, 1000)
	if !r.Passed || !strings.Contains(r.Detail, "docker not available") {
		t.Errorf("docker down = %+v", r)
	}

	// Compose present, docker fine, but no g2 config: skip.
	noCfg := g2Repo(t, nil)
	noCfgRunner := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		return spawn.Result{ExitCode: 0, Stdout: "24.0"}
	}}
	r = RunMigrationApplyGate(noCfgRunner, noCfg, 1000)
	if !r.Passed || !r.Skipped || !strings.Contains(r.Detail, "g2.dbService") {
		t.Errorf("no g2 config = %+v", r)
	}
}

func TestRunMigrationApplyGateUpFailsAndTearsDown(t *testing.T) {
	dir := g2Repo(t, map[string]string{
		"devagent.json": `{"g2": {"dbService": "db", "migrationUp": "false", "migrationDown": "true"}}`,
	})
	fr := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "docker" && args[0] == "info" {
			return spawn.Result{ExitCode: 0, Stdout: "24.0"}
		}
		if name == "sh" {
			return spawn.Result{ExitCode: 1, Stderr: "boom"}
		}
		return spawn.Result{ExitCode: 0} // compose up/down
	}}
	r := RunMigrationApplyGate(fr, dir, 5000)
	if r.Passed {
		t.Errorf("up failure must fail the gate: %+v", r)
	}
	if !strings.Contains(r.Detail, "up-migration: exit 1") {
		t.Errorf("detail = %q", r.Detail)
	}
	// teardown (compose down -v) was invoked
	if !fr.called("docker", "down") {
		t.Errorf("expected compose down teardown, calls: %+v", fr.calls)
	}
}

func TestRunMigrationApplyGateUpAndDownSucceed(t *testing.T) {
	dir := g2Repo(t, map[string]string{
		"devagent.json": `{"g2": {"dbService": "db", "migrationUp": "migrate up", "migrationDown": "migrate down"}}`,
	})
	shCalls := 0
	fr := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "docker" && args[0] == "info" {
			return spawn.Result{ExitCode: 0, Stdout: "24.0"}
		}
		if name == "sh" {
			shCalls++
			return spawn.Result{ExitCode: 0, Stdout: "ok"}
		}
		return spawn.Result{ExitCode: 0} // compose up/down
	}}
	r := RunMigrationApplyGate(fr, dir, 5000)
	if !r.Passed || r.Skipped {
		t.Errorf("expected pass, got %+v", r)
	}
	if !strings.Contains(r.Detail, "down-migration: exit 0") || !strings.Contains(r.Detail, "rollback round-trip re-up: exit 0") {
		t.Errorf("detail = %q", r.Detail)
	}
	if shCalls != 3 {
		t.Errorf("sh calls = %d, want 3 (up, down, re-up)", shCalls)
	}
}

func TestRunMigrationApplyGateReUpBreaks(t *testing.T) {
	dir := g2Repo(t, map[string]string{
		"devagent.json": `{"g2": {"dbService": "db", "migrationUp": "migrate up", "migrationDown": "migrate down"}}`,
	})
	shCalls := 0
	fr := &fakeRunner{response: func(name string, args []string, opts spawn.Options) spawn.Result {
		if name == "docker" && args[0] == "info" {
			return spawn.Result{ExitCode: 0, Stdout: "24.0"}
		}
		if name == "sh" {
			shCalls++
			// up ok, down ok, re-up broken (e.g. non-idempotent migration)
			if shCalls <= 2 {
				return spawn.Result{ExitCode: 0, Stdout: "ok"}
			}
			return spawn.Result{ExitCode: 1, Stderr: "column already exists"}
		}
		return spawn.Result{ExitCode: 0} // compose up/down
	}}
	r := RunMigrationApplyGate(fr, dir, 5000)
	if shCalls != 3 {
		t.Errorf("sh calls = %d, want 3", shCalls)
	}
	if r.Passed {
		t.Errorf("broken re-up must fail: %+v", r)
	}
	if !strings.Contains(r.Detail, "rollback round-trip re-up: exit 1") {
		t.Errorf("detail = %q", r.Detail)
	}
	if !fr.called("docker", "down") {
		t.Errorf("expected teardown after re-up failure, calls: %+v", fr.calls)
	}
}
