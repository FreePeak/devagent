import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it, afterEach, beforeEach, vi } from 'vitest';
import type { Mock } from 'vitest';
import {
  buildGrokArgs,
  interpretGrokForTest,
  GrokAdapter,
  isGrokProgressLine,
  grokChainNext,
  grokRetryModel,
  grokPromptCacheKey,
  grokPromptCacheEnv,
  GROK_PROMPT_CACHE_KEY_ENV,
} from '../../src/workers/grok.js';
import { isTransientProviderError, transientErrorClass } from '../../src/resilience/classify.js';
import { getWorker, workers } from '../../src/workers/index.js';

const fixture = (name: string): string =>
  readFileSync(fileURLToPath(new URL(`./__fixtures__/${name}`, import.meta.url)), 'utf8');

const baseOpts = (overrides: Partial<WorkerSpawnOptions> = {}): WorkerSpawnOptions => ({
  prompt: 'do the thing',
  cwd: '/tmp/work',
  timeoutMs: 60_000,
  ...overrides,
});

describe('grok adapter - registry', () => {
  it('registers the GrokAdapter under the grok name', () => {
    expect(workers.grok).toBeInstanceOf(GrokAdapter);
    expect(getWorker('grok')).toBe(workers.grok);
    expect(getWorker('grok').name).toBe('grok');
  });
});

describe('grok adapter - Seam A: argument building', () => {
  it('builds the minimum headless argv: -p, prompt, --output-format streaming-json', () => {
    const args = buildGrokArgs(baseOpts());
    expect(args).toEqual(['-p', 'do the thing', '--output-format', 'streaming-json']);
  });

  it('forwards --model for exact xAI slugs', () => {
    const args = buildGrokArgs(baseOpts({ model: 'grok-4.6' }));
    expect(args).toEqual(['-p', 'do the thing', '--output-format', 'streaming-json', '--model', 'grok-4.6']);
  });

  it('forwards --model for xai/-qualified ids', () => {
    const args = buildGrokArgs(baseOpts({ model: 'xai/grok-build-0.1' }));
    expect(args).toContain('--model');
    expect(args).toContain('xai/grok-build-0.1');
  });

  it('drops tier aliases and bare config aliases (loop-58 precedent); CLI default applies', () => {
    expect(buildGrokArgs(baseOpts({ model: 'coding' }))).not.toContain('--model');
    expect(buildGrokArgs(baseOpts({ model: 'free' }))).not.toContain('--model');
    expect(buildGrokArgs(baseOpts({ model: '  ' }))).not.toContain('--model');
  });

  it('builds a resume argv using -c with the resume prompt (verified live 2026-09-06)', () => {
    const args = buildGrokArgs(baseOpts(), { resume: true });
    expect(args).toEqual(['-c', '-p', 'Continue', '--output-format', 'streaming-json']);
  });

  it('omits the --api-key flag from CLI args (OAuth/XAI_API_KEY env are the supported channels)', () => {
    const args = buildGrokArgs(baseOpts());
    expect(args.some((a) => a.startsWith('--api-key'))).toBe(false);
  });
});

