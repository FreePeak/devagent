/**
 * Probe argv for guided setup (`devagent init`, FR-SIMPLE-01), extracted from
 * cli.ts's operator-preflight helper so both surfaces share one definition.
 * Mirrors scripts/orchestrate-loop.sh: omp gets the headless hardening flags
 * and requires a provider-qualified model id — an unqualified alias is
 * dropped so the CLI default applies (same normalization buildOmpArgs does);
 * grok gets the headless streaming-json form (`grok -p "OK" --output-format
 * streaming-json`) with the FR-GROK-02 exact-slug/`xai/` model predicate;
 * the other workers take a plain prompt.
 */
import { isGrokModelId } from '../workers/model-id.js';

export function buildProbeArgvFor(worker: string, model: string | undefined): string[] {
  if (worker === 'omp') {
    const ompModel = model && model.includes('/') ? model : undefined;
    return [
      'omp',
      '-p',
      '--mode',
      'json',
      '--no-prewalk',
      '--no-lsp',
      '--no-extensions',
      ...(ompModel ? ['--model', ompModel] : []),
    ];
  }
  if (worker === 'grok') {
    // Same normalization buildGrokArgs applies: only exact xAI slugs or
    // `xai/`-qualified ids reach the CLI; anything else falls back to the
    // grok-configured default model.
    const grokModel = model !== undefined && isGrokModelId(model) ? model.trim() : undefined;
    return [
      'grok',
      '-p',
      '--output-format',
      'streaming-json',
      ...(grokModel ? ['--model', grokModel] : []),
    ];
  }
  return [worker, '-p'];
}
