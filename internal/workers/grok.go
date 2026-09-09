// Go port of src/workers/grok.ts (FR-GO-05).
//
// Adapter over the `grok` (Grok Build CLI) headless mode. Mirrors the
// structure of the omp adapter so the executor can treat it as just
// another WorkerAdapter. grok-specific behavior (FR-GROK-01, docs/GROK.md
// §2.2):
//   - Uses `-p <prompt> --output-format streaming-json` (NDJSON ACP events).
//   - Resume via `-c` (continue most-recent session in cwd) + RESUME_PROMPT.
//   - Model forwarded only when exact-slug or `xai/`-qualified (FR-GROK-02).
//   - No `--api-key` CLI flag — browser OAuth or `XAI_API_KEY` env are the
//     supported credential channels (XAI_API_KEY is sandbox-allowlisted).
//   - Per-task prompt-cache key (FR-GROK-04) rides the spawn env as
//     `DEVAGENT_PROMPT_CACHE_KEY=devagent-<taskId>`, derived once per spawn
//     so every attempt of the retry loop keeps the cache warm; never argv.
//   - Provider failures can surface as in-stream `error` events at exit 0;
//     the parser captures them (see interpretGrok).
//   - TPM-class 429s step the within-xAI fallback chain
//     (`grok-4.6 → grok-4.3 → grok-build-0.1`, FR-GROK-06) before the
//     exhausted-chain result can reach cross-provider fallback above.

package workers

import (
	"strings"

	"github.com/FreePeak/devagent/internal/workers/modelid"
)

const grokResumePrompt = "Continue"

// grokDefaultNoProgressTimeoutMs mirrors the omp default: a silent
// provider stall must trip the watchdog so the retry loop fires, instead
// of the wall clock being the only net.
const grokDefaultNoProgressTimeoutMs = 10 * 60 * 1000

// GrokArgsOptions mirrors TS GrokArgsOptions.
type GrokArgsOptions struct {
	// Resume builds a resume argv (uses -c + -p RESUME_PROMPT).
	Resume bool
	// Model is the FR-GROK-06 within-xAI chain override — replaces
	// opts.model for this attempt when a TPM-class retry steps to the next
	// rung. Still validated by the FR-GROK-02 predicate at forward time.
	Model string
}

// BuildGrokArgs builds the exact argv passed to `grok` (Grok Build CLI)
// for a given spawn. Pure function — exercised at the test seam without
// spawning the CLI.
//
//	grok -p <prompt> --output-format streaming-json [--model <m>]
//	grok -c -p Continue --output-format streaming-json [--model <m>]   (resume)
//
// `--output-format streaming-json` is the NDJSON ACP session-update stream
// (captured live 2026-09-06 against grok 1.0.13; same adapter shape as
// omp). Resume uses `-c` (continue the most recent session for the cwd) —
// verified live: the continued run restored the prior sessionId and
// cache-read the earlier turn. Like omp, we do NOT thread `-r <id>`
// between attempts: an explicit resume id would cross-talk between
// concurrent devagent runs sharing the same cwd.
//
// Model forwarding follows the FR-GROK-02 predicate (modelid package):
// only exact xAI slugs (`grok-4.6`, `grok-build-0.1`, dated pins) or
// `xai/`-qualified ids reach the CLI. Driver tier aliases like "coding"
// (devagent.json model) are claude-code proxy selectors, not grok ids —
// the loop-58 `--model coding` burn is the precedent. Dropping them lets
// grok fall back to its configured default (~/.grok/config.toml [models]
// default), which is the intended worker model anyway.
func BuildGrokArgs(opts WorkerSpawnOptions, o GrokArgsOptions) []string {
	rawModel := strings.TrimSpace(opts.Model)
	if o.Model != "" {
		rawModel = strings.TrimSpace(o.Model)
	}
	var grokModel string
	if rawModel != "" && isGrokModelId(rawModel) {
		grokModel = rawModel
	}
	base := []string{"--output-format", "streaming-json"}
	if o.Resume {
		args := []string{"-c", "-p", grokResumePrompt}
		args = append(args, base...)
		if grokModel != "" {
			args = append(args, "--model", grokModel)
		}
		return args
	}
	args := []string{"-p", opts.Prompt}
	args = append(args, base...)
	if grokModel != "" {
		args = append(args, "--model", grokModel)
	}
	return args
}

