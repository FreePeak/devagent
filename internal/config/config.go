// Package config is the Go port of src/config.ts: effective configuration =
// defaults <- devagent.json (repo dir) <- env credentials/overrides.
// Credentials come exclusively from the environment (FR-OPS-02).
//
// Validation error strings are byte-identical to the TypeScript originals —
// they are embedded in operator-facing output and pinned by tests on both
// sides. Raw JSON is validated from map[string]any (exactly like the TS
// typeof checks) before decoding into the typed struct, so a wrong-typed
// value produces the same error a Node run produces.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/FreePeak/devagent/internal/workers/modelid"
)

func validateModelId(worker, model string) string {
	return modelid.ValidateModelId(worker, model)
}

// CleanupMode: post-run worktree disposal policy (auto-cleanup stage).
//   - "auto" (default): success -> snapshot+remove the run worktree;
//     failure -> preserve it for debugging.
//   - "keep": never remove (pre-2.0 preserve-for-inspection behavior).
//   - "always": snapshot+remove even when the run failed.
type CleanupMode = string

const (
	CleanupAuto   CleanupMode = "auto"
	CleanupKeep   CleanupMode = "keep"
	CleanupAlways CleanupMode = "always"
)

type ScoutConfig struct {
	Enabled         *bool    `json:"enabled,omitempty"`
	Worker          string   `json:"worker,omitempty"`
	IntervalMinutes *float64 `json:"intervalMinutes,omitempty"`
	MaxQueued       *float64 `json:"maxQueued,omitempty"`
	// Model override forwarded to the scout worker CLI (provider/model).
	Model string `json:"model,omitempty"`
	// SyncDocs: fetch + fast-forward origin before each live cycle so
	// docs/PRD.md reads see the operator's latest edits (default true).
	SyncDocs *bool `json:"syncDocs,omitempty"`
}

// HerdrSweepConfig: sweep-safety controls for `devagent herdr-sweep`
// (PRD §18 Q23, FR-VIS-10) — the managed-settings-style deny toggle.
type HerdrSweepConfig struct {
	// Enabled: master toggle for the whole sweep (default true — today's
	// behavior; loop drivers call it every iteration). Env override:
	// DEVAGENT_HERDR_SWEEP=0|1.
	Enabled *bool `json:"enabled,omitempty"`
	// Orphans: also close LIVE panes whose pane-run owner detached from any
	// loop driver. Unset defers to the caller's --orphans flag; env override
	// DEVAGENT_HERDR_SWEEP_ORPHANS=0|1 wins over both.
	Orphans *bool `json:"orphans,omitempty"`
	// DenySessions: sessions the sweep must never list or close, even when
	// they are the resolved target (the 2026-08-26 mass-kill class). Exact
	// session names.
	DenySessions []string `json:"denySessions,omitempty"`
}

type QueueConfig struct {
	Dir     string `json:"dir,omitempty"`
	PrdsDir string `json:"prdsDir,omitempty"`
}

// Config mirrors DevAgentConfig. Pointer fields distinguish "absent" from
// zero so the JSON round-trip and the TS semantics (undefined vs value)
// stay aligned.
type Config struct {
	Worker                  string             `json:"worker"`
	MaxLoops                int                `json:"maxLoops"`
	TimeoutMinutes          int                `json:"timeoutMinutes"`
	PinnedVersions          map[string]string  `json:"pinnedVersions,omitempty"`
	LinearTeamID            string             `json:"linearTeamId,omitempty"`
	GithubBaseBranch        string             `json:"githubBaseBranch,omitempty"`
	AutoMerge               *bool              `json:"autoMerge,omitempty"`
	Model                   string             `json:"model,omitempty"`
	Variant                 string             `json:"variant,omitempty"`
	TestCommand             string             `json:"testCommand,omitempty"`
	LessonsFile             string             `json:"lessonsFile,omitempty"`
	Cleanup                 string             `json:"cleanup,omitempty"`
	DropOrcaWorkspace       *bool              `json:"dropOrcaWorkspace,omitempty"`
	Scout                   *ScoutConfig       `json:"scout,omitempty"`
	Queue                   *QueueConfig       `json:"queue,omitempty"`
	SelfUpdate              *bool              `json:"selfUpdate,omitempty"`
	Resilience              *ResilienceConfig  `json:"resilience,omitempty"`
	Herdr                   *HerdrConfig       `json:"herdr,omitempty"`
	Spawn                   *SpawnConfig       `json:"spawn,omitempty"`
	LessonsMaxChars         *float64           `json:"lessonsMaxChars,omitempty"`
	Context                 *ContextConfig     `json:"context,omitempty"`
	LessonsDedupeSimilarity *float64           `json:"lessonsDedupeSimilarity,omitempty"`
	ZombiePrs               *ZombiePrsConfig   `json:"zombiePrs,omitempty"`
	PRHygiene               *PRHygieneConfig   `json:"prHygiene,omitempty"`
	Orchestrate             *OrchestrateConfig `json:"orchestrate,omitempty"`
}

