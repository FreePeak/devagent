// Package file mirrors src/resilience/preflight.ts (FR-GO-07, issue #194):
// the typed operator preflight gate (Q40) — up to PreflightProbeAttempts
// probes with PreflightRetryDelayMs between failures, a circuit transition
// via RecordProxyProbe, one structured `operator-degraded` ledger row on
// degradation (via internal/ledger.AppendOperatorDegradedRecord), wedge
// detection via OMPStartupWedgePattern, and the shared once-per-episode
// pager. The single probe function already landed here with FR-GO-02
// (preflight.go, untouched by this port).

package resilience

import (
	"errors"
	"time"

	"github.com/FreePeak/devagent/internal/ledger"
)

// PreflightRole is a role a preflight can gate; each maps to one operator
// loop script.
type PreflightRole string

// PreflightRoles are the roles a preflight can gate (TS PREFLIGHT_ROLES).
var PreflightRoles = []PreflightRole{"prd-curator", "po", "selfbuild", "warroom", "reviewer"}

// IsPreflightRole reports whether value names a declared operator role.
func IsPreflightRole(value string) bool {
	for _, r := range PreflightRoles {
		if string(r) == value {
			return true
		}
	}
	return false
}

// PreflightProbeAttempts is the probe invocations per gate run.
const PreflightProbeAttempts = 3

// PreflightProbeTimeoutMs is the hard wall-clock cap per probe attempt
// (PreflightProbeAttempts attempts, PreflightRetryDelayMs apart).
//
// Measured 2026-09-11 on the live route: `omp -p OK --mode json --no-prewalk
// --no-lsp --no-extensions` completes in 15-75s (one gate cleared at 56s on
// attempt 1), while a raw gateway completion for the SAME model answers in
// 1.0-2.1s (5/5). So the cost is the worker CLI's per-invocation machinery —
// session start, memory and advisor are not separated by these measurements —
// not the model route: pinning the combo to a single leg changed nothing and
// was reverted (08dc339).
//
// 60s is deliberately NOT raised. Two distinct failure modes exist and the cap
// only addresses the first: (a) the 15-75s tail; (b) a hard-stall mode that
// never answered within 170s (5/5 in one sample window). 120s cannot fix (b) —
// it only doubles the burn per degraded iteration, and preflight runs twice per
// iteration (selfbuild + po), so it would mean up to ~12 min of probing before
// the breaker stops the cycle. The 2026-09-10 60s→120s raise was reverted for
// exactly that reason; don't re-raise it to chase (b).
//
// The real lever for (a) is that RunPreflightProbe waits for process EXIT, so
// it pays the CLI's full teardown even after the answer has streamed — at least
// one sample shows "text":"OK" present in the captured output while the cap
// still fired (answered, process lingering). Fixed by completing on the
// streamed marker instead of the exit code; tracked as a separate issue.
//
// Unconfirmed confounds, present in BOTH arms of every measurement, so nothing
// here attributes causality to either: the user's `advisor: enabled: true`
// (second route `onegw/dev:auto`; every degraded detail ends
// `advisor_cost_changed`, but the event also fired under a profile with
// `advisor.enabled: false`, so that profile did not actually suppress it) and
// `retry.maxRetries: 999999` with `maxDelayMs: 0`, which can retry one stalled
// request indefinitely inside a single probe.
//
// Correction to earlier text on this constant: it blamed upstream combo
// saturation (b-ai 429001 concurrency caps, tokenharbor free-tier quota,
// tokenrouter hangs) as the binding cause. Those symptoms are real but the
// raw-HTTP-vs-probe split above refutes them as the cause of the degraded
// iterations — the same route answers in ~1s while the CLI stalls.
const PreflightProbeTimeoutMs = 60_000

// PreflightRetryDelayMs is the sleep between failed probes (mirrors
// orchestrate-loop's 5s).
const PreflightRetryDelayMs = 5_000