describe('grok adapter - Seam B: streaming-json NDJSON parsing (captured fixtures)', () => {
  it('parses the clean answer run (grok-smoke-2026-09-06.jsonl)', () => {
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-smoke-2026-09-06.jsonl'),
      stderr: '',
      timedOut: false,
    });
    expect(o.isError).toBe(false);
    expect(o.sessionId).toBe('01a0779c-edce-7d70-91a5-214626e30396');
    expect(o.resultText).toBe('OK');
    expect(o.errorText).toBeUndefined();
    // The terminal `end` event carries usage/cost metadata (FR-GROK-03 fuel).
    expect(o.parsed).not.toBeNull();
    expect(o.parsed?.type).toBe('end');
    expect(o.parsed?.stopReason).toBe('end_turn');
  });

  it('concatenates chunked assistant text across tool calls (grok-toolrun-2026-09-06.jsonl)', () => {
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-toolrun-2026-09-06.jsonl'),
      stderr: '',
      timedOut: false,
    });
    expect(o.isError).toBe(false);
    expect(o.sessionId).toBe('01a0779c-edce-7d70-91a5-214626e30396');
    expect(o.resultText).toBe("I'll run `echo hi` and reply with only its output.hi");
    expect(o.parsed?.num_turns).toBe(2);
  });

  it('captures the in-stream error even though the CLI envelope is exit-0-shaped (grok-error-2026-09-06.jsonl)', () => {
    // Live capture: the provider 401 surfaced as {"type":"error"}; Grok
    // Build is documented to exit 0 on provider failures (same trap as omp
    // 2026-08-30), so the walk must not rely on the exit code alone.
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-error-2026-09-06.jsonl'),
      stderr: '',
      timedOut: false,
    });
    expect(o.isError).toBe(true);
    expect(o.resultText).toBeNull();
    expect(o.errorText).toContain('Unauthorized (401)');
    expect(o.sessionId).toBeNull();
    expect(o.parsed).toBeNull();
  });

  it('classifies the captured 401 as non-retryable end-to-end (FR-GROK-06)', () => {
    // The grok error fixture's message contains "temporarily unavailable"
    // and "retry in a few seconds" — transient wording that must NOT flip
    // an auth failure into an infinite infra retry.
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-error-2026-09-06.jsonl'),
      stderr: '',
      timedOut: false,
    });
    expect(o.isError).toBe(true);
    expect(o.errorText).toContain('Unauthorized (401)');
    expect(isTransientProviderError(o.errorText)).toBe(false);
    expect(transientErrorClass(o.errorText)).toBeNull();
  });

  it('surfaces stderr on garbage stdout with no end event', () => {
    const o = interpretGrokForTest({
      exitCode: 1,
      stdout: 'not json at all\n{"broken\n',
      stderr: 'grok boom',
      timedOut: false,
    });
    expect(o.parsed).toBeNull();
    expect(o.sessionId).toBeNull();
    expect(o.resultText).toBeNull();
    expect(o.errorText).toBe('grok boom');
  });

  it('reports a timeout-shaped run without throwing', () => {
    const o = interpretGrokForTest({
      exitCode: 124,
      stdout: '',
      stderr: 'killed by watchdog',
      timedOut: true,
    });
    expect(o.parsed).toBeNull();
    expect(o.timedOut).toBe(true);
  });

  it('reads a stream ending on whole tool_call events as progress, never empty/failed (FR-GROK-06)', () => {
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-toolcall-whole-2026-09-07.jsonl'),
      stderr: '',
      timedOut: false,
    });
    expect(o.isError).toBe(false);
    expect(o.errorText).toBeUndefined();
    expect(o.toolCallCount).toBe(2);
    // The last tool event survives as `parsed`, so finalize emits a result
    // event: the zero-events "empty" signature cannot fire.
    expect(o.parsed).not.toBeNull();
    expect(o.parsed?.type).toBe('tool_call');
    expect(o.parsed?.toolCallId).toBe('call-whole-2');
    // No assistant text was streamed — resultText stays honestly null.
    expect(o.resultText).toBeNull();
    // Cost still rides the usage row.
    expect(o.costUsdTicks).toBe(981000);
  });

  it('does not promote stderr noise to errorText while tool activity is present (FR-GROK-06)', () => {
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-toolcall-whole-2026-09-07.jsonl'),
      stderr: 'warning: telemetry flush failed',
      timedOut: false,
    });
    expect(o.isError).toBe(false);
    expect(o.errorText).toBeUndefined();
  });
});

