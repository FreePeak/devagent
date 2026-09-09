package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// scrubResilienceEnv clears the DEVAGENT_* resilience overrides that
// config.Load folds into Resilience (config.go). Worker sessions inherit
// them from the daemon's dispatch env, so unset-by-assertion keeps the
// defaults/error-shape tests hermetic (same treatment pipeline tests get;
// issue #280).
func scrubResilienceEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"DEVAGENT_API_MAX_ATTEMPTS", "DEVAGENT_NO_PROGRESS_TIMEOUT_MS",
		"DEVAGENT_COLD_START_TIMEOUT_MS", "DEVAGENT_DEGRADE_WEBHOOK_URL",
		"DEVAGENT_ARCHIVE_KEEP", "DEVAGENT_MAX_PROMPT_BYTES",
	} {
		t.Setenv(v, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	scrubResilienceEnv(t)
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Worker: "omp", MaxLoops: 3, TimeoutMinutes: 30, GithubBaseBranch: "main"}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadFileOverrides(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		"devagent.json": `{"worker": "opencode", "maxLoops": 5, "model": "omniroute/x", "autoMerge": true}`,
	})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Worker != "opencode" || cfg.MaxLoops != 5 || cfg.Model != "omniroute/x" || cfg.AutoMerge == nil || !*cfg.AutoMerge {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if cfg.GithubBaseBranch != "main" {
		t.Fatalf("githubBaseBranch default lost: %+v", cfg)
	}
}

func TestLoadDotDevagentJsonFallback(t *testing.T) {
	dir := writeRepo(t, map[string]string{
		".devagent.json": `{"worker": "pi"}`,
		"devagent.json":  `{"worker": "grok"}`,
	})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	// First filename in CONFIG_FILENAMES wins; devagent.json is checked first.
	if cfg.Worker != "grok" {
		t.Fatalf("worker = %q, want grok", cfg.Worker)
	}
}