// ResilienceConfig: worker retry budget is Infinity by default (apiMaxAttempts
// caps it when set); noProgressTimeoutMs=0 disables the watchdog, a number
// enables it (default 10m when the block is present); coldStartTimeoutMs is
// the first-progress deadline (Q31); degradeWebhookUrl pages a human (Q41,
// also carries board-archived alerts, Q16); archiveKeep bounds
// .devagent/archive/ retention; maxPromptBytes refuses oversized prescriptive
// prompts at dispatch (Q18). Env overrides: DEVAGENT_API_MAX_ATTEMPTS,
// DEVAGENT_NO_PROGRESS_TIMEOUT_MS, DEVAGENT_COLD_START_TIMEOUT_MS,
// DEVAGENT_DEGRADE_WEBHOOK_URL, DEVAGENT_ARCHIVE_KEEP, DEVAGENT_MAX_PROMPT_BYTES.
type ResilienceConfig struct {
	APIMaxAttempts      *float64 `json:"apiMaxAttempts,omitempty"`
	NoProgressTimeoutMs *float64 `json:"noProgressTimeoutMs,omitempty"`
	ColdStartTimeoutMs  *float64 `json:"coldStartTimeoutMs,omitempty"`
	DegradeWebhookURL   string   `json:"degradeWebhookUrl,omitempty"`
	ArchiveKeep         *float64 `json:"archiveKeep,omitempty"`
	MaxPromptBytes      *float64 `json:"maxPromptBytes,omitempty"`
}

// MarshalJSON mirrors JSON.stringify semantics: a JS Infinity serializes as
// null, so the Go side does the same (encoding/json would error on +Inf).
func (r ResilienceConfig) MarshalJSON() ([]byte, error) {
	// JS semantics, emitted in declaration order: undefined (nil) is omitted,
	// Infinity serializes as null, numbers as numbers.
	pairs := []struct {
		name  string
		value *float64
	}{
		{"apiMaxAttempts", r.APIMaxAttempts},
		{"noProgressTimeoutMs", r.NoProgressTimeoutMs},
		{"coldStartTimeoutMs", r.ColdStartTimeoutMs},
		{"archiveKeep", r.ArchiveKeep},
		{"maxPromptBytes", r.MaxPromptBytes},
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.value == nil {
			continue // undefined: omitted by JSON.stringify
		}
		if math.IsInf(*p.value, 0) {
			parts = append(parts, `"`+p.name+`":null`)
			continue
		}
		b, err := json.Marshal(*p.value)
		if err != nil {
			return nil, err
		}
		parts = append(parts, `"`+p.name+`":`+string(b))
	}
	if r.DegradeWebhookURL != "" {
		b, err := json.Marshal(r.DegradeWebhookURL)
		if err != nil {
			return nil, err
		}
		// degradeWebhookUrl is declared before archiveKeep in the TS
		// interface; keep that order.
		parts = append(parts[:5], append([]string{`"degradeWebhookUrl":` + string(b)}, parts[5:]...)...)
	}
	return []byte("{" + strings.Join(parts, ",") + "}"), nil
}

// HerdrConfig: run worker CLIs inside herdr panes in a dedicated persistent
// session so runs are visible, reattachable, and survive disconnects. Opt-in.
// Env overrides: DEVAGENT_HERDR=1|0, DEVAGENT_HERDR_SESSION=<name>. `sweep`
// bounds the blast radius of `devagent herdr-sweep` (Q23/FR-VIS-10).
type HerdrConfig struct {
	Enabled *bool             `json:"enabled,omitempty"`
	Session string            `json:"session,omitempty"`
	Sweep   *HerdrSweepConfig `json:"sweep,omitempty"`
}

