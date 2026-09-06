import type { WorkerAdapter, WorkerCapabilities, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import { resolveNoProgressTimeoutMs, type SpawnCliResult } from './spawn-utils.js';
import { runWorkerCli } from './herdr-runtime.js';
import { prepareWorkerSpawn } from './sandbox.js';
import { isGrokModelId } from './model-id.js';
import { appendWorkerCostRecord } from '../orchestrator/ledger.js';
import { transientErrorClass } from '../resilience/classify.js';

export interface GrokArgsOptions {
  /** When true, build a resume argv (uses -c + -p RESUME_PROMPT). */
  resume?: boolean;
  /**
   * FR-GROK-06: within-xAI chain override — replaces opts.model for this
   * attempt when a TPM-class retry steps to the next rung. Still validated
   * by the FR-GROK-02 predicate at forward time.
   */
  model?: string;
}

const RESUME_PROMPT = 'Continue';
/**
 * If set on opts, override the per-attempt no-progress watchdog for grok.
 * Mirrors the omp default: a silent provider stall must trip the watchdog
 * so the retry loop fires, instead of the wall clock being the only net.
 *
 * Q30: declared as the adapter's capability so the shared spawn-path resolver
 * owns the precedence instead of a per-adapter copy.
 */
const DEFAULT_NO_PROGRESS_TIMEOUT_MS = 10 * 60 * 1000;

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
  const rawModel = (o.model !== undefined ? o.model : opts.model)?.trim();
  const grokModel = rawModel !== undefined && isGrokModelId(rawModel) ? rawModel : undefined;
  const base: string[] = ['--output-format', 'streaming-json'];
  if (o.resume) {
    return ['-c', '-p', RESUME_PROMPT, ...base, ...(grokModel ? ['--model', grokModel] : [])];
  }
  return ['-p', opts.prompt, ...base, ...(grokModel ? ['--model', grokModel] : [])];
}

/**
 * FR-GROK-04: env var carrying the per-task prompt-cache key into the grok
 * child. The Grok Build CLI (argv verified against 1.0.13) has no cache-key
 * flag — the supported per-request channel is config-driven:
 * `[model.<id>].env_http_headers = { "x-grok-conv-id" =
 * "DEVAGENT_PROMPT_CACHE_KEY" }` in ~/.grok/config.toml maps this env var to
 * the sticky-routing header xAI prices cached input against (~25% of input
 * cost, PRD §20.2). Like XAI_API_KEY, it reaches the CLI through the spawn
 * env, never argv.
 */
export const GROK_PROMPT_CACHE_KEY_ENV = 'DEVAGENT_PROMPT_CACHE_KEY';

/**
 * FR-GROK-04: derive the per-task prompt-cache key. Deterministic function
 * of the task id the dispatcher already threads (watchdogLedger.taskId —
 * same identity source as the FR-GROK-03 cost rows), so every attempt in a
 * task's retry loop and every re-dispatch of that task carries the identical
 * key instead of going cache-cold. Deliberately excludes attempt/retry
 * fields: a key that moved per attempt would defeat stickiness. Returns
 * undefined for probe/one-off spawns without ledger context — no key is
 * emitted rather than a fabricated per-process one.
 */
export function grokPromptCacheKey(opts: WorkerSpawnOptions): string | undefined {
  const taskId = opts.watchdogLedger?.taskId?.trim();
  return taskId ? `devagent-${taskId}` : undefined;
}

/**
 * Env overlay for the grok child carrying the FR-GROK-04 cache key. Empty
 * when the spawn has no task context, so the caller env passes through
 * untouched and no key is emitted.
 */
export function grokPromptCacheEnv(opts: WorkerSpawnOptions): Record<string, string> {
  const key = grokPromptCacheKey(opts);
  return key ? { [GROK_PROMPT_CACHE_KEY_ENV]: key } : {};
}