func TestLoadInvalidJSONPrefix(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{broken`})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("want error")
	}
	if got := err.Error(); !startsWith(got, "Invalid JSON in ") {
		t.Fatalf("error %q lacks the Node-compatible prefix", got)
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestLoadInvalidWorker(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"worker": "codex"}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid worker "codex" in config; expected claude-code, opencode, omp, pi, grok, or both` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidCleanup(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"cleanup": "sometimes"}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid cleanup "sometimes" in config; expected auto, keep, or always` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidScoutWorker(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"scout": {"worker": "codex"}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid scout.worker "codex"; expected claude-code, opencode, omp, pi, or grok` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidScoutInterval(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"scout": {"intervalMinutes": 0.5}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid scout.intervalMinutes "0.5"; expected >= 1` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidScoutMaxQueued(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"scout": {"maxQueued": 0}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid scout.maxQueued "0"; expected >= 1` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidAPIMaxAttempts(t *testing.T) {
	// A valid DEVAGENT_API_MAX_ATTEMPTS overrides the invalid file value in
	// the merged config, masking the error — scrub it first.
	scrubResilienceEnv(t)
	dir := writeRepo(t, map[string]string{"devagent.json": `{"resilience": {"apiMaxAttempts": -1}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid resilience.apiMaxAttempts "-1"; expected positive number or Infinity` {
		t.Fatalf("err = %v", err)
	}
}
func TestLoadInfinityAPIMaxAttempts(t *testing.T) {
	// JSON has no Infinity literal; both sides only reach it via the env
	// override (TestLoadEnvInfinityAPIMaxAttempts) or a string the TS side
	// would reject as a type error. Here: +Inf in the file is invalid JSON
	// for Go and a TS `JSON.parse` error too, so the byte-identical prefix
	// is the contract.
	dir := writeRepo(t, map[string]string{"devagent.json": `{"resilience": {"apiMaxAttempts": 1e999}}`})
	_, err := Load(dir)
	if err == nil || !startsWith(err.Error(), "Invalid JSON in ") {
		t.Fatalf("1e999 must surface as invalid JSON: %v", err)
	}
}

func TestLoadEnvInfinityAPIMaxAttempts(t *testing.T) {
	scrubResilienceEnv(t) // other overrides would leak into the exact marshal
	dir := t.TempDir()
	t.Setenv("DEVAGENT_API_MAX_ATTEMPTS", "Infinity")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resilience == nil || cfg.Resilience.APIMaxAttempts == nil || *cfg.Resilience.APIMaxAttempts <= 0 {
		t.Fatalf("env Infinity not applied: %+v", cfg.Resilience)
	}
	out, err := json.Marshal(cfg.Resilience)
	if err != nil {
		t.Fatal(err)
	}
	// JSON.stringify(Infinity) === null — the Go marshal must match.
	want := `{"apiMaxAttempts":null}`
	if string(out) != want {
		t.Fatalf("marshal = %s, want %s", out, want)
	}
}

func TestLoadEnvResilienceOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS", "600000")
	t.Setenv("DEVAGENT_COLD_START_TIMEOUT_MS", "90000")
	t.Setenv("DEVAGENT_DEGRADE_WEBHOOK_URL", "https://hooks.example/x")
	t.Setenv("DEVAGENT_ARCHIVE_KEEP", "5")
	t.Setenv("DEVAGENT_MAX_PROMPT_BYTES", "4096")
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Resilience
	if r == nil || *r.NoProgressTimeoutMs != 600000 || *r.ColdStartTimeoutMs != 90000 ||
		r.DegradeWebhookURL != "https://hooks.example/x" || *r.ArchiveKeep != 5 || *r.MaxPromptBytes != 4096 {
		t.Fatalf("env overrides missing: %+v", r)
	}
}

func TestLoadInvalidArchiveKeepNonInteger(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"resilience": {"archiveKeep": 2.5}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid resilience.archiveKeep "2.5"; expected a non-negative integer` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidDegradeWebhook(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"resilience": {"degradeWebhookUrl": "ftp://x"}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid resilience.degradeWebhookUrl "ftp://x"; expected an http(s) URL` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidHerdrSession(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"herdr": {"session": "Bad Session"}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid herdr.session "Bad Session"; expected [a-z][a-z0-9_-]{0,31}` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidHerdrSweepEnabledType(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"herdr": {"sweep": {"enabled": "yes"}}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid herdr.sweep.enabled "yes"; expected true or false` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidDenySessionsEntry(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"herdr": {"sweep": {"denySessions": ["ok", "Bad Name"]}}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid herdr.sweep.denySessions entry "Bad Name"; expected [a-z][a-z0-9_-]{0,31}` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidDenySessionsNotArray(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"herdr": {"sweep": {"denySessions": "devagent"}}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid herdr.sweep.denySessions "devagent"; expected an array of session names` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidZombieGraceDays(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"zombiePrs": {"graceDays": -1}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid zombiePrs.graceDays "-1"; expected >= 0` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidPRHygieneGraceHours(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"prHygiene": {"graceHours": -1}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid prHygiene.graceHours "-1"; expected >= 0` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidOrchestrateRegressionOracle(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"orchestrate": {"regressionOracle": "on"}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid orchestrate.regressionOracle "on"; expected true or false` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidContextKg(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"context": {"kg": "neo4j"}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid context.kg "neo4j"; expected leankg or off` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidContextAgentsMd(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"context": {"agentsMd": "maybe"}}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid context.agentsMd "maybe"; expected ask, on, or off` {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadInvalidLessonsDedupeSimilarity(t *testing.T) {
	dir := writeRepo(t, map[string]string{"devagent.json": `{"lessonsDedupeSimilarity": 1.5}`})
	_, err := Load(dir)
	if err == nil || err.Error() != `Invalid lessonsDedupeSimilarity "1.5"; expected a number between 0 and 1` {
		t.Fatalf("err = %v", err)
	}
}

func TestResolversDefaultsAndEnv(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if HerdrEnabled(cfg) {
		t.Error("herdr disabled by default")
	}
	t.Setenv("DEVAGENT_HERDR", "1")
	if !HerdrEnabled(cfg) {
		t.Error("DEVAGENT_HERDR=1 override ignored")
	}
	t.Setenv("DEVAGENT_HERDR", "0")
	if HerdrEnabled(cfg) {
		t.Error("DEVAGENT_HERDR=0 override ignored")
	}
	_ = os.Unsetenv("DEVAGENT_HERDR")

	if SpawnVisibility(cfg) != "visible" {
		t.Error("default visibility should be visible")
	}
	t.Setenv("DEVAGENT_VISIBILITY", "headless")
	if SpawnVisibility(cfg) != "headless" {
		t.Error("env visibility ignored")
	}
	_ = os.Unsetenv("DEVAGENT_VISIBILITY")

	if HerdrSessionName(cfg) != "devagent" {
		t.Error("default session should be devagent")
	}
	t.Setenv("DEVAGENT_HERDR_SESSION", "other")
	if HerdrSessionName(cfg) != "other" {
		t.Error("env session ignored")
	}
	_ = os.Unsetenv("DEVAGENT_HERDR_SESSION")

	sweep := ResolveHerdrSweep(cfg)
	if !sweep.Enabled || sweep.Orphans != nil || sweep.DenySessions != nil {
		t.Fatalf("default sweep = %+v", sweep)
	}
	t.Setenv("DEVAGENT_HERDR_SWEEP", "0")
	t.Setenv("DEVAGENT_HERDR_SWEEP_ORPHANS", "1")
	sweep = ResolveHerdrSweep(cfg)
	if sweep.Enabled != false || sweep.Orphans == nil || !*sweep.Orphans {
		t.Fatalf("env sweep overrides lost: %+v", sweep)
	}
}

func TestConfigCommandJSONShape(t *testing.T) {
	// The `config` command output: {config, credentials} — Infinity must
	// marshal as null (JSON.stringify parity) and booleans as booleans.
	dir := writeRepo(t, map[string]string{"devagent.json": `{"resilience": {"apiMaxAttempts": 3}}`})
	cfg, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(map[string]any{"config": cfg, "credentials": CredentialStatus(LoadCredentials())})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	c := parsed["config"].(map[string]any)
	if c["worker"] != "omp" || c["maxLoops"] != float64(3) {
		t.Fatalf("config output wrong: %s", out)
	}
	creds := parsed["credentials"].(map[string]any)
	if _, ok := creds["LINEAR_API_KEY"]; !ok {
		t.Fatalf("credentials keys missing: %s", out)
	}
}

func TestCredentialStatus(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "x")
	t.Setenv("PATH", t.TempDir()) // no gh on PATH: keep the #234 fallback out of this env-only test
	t.Setenv("GITHUB_TOKEN", "")
	resetGithubTokenCacheForTest()
	creds := LoadCredentials()
	status := CredentialStatus(creds)
	if !status["LINEAR_API_KEY"] || status["GITHUB_TOKEN"] {
		t.Fatalf("status = %v", status)
	}
}