// SpawnConfig: spawn visibility (FR-VIS-01/04) — whether worker CLIs launch
// in herdr panes the operator can jump into ("visible") or as plain child
// processes ("headless"). Resolved by SpawnVisibility(): DEVAGENT_VISIBILITY
// env > config > "visible".
type SpawnConfig struct {
	Visibility string `json:"visibility,omitempty"`
}

// ContextConfig: knowledge-context layers (FR-CTX-01..03). The
// .devagent/context/*.md baseline is always-on and needs no configuration;
// `kg` opts into the structural KG digest tier. `agentsMd` gates the
// .devagent/AGENTS.md auto-load (PRD §18 Q11): "ask" (the default) injects
// nothing until `devagent trust agents-md` records a one-time per-repo
// confirm in .devagent/trust.json; "on" loads without the confirm; "off"
// disables loading.
type ContextConfig struct {
	Kg       string `json:"kg,omitempty"`
	AgentsMd string `json:"agentsMd,omitempty"`
}

type ZombiePrsConfig struct {
	GraceDays *float64 `json:"graceDays,omitempty"`
	DryRun    *bool    `json:"dryRun,omitempty"`
}

type PRHygieneConfig struct {
	GraceHours *float64 `json:"graceHours,omitempty"`
	DryRun     *bool    `json:"dryRun,omitempty"`
}

// OrchestrateConfig: regressionOracle — before an auto-merge, check out the
// PR branch in a throwaway worktree and run the repo's full test suite; a red
// suite blocks the merge. Default on for JS repos (a test command is
// detected); non-JS repos skip the gate unless set true.
type OrchestrateConfig struct {
	RegressionOracle *bool `json:"regressionOracle,omitempty"`
}

// Credentials: env-only, never logged (FR-OPS-02).
type Credentials struct {
	LinearAPIKey string
	GithubToken  string
}

// DefaultConfig mirrors DEFAULT_CONFIG.
func DefaultConfig() Config {
	return Config{
		Worker:           "omp",
		MaxLoops:         3,
		TimeoutMinutes:   30,
		GithubBaseBranch: "main",
	}
}

var CONFIG_FILENAMES = []string{"devagent.json", ".devagent.json"}

// HERDR_SESSION_RE: herdr session names — the shape `herdr.session` and every
// `herdr.sweep.denySessions` entry must match.
var HERDR_SESSION_RE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// tsString mimics JS String() for the values that can reach the
// "Invalid ..." interpolations: bool, float64, string, nil, []any, map.
func tsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return jsNumber(x)
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = tsString(e)
		}
		return strings.Join(parts, ",")
	default:
		return fmt.Sprint(x)
	}
}

