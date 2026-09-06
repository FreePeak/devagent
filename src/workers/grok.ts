import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import type { SpawnCliResult } from './spawn-utils.js';
import { runWorkerCli } from './herdr-runtime.js';
import { prepareWorkerSpawn } from './sandbox.js';
import { isGrokModelId } from './model-id.js';

const RESUME_PROMPT = 'Continue';
/**
 * If set on opts, override the per-attempt no-progress watchdog for grok.
 * Mirrors the omp default: a silent provider stall must trip the watchdog
 * so the retry loop fires, instead of the wall clock being the only net.
 */
const DEFAULT_NO_PROGRESS_TIMEOUT_MS = 10 * 60 * 1000;

export interface GrokArgsOptions {
  /** When true, build a resume argv (uses -c + -p RESUME_PROMPT). */
  resume?: boolean;
}

/**
 * Build the exact argv we pass to `grok` (Grok Build CLI) for a given spawn.
 * Pure function — exercised at the test seam without spawning the CLI.
 *
 *   grok -p <prompt> --output-format streaming-json [--model <m>]
 *   grok -c -p Continue --output-format streaming-json [--model <m>]   (resume)
 *
 * `--output-format streaming-json` is the NDJSON ACP session-update stream
 * (captured live 2026-09-06 against grok 1.0.13; same adapter shape as omp).
 * Resume uses `-c` (continue the most recent session for the cwd) — verified
 * live: the continued run restored the prior sessionId and cache-read the
 * earlier turn. Like omp, we do NOT thread `-r <id>` between attempts: an
 * explicit resume id would cross-talk between concurrent devagent runs
 * sharing the same cwd.
 *
 * Model forwarding follows the FR-GROK-02 predicate (src/workers/model-id.ts):
 * only exact xAI slugs (`grok-4.6`, `grok-build-0.1`, dated pins) or
 * `xai/`-qualified ids reach the CLI. Driver tier aliases like "coding"
 * (devagent.json model) are claude-code proxy selectors, not grok ids —
 * the loop-58 `--model coding` burn is the precedent. Dropping them lets
 * grok fall back to its configured default (~/.grok/config.toml
 * [models] default), which is the intended worker model anyway.
 */
export function buildGrokArgs(opts: WorkerSpawnOptions, o: GrokArgsOptions = {}): string[] {
  const rawModel = opts.model?.trim();
  const grokModel = rawModel !== undefined && isGrokModelId(rawModel) ? rawModel : undefined;
  const base: string[] = ['--output-format', 'streaming-json'];
  if (o.resume) {
    return ['-c', '-p', RESUME_PROMPT, ...base, ...(grokModel ? ['--model', grokModel] : [])];
  }
  return ['-p', opts.prompt, ...base, ...(grokModel ? ['--model', grokModel] : [])];
}

/**
 * Meaningful-line filter for grok's streaming-json stream (PRD Q33 port of
 * the omp/pi precedent): `thought` chunks are pure deliberation (48 of them
 * in a 10s echo run, 2026-09-06 capture) and must never reset the watchdog;
 * tool calls and finalized text chunks are new work.
 */
export function isGrokProgressLine(line: string): boolean {
  if (!line.trim()) return false;
  if (line.includes('"type":"thought"')) return false;
  if (line.includes('"type":"tool_call"')) return true;
  if (line.includes('"type":"tool_call_update"')) return true;
  if (line.includes('"type":"text","data"')) return true;
  return false;
}

export interface GrokOutcome {
  isError: boolean;
  sessionId: string | null;
  errorText?: string;
  resultText: string | null;
  parsed: Record<string, unknown> | null;
  timedOut: boolean;
}

/** Test seam: re-export of the parser. */
export function interpretGrokForTest(run: SpawnCliResult): GrokOutcome {
  return interpretGrok(run);
}

/**
 * Parse grok's stdout into the worker outcome shape. grok --output-format
 * streaming-json emits NDJSON ACP session updates, one JSON object per
 * line, per the live captures in test/workers/__fixtures__/
 * grok-smoke-2026-09-06.jsonl (clean answer run),
 * grok-toolrun-2026-09-06.jsonl (chunked text + tool events), and
 * grok-error-2026-09-06.jsonl (in-stream provider failure). Event shapes
 * (grok 1.0.13):
 *   {"type":"available_commands","tools":[...]}   startup header
 *   {"type":"thought","data":"<chunk>"}           thinking (ignored)
 *   {"type":"text","data":"<chunk>"}              assistant text chunks
 *   {"type":"tool_call"|"tool_call_update",...}   ACP tool events (ignored)
 *   {"type":"usage","usage":{...}}                token accounting
 *   {"type":"error","message":"..."}              provider failure
 *   {"type":"end","sessionId":"...","stopReason":"end_turn","usage":...}
 *
 * Assistant text policy: concatenate every `text` chunk in stream order —
 * grok emits the answer as word-level chunks, so the full assistant text is
 * the join (single-chunk answers like "OK" pass through unchanged).
 *
 * Like omp, grok can exit 0 while the provider call failed: the failure
 * surfaces as an in-stream `error` event. Capture the first message so the
 * failure is not misread as an empty-but-successful run.
 */