describe('grok adapter - Seam B: exact cost ticks (FR-GROK-03)', () => {
  const run = (stdout: string) =>
    interpretGrokForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });

  it('copies total_cost_usd_ticks off the end event verbatim', () => {
    const o = run(
      '{"type":"end","sessionId":"s1","stopReason":"end_turn","usage":{"input_tokens":10},"total_cost_usd_ticks":527119000}\n',
    );
    expect(o.costUsdTicks).toBe(527119000);
  });

  it('reads cost_in_usd_ticks off a usage event when the end event omits it', () => {
    const o = run(
      '{"type":"usage","usage":{"input_tokens":5,"cost_in_usd_ticks":12345}}\n' +
        '{"type":"end","sessionId":"s1","stopReason":"end_turn"}\n',
    );
    expect(o.costUsdTicks).toBe(12345);
  });

  it('prefers the terminal end total over an earlier usage row', () => {
    const o = run(
      '{"type":"usage","usage":{"cost_in_usd_ticks":111}}\n' +
        '{"type":"end","total_cost_usd_ticks":999}\n',
    );
    expect(o.costUsdTicks).toBe(999);
  });

  it('leaves cost undefined when no event carries it — never coerced to 0', () => {
    const o = run('{"type":"end","sessionId":"s1","stopReason":"end_turn","usage":{"input_tokens":1}}\n');
    expect(o.costUsdTicks).toBeUndefined();
  });

  it('keeps a genuine 0-tick run as 0, not undefined', () => {
    const o = run('{"type":"end","total_cost_usd_ticks":0}\n');
    expect(o.costUsdTicks).toBe(0);
  });

  it('ignores a non-numeric cost field instead of coercing it', () => {
    const o = run('{"type":"end","total_cost_usd_ticks":"527119000"}\n');
    expect(o.costUsdTicks).toBeUndefined();
  });

  it('records the captured smoke run cost verbatim', () => {
    const o = interpretGrokForTest({
      exitCode: 0,
      stdout: fixture('grok-smoke-2026-09-06.jsonl'),
      stderr: '',
      timedOut: false,
    });
    expect(o.costUsdTicks).toBe(527119000);
  });
});

describe('grok adapter - Seam C: meaningful-line progress filter (Q33)', () => {
  const adapter = new GrokAdapter();

  it('thought chunks are never progress (48 of them in a 10s echo run)', () => {
    expect(isGrokProgressLine('{"type":"thought","data":"The"}')).toBe(false);
    expect(adapter.isProgress!('{"type":"thought","data":" user"}')).toBe(false);
  });

  it('tool calls and finalized text chunks are progress', () => {
    expect(isGrokProgressLine('{"type":"tool_call","toolCallId":"c1","title":"run_terminal_command"}')).toBe(true);
    expect(isGrokProgressLine('{"type":"tool_call_update","toolCallId":"c1","status":"completed"}')).toBe(true);
    expect(isGrokProgressLine('{"type":"text","data":"OK"}')).toBe(true);
    expect(adapter.isProgress!('{"type":"text","data":"OK"}')).toBe(true);
  });

  it('headers, usage rows, blanks, and garbage are not progress', () => {
    expect(isGrokProgressLine('{"type":"available_commands","tools":["read_file"]}')).toBe(false);
    expect(isGrokProgressLine('{"type":"usage","usage":{"input_tokens":1}}')).toBe(false);
    expect(isGrokProgressLine('')).toBe(false);
    expect(isGrokProgressLine('   ')).toBe(false);
    expect(isGrokProgressLine('not json')).toBe(false);
  });
});

describe('grok adapter - Seam A: chain model override on argv (FR-GROK-06)', () => {
  it('forwards the chain override even when opts.model is unset', () => {
    const args = buildGrokArgs(baseOpts({ model: undefined }), { model: 'grok-4.3' });
    expect(args).toContain('--model');
    expect(args[args.indexOf('--model') + 1]).toBe('grok-4.3');
  });

  it('the override still passes the FR-GROK-02 predicate (aliases are dropped)', () => {
    const args = buildGrokArgs(baseOpts({ model: undefined }), { model: 'coding' });
    expect(args).not.toContain('--model');
  });
});

