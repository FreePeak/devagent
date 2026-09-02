/**
 * Per-adapter `config.model` id validation (PRD Phase 4 backlog: "Provider
 * model-id validation at dispatch", Q32).
 *
 * Following the Q33 `isProgress` precedent, each adapter's accepted id shape
 * is declared here as a predicate in one registry so dispatch preflight can
 * reject unsupported ids at the gate (seconds, `failureClass: "config"`)
 * instead of letting each adapter interpret `config.model` freely and burn
 * attempts mid-board. Sep 1 evidence: provider-unqualified aliases now
 * 400/403 at provider boundaries (Gemini/Opus/Sonnet deprecation wave); loop
 * 58: `--model coding` on omp/pi exited 1 in ~12s per attempt.
 *
 * Semantics mirror what buildOmpArgs / buildPiArgs already do defensively at
 * argv build time:
 * - omp / pi: require provider-qualified `provider/model` ids (fuzzy
 *   matched). Driver tier aliases like "coding" are claude-proxy selectors,
 *   not omp/pi ids. Rejecting at the gate turns a mid-board burn into a
 *   fast, explicit failure.
 * - claude-code / opencode: pass `config.model` through untouched (their
 *   adapters own id normalization), so any value is accepted here.
 * - unset/empty model: always valid (the worker CLI falls back to its own
 *   configured default).
 */

/** Signature of a per-adapter model-id predicate. */
export type ModelIdPredicate = (model: string | undefined) => string | null;

/**
 * Shared core for adapters that require provider-qualified ids
 * (`provider/model`, fuzzy matched; multi-segment providers like
 * omniroute/bai/glm-5.3-flash are real ids). Returns null when acceptable,
 * otherwise a one-line actionable reason.
 */
function providerQualifiedReason(worker: string, model: string | undefined): string | null {
  const raw = model?.trim();
  if (raw === undefined || raw === '') return null; // unset = adapter default
  if (raw.includes('/')) return null; // provider-qualified: pass through
  return `worker "${worker}" requires a provider-qualified model id ("provider/model", e.g. omniroute/bai/glm-5.3-flash); got "${raw}" (driver tier aliases like "coding" are not valid ${worker} ids)`;
}

/**
 * The per-adapter registry of accepted id shapes: one predicate per
 * registered worker (Q33 `isProgress` precedent). Passthrough adapters own
 * their id normalization at argv build time and deliberately accept
 * anything; an unknown worker falls back to passthrough so a registry miss
 * can never block a dispatch.
 */
const MODEL_ID_VALIDATORS: Record<string, ModelIdPredicate> = {
  omp: (model) => providerQualifiedReason('omp', model),
  pi: (model) => providerQualifiedReason('pi', model),
  'claude-code': () => null,
  opencode: () => null,
};

/**
 * Validate `config.model` against the adapter's accepted id shape.
 *
 * Returns null when the model is acceptable (unset/empty always is — the
 * worker CLI falls back to its own configured default); otherwise a
 * one-line reason suitable for failure detail / log output.
 */
export function validateModelId(worker: string, model: string | undefined): string | null {
  const validate = MODEL_ID_VALIDATORS[worker];
  return validate ? validate(model) : null;
}

/** True when the registry declares an explicit shape for this worker. */
export function hasDeclaredModelIdShape(worker: string): boolean {
  return worker in MODEL_ID_VALIDATORS;
}