// isGrokModelId delegates to the modelid package port
// (src/workers/model-id.ts isGrokModelId; internal/workers/modelid is the
// merged FR-GO-01/02 port — reuse, do not duplicate).
func isGrokModelId(model string) bool {
	return modelid.IsGrokModelId(model)
}

// GrokPromptCacheKeyEnv is the FR-GROK-04 env var carrying the per-task
// prompt-cache key into the grok child. The Grok Build CLI (argv verified
// against 1.0.13) has no cache-key flag — the supported per-request
// channel is config-driven:
// `[model.<id>].env_http_headers = { "x-grok-conv-id" =
// "DEVAGENT_PROMPT_CACHE_KEY" }` in ~/.grok/config.toml maps this env var
// to the sticky-routing header xAI prices cached input against (~25% of
// input cost, PRD §20.2). Like XAI_API_KEY, it reaches the CLI through the
// spawn env, never argv.
const GrokPromptCacheKeyEnv = "DEVAGENT_PROMPT_CACHE_KEY"

// GrokPromptCacheKey derives the per-task prompt-cache key (FR-GROK-04).
// Deterministic function of the task id the dispatcher already threads
// (watchdogLedger.taskId — same identity source as the FR-GROK-03 cost
// rows), so every attempt in a task's retry loop and every re-dispatch of
// that task carries the identical key instead of going cache-cold.
// Deliberately excludes attempt/retry fields: a key that moved per attempt
// would defeat stickiness. Returns "" for probe/one-off spawns without
// ledger context — no key is emitted rather than a fabricated
// per-process one.
func GrokPromptCacheKey(opts WorkerSpawnOptions) string {
	if opts.WatchdogLedger == nil {
		return ""
	}
	taskId := strings.TrimSpace(opts.WatchdogLedger.TaskId)
	if taskId == "" {
		return ""
	}
	return "devagent-" + taskId
}

// GrokPromptCacheEnv is the env overlay for the grok child carrying the
// FR-GROK-04 cache key. Empty when the spawn has no task context, so the
// caller env passes through untouched and no key is emitted.
func GrokPromptCacheEnv(opts WorkerSpawnOptions) map[string]string {
	key := GrokPromptCacheKey(opts)
	if key == "" {
		return map[string]string{}
	}
	return map[string]string{GrokPromptCacheKeyEnv: key}
}

// GrokFallbackChain is the FR-GROK-06 within-xAI fallback chain, ordered
// by preference (PRD §20.2: grok-4.6 is the recommended coding default,
// grok-4.3 the 1M-ctx sibling, grok-build-0.1 the small-context escape
// hatch). A TPM (tokens-per-minute) 429 is context-shaped — a
// smaller-context model in the same provider can serve the same turn
// where a cooldown cannot — so the retry loop steps this chain before any
// cross-provider fallback, which lives above the adapter and only sees
// the exhausted-chain result.
var GrokFallbackChain = []string{"grok-4.6", "grok-4.3", "grok-build-0.1"}

// grokChainRung positions a model at its chain rung: the first chain
// prefix it starts with (dated pins like `grok-4.6-2026-08-14` sit at the
// `grok-4.6` rung; `xai/grok-4.3` at `grok-4.3`). -1 when not a chain
// family.
func grokChainRung(model string) int {
	raw := strings.TrimSpace(model)
	for i, rung := range GrokFallbackChain {
		if raw == rung || strings.HasPrefix(raw, rung+"-") || strings.HasPrefix(raw, "xai/"+rung) {
			return i
		}
	}
	return -1
}

