/**
 * GRADIENT — adjacent-category scan text (docs/PRD.md Phase 4 backlog,
 * "GRADIENT — structural gradient sensor").
 *
 * Canonical source of the scan text embedded in the scout prompt
 * (`buildScoutPrompt`) and the selfbuild loop's RESEARCH_PROMPT / PO_PROMPT.
 * The shell side consumes it via the `devagent scan-text` subcommand — the
 * prompts never hand-copy this string, so they cannot drift from this file.
 *
 * Scope: adjacent-category scan only. The exit-code scalar architecture gate
 * half of GRADIENT (Q38) is explicitly out of scope; it ships as an advisory
 * hint in the text below, not as a gate.
 */

/** Adjacent product categories the funnel must survey — not agent products alone. */
export const SCAN_CATEGORIES: readonly string[] = ['sensors', 'MCP servers', 'harness tooling'];

/** The miss that motivated the scan: the agent-products-only funnel (2026-09-01 human deep-dive). */
export const FUNNEL_MISS_NOTE =
  'Known funnel miss: the agent-products-only funnel is why sentrux was missed entirely (2026-09-01 human deep-dive).';

/** Advisory architecture-gate hint; the scalar gate itself stays out of scope (Q38). */
export const ARCHITECTURE_GATE_HINT =
  'Advisory architecture-gate hint: note the lowest-scoring root cause per change; the exit-code scalar architecture gate (Q38) is out of scope and stays advisory.';

/**
 * Build the canonical scan text. Prompt consumers (scout, selfbuild loop) and
 * the `devagent scan-text` subcommand all render exactly this string.
 */
export function buildAdjacentCategoryScanText(): string {
  return [
    'GRADIENT selection prior — when picking or justifying a backlog item, look beyond agent products.',
    `Adjacent categories to consider: ${SCAN_CATEGORIES.join(', ')}.`,
    FUNNEL_MISS_NOTE,
    ARCHITECTURE_GATE_HINT,
  ].join('\n');
}
