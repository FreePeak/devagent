// Package workers is the Go port of src/workers/* (FR-GO-05): the worker
// adapter layer. Worker CLIs stay external processes; this package owns
// their argv builders, NDJSON stream parsers (with errorMessage capture),
// the progress classifier backing the no-progress watchdogs, the sandbox
// env scrub, and the fan-out winner ranking.
//
// Byte-parity: error strings and stream-shape semantics mirror the
// TypeScript originals; tests pin them.

package workers

// WorkerEvent mirrors the TS WorkerEvent: a parsed stream record. The
// "type" key follows the source event; result events overlay the parsed
// object exactly like the TS spread `{ type: 'result', ...parsed }` (a
// parsed "type" key wins, e.g. grok's terminal `end` event).
type WorkerEvent = map[string]any

// WorkerSpawnOptions mirrors TS WorkerSpawnOptions. Pointer fields model
// TS optionality: nil = unset.
type WorkerSpawnOptions struct {
	Prompt    string
	Cwd       string
	TimeoutMs int
	// MaxSteps forwards --max-turns to claude-code. Nil = unset.
	MaxSteps *int
	// Model override forwarded to the worker CLI (provider/model).
	Model string
	// Variant override forwarded to opencode (--variant or #variant) and
	// omp/pi (--thinking).
	Variant string
	// Env is merged over the scrubbed base environment by the sandbox.
	Env map[string]string
	// APIMaxAttempts caps total launches when the worker auto-resumes a
	// session killed by an API failure. Includes the first launch. Nil =
	// adapter default (Infinity for claude-code/opencode/pi).
	APIMaxAttempts *int
	// NoProgressTimeoutMs: kill the child when no adapter-classified
	// progress arrives for this long. Nil = unset (adapter capability
	// decides); 0 = caller-requested disable (floored by nonzero
	// capability declarations, see ResolveNoProgressTimeoutMs).
	NoProgressTimeoutMs *int
	// ColdStartTimeoutMs: Q31 first-progress deadline. 0 disables.
	ColdStartTimeoutMs int
	// Herdr routes this launch through the herdr pane runtime when set
	// (FR-VIS-01); nil = decide via env/visibility, see RunWorkerCli.
	Herdr *bool
	// WatchdogLedger is the Q34 structured watchdog-health context. Nil =
	// no ledger row.
	WatchdogLedger *WatchdogLedgerContext
}

// WorkerResult mirrors TS WorkerResult. Empty-string optional fields model
// TS null/undefined (ResultText, SessionId, ErrorText).
type WorkerResult struct {
	ExitCode   int
	Events     []WorkerEvent
	ResultText string // "" = null
	SessionId  string // "" = null
	DurationMs int64
	TimedOut   bool
	// ErrorText: last error text from the worker (stitched from
	// stderr/parsed stream) so callers can classify transient vs
	// non-retryable failures even when ResultText is empty.
	ErrorText string // "" = undefined
	// NoProgress: worker exited cleanly (exit 0) but produced zero events
	// and no result text — the hung-worker/empty-output signature.
	NoProgress bool
	// ColdStart: true when the launch was killed by the cold-start
	// (first-progress) deadline. Classified transient alongside NoProgress.
	ColdStart bool
	// CostUsdTicks: FR-GROK-03 xAI cost recorded verbatim for this run.
	// Nil = undefined (a missing cost is never coerced to 0).
	CostUsdTicks *float64
}

// WorkerCapabilities mirrors TS WorkerCapabilities: the per-adapter
// no-progress watchdog declaration (Q30).
type WorkerCapabilities struct {
	// DefaultNoProgressTimeoutMs arms the silence clock when the caller
	// passed no explicit budget. 0 = clock disarmed unless config/env arms
	// it (claude-code, opencode). A nonzero declaration is also a floor:
	// those CLIs must never run silent-and-unwatched (omp/pi/grok), so a
	// caller-passed 0 falls back to this value.
	DefaultNoProgressTimeoutMs int
}

// WatchdogLedgerContext mirrors TS WatchdogLedgerContext (Q34): the
// identity fields a watchdog-health ledger row needs.
type WatchdogLedgerContext struct {
	RepoPath string
	TaskId   string
	Attempt  int
	Worker   string
}

// WorkerAdapter mirrors the TS WorkerAdapter interface. IsProgress is the
// per-adapter stream-shape predicate (Q33); adapters without a specific
// shape return nil and the shared NDJSON core applies.
type WorkerAdapter interface {
	Name() string
	Capabilities() WorkerCapabilities
	IsProgress(line string) bool
	Spawn(opts WorkerSpawnOptions) WorkerResult
}

// workers is the registry of every worker adapter (src/workers/index.ts).
func workerRegistry() map[string]WorkerAdapter {
	return map[string]WorkerAdapter{
		"claude-code": &ClaudeCodeAdapter{},
		"opencode":    &OpenCodeAdapter{},
		"omp":         &OmpAdapter{},
		"pi":          &PiAdapter{},
		"grok":        &GrokAdapter{},
	}
}

// GetWorker resolves a worker adapter by name. The error message matches
// the TS factory (`Unknown worker: <name>`).
func GetWorker(name string) (WorkerAdapter, error) {
	w, ok := workerRegistry()[name]
	if !ok {
		return nil, &unknownWorkerError{name}
	}
	return w, nil
}

// WorkerNames lists the registered worker names.
func WorkerNames() []string {
	return []string{"claude-code", "opencode", "omp", "pi", "grok"}
}

type unknownWorkerError struct{ name string }

func (e *unknownWorkerError) Error() string { return "Unknown worker: " + e.name }
