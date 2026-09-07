// Package scantext is the Go port of src/research/scan-text.ts — GRADIENT
// adjacent-category scan text. Canonical source of the scan text embedded in
// the scout prompt (buildScoutPrompt) and the selfbuild loop's
// RESEARCH_PROMPT / PO_PROMPT; the shell side consumes it via the
// `devagent scan-text` subcommand, so the prompts never hand-copy this
// string and cannot drift from this file. Scope: adjacent-category scan only.
// The exit-code scalar architecture gate half of GRADIENT (Q38) is out of
// scope; it ships as an advisory hint in the text, not as a gate.
package scantext

import "strings"

// ScanCategories: adjacent product categories the funnel must survey — not
// agent products alone.
var ScanCategories = []string{"sensors", "MCP servers", "harness tooling"}

// FunnelMissNote: the miss that motivated the scan (2026-09-01 deep-dive).
const FunnelMissNote = "Known funnel miss: the agent-products-only funnel is why sentrux was missed entirely (2026-09-01 human deep-dive)."

// ArchitectureGateHint: advisory architecture-gate hint; the scalar gate
// itself stays out of scope (Q38).
const ArchitectureGateHint = "Advisory architecture-gate hint: note the lowest-scoring root cause per change; the exit-code scalar architecture gate (Q38) is out of scope and stays advisory."

// BuildAdjacentCategoryScanText builds the canonical scan text. Prompt
// consumers (scout, selfbuild loop) and the `devagent scan-text` subcommand
// all render exactly this string.
func BuildAdjacentCategoryScanText() string {
	return strings.Join([]string{
		"GRADIENT selection prior — when picking or justifying a backlog item, look beyond agent products.",
		"Adjacent categories to consider: " + strings.Join(ScanCategories, ", ") + ".",
		FunnelMissNote,
		ArchitectureGateHint,
	}, "\n")
}