// GrokChainNext is the pure chain selector: the next model to try after
// `model` hits a TPM-class limit. Accepts `xai/`-qualified ids and dated
// pins (`grok-4.6-2026-08-14` sits at the `grok-4.6` rung). Unset/empty
// starts at the chain head (the CLI default just burned its TPM budget —
// pin the next attempt explicitly). A non-chain grok family (e.g.
// `grok-4.5`) or the chain tail yields "" (TS null): the selector never
// second-guesses an operator-pinned model and never invents a fourth rung.
func GrokChainNext(model string) string {
	raw := strings.TrimSpace(model)
	if raw == "" {
		return GrokFallbackChain[0]
	}
	rung := grokChainRung(raw)
	if rung < 0 || rung == len(GrokFallbackChain)-1 {
		return ""
	}
	return GrokFallbackChain[rung+1]
}

// GrokRetryModel is the pure retry-model decision: advance the within-xAI
// chain only on a `rate-limit-tpm` transient class (FR-GROK-06). Every
// other outcome — RPS 429 (the same-model cooldown is the fix), 5xx,
// timeouts, non-transient — keeps the current model. Chain exhaustion
// returns the current model unchanged. current == "" (TS undefined) maps
// to "" return when the chain cannot advance.
func GrokRetryModel(current string, errorText string) string {
	if TransientErrorClass(errorText) != "rate-limit-tpm" {
		return current
	}
	next := GrokChainNext(current)
	if next == "" {
		return current
	}
	return next
}

// GrokOutcome mirrors TS GrokOutcome.
type GrokOutcome struct {
	IsError    bool
	SessionId  string
	ErrorText  string
	ResultText string
	Parsed     map[string]any
	TimedOut   bool
	// CostUsdTicks: FR-GROK-03 exact xAI cost in integer USD ticks, copied
	// verbatim from the stream's `usage`/`end` events. Nil when neither
	// event carried the field — a missing cost is never coerced to 0.
	CostUsdTicks *float64
	// ToolCallCount: FR-GROK-06 number of whole `tool_call` events seen.
	// xAI streams tool calls whole (not token-streamed), so a run can
	// legitimately end on one; the count is the evidence the stream was
	// not empty.
	ToolCallCount int
}

// IsGrokProgressLine is the meaningful-line filter for grok's
// streaming-json stream (PRD Q33 port of the omp/pi precedent): `thought`
// chunks are pure deliberation (48 of them in a 10s echo run, 2026-09-06
// capture) and must never reset the watchdog; tool calls and finalized
// text chunks are new work.
func IsGrokProgressLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	if strings.Contains(line, `"type":"thought"`) {
		return false
	}
	if strings.Contains(line, `"type":"tool_call"`) {
		return true
	}
	if strings.Contains(line, `"type":"tool_call_update"`) {
		return true
	}
	if strings.Contains(line, `"type":"text","data"`) {
		return true
	}
	return false
}

// InterpretGrokForTest is the test seam re-export of the parser.
func InterpretGrokForTest(run SpawnCliResult) GrokOutcome {
	return interpretGrok(run)
}

// pickCostTicks: FR-GROK-03 — read the exact cost figure off one stream
// event, verbatim — no rounding, no currency conversion. grok's `end`
// event carries the run total as `total_cost_usd_ticks`; the xAI API/PRD
// names the same quantity `usage.cost_in_usd_ticks`. Accept either
// spelling, at the event top level or nested under `usage`, and return
// the first finite number found. Absent or non-finite yields nil — never
// 0 — so a run the provider did not price stays distinct from a genuinely
// free (0-tick) run.
func pickCostTicks(event map[string]any) *float64 {
	var candidates []any
	candidates = append(candidates, event["cost_in_usd_ticks"])
	candidates = append(candidates, event["total_cost_usd_ticks"])
	if usage, ok := event["usage"].(map[string]any); ok {
		candidates = append(candidates, usage["cost_in_usd_ticks"])
		candidates = append(candidates, usage["total_cost_usd_ticks"])
	}
	for _, c := range candidates {
		if n, ok := c.(float64); ok && !isInfOrNaN(n) {
			v := n
			return &v
		}
	}
	return nil
}

