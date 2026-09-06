import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { buildGrokArgs, interpretGrokForTest, GrokAdapter, isGrokProgressLine } from '../../src/workers/grok.js';
import { getWorker, workers } from '../../src/workers/index.js';
import type { WorkerSpawnOptions } from '../../src/types.js';

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
