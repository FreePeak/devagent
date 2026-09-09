// Package workers is the Go port of src/workers/* (FR-GO-05): the worker
// adapter layer. Worker CLIs stay external processes; this package owns
// their argv builders, NDJSON stream parsers (with errorMessage capture),
// the progress classifier backing the no-progress watchdogs, the sandbox
// env scrub, and the fan-out winner ranking.
//
// Byte-parity: error strings and stream-shape semantics mirror the
// TypeScript originals; tests pin them.
//
// Shared adapter plumbing: injectable hooks (clock, sleep, prepare, run)
// mirroring the TS constructor-injected sleep + test seams, plus small
// parsing helpers used by the interpreter ports.
package workers

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// adapterBase carries the per-adapter injectable hooks. nil = production
// behavior.
type adapterBase struct {
	// Sleep replaces the inter-attempt backoff in tests (nil = real sleep).
	Sleep func(ms int)
	// Run replaces the CLI launch in tests (nil = RunWorkerCli).
	Run func(cmd string, args []string, opts RunWorkerCliOptions) SpawnCliResult
	// Prepare replaces prepareWorkerSpawn in tests (nil = real sandbox).
	Prepare func(cmd string, args []string, opts SpawnCliOptions) (PreparedWorkerSpawn, error)
	// NowMs is Date.now() as unix millis (nil = real clock).
	NowMs func() int64
}

func (b *adapterBase) nowMs() int64 {
	if b.NowMs != nil {
		return b.NowMs()
	}
	return time.Now().UnixMilli()
}

func (b *adapterBase) sleepMs(ms int) {
	if b.Sleep != nil {
		b.Sleep(ms)
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func (b *adapterBase) run(cmd string, args []string, opts RunWorkerCliOptions) SpawnCliResult {
	if b.Run != nil {
		return b.Run(cmd, args, opts)
	}
	return RunWorkerCli(cmd, args, opts)
}

func (b *adapterBase) prepare(cmd string, args []string, opts SpawnCliOptions) (PreparedWorkerSpawn, error) {
	if b.Prepare != nil {
		return b.Prepare(cmd, args, opts)
	}
	return PrepareWorkerSpawn(cmd, args, opts)
}

// intPtrIf returns a pointer to n when cond holds, else nil (TS spread
// `...(x ? { v } : {})`).
func intPtrIf(cond bool, n int) *int {
	if !cond {
		return nil
	}
	return &n
}

func itoa(n int) string { return strconv.Itoa(n) }

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}

// lastNLines mirrors TS s.trim().split('\n').slice(-n).join('\n'). The
// input is trimmed first (TS call sites always trim before splitting).
func lastNLines(s string, n int) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return ""
	}
	parts := strings.Split(t, "\n")
	if len(parts) > n {
		parts = parts[len(parts)-n:]
	}
	return strings.Join(parts, "\n")
}

// derefSpawn maps a nil spawn result to the TS fallbackEmpty() shape.
func derefSpawn(r *SpawnCliResult) SpawnCliResult {
	if r != nil {
		return *r
	}
	return SpawnCliResult{}
}

// stampStreamMetrics copies the launch's Q34/Q33 stream evidence onto the
// finalized WorkerResult (issue #248 required change 3). The metrics of
// the FINAL attempt are the classification signal: a wall kill on an
// attempt that streamed work is a productive wall-kill; a no-progress
// watchdog firing on the last attempt is a burn regardless of earlier
// attempts.
func stampStreamMetrics(res WorkerResult, run SpawnCliResult) WorkerResult {
	res.WatchdogFired = run.WatchdogFired
	res.ClockResets = run.ClockResets
	res.MeaningfulBytes = run.MeaningfulBytes
	return res
}

// parseJSONAny parses s into any (TS JSON.parse); err swallowed by caller.
func parseJSONAny(s string) (v any, ok bool) {
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, false
	}
	return v, true
}

// eventFromParsed builds the TS spread `{ type: 'result', ...parsed }`:
// the literal type first, then every parsed key overlays it (a parsed
// "type" wins — grok's terminal `end` event keeps its own type).
func eventFromResult(parsed map[string]any) WorkerEvent {
	ev := WorkerEvent{"type": "result"}
	for k, v := range parsed {
		ev[k] = v
	}
	return ev
}
