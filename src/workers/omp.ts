import type { WorkerAdapter, WorkerEvent, WorkerResult, WorkerSpawnOptions } from '../types.js';
import type { SpawnCliResult } from './spawn-utils.js';
import { runWorkerCli } from './herdr-runtime.js';
import { prepareWorkerSpawn } from './sandbox.js';

const RESUME_PROMPT = 'Continue';
/**
 * If set on opts, override the per-attempt no-progress watchdog for omp.
 * omp is observed to hang silently on slow providers (2026-08-30 lesson:
 * `omp -p` on omniroute/bai/glm-5.3-flash produced no stdout for 240s).
 * The 10-minute default mirrors the resilience default elsewhere in
 * devagent so retries fire instead of letting the wall clock be the only
 * safety net.
 */
const DEFAULT_NO_PROGRESS_TIMEOUT_MS = 10 * 60 * 1000;

export interface OmpArgsOptions {
  /** When true, build a resume argv (uses -c + RESUME_PROMPT, no -p). */
  resume?: boolean;
}

/**
 * Build the exact argv we pass to `omp` for a given spawn. Pure function —
 * exercised at the test seam without spawning the CLI.
 *
 *   omp -p <prompt> --mode json [--model <m>] [--thinking <v>]
 *   omp --mode json -c <resumePrompt>   (resume)
 */
export function buildOmpArgs(opts: WorkerSpawnOptions, o: OmpArgsOptions = {}): string[] {
  const rawThinking = opts.variant?.trim();
  // omp requires provider-qualified model ids (`provider/model`, fuzzy
  // matched). Driver tier aliases like "coding" (devagent.json model) are
  // claude-code proxy selectors, not omp ids: `--model coding` exits 1 in
  // ~12s with no output (2026-08-31 live loop 58 attempts 1-3). Drop any
  // value without a `/` so omp falls back to its configured
  // modelRoles.default (~/.omp/agent/config.yml), which is the intended
  // worker model anyway. Pass provider/model values through untouched.
  const rawModel = opts.model?.trim();
  const ompModel =
    rawModel !== undefined && rawModel !== '' && rawModel.includes('/') ? rawModel : undefined;
  // --no-prewalk: the interactive config's prewalk (second planning turn)
  // loops forever on some models (2026-08-30 A/B: glm-5.3-flash prewalk turn
  // streamed 986+ thinking events and never terminated; with --no-prewalk the
  // same prompt completed in 12 events / 20s). Headless runs must not
  // inherit prewalk.enabled from ~/.omp/agent/config.yml.
  // --no-lsp --no-extensions: LSP/MCP discovery in devagent worktrees stalls
  // omp startup for 60-487s (observed: "Still starting after 487s — phase:
  // discoverAndLoadMCPTools"); with them stripped, identical runs complete
  // in ~17-21s. Coding workers exercise tools via the provider, not LSP.
  const base: string[] = ['--mode', 'json', '--no-prewalk', '--no-lsp', '--no-extensions'];
  if (o.resume) {
    return [
      ...base,
      ...(ompModel ? ['--model', ompModel] : []),
      ...(rawThinking ? ['--thinking', rawThinking] : []),
      '-c',
      RESUME_PROMPT,
    ];
  }
  return [
    '-p',
    opts.prompt,
    ...base,
    ...(ompModel ? ['--model', ompModel] : []),
    ...(rawThinking ? ['--thinking', rawThinking] : []),
  ];
}

export interface OmpOutcome {
  isError: boolean;
  sessionId: string | null;
  errorText?: string;
  resultText: string | null;
  parsed: Record<string, unknown> | null;
  timedOut: boolean;
}

/** Test seam: re-export of the parser. */
export function interpretOmpForTest(run: SpawnCliResult): OmpOutcome {
  return interpretOmp(run);
}

/**
 * Parse omp's stdout into the worker outcome shape. omp --mode json emits
 * an NDJSON event stream (one JSON object per line) per the live smoke
 * captured 2026-08-30 in test/workers/__fixtures__/omp-smoke-2026-08-30.jsonl
 * (175KB, 1591 lines). The terminal turn carries the assistant text.
 *
 * For backward compat with hand-rolled object/array envelopes, we also
 * accept a single JSON document when NDJSON parsing fails to find any
 * session/turn events.
 */
