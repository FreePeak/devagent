// Package scout is the Go port of FR-GO-06 (issue #192): the selfbuild
// text extractor (scripts/selfbuild-extract-text.mjs), the deterministic
// core of src/scout.ts (fallback ids, payload extraction, golden replay,
// heartbeat, single-instance lock, prompt assembly), src/prompt.ts
// (compact-context splice, lessons digest, knowledge context) and
// src/planner.ts (ticket classification + plan outline).
//
// Live scout dispatch (runScoutOnce / runScoutLoop) is intentionally NOT
// ported here: it depends on the queue (enqueueTask, FR-GO-04), the doc
// sync gate (FR-GO-05) and the worker CLI runtime — sibling wave-1
// packages. The CLI commands `scout` (with --replay) and `scout-status`
// become wireable from this package's exported surface.
package scout

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// The extractor decides by worker OUTPUT SHAPE, never by a caller flag:
//   - assistant text found (omp/pi `message_end`, or a claude
//     `-p --output-format json` `result`) -> publish that text;
//   - empty input (both dispatch paths died before writing) -> a small
//     "[extract-aborted] empty worker output" diagnostic, never a blank
//     file (a blank goal is a zero-signal invalid ledger row);
//   - NDJSON with no assistant text = the dispatch timed out or died
//     mid-turn: publish a small "[extract-aborted] ..." diagnostic WITHOUT
//     the "Goal:" prefix (so the driver's ^Goal: gate rejects it) and, when
//     an explicit non-dash path is given, preserve the raw stream there for
//     triage — never publish the raw blob to <out> itself;
//   - plain text (e.g. a `claude -p` worker without --output-format json):
//     the raw output IS the result, pass it through unchanged.

// ExtractResult is the outcome of one extraction pass over a raw worker
// stream (already read from <raw-file>).
type ExtractResult struct {
	// Out is the exact text to publish to <out-file>.
	Out string
	// PreserveRawAt, when non-empty, names the resolved triage destination:
	// the raw NDJSON stream should be preserved there (best-effort). Only
	// the aborted-NDJSON branch sets it, and only for a real (non-dash)
	// path — the dash-guard keeps hot-load sentinels like "--sentinel"
	// from creating stray scratch files.
	PreserveRawAt string
	// Aborted is true when the published Out is an [extract-aborted]
	// diagnostic instead of worker text.
	Aborted bool
}

// Extract mirrors the extractor's shape-based decision on the raw stream
// body. abortedPathArg mirrors argv[3] verbatim ("" for absent).
func Extract(raw, abortedPathArg string) ExtractResult {
	// Empty input: both dispatch paths died before writing anything.
	if strings.TrimSpace(raw) == "" {
		return ExtractResult{Out: "[extract-aborted] empty worker output", Aborted: true}
	}

	lines := strings.Split(raw, "\n")
	out := ""
	for _, line := range lines {
		var v any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			// non-JSON line: not an event, ignore
			continue
		}
		o, ok := v.(map[string]any)
		if !ok {
			// A JSON primitive/array line carries no .type fields we read.
			continue
		}
		// omp/pi --mode json: assistant text arrives on message_end.
		if o["type"] == "message_end" {
			if msg, ok := o["message"].(map[string]any); ok && msg["role"] == "assistant" {
				// JS iterates `o.message.content ?? []`; a non-array content
				// would throw inside the per-line try and skip the line, so
				// the effective behavior is: only array content contributes.
				if content, ok := msg["content"].([]any); ok {
					for _, c := range content {
						if cm, ok := c.(map[string]any); ok && cm["type"] == "text" {
							if t, ok := cm["text"].(string); ok && t != "" {
								out = t // last text block wins, as in JS
							}
						}
					}
				}
			}
		}
		// claude -p --output-format json: a single {"type":"result","result":"..."}
		// line. Without this, its first line parses as NDJSON with no
		// message_end, so the worker's actual answer would be discarded as
		// an abort. SELFBUILD_*_BIN is overridable, so this shape is live.
		if o["type"] == "result" {
			if r, ok := o["result"].(string); ok && r != "" {
				out = r
			}
		}
	}
	if out != "" {
		return ExtractResult{Out: out}
	}

	// No assistant text: discriminate the two worker shapes before deciding
	// what raw is. NDJSON (omp/pi --mode json) vs plain text (claude -p).
	isNdjson := false
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(line), &v); err == nil {
			if o, ok := v.(map[string]any); ok {
				if _, ok := o["type"].(string); ok {
					isNdjson = true
				}
			}
		}
		break // only the first non-blank line decides
	}
	if !isNdjson {
		// Plain-text worker: raw output IS the result.
		return ExtractResult{Out: raw}
	}

	// Aborted NDJSON stream. Preserve the raw for triage ONLY when the
	// caller passed an explicit non-dash destination (a path under $STATE,
	// gitignored and never pushed).
	abPath := ""
	if abortedPathArg != "" && !strings.HasPrefix(abortedPathArg, "-") {
		abPath = abortedPathArg
	}
	evs := 0
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			evs++
		}
	}
	diag := fmt.Sprintf("[extract-aborted] no assistant text in %d NDJSON events", evs)
	if abPath != "" {
		diag += fmt.Sprintf(" (raw kept at %s)", abPath)
	}
	return ExtractResult{Out: diag, PreserveRawAt: abPath, Aborted: true}
}

// RunExtractText is the CLI-ready entry point behind the Node script
// `scripts/selfbuild-extract-text.mjs <raw-file> <out-file> [aborted-file]`
// (byte-identical usage/error surface so the driver call sites keep their
// contract when the parent wires the command). It returns the process exit
// code: 0 on any extraction outcome, 2 on usage errors, 1 when the raw file
// cannot be read (the Node script throws on the same condition).
func RunExtractText(args []string, stderr io.Writer) int {
	if len(args) < 2 {
		_, _ = fmt.Fprintln(stderr, "usage: selfbuild-extract-text.mjs <raw-file> <out-file> [aborted-file]")
		return 2
	}
	rawPath, outPath := args[0], args[1]
	abortedPathArg := ""
	if len(args) > 2 {
		abortedPathArg = args[2]
	}
	raw, err := os.ReadFile(rawPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	res := Extract(string(raw), abortedPathArg)
	if res.PreserveRawAt != "" {
		// best-effort; the diagnostic below still lands
		_ = os.WriteFile(res.PreserveRawAt, raw, 0o644)
	}
	if err := os.WriteFile(outPath, []byte(res.Out), 0o644); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