// jsNumber formats a float64 the way JS template interpolation does for the
// ranges these configs use.
func jsNumber(f float64) string {
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func isInt(f float64) bool { return f == math.Trunc(f) && !math.IsInf(f, 0) }

func rawString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func rawFloat(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// Load mirrors loadConfig(repoPath): find devagent.json / .devagent.json in
// repoPath, layer env overrides, validate with byte-identical error strings,
// return the effective config.
func Load(repoPath string) (Config, error) {
	cfg := DefaultConfig()

	raw := map[string]any{}
	for _, name := range CONFIG_FILENAMES {
		p := filepath.Join(repoPath, name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			// Node message shape: `Invalid JSON in <path>: <reason>`. The
			// reason text is runtime-specific (V8 vs Go); the prefix is the
			// contract callers match on.
			return Config{}, fmt.Errorf("Invalid JSON in %s: %v", p, err)
		}
		break
	}

	// Env overrides for the resilience block (exactly the TS envResilience).
	envResilience := map[string]any{}
	if v := os.Getenv("DEVAGENT_API_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 && !math.IsInf(n, 0) {
			envResilience["apiMaxAttempts"] = n
		} else if strings.EqualFold(v, "infinity") {
			envResilience["apiMaxAttempts"] = math.Inf(1)
		}
	}
	if v := os.Getenv("DEVAGENT_NO_PROGRESS_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 && !math.IsInf(n, 0) {
			envResilience["noProgressTimeoutMs"] = n
		}
	}
	if v := os.Getenv("DEVAGENT_COLD_START_TIMEOUT_MS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 && !math.IsInf(n, 0) {
			envResilience["coldStartTimeoutMs"] = n
		}
	}
	if v := os.Getenv("DEVAGENT_DEGRADE_WEBHOOK_URL"); v != "" {
		envResilience["degradeWebhookUrl"] = v
	}
	if v := os.Getenv("DEVAGENT_ARCHIVE_KEEP"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && isInt(n) && n >= 0 {
			envResilience["archiveKeep"] = n
		}
	}
	if v := os.Getenv("DEVAGENT_MAX_PROMPT_BYTES"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 && !math.IsInf(n, 0) {
			envResilience["maxPromptBytes"] = n
		}
	}

	fileResilience, _ := raw["resilience"].(map[string]any)
	mergedResilience := map[string]any{}
	for k, v := range fileResilience {
		mergedResilience[k] = v
	}
	for k, v := range envResilience {
		mergedResilience[k] = v
	}
	if len(mergedResilience) > 0 {
		raw["resilience"] = mergedResilience
	}

	// ---- Validation (order and messages byte-identical to src/config.ts) ----
	worker, _ := rawString(raw, "worker")
	if worker == "" {
		worker = cfg.Worker // default applies before validation reads it
	}
	switch worker {
	case "claude-code", "opencode", "omp", "pi", "grok", "both":
	default:
		return Config{}, fmt.Errorf("Invalid worker %q in config; expected claude-code, opencode, omp, pi, grok, or both", worker)
	}
	if c, ok := rawString(raw, "cleanup"); ok {
		switch c {
		case "auto", "keep", "always":
		default:
			return Config{}, fmt.Errorf("Invalid cleanup %q in config; expected auto, keep, or always", c)
		}
	}
	if scout, ok := raw["scout"].(map[string]any); ok {
		if w, present := rawString(scout, "worker"); present {
			switch w {
			case "claude-code", "opencode", "omp", "pi", "grok":
			default:
				return Config{}, fmt.Errorf("Invalid scout.worker %q; expected claude-code, opencode, omp, pi, or grok", w)
			}
		}
		if n, present := rawFloat(scout, "intervalMinutes"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 1 {
				return Config{}, fmt.Errorf("Invalid scout.intervalMinutes %q; expected >= 1", jsNumber(n))
			}
		}
		if n, present := rawFloat(scout, "maxQueued"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 1 {
				return Config{}, fmt.Errorf("Invalid scout.maxQueued %q; expected >= 1", jsNumber(n))
			}
		}
	} else if _, present := raw["scout"]; present && raw["scout"] != nil {
		return Config{}, fmt.Errorf("Invalid scout %q; expected an object", tsString(raw["scout"]))
	}
	if r, ok := raw["resilience"].(map[string]any); ok {
		if n, present := rawFloat(r, "apiMaxAttempts"); present {
			if !((!math.IsInf(n, 0) && !math.IsNaN(n) && n > 0) || math.IsInf(n, 1)) {
				return Config{}, fmt.Errorf("Invalid resilience.apiMaxAttempts %q; expected positive number or Infinity", jsNumber(n))
			}
		}
		if n, present := rawFloat(r, "noProgressTimeoutMs"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
				return Config{}, fmt.Errorf("Invalid resilience.noProgressTimeoutMs %q; expected >= 0", jsNumber(n))
			}
		}
		if n, present := rawFloat(r, "coldStartTimeoutMs"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
				return Config{}, fmt.Errorf("Invalid resilience.coldStartTimeoutMs %q; expected >= 0", jsNumber(n))
			}
		}
		if u, present := rawString(r, "degradeWebhookUrl"); present {
			if !regexp.MustCompile(`^https?://\S+$`).MatchString(u) {
				return Config{}, fmt.Errorf("Invalid resilience.degradeWebhookUrl %q; expected an http(s) URL", u)
			}
		}
		if n, present := rawFloat(r, "archiveKeep"); present {
			if !isInt(n) || n < 0 {
				return Config{}, fmt.Errorf("Invalid resilience.archiveKeep %q; expected a non-negative integer", jsNumber(n))
			}
		}
		if n, present := rawFloat(r, "maxPromptBytes"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
				return Config{}, fmt.Errorf("Invalid resilience.maxPromptBytes %q; expected >= 0", jsNumber(n))
			}
		}
	}
	if herdr, ok := raw["herdr"].(map[string]any); ok {
		if s, present := rawString(herdr, "session"); present {
			if !HERDR_SESSION_RE.MatchString(s) {
				return Config{}, fmt.Errorf("Invalid herdr.session %q; expected [a-z][a-z0-9_-]{0,31}", s)
			}
		}
		if sw, ok := herdr["sweep"].(map[string]any); ok {
			if v, present := sw["enabled"]; present && v != nil {
				if _, isBool := v.(bool); !isBool {
					return Config{}, fmt.Errorf("Invalid herdr.sweep.enabled %q; expected true or false", tsString(v))
				}
			}
			if v, present := sw["orphans"]; present && v != nil {
				if _, isBool := v.(bool); !isBool {
					return Config{}, fmt.Errorf("Invalid herdr.sweep.orphans %q; expected true or false", tsString(v))
				}
			}
			if v, present := sw["denySessions"]; present && v != nil {
				arr, isArr := v.([]any)
				if !isArr {
					return Config{}, fmt.Errorf("Invalid herdr.sweep.denySessions %q; expected an array of session names", tsString(v))
				}
				for _, denied := range arr {
					s, isStr := denied.(string)
					if !isStr || !HERDR_SESSION_RE.MatchString(s) {
						return Config{}, fmt.Errorf("Invalid herdr.sweep.denySessions entry %q; expected [a-z][a-z0-9_-]{0,31}", tsString(denied))
					}
				}
			}
		}
	}
	if z, ok := raw["zombiePrs"].(map[string]any); ok {
		if n, present := rawFloat(z, "graceDays"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
				return Config{}, fmt.Errorf("Invalid zombiePrs.graceDays %q; expected >= 0", jsNumber(n))
			}
		}
	}
	if h, ok := raw["prHygiene"].(map[string]any); ok {
		if n, present := rawFloat(h, "graceHours"); present {
			if math.IsInf(n, 0) || math.IsNaN(n) || n < 0 {
				return Config{}, fmt.Errorf("Invalid prHygiene.graceHours %q; expected >= 0", jsNumber(n))
			}
		}
	}
	if o, ok := raw["orchestrate"].(map[string]any); ok {
		if v, present := o["regressionOracle"]; present && v != nil {
			if _, isBool := v.(bool); !isBool {
				return Config{}, fmt.Errorf("Invalid orchestrate.regressionOracle %q; expected true or false", tsString(v))
			}
		}
	}
	if ctx, ok := raw["context"].(map[string]any); ok {
		if kg, present := rawString(ctx, "kg"); present {
			switch kg {
			case "leankg", "off":
			default:
				return Config{}, fmt.Errorf("Invalid context.kg %q; expected leankg or off", kg)
			}
		}
		if am, present := rawString(ctx, "agentsMd"); present {
			switch am {
			case "ask", "on", "off":
			default:
				return Config{}, fmt.Errorf("Invalid context.agentsMd %q; expected ask, on, or off", am)
			}
		}
	}
	if t, present := rawFloat(raw, "lessonsDedupeSimilarity"); present {
		if math.IsInf(t, 0) || math.IsNaN(t) || t < 0 || t > 1 {
			return Config{}, fmt.Errorf("Invalid lessonsDedupeSimilarity %q; expected a number between 0 and 1", jsNumber(t))
		}
	}

	// ---- Decode the validated raw into the typed struct ----
	// encoding/json cannot carry +Inf (JSON.stringify emits null for it), so
	// Inf values are stripped for the decode pass and re-applied from the
	// env-override map afterwards — the only path that can produce Inf.
	blob, err := json.Marshal(sanitizeInfinity(raw))
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(blob, &cfg); err != nil {
		return Config{}, err
	}
	if worker != "" {
		cfg.Worker = worker
	}
	if v, ok := envResilience["apiMaxAttempts"]; ok {
		if f, ok := v.(float64); ok && math.IsInf(f, 1) {
			if cfg.Resilience == nil {
				cfg.Resilience = &ResilienceConfig{}
			}
			inf := math.Inf(1)
			cfg.Resilience.APIMaxAttempts = &inf
		}
	}
	return cfg, nil
}