func isInfOrNaN(f float64) bool {
	return f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308
}

// interpretGrok parses grok's stdout into the worker outcome shape. grok
// --output-format streaming-json emits NDJSON ACP session updates, one
// JSON object per line, per the live captures in testdata/:
// grok-smoke-2026-09-06.jsonl (clean answer run),
// grok-toolrun-2026-09-06.jsonl (chunked text + tool events), and
// grok-error-2026-09-06.jsonl (in-stream provider failure). Event shapes
// (grok 1.0.13):
//
//	{"type":"available_commands","tools":[...]}   startup header
//	{"type":"thought","data":"<chunk>"}           thinking (ignored)
//	{"type":"text","data":"<chunk>"}              assistant text chunks
//	{"type":"tool_call"|"tool_call_update",...}   ACP tool events (whole, not token-streamed)
//	{"type":"usage","usage":{...}}                token accounting
//	{"type":"error","message":"..."}              provider failure
//	{"type":"end","sessionId":"...","stopReason":"end_turn","usage":...}
//
// Assistant text policy: concatenate every `text` chunk in stream order —
// grok emits the answer as word-level chunks, so the full assistant text
// is the join (single-chunk answers like "OK" pass through unchanged).
//
// Like omp, grok can exit 0 while the provider call failed: the failure
// surfaces as an in-stream `error` event. Capture the first message so
// the failure is not misread as an empty-but-successful run.
//
// Stream-tool-call-whole (FR-GROK-06): because tool calls arrive whole, a
// turn can end on a `tool_call`/`tool_call_update` event with no trailing
// `text` or `end` event. That shape is real progress, not an empty/failed
// run: the parser keeps the last tool event as the outcome's `parsed`
// (finalize turns it into a result event, defeating the zero-events empty
// signature) and stderr noise is never promoted to errorText while tool
// activity is present.
//
// Cost accounting (FR-GROK-03): the `usage` and `end` events carry the
// exact xAI cost as integer USD ticks. Copy it verbatim onto the outcome
// (no rounding, no currency conversion); the `end` total wins over any
// earlier `usage` row. A run whose events omit the field leaves the cost
// nil — never 0.
func interpretGrok(run SpawnCliResult) GrokOutcome {
	sessionId := ""
	streamError := ""
	var endEvent map[string]any
	var costUsdTicks *float64
	toolCallCount := 0
	var lastToolCall map[string]any
	var textChunks []string

	for _, line := range strings.Split(run.Stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		v, ok := parseJSONAny(trimmed)
		if !ok {
			continue
		}
		event, ok := v.(map[string]any)
		if !ok {
			continue
		}
		// Forward-compat: a `session` header (omp shape) also names the id.
		if event["type"] == "session" {
			if id, ok := event["id"].(string); ok {
				sessionId = id
			}
		}
		if event["type"] == "text" {
			if data, ok := event["data"].(string); ok {
				textChunks = append(textChunks, data)
			}
		}
		switch event["type"] {
		case "tool_call":
			toolCallCount++
			lastToolCall = event
		case "tool_call_update":
			lastToolCall = event
		}
		if event["type"] == "error" && streamError == "" {
			if msg, ok := event["message"].(string); ok {
				streamError = msg
			}
		}
		if event["type"] == "usage" {
			if c := pickCostTicks(event); c != nil {
				costUsdTicks = c
			}
		}
		if event["type"] == "end" {
			endEvent = event
			if sid, ok := event["sessionId"].(string); ok {
				sessionId = sid
			}
			// The terminal `end` event is the authoritative accounting; it
			// overrides any earlier `usage` row. Absent cost here leaves
			// the usage value intact.
			if c := pickCostTicks(event); c != nil {
				costUsdTicks = c
			}
		}
	}

	joined := strings.Join(textChunks, "")
	isError := streamError != ""
	resultText := ""
	if run.ExitCode == 0 && !isError && joined != "" {
		resultText = joined
	}
	errorText := streamError
	if errorText == "" && joined != "" && isError {
		errorText = joined
	}
	if errorText == "" && endEvent == nil && joined == "" && toolCallCount == 0 && strings.TrimSpace(run.Stderr) != "" {
		errorText = strings.TrimSpace(run.Stderr)
	}

	parsed := endEvent
	if parsed == nil {
		parsed = lastToolCall
	}
	return GrokOutcome{
		IsError:       isError,
		SessionId:     sessionId,
		ErrorText:     errorText,
		ResultText:    resultText,
		Parsed:        parsed,
		TimedOut:      run.TimedOut,
		CostUsdTicks:  costUsdTicks,
		ToolCallCount: toolCallCount,
	}
}