describe('grok adapter - Seam D: within-xAI fallback chain (FR-GROK-06)', () => {
  it('steps grok-4.6 → grok-4.3 → grok-build-0.1 and stops', () => {
    expect(grokChainNext('grok-4.6')).toBe('grok-4.3');
    expect(grokChainNext('grok-4.3')).toBe('grok-build-0.1');
    expect(grokChainNext('grok-build-0.1')).toBeNull();
  });

  it('positions dated pins and xai/-qualified ids at their chain rung', () => {
    expect(grokChainNext('grok-4.6-2026-08-14')).toBe('grok-4.3');
    expect(grokChainNext('xai/grok-4.3')).toBe('grok-build-0.1');
  });

  it('unset starts at the chain head; a non-chain family yields null', () => {
    expect(grokChainNext(undefined)).toBe('grok-4.6');
    expect(grokChainNext('')).toBe('grok-4.6');
    expect(grokChainNext('grok-4.5')).toBeNull();
  });

  it('advances only on the rate-limit-tpm class', () => {
    const tpm = '429 rate_limit_error: exceeded tokens per minute (TPM) limit';
    expect(grokRetryModel('grok-4.6', tpm)).toBe('grok-4.3');
    // RPS is a cooldown problem, not a context problem: same model.
    expect(
      grokRetryModel('grok-4.6', '429 requests per second (RPS) limit exceeded'),
    ).toBe('grok-4.6');
    // 5xx, generic rate-limit, and non-transient text never step the chain.
    expect(grokRetryModel('grok-4.6', 'Server error (503) from https://api.x.ai/v1')).toBe(
      'grok-4.6',
    );
    expect(grokRetryModel('grok-4.6', '429 too many requests')).toBe('grok-4.6');
    expect(grokRetryModel('grok-4.6', null)).toBe('grok-4.6');
  });

  it('chain exhaustion keeps the tail model (cross-provider fallback is above)', () => {
    expect(grokRetryModel('grok-build-0.1', 'tokens per minute limit reached')).toBe(
      'grok-build-0.1',
    );
  });
});

const ledger = (taskId: string, attempt = 1) => ({
  repoPath: '/repo',
  taskId,
  attempt,
  worker: 'grok',
});

describe('grok adapter - Seam A: per-task prompt-cache key (FR-GROK-04)', () => {
  it('same task → same key across retries (attempt moves, key does not)', () => {
    const first = grokPromptCacheKey(baseOpts({ watchdogLedger: ledger('TASK-abc', 1) }));
    const retry = grokPromptCacheKey(baseOpts({ watchdogLedger: ledger('TASK-abc', 2) }));
    expect(first).toBe('devagent-TASK-abc');
    expect(retry).toBe(first);
  });

  it('distinct tasks → distinct keys', () => {
    const a = grokPromptCacheKey(baseOpts({ watchdogLedger: ledger('TASK-abc') }));
    const b = grokPromptCacheKey(baseOpts({ watchdogLedger: ledger('TASK-def') }));
    expect(a).not.toBe(b);
  });

  it('absent watchdogLedger → no key derived and no key emitted', () => {
    expect(grokPromptCacheKey(baseOpts())).toBeUndefined();
    expect(grokPromptCacheEnv(baseOpts())).toEqual({});
  });

  it('blank taskId → no key (probe/one-off spawns stay keyless)', () => {
    expect(grokPromptCacheKey(baseOpts({ watchdogLedger: ledger('   ') }))).toBeUndefined();
    expect(grokPromptCacheEnv(baseOpts({ watchdogLedger: ledger('  ') }))).toEqual({});
  });

  it('the key never rides argv (grok 1.0.13 has no cache-key flag; env is the channel)', () => {
    const opts = baseOpts({ watchdogLedger: ledger('TASK-abc') });
    expect(buildGrokArgs(opts).join(' ')).not.toContain('devagent-TASK-abc');
    expect(buildGrokArgs(opts, { resume: true }).join(' ')).not.toContain('devagent-TASK-abc');
  });
});