// PreflightProbePrompt is the exact prompt every probe sends.
const PreflightProbePrompt = "OK"

// PreflightLedgerTaskID is the stable ledger taskId for operator preflight
// rows.
const PreflightLedgerTaskID = "operator-preflight"

// PreflightProbeFunc is the probe injection seam (TS args.probe). It
// receives the full probe argv (prompt flag + prompt + caller flags). A
// non-nil error mirrors the TS thrown probe: the gate bounds its message
// into a failed probe.
type PreflightProbeFunc func(cmd string, args []string, cwd string) (Probe, error)

// Sleeper is the sleep-between-failed-probes injection seam (TS
// args.delayMs).
type Sleeper func(ms int)

// DefaultPreflightProbe adapts the FR-GO-02 RunPreflightProbe (cmd, args,
// dir, timeoutMs) to the gate's probe seam shape.
func DefaultPreflightProbe(cmd string, args []string, cwd string) (Probe, error) {
	return RunPreflightProbe(cmd, args, cwd, PreflightProbeTimeoutMs), nil
}

// PreflightGateArgs is the typed gate's argument object (TS
// runPreflightGate args).
type PreflightGateArgs struct {
	RepoPath string
	Role     PreflightRole
	// Probe argv WITHOUT the prompt: ["<cmd>", "-p", ...flags]. The gate owns
	// prompt placement — the prompt immediately follows the `-p` flag, then
	// caller flags, matching buildOmpArgs and the orchestrate-loop probe (a
	// trailing prompt would be swallowed as `-p`'s value).
	Argv []string
	// Working dir for the probe; "" = RepoPath.
	Cwd string
	// Worker CLI being probed (repo config; recorded on the ledger row).
	Worker string
	// Model id being probed (repo config; empty string = CLI default).
	Model string
	// Injection seam for tests. Nil = DefaultPreflightProbe
	// (RunPreflightProbe via internal/spawn).
	Probe PreflightProbeFunc
	// Injection seam for tests: sleep between failed probes. Nil = real
	// time.Sleep.
	DelayMs Sleeper
	// Injection seam for tests: outbound paging transport, forwarded to the
	// shared pager (degrade-pager.go), which defaults it to
	// PostOperatorAlert.
	Notify DegradeBreachNotifier
	// Injectable clock for the ledger row ts (tests); nil = wall clock.
	NowFunc func() time.Time
}

// PreflightDecision is the final gate decision.
type PreflightDecision struct {
	// true = proceed with the cycle's agent dispatch.
	OK       bool          `json:"ok"`
	Role     PreflightRole `json:"role"`
	Attempts int           `json:"attempts"`
	// Worker CLI probed, when the caller declared it (repo config).
	Worker string `json:"worker,omitempty"`
	// Model id probed, when the caller declared it (empty = CLI default).
	Model string `json:"model,omitempty"`
	// Bounded last-failure excerpt; present when the gate degraded.
	Detail string `json:"detail,omitempty"`
	// true when this cycle fired the operator paging POST (Q41 write side).
	Paged bool `json:"paged,omitempty"`
}