// sanitizeInfinity returns a deep copy of raw with +Inf/-Inf values removed
// so the decode pass can marshal it. JSON.stringify emits null for Infinity;
// the typed decode treats an absent field the same way, and Load re-applies
// the env-provided Inf after the decode.
func sanitizeInfinity(raw map[string]any) map[string]any {
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		switch x := v.(type) {
		case float64:
			if !math.IsInf(x, 0) {
				out[k] = v
			}
		case map[string]any:
			out[k] = sanitizeInfinity(x)
		default:
			out[k] = v
		}
	}
	return out
}

// parseEnvFlag is the tri-state boolean env flag: 1/true -> true, 0/false ->
// false, unset or unrecognized -> nil so the caller falls through to
// config/default (the spawnVisibility precedent — a typo must not silently
// arm a destructive path).
func parseEnvFlag(value string) *bool {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "1", "true":
		b := true
		return &b
	case "0", "false":
		b := false
		return &b
	default:
		return nil
	}
}

// HerdrEnabled: whether worker commands should execute inside herdr panes.
// Config opt-in (herdr.enabled), with DEVAGENT_HERDR=1|0 as an env override.
func HerdrEnabled(cfg Config) bool {
	if env := os.Getenv("DEVAGENT_HERDR"); env != "" {
		return env != "0" && strings.ToLower(env) != "false"
	}
	return cfg.Herdr != nil && cfg.Herdr.Enabled != nil && *cfg.Herdr.Enabled
}