// grokFallbackEmpty mirrors TS fallbackEmpty().
func grokFallbackEmpty() SpawnCliResult {
	return SpawnCliResult{
		ExitCode: -1,
		Stdout:   "",
		Stderr:   "grok adapter produced no spawn result",
		TimedOut: false,
	}
}

// grokFinalize mirrors TS finalize.
func grokFinalize(run SpawnCliResult, sessionId string, durationMs int64) WorkerResult {
	outcome := interpretGrok(run)
	if run.TimedOut {
		result := WorkerResult{
			ExitCode:     run.ExitCode,
			Events:       []WorkerEvent{},
			ResultText:   "",
			SessionId:    sessionId,
			DurationMs:   durationMs,
			TimedOut:     true,
			ErrorText:    strings.TrimSpace(run.Stderr),
			CostUsdTicks: outcome.CostUsdTicks,
		}
		if run.ColdStart {
			result.ColdStart = true
		}
		return result
	}
	var events []WorkerEvent
	if outcome.Parsed != nil {
		events = []WorkerEvent{eventFromResult(outcome.Parsed)}
	} else {
		events = []WorkerEvent{}
	}
	return WorkerResult{
		ExitCode:     run.ExitCode,
		Events:       events,
		ResultText:   outcome.ResultText,
		SessionId:    sessionId,
		DurationMs:   durationMs,
		TimedOut:     false,
		ErrorText:    outcome.ErrorText,
		CostUsdTicks: outcome.CostUsdTicks,
	}
}

// GrokAdapter is the adapter over the `grok` (Grok Build CLI) headless
// mode.
type GrokAdapter struct {
	adapterBase
	// OnCost is the FR-GROK-03 ledger seam: invoked with the exact cost
	// ticks for every priced run when a watchdog ledger context is
	// present. Best-effort — a ledger write must never fail the run.
	// TODO(FR-GO-05 #190): wire to the orchestrator ledger port.
	OnCost func(taskId string, attempt int, costUsdTicks float64)
}

// Name mirrors the TS readonly name.
func (a *GrokAdapter) Name() string { return "grok" }

// Capabilities: Q30 — arms a 10-minute silence clock by default; the
// declaration is a floor, so a caller-passed 0 falls back to it rather
// than disarming the watchdog this adapter's retry loop depends on.
func (a *GrokAdapter) Capabilities() WorkerCapabilities {
	return WorkerCapabilities{DefaultNoProgressTimeoutMs: grokDefaultNoProgressTimeoutMs}
}

// IsProgress: grok's streaming-json shapes (Q33).
func (a *GrokAdapter) IsProgress(line string) bool { return IsGrokProgressLine(line) }