function interpretOmp(run: SpawnCliResult): OmpOutcome {
  let parsed: Record<string, unknown> | null = null;
  let sessionId: string | null = null;
  let terminalMessage: Record<string, unknown> | null = null;
  let streamError: string | null = null;
  // Try single-JSON first (legacy callers). If it parses as object/array,
  // skip the NDJSON walk.
  if (run.stdout.trim()) {
    try {
      const candidate: unknown = JSON.parse(run.stdout);
      if (Array.isArray(candidate)) {
        for (let i = candidate.length - 1; i >= 0; i--) {
          const e = candidate[i] as Record<string, unknown> | null;
          if (e !== null && typeof e === 'object' && !Array.isArray(e) && e.type === 'result') {
            parsed = e;
            break;
          }
        }
      } else if (candidate !== null && typeof candidate === 'object') {
        const obj = candidate as Record<string, unknown>;
        // NDJSON guard: real omp's first line {"type":"session",...} parses
        // as a single object but is just a stream header. Only treat the
        // single-JSON fast path as valid when the object has result
        // metadata; otherwise fall through to the NDJSON walk.
        if (
          'result' in obj ||
          'is_error' in obj ||
          ('type' in obj && obj.type === 'result') ||
          'session_id' in obj
        ) {
          parsed = obj;
        }
      }
    } catch {
      // Not a single JSON document — fall through to NDJSON walk.
    }
  }

  let rawResult: string | undefined;
  if (parsed !== null) {
    const raw = parsed.result;
    if (typeof raw === 'string') rawResult = raw;
    if (typeof parsed.session_id === 'string') sessionId = parsed.session_id;
  } else {
    // NDJSON walk: each non-empty line is one event. We keep the FIRST
    // turn_end/message_end (the user-answer turn), not the last — real omp
    // emits a prewalk turn after the answer when --prewalk is configured,
    // and we want the original assistant text, not the prewalk thought.
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
      if (event.type === 'session' && typeof event.id === 'string') {
        sessionId = event.id;
      }
      if (event.type === 'turn_end' || event.type === 'message_end') {
        const msg = event.message;
        if (msg !== null && typeof msg === 'object' && !Array.isArray(msg)) {
          const m = msg as Record<string, unknown>;
          // Both user and assistant turns emit message_end; only the
          // assistant's text is the worker result.
          if (m.role === 'assistant' && terminalMessage === null) {
            terminalMessage = m;
          }
          // omp exits 0 even when the model call fails (observed 2026-08-30:
          // GitLab Duo 401 surfaces as assistant message_end with errorMessage
          // and NO result event). Capture the first errorMessage so the
          // failure is not misread as an empty-but-successful run.
          if (streamError === null && typeof m.errorMessage === 'string') {
            streamError = m.errorMessage;
          }
        }
      }
    }
    if (terminalMessage !== null) {
      const content = terminalMessage.content;
      if (Array.isArray(content)) {
        for (const part of content) {
          if (
            part !== null &&
            typeof part === 'object' &&
            (part as Record<string, unknown>).type === 'text' &&
            typeof (part as Record<string, unknown>).text === 'string'
          ) {
            rawResult = (part as Record<string, unknown>).text as string;
            break;
          }
        }
      }
    }
  }
  const isError = parsed?.is_error === true || streamError !== null;
  const resultText =
    run.exitCode === 0 && !isError && typeof rawResult === 'string' ? rawResult : null;
  const errorText =
    streamError ??
    (typeof rawResult === 'string' && rawResult && isError
      ? rawResult
      : parsed === null && !rawResult && run.stderr.trim()
        ? run.stderr.trim()
        : undefined);
  return {
    isError,
    sessionId,
    errorText,
    resultText,
    parsed,
    timedOut: run.timedOut,
  };
}

function fallbackEmpty(): SpawnCliResult {
  return { exitCode: -1, stdout: '', stderr: 'omp adapter produced no spawn result', timedOut: false };
}

function finalize(run: SpawnCliResult, sessionId: string | null, start: number): WorkerResult {
  const outcome = interpretOmp(run);
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
 * Adapter over the `omp` headless CLI. Mirrors the structure of the
 * claude-code and opencode adapters so the executor can treat it as just
 * another WorkerAdapter. omp-specific behavior:
 *   - Uses `--mode json` (not `--output-format json`).
 *   - Resume via `-c` (continue) + RESUME_PROMPT (no session-id flag).
 *   - No `--api-key` CLI flag — env is the supported credential channel.
 *   - stdout is NDJSON event stream; parser walks lines (see interpretOmp).
 */
export class OmpAdapter implements WorkerAdapter {
  readonly name = 'omp' as const;

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

    let args = buildOmpArgs(opts);
    let sessionId: string | null = null;
    let last: SpawnCliResult | null = null;
    // omp exposes -r/--resume <id> (see docs/plans/omp-support-plan.md recon),
    // but using it requires carrying the session_id between attempts. The
    // adapter parses sessionId from the prior attempt's output but does not
    // feed it to -r because that would cross-talk between concurrent devagent
    // runs sharing the same cwd. Instead, retries use -c (continue most-recent
    // session in cwd) and we cap at maxAttempts so a hang cannot loop forever.
    const maxAttempts = 3;

    for (let attempt = 1; attempt <= maxAttempts; attempt++) {
      if (Date.now() >= wallDeadline) {
        if (last) last.timedOut = true;
        break;
      }
      const prepared = await prepareWorkerSpawn('omp', args, {
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

      const outcome = interpretOmp(last);
      if (outcome.sessionId) sessionId = outcome.sessionId;
      const ok = !last.timedOut && last.exitCode === 0 && !outcome.isError;
      if (ok) break;

      if (last.exitCode === -1 && !last.timedOut) break; // ENOENT
      if (attempt === maxAttempts) break;
      if (Date.now() >= wallDeadline) break;
      await this.sleep(2_000 * attempt);
      args = buildOmpArgs(opts, { resume: true });
    }

    return finalize(last ?? fallbackEmpty(), sessionId, start);
  }
}