function interpretGrok(run: SpawnCliResult): GrokOutcome {
  let sessionId: string | null = null;
  let streamError: string | null = null;
  let endEvent: Record<string, unknown> | null = null;
  const textChunks: string[] = [];

  for (const line of run.stdout.split('\n')) {
    const trimmed = line.trim();
    if (!trimmed) continue;
    let event: Record<string, unknown> | null = null;
    try {
      const v: unknown = JSON.parse(trimmed);
      if (v !== null && typeof v === 'object' && !Array.isArray(v)) {
        event = v as Record<string, unknown>;
      }
    } catch {
      continue;
    }
    if (event === null) continue;
    // Forward-compat: a `session` header (omp shape) also names the id.
    if (event.type === 'session' && typeof event.id === 'string') {
      sessionId = event.id;
    }
    if (event.type === 'text' && typeof event.data === 'string') {
      textChunks.push(event.data);
    }
    if (event.type === 'error' && streamError === null && typeof event.message === 'string') {
      streamError = event.message;
    }
    if (event.type === 'end') {
      endEvent = event;
      if (typeof event.sessionId === 'string') sessionId = event.sessionId;
    }
  }

  const joined = textChunks.join('');
  const isError = streamError !== null;
  const resultText =
    run.exitCode === 0 && !isError && joined !== '' ? joined : null;
  const errorText =
    streamError ??
    (joined !== '' && isError ? joined : undefined) ??
    (endEvent === null && joined === '' && run.stderr.trim() ? run.stderr.trim() : undefined);
  return {
    isError,
    sessionId,
    errorText,
    resultText,
    parsed: endEvent,
    timedOut: run.timedOut,
  };
}

function fallbackEmpty(): SpawnCliResult {
  return { exitCode: -1, stdout: '', stderr: 'grok adapter produced no spawn result', timedOut: false };
}

function finalize(run: SpawnCliResult, sessionId: string | null, start: number): WorkerResult {
  const outcome = interpretGrok(run);
  if (run.timedOut) {
    return {
      exitCode: run.exitCode,
      events: [],
      resultText: null,
      sessionId,
      durationMs: Date.now() - start,
      timedOut: true,
      errorText: run.stderr.trim() || undefined,
    };
  }
  const events: WorkerEvent[] = outcome.parsed ? [{ type: 'result', ...outcome.parsed }] : [];
  return {
    exitCode: run.exitCode,
    events,
    resultText: outcome.resultText,
    sessionId,
    durationMs: Date.now() - start,
    timedOut: false,
    errorText: outcome.errorText,
  };
}

/**
 * Adapter over the `grok` (Grok Build CLI) headless mode. Mirrors the
 * structure of the omp adapter so the executor can treat it as just another
 * WorkerAdapter. grok-specific behavior (FR-GROK-01, docs/GROK.md §2.2):
 *   - Uses `-p <prompt> --output-format streaming-json` (NDJSON ACP events).
 *   - Resume via `-c` (continue most-recent session in cwd) + RESUME_PROMPT.
 *   - Model forwarded only when exact-slug or `xai/`-qualified (FR-GROK-02).
 *   - No `--api-key` CLI flag — browser OAuth or `XAI_API_KEY` env are the
 *     supported credential channels (XAI_API_KEY is sandbox-allowlisted).
 *   - Provider failures can surface as in-stream `error` events at exit 0;
 *     the parser captures them (see interpretGrok).
 */
export class GrokAdapter implements WorkerAdapter {
  readonly name = 'grok' as const;

  isProgress(line: string): boolean {
    return isGrokProgressLine(line);
  }

  constructor(
    private readonly sleep: (ms: number) => Promise<void> = (ms) =>
      new Promise((resolve) => setTimeout(resolve, ms)),
  ) {}

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const noProgressTimeoutMs =
      opts.noProgressTimeoutMs !== undefined && opts.noProgressTimeoutMs > 0
        ? opts.noProgressTimeoutMs
        : DEFAULT_NO_PROGRESS_TIMEOUT_MS;
    const wallDeadline = opts.timeoutMs > 0 ? start + opts.timeoutMs : Infinity;

    let args = buildGrokArgs(opts);
    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;
    // Retries use -c (continue most-recent session in cwd) and we cap at
    // maxAttempts so a hang cannot loop forever — same rationale as omp:
    // threading an explicit -r <id> would cross-talk between concurrent
    // devagent runs sharing the same cwd.
    const maxAttempts = 3;

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      if (Date.now() >= wallDeadline) {
        if (last) last.timedOut = true;
        break;
      }
      const prepared = await prepareWorkerSpawn('grok', args, {
        cwd: opts.cwd,
        timeoutMs: opts.timeoutMs,
        ...(opts.env ? { env: opts.env } : {}),
        noProgressTimeoutMs,
        ...(opts.watchdogLedger ? { watchdogLedger: opts.watchdogLedger } : {}),
      });
      last = await runWorkerCli(prepared.cmd, prepared.args, {
        ...prepared.opts,
        noProgressTimeoutMs,
        ...(opts.herdr ? { herdr: true } : {}),
      });

      const outcome = interpretGrok(last);
      if (outcome.sessionId) sessionId = outcome.sessionId;
      const ok = !last.timedOut && last.exitCode === 0 && !outcome.isError;
      if (ok) break;

      if (last.exitCode === -1 && !last.timedOut) break; // ENOENT
      if (attempt === maxAttempts) break;
      if (Date.now() >= wallDeadline) break;
      await this.sleep(2_000 * attempt);
      args = buildGrokArgs(opts, { resume: true });
    }

    return finalize(last ?? fallbackEmpty(), sessionId, start);
  }
}