// RunPreflightGate is the typed gate. Runs up to PreflightProbeAttempts
// probes with PreflightRetryDelayMs between failures, records the circuit
// transition (RecordProxyProbe) and — on failure — one structured
// `operator-degraded` ledger row, then decides. Callers MUST skip the
// cycle's agent dispatch when Decision.OK is false; that skip is the visible
// degradation.
func RunPreflightGate(args PreflightGateArgs) (PreflightDecision, error) {
	probe := args.Probe
	if probe == nil {
		probe = DefaultPreflightProbe
	}
	delayMs := args.DelayMs
	if delayMs == nil {
		delayMs = func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	}
	nowISO := func() string { return ledger.NowISO() }
	if args.NowFunc != nil {
		nowISO = func() string { return args.NowFunc().UTC().Format("2006-01-02T15:04:05.000Z07:00") }
	}
	cwd := args.Cwd
	if cwd == "" {
		cwd = args.RepoPath
	}
	if len(args.Argv) < 2 || args.Argv[0] == "" || args.Argv[1] != "-p" {
		return PreflightDecision{}, errors.New(`preflight argv must start with ["<cmd>", "-p", ...flags] (prompt flag before flags)`)
	}
	cmd, promptFlag := args.Argv[0], args.Argv[1]
	flags := args.Argv[2:]

	attempts := 0
	last := Probe{OK: false, Detail: "no probe ran"}
	for attempts < PreflightProbeAttempts {
		attempts++
		argv := append([]string{promptFlag, PreflightProbePrompt}, flags...)
		p, perr := probe(cmd, argv, cwd)
		if perr != nil {
			// TS catch: a thrown probe bounds its message into a failed probe.
			p = Probe{OK: false, Detail: boundDetail(perr.Error())}
		}
		last = p
		if last.OK {
			break
		}
		if attempts < PreflightProbeAttempts {
			delayMs(PreflightRetryDelayMs)
		}
	}

	decision := PreflightDecision{
		OK:       last.OK,
		Role:     args.Role,
		Attempts: attempts,
	}
	if args.Worker != "" {
		decision.Worker = args.Worker
	}
	// TS: ...(args.model !== undefined ? { model: args.Model } : {}) — the
	// caller-declared model is recorded even when empty (CLI default); the
	// args struct cannot distinguish "unset" from "", and the TS CLI never
	// passes undefined-with-empty-string, so assign unconditionally.
	decision.Model = args.Model
	if last.OK {
		// Advance the shared circuit on success too: without this a provider
		// recovery leaves a stale `open` from the last failure, and consumers
		// reading the circuit keep short-circuiting (2026-09-03: circuit stuck
		// open since 13:21Z despite green probes from 01:58Z).
		RecordProxyProbe(args.RepoPath, ProxyProbe{OK: true})
	} else {
		detail := last.Detail
		wedge := OMPStartupWedgePattern.MatchString(detail)
		// Shared circuit state so `devagent status --providers` reports the
		// operator-loop degradation exactly like the orchestrate-loop gate —
		// EXCEPT for an omp startup wedge (local plugin/MCP init stall,
		// provider untouched): opening the circuit there idles the whole
		// factory on a healthy provider (2026-09-05). The wedge still records
		// the degraded row so the skipped cycle stays visible.
		if !wedge {
			RecordProxyProbe(args.RepoPath, ProxyProbe{OK: false, Detail: "preflight[" + string(args.Role) + "]: " + detail})
		}
		// Structured degradation row: the ledger is the evidence a degraded
		// factory cycle was skipped, not silently noop'd (Q40). TS writes the
		// detail key only when the failure detail is truthy.
		record := ledger.OperatorDegradedRecord{
			TS:       nowISO(),
			Kind:     "event",
			TaskID:   PreflightLedgerTaskID,
			Attempt:  attempts,
			Event:    "operator-degraded",
			Role:     string(args.Role),
			Worker:   args.Worker,
			Model:    args.Model,
			OK:       false,
			Attempts: attempts,
		}
		if detail != "" {
			d := detail
			record.Detail = &d
		}
		ledger.AppendOperatorDegradedRecord(args.RepoPath, record)
		// One page per outage episode: only the cycle that completes the
		// streak reaches the threshold, so mid-streak failures stay silent
		// (Q41).
		paged := PageDegradeBreach(PageDegradeBreachArgs{
			RepoPath: args.RepoPath,
			Source:   DegradeBreachSourcePreflight,
			Role:     string(args.Role),
			Worker:   args.Worker,
			Model:    args.Model,
			Detail:   detail,
			Notify:   args.Notify,
		})
		if paged {
			decision.Paged = true
		}
		if detail != "" {
			decision.Detail = detail
		}
	}
	return decision, nil
}