// Spawn runs the grok retry loop.
func (a *GrokAdapter) Spawn(opts WorkerSpawnOptions) WorkerResult {
	start := a.nowMs()
	caps := a.Capabilities()
	noProgressTimeoutMs := ResolveNoProgressTimeoutMs(opts.NoProgressTimeoutMs, &caps)
	wallDeadline := int64(1 << 62)
	if opts.TimeoutMs > 0 {
		wallDeadline = start + int64(opts.TimeoutMs)
	}

	// FR-GROK-06: the model actually forwarded to the CLI. Tier aliases are
	// dropped by BuildGrokArgs, so the chain starts unpositioned ("").
	activeModel := ""
	if isGrokModelId(strings.TrimSpace(opts.Model)) {
		activeModel = strings.TrimSpace(opts.Model)
	}
	args := BuildGrokArgs(opts, GrokArgsOptions{})
	// FR-GROK-04: derive the per-task cache key once per spawn — the retry
	// loop rebuilds argv but must never re-derive a different key, or every
	// re-dispatch goes cache-cold. Merged over the caller env so dispatch
	// extras survive; the per-task key wins over any inherited value.
	cacheEnv := GrokPromptCacheEnv(opts)
	var spawnEnv map[string]string
	if opts.Env != nil || len(cacheEnv) > 0 {
		spawnEnv = map[string]string{}
		for k, v := range opts.Env {
			spawnEnv[k] = v
		}
		for k, v := range cacheEnv {
			spawnEnv[k] = v
		}
	}
	sessionId := ""
	var last *SpawnCliResult
	// Retries use -c (continue most-recent session in cwd) and we cap at
	// maxAttempts so a hang cannot loop forever — same rationale as omp:
	// threading an explicit -r <id> would cross-talk between concurrent
	// devagent runs sharing the same cwd.
	const maxAttempts = 3

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if a.nowMs() >= wallDeadline {
			if last != nil {
				last.TimedOut = true
			}
			break
		}
		prepared, err := a.prepare("grok", args, SpawnCliOptions{
			Dir:                 opts.Cwd,
			TimeoutMs:           launchBudgetMs(wallDeadline, a.nowMs(), int64(opts.TimeoutMs)),
			Env:                 spawnEnv,
			NoProgressTimeoutMs: &noProgressTimeoutMs,
			WatchdogLedger:      opts.WatchdogLedger,
			ColdStartTimeoutMs:  opts.ColdStartTimeoutMs,
		})
		if err != nil {
			res := SpawnCliResult{ExitCode: -1, Stderr: err.Error()}
			last = &res
			break
		}
		raw := a.run(prepared.Cmd, prepared.Args, RunWorkerCliOptions{
			SpawnCliOptions: prepared.Opts,
			Herdr:           opts.Herdr,
		})
		last = &raw

		outcome := interpretGrok(raw)
		if outcome.SessionId != "" {
			sessionId = outcome.SessionId
		}
		ok := !raw.TimedOut && raw.ExitCode == 0 && !outcome.IsError
		if ok {
			break
		}

		if raw.ExitCode == -1 && !raw.TimedOut {
			break // ENOENT
		}
		if attempt == maxAttempts {
			break
		}
		if a.nowMs() >= wallDeadline {
			break
		}
		// FR-GROK-06: a TPM-class 429 is context-shaped — step the
		// within-xAI chain before retrying; every other class keeps the
		// model (cooldown/backoff is the fix there).
		activeModel = GrokRetryModel(activeModel, outcome.ErrorText)
		a.sleepMs(2000 * attempt)
		args = BuildGrokArgs(opts, GrokArgsOptions{Resume: true, Model: activeModel})
	}

	if last == nil {
		fallback := grokFallbackEmpty()
		last = &fallback
	}
	result := stampStreamMetrics(grokFinalize(*last, sessionId, a.nowMs()-start), *last)
	// FR-GROK-03: persist the exact cost onto the run ledger for every grok
	// worker run the provider priced. Best-effort — a ledger write must
	// never fail the run. Identity comes from the dispatcher's watchdog
	// context; probe/one-off spawns without it are not orchestrated runs.
	if opts.WatchdogLedger != nil && result.CostUsdTicks != nil && a.OnCost != nil {
		a.OnCost(opts.WatchdogLedger.TaskId, opts.WatchdogLedger.Attempt, *result.CostUsdTicks)
	}
	return result
}