// SpawnVisibility resolves spawn visibility (FR-VIS-01/04): DEVAGENT_VISIBILITY
// env wins, then config spawn.visibility, defaulting to "visible" — worker
// CLIs land in observable herdr panes unless the operator opts out.
// Unrecognized env values fall through to the config/default rather than
// failing dispatch.
func SpawnVisibility(cfg Config) string {
	if env := os.Getenv("DEVAGENT_VISIBILITY"); env == "visible" || env == "headless" {
		return env
	}
	if cfg.Spawn != nil && cfg.Spawn.Visibility == "headless" {
		return "headless"
	}
	return "visible"
}

// HerdrSessionName: target herdr session name (config herdr.session, env
// DEVAGENT_HERDR_SESSION, else "devagent").
func HerdrSessionName(cfg Config) string {
	if env := os.Getenv("DEVAGENT_HERDR_SESSION"); env != "" {
		return env
	}
	if cfg.Herdr != nil && cfg.Herdr.Session != "" {
		return cfg.Herdr.Session
	}
	return "devagent"
}

// HerdrSweepSettings is the resolved herdr.sweep section.
type HerdrSweepSettings struct {
	Enabled bool
	// Orphans: nil = unset — the caller's own --orphans flag decides.
	Orphans      *bool
	DenySessions []string
}

// ResolveHerdrSweep resolves sweep-safety (PRD §18 Q23, FR-VIS-10): env wins
// over config, config wins over the defaults — which are today's behavior
// (sweep on, orphan class left to the caller's --orphans flag), so
// scripts/selfbuild-loop.sh keeps working with no config at all. (The TS
// side names both the interface and the resolver herdrSweepConfig; Go keeps
// the type HerdrSweepConfig and the resolver ResolveHerdrSweep.)
func ResolveHerdrSweep(cfg Config) HerdrSweepSettings {
	out := HerdrSweepSettings{Enabled: true}
	if cfg.Herdr != nil && cfg.Herdr.Sweep != nil {
		if cfg.Herdr.Sweep.Enabled != nil {
			out.Enabled = *cfg.Herdr.Sweep.Enabled
		}
		out.Orphans = cfg.Herdr.Sweep.Orphans
		out.DenySessions = append([]string(nil), cfg.Herdr.Sweep.DenySessions...)
	}
	if b := parseEnvFlag(os.Getenv("DEVAGENT_HERDR_SWEEP")); b != nil {
		out.Enabled = *b
	}
	if b := parseEnvFlag(os.Getenv("DEVAGENT_HERDR_SWEEP_ORPHANS")); b != nil {
		out.Orphans = b
	}
	return out
}

// LoadCredentials: credentials come exclusively from the environment
// (FR-OPS-02).
func LoadCredentials() Credentials {
	return Credentials{
		LinearAPIKey: os.Getenv("LINEAR_API_KEY"),
		GithubToken:  os.Getenv("GITHUB_TOKEN"),
	}
}

// CredentialStatus reports which credentials are present without ever
// printing values (FR-OPS-02).
func CredentialStatus(creds Credentials) map[string]bool {
	return map[string]bool{
		"LINEAR_API_KEY": creds.LinearAPIKey != "",
		"GITHUB_TOKEN":   creds.GithubToken != "",
	}
}

// ValidateWorkerModel validates config.model against the worker adapter's
// accepted id shape at dispatch preflight (Q32). Returns "" when acceptable,
// otherwise a one-line reason.
func ValidateWorkerModel(worker, model string) string {
	return modelidValidate(worker, model)
}

// modelidValidate forwards to the model-id registry without importing it at
// package top (kept here so Load's callers have one import surface, matching
// the TS validateWorkerModel wrapper).
func modelidValidate(worker, model string) string {
	return validateModelId(worker, model)
}
