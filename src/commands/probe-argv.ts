/**
 * Probe argv for guided setup (`devagent init`, FR-SIMPLE-01), extracted from
 * cli.ts's operator-preflight helper so both surfaces share one definition.
 * Mirrors scripts/orchestrate-loop.sh: omp gets the headless hardening flags
 * and requires a provider-qualified model id — an unqualified alias is
 * dropped so the CLI default applies (same normalization buildOmpArgs does);
 * the other workers take a plain prompt.
 */
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
  return [worker, '-p'];
}