/**
 * FR-GROK-06: the within-xAI fallback chain, ordered by preference (PRD
 * §20.2: grok-4.6 is the recommended coding default, grok-4.3 the 1M-ctx
 * sibling, grok-build-0.1 the small-context escape hatch). A TPM
 * (tokens-per-minute) 429 is context-shaped — a smaller-context model in
 * the same provider can serve the same turn where a cooldown cannot — so
 * the retry loop steps this chain before any cross-provider fallback,
 * which lives above the adapter and only sees the exhausted-chain result.
 */
export const GROK_FALLBACK_CHAIN = ['grok-4.6', 'grok-4.3', 'grok-build-0.1'] as const;

/**
 * Pure chain selector: the next model to try after `model` hits a
 * TPM-class limit. Accepts `xai/`-qualified ids and dated pins
 * (`grok-4.6-2026-08-14` sits at the `grok-4.6` rung). Unset/empty starts
 * at the chain head (the CLI default just burned its TPM budget — pin the
 * next attempt explicitly). A non-chain grok family (e.g. `grok-4.5`) or
 * the chain tail yields null: the selector never second-guesses an
 * operator-pinned model and never invents a fourth rung.
 */
export function grokChainNext(model: string | undefined | null): string | null {
  const raw = model?.trim().toLowerCase().replace(/^xai\//, '');
  if (!raw) return GROK_FALLBACK_CHAIN[0] ?? null;
  const idx = GROK_FALLBACK_CHAIN.findIndex((m) => raw === m || raw.startsWith(`${m}-`));
  if (idx === -1) return null;
  return GROK_FALLBACK_CHAIN[idx + 1] ?? null;
}

/**
 * Pure retry-model decision: advance the within-xAI chain only on a
 * `rate-limit-tpm` transient class (FR-GROK-06). Every other outcome —
 * RPS 429 (the same-model cooldown is the fix), 5xx, timeouts,
 * non-transient — keeps the current model. Chain exhaustion returns the
 * current model unchanged.
 */
export function grokRetryModel(
  current: string | undefined,
  errorText: string | null | undefined,
): string | undefined {
  if (transientErrorClass(errorText ?? null) !== 'rate-limit-tpm') return current;
  return grokChainNext(current) ?? current;
}

export interface GrokOutcome {
  isError: boolean;
  sessionId: string | null;
  errorText?: string;
  resultText: string | null;
  parsed: Record<string, unknown> | null;
  timedOut: boolean;
  /**
   * FR-GROK-03: exact xAI cost in integer USD ticks, copied verbatim from the
   * stream's `usage`/`end` events. Undefined when neither event carried the
   * field — a missing cost is never coerced to 0.
   */
  costUsdTicks?: number;
  /**
   * FR-GROK-06: number of whole `tool_call` events seen. xAI streams tool
   * calls whole (not token-streamed), so a run can legitimately end on one;
   * the count is the evidence the stream was not empty.
   */
  toolCallCount: number;
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

/** Test seam: re-export of the parser. */
export function interpretGrokForTest(run: SpawnCliResult): GrokOutcome {
  return interpretGrok(run);
}

/**
 * FR-GROK-03: read the exact cost figure off one stream event, verbatim — no
 * rounding, no currency conversion. grok's `end` event carries the run total
 * as `total_cost_usd_ticks`; the xAI API/PRD names the same quantity
 * `usage.cost_in_usd_ticks`. Accept either spelling, at the event top level or
 * nested under `usage`, and return the first finite number found. Absent or
 * non-finite yields undefined — never 0 — so a run the provider did not price
 * stays distinct from a genuinely free (0-tick) run.
 */
function pickCostTicks(event: Record<string, unknown>): number | undefined {
  const usage = event.usage;
  const nested =
    usage !== null && typeof usage === 'object' && !Array.isArray(usage)
      ? (usage as Record<string, unknown>)
      : undefined;
  for (const candidate of [
    event.cost_in_usd_ticks,
    event.total_cost_usd_ticks,
    nested?.cost_in_usd_ticks,
    nested?.total_cost_usd_ticks,
  ]) {
    if (typeof candidate === 'number' && Number.isFinite(candidate)) return candidate;
  }
  return undefined;
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
 *   {"type":"tool_call"|"tool_call_update",...}   ACP tool events (whole, not token-streamed)
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
 *
 * Stream-tool-call-whole (FR-GROK-06): because tool calls arrive whole, a
 * turn can end on a `tool_call`/`tool_call_update` event with no trailing
 * `text` or `end` event. That shape is real progress, not an empty/failed
 * run: the parser keeps the last tool event as the outcome's `parsed`
 * (finalize turns it into a result event, defeating the zero-events empty
 * signature) and stderr noise is never promoted to errorText while tool
 * activity is present.
 *
 * Cost accounting (FR-GROK-03): the `usage` and `end` events carry the exact
 * xAI cost as integer USD ticks. Copy it verbatim onto the outcome (no
 * rounding, no currency conversion); the `end` total wins over any earlier
 * `usage` row. A run whose events omit the field leaves the cost undefined —
 * never 0.
 */
function interpretGrok(run: SpawnCliResult): GrokOutcome {
  let sessionId: string | null = null;
  let streamError: string | null = null;
  let endEvent: Record<string, unknown> | null = null;
  let costUsdTicks: number | undefined;
  let toolCallCount = 0;
  let lastToolCall: Record<string, unknown> | null = null;
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
    if (event.type === 'tool_call') {
      toolCallCount++;
      lastToolCall = event;
    } else if (event.type === 'tool_call_update') {
      lastToolCall = event;
    }
    if (event.type === 'error' && streamError === null && typeof event.message === 'string') {
      streamError = event.message;
    }
    if (event.type === 'usage') {
      const c = pickCostTicks(event);
      if (c !== undefined) costUsdTicks = c;
    }
    if (event.type === 'end') {
      endEvent = event;
      if (typeof event.sessionId === 'string') sessionId = event.sessionId;
      // The terminal `end` event is the authoritative accounting; it overrides
      // any earlier `usage` row. Absent cost here leaves the usage value intact.
      const c = pickCostTicks(event);
      if (c !== undefined) costUsdTicks = c;
    }
  }

  const joined = textChunks.join('');
  const isError = streamError !== null;
  const resultText =
    run.exitCode === 0 && !isError && joined !== '' ? joined : null;
  const errorText =
    streamError ??
    (joined !== '' && isError ? joined : undefined) ??
    (endEvent === null && joined === '' && toolCallCount === 0 && run.stderr.trim()
      ? run.stderr.trim()
      : undefined);
  return {
    isError,
    sessionId,
    errorText,
    resultText,
    parsed: endEvent ?? lastToolCall,
    timedOut: run.timedOut,
    costUsdTicks,
    toolCallCount,
  };
}

function fallbackEmpty(): SpawnCliResult {
  return { exitCode: -1, stdout: '', stderr: 'grok adapter produced no spawn result', timedOut: false };
}

function finalize(run: SpawnCliResult, sessionId: string | null, start: number): WorkerResult {
  const outcome = interpretGrok(run);
  const cost =
    outcome.costUsdTicks !== undefined ? { costUsdTicks: outcome.costUsdTicks } : {};
  if (run.timedOut) {
    return {
      exitCode: run.exitCode,
      events: [],
      resultText: null,
      sessionId,
      durationMs: Date.now() - start,
      timedOut: true,
      errorText: run.stderr.trim() || undefined,
      ...(run.coldStart ? { coldStart: true } : {}),
      ...cost,
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
    ...cost,
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
 *   - Per-task prompt-cache key (FR-GROK-04) rides the spawn env as
 *     `DEVAGENT_PROMPT_CACHE_KEY=devagent-<taskId>`, derived once per spawn
 *     so every attempt of the retry loop keeps the cache warm; never argv.
 *   - Provider failures can surface as in-stream `error` events at exit 0;
 *     the parser captures them (see interpretGrok).
 *   - TPM-class 429s step the within-xAI fallback chain
 *     (`grok-4.6 → grok-4.3 → grok-build-0.1`, FR-GROK-06) before the
 *     exhausted-chain result can reach cross-provider fallback above.
 */
export class GrokAdapter implements WorkerAdapter {
  readonly name = 'grok' as const;

  /** Q30: arms a 10-minute silence clock by default; the declaration is a
   * floor, so a caller-passed 0 falls back to it rather than disarming the
   * watchdog this adapter's retry loop depends on.
   */
  readonly capabilities: WorkerCapabilities = {
    defaultNoProgressTimeoutMs: DEFAULT_NO_PROGRESS_TIMEOUT_MS,
  };

  isProgress(line: string): boolean {
    return isGrokProgressLine(line);
  }

  constructor(
    private readonly sleep: (ms: number) => Promise<void> = (ms) =>
      new Promise((resolve) => setTimeout(resolve, ms)),
  ) {}

  async spawn(opts: WorkerSpawnOptions): Promise<WorkerResult> {
    const start = Date.now();
    const noProgressTimeoutMs = resolveNoProgressTimeoutMs(opts.noProgressTimeoutMs, this.capabilities);
    const wallDeadline = opts.timeoutMs > 0 ? start + opts.timeoutMs : Infinity;

    // FR-GROK-06: the model actually forwarded to the CLI. Tier aliases are
    // dropped by buildGrokArgs, so the chain starts unpositioned (undefined).
    let activeModel =
      opts.model !== undefined && isGrokModelId(opts.model) ? opts.model.trim() : undefined;
    let args = buildGrokArgs(opts);
    // FR-GROK-04: derive the per-task cache key once per spawn — the retry
    // loop rebuilds argv but must never re-derive a different key, or every
    // re-dispatch goes cache-cold. Merged over the caller env so dispatch
    // extras survive; the per-task key wins over any inherited value.
    const cacheEnv = grokPromptCacheEnv(opts);
    const spawnEnv =
      (opts.env || Object.keys(cacheEnv).length > 0) ? { ...opts.env, ...cacheEnv } : undefined;
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
        ...(spawnEnv ? { env: spawnEnv } : {}),
        noProgressTimeoutMs,
        ...(opts.watchdogLedger ? { watchdogLedger: opts.watchdogLedger } : {}),
        ...(opts.coldStartTimeoutMs ? { coldStartTimeoutMs: opts.coldStartTimeoutMs } : {}),
      });
      last = await runWorkerCli(prepared.cmd, prepared.args, {
        ...prepared.opts,
        noProgressTimeoutMs,
        ...(opts.coldStartTimeoutMs ? { coldStartTimeoutMs: opts.coldStartTimeoutMs } : {}),
        ...(opts.herdr ? { herdr: true } : {}),
      });

      const outcome = interpretGrok(last);
      if (outcome.sessionId) sessionId = outcome.sessionId;
      const ok = !last.timedOut && last.exitCode === 0 && !outcome.isError;
      if (ok) break;

      if (last.exitCode === -1 && !last.timedOut) break; // ENOENT
      if (attempt === maxAttempts) break;
      if (Date.now() >= wallDeadline) break;
      // FR-GROK-06: a TPM-class 429 is context-shaped — step the
      // within-xAI chain before retrying; every other class keeps the
      // model (cooldown/backoff is the fix there).
      activeModel = grokRetryModel(activeModel, outcome.errorText);
      await this.sleep(2_000 * attempt);
      args = buildGrokArgs(opts, { resume: true, model: activeModel });
    }

    const result = finalize(last ?? fallbackEmpty(), sessionId, start);
    // FR-GROK-03: persist the exact cost onto the run ledger for every grok
    // worker run the provider priced. Best-effort — a ledger write must never
    // fail the run. Identity comes from the dispatcher's watchdog context;
    // probe/one-off spawns without it are not orchestrated runs.
    if (opts.watchdogLedger && result.costUsdTicks !== undefined) {
      appendWorkerCostRecord(opts.watchdogLedger.repoPath, {
        ts: new Date().toISOString(),
        kind: 'event',
        event: 'worker-cost',
        taskId: opts.watchdogLedger.taskId,
        attempt: opts.watchdogLedger.attempt,
        worker: 'grok',
        costUsdTicks: result.costUsdTicks,
      });
    }
    return result;
  }
}
