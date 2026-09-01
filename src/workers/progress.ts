/**
 * Shared progress-classifier helpers for worker adapters (PRD Q33).
 *
 * The no-progress watchdog must not treat model deliberation as progress:
 * glm-style providers stream tens of MB of `thinking_delta` while making
 * zero tool calls (2026-08-31 evidence: 60k+ deltas over a full hour), and
 * pi does the same on hard goals (2026-09-01: 25MB thinking-only, 13+ min).
 * A stream line counts as progress when it carries *new work* — a tool call
 * starting/ending, or assistant text (the answer) — never bare thinking.
 *
 * Each adapter can re-declare this on WorkerAdapter.isProgress with a
 * stream-shape-specific predicate; these helpers are the common core so the
 * per-adapter versions stay one-liners and the runtime fallback keeps the
 * exact same semantics.
 */

/** Lines that are pure model deliberation: never progress, any adapter. */
export function isPureThinkingLine(line: string): boolean {
  if (line.includes('"thinking_delta"')) return true;
  // pi: message_update whose assistantMessageEvent is a thinking variant.
  if (line.includes('"type":"thinking_delta"') || line.includes('"type":"thinking_start"')) return true;
  return false;
}

/**
 * NDJSON-shape progress predicate shared by omp and pi (both emit
 * `{"type":"tool_execution_start|end", ...}` and text-bearing
 * message_update events). Returns true when the line evidences new work.
 */
export function isNdjsonProgressLine(line: string): boolean {
  if (!line.trim()) return false;
  if (isPureThinkingLine(line)) return false;
  if (line.includes('"tool_execution_start"')) return true;
  if (line.includes('"tool_execution_end"')) return true;
  // Assistant text completed (answer turn) counts; thinking text does not.
  if (line.includes('"type":"text_end"')) return true;
  if (line.includes('"type":"toolcall_start"')) return true;
  return false;
}