describe('grok adapter - Seam E: cache key through the prepareWorkerSpawn env channel (FR-GROK-04)', () => {
  let runWorkerCliMock: Mock;
  let prepareWorkerSpawnMock: Mock;

  const errRun = {
    exitCode: 0,
    stdout: '{"type":"error","message":"upstream 503 from api.x.ai"}\n',
    stderr: '',
    timedOut: false,
  };
  const okRun = {
    exitCode: 0,
    stdout:
      '{"type":"text","data":"done"}\n{"type":"end","sessionId":"s-1","stopReason":"end_turn"}\n',
    stderr: '',
    timedOut: false,
  };

  const spawnEnvOf = (call: unknown[]): Record<string, string> | undefined => {
    const preparedOpts = call[2];
    if (!preparedOpts || typeof preparedOpts !== 'object' || !('env' in preparedOpts)) {
      return undefined;
    }
    const env: unknown = preparedOpts.env;
    if (typeof env !== 'object' || env === null) return undefined;
    // Mocked prepareWorkerSpawn options carry the adapter's merged env verbatim.
    return env as Record<string, string>;
  };

  // vi.doMock only reaches modules imported after resetModules — a static
  // import cannot pick the mocks up, so the adapter is re-imported per test
  // (module-loading-boundary exception to the static-import rule).
  async function freshGrokAdapter() {
    const { GrokAdapter: Fresh } = await import('../../src/workers/grok.js');
    return Fresh;
  }

  beforeEach(() => {
    vi.resetModules();
    prepareWorkerSpawnMock = vi.fn().mockResolvedValue({
      cmd: 'grok',
      args: [],
      opts: {},
      strippedEnv: [],
    });
    vi.doMock('../../src/workers/sandbox.js', () => ({
      prepareWorkerSpawn: prepareWorkerSpawnMock,
    }));
    runWorkerCliMock = vi.fn();
    vi.doMock('../../src/workers/herdr-runtime.js', () => ({
      runWorkerCli: runWorkerCliMock,
    }));
  });

  afterEach(() => {
    vi.doUnmock('../../src/workers/sandbox.js');
    vi.doUnmock('../../src/workers/herdr-runtime.js');
    vi.resetModules();
  });

  it('emits the same key on the first attempt and the retry (cache hits stick across the loop)', async () => {
    const Grok = await freshGrokAdapter();
    runWorkerCliMock.mockResolvedValueOnce(errRun).mockResolvedValueOnce(okRun);
    const adapter = new Grok(async () => {});
    await adapter.spawn(baseOpts({ watchdogLedger: ledger('TASK-abc') }));
    expect(runWorkerCliMock).toHaveBeenCalledTimes(2);
    const first = spawnEnvOf(prepareWorkerSpawnMock.mock.calls[0] ?? []);
    const retry = spawnEnvOf(prepareWorkerSpawnMock.mock.calls[1] ?? []);
    expect(first?.[GROK_PROMPT_CACHE_KEY_ENV]).toBe('devagent-TASK-abc');
    expect(retry?.[GROK_PROMPT_CACHE_KEY_ENV]).toBe(first?.[GROK_PROMPT_CACHE_KEY_ENV]);
  });

  it('merges over caller env without dropping it; the per-task key wins', async () => {
    const Grok = await freshGrokAdapter();
    runWorkerCliMock.mockResolvedValue(okRun);
    const adapter = new Grok(async () => {});
    await adapter.spawn(
      baseOpts({
        watchdogLedger: ledger('TASK-abc'),
        env: { FOO: 'bar', [GROK_PROMPT_CACHE_KEY_ENV]: 'stale' },
      }),
    );
    const env = spawnEnvOf(prepareWorkerSpawnMock.mock.calls[0] ?? []);
    expect(env?.FOO).toBe('bar');
    expect(env?.[GROK_PROMPT_CACHE_KEY_ENV]).toBe('devagent-TASK-abc');
  });

  it('no watchdogLedger → no env overlay reaches prepareWorkerSpawn', async () => {
    const Grok = await freshGrokAdapter();
    runWorkerCliMock.mockResolvedValue(okRun);
    const adapter = new Grok(async () => {});
    await adapter.spawn(baseOpts());
    const env = spawnEnvOf(prepareWorkerSpawnMock.mock.calls[0] ?? []);
    expect(env).toBeUndefined();
  });
});
