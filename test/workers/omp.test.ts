import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { buildOmpArgs, interpretOmpForTest } from '../../src/workers/omp.js';
import type { WorkerSpawnOptions } from '../../src/types.js';

const baseOpts = (overrides: Partial<WorkerSpawnOptions> = {}): WorkerSpawnOptions => ({
  prompt: 'do the thing',
  cwd: '/tmp/work',
  timeoutMs: 60_000,
  ...overrides,
});

describe('omp adapter - Seam A: argument building', () => {
  it('builds the minimum non-interactive argv: -p, prompt, --mode json, --no-prewalk --no-lsp --no-extensions', () => {
    const args = buildOmpArgs(baseOpts());
    expect(args).toEqual(['-p', 'do the thing', '--mode', 'json', '--no-prewalk', '--no-lsp', '--no-extensions']);
  });

  it('forwards --model when provided', () => {
    const args = buildOmpArgs(baseOpts({ model: 'omniroute/bai/glm-5.3-flash' }));
    expect(args).toEqual([
      '-p',
      'do the thing',
      '--mode',
      'json',
      '--no-prewalk',
      '--no-lsp',
      '--no-extensions',
      '--model',
      'omniroute/bai/glm-5.3-flash',
    ]);
  });

  it('forwards --thinking when variant provided', () => {
    const args = buildOmpArgs(baseOpts({ variant: 'high' }));
    expect(args).toEqual(['-p', 'do the thing', '--mode', 'json', '--no-prewalk', '--no-lsp', '--no-extensions', '--thinking', 'high']);
  });

  it('builds a resume argv using -c with the resume prompt and no -p', () => {
    const args = buildOmpArgs(baseOpts(), { resume: true });
    expect(args).toEqual(['--mode', 'json', '--no-prewalk', '--no-lsp', '--no-extensions', '-c', 'Continue']);
  });

  it('drops provider-unqualified model aliases (driver tiers like "coding") and passes provider/model through', () => {
    expect(buildOmpArgs(baseOpts({ model: 'coding' }))).toEqual([
      '-p',
      'do the thing',
      '--mode',
      'json',
      '--no-prewalk',
      '--no-lsp',
      '--no-extensions',
    ]);
    expect(buildOmpArgs(baseOpts({ model: 'omniroute/bai/glm-5.3-flash' }))).toContain('--model');
  });

  it('omits the --api-key flag from CLI args (env is the supported channel)', () => {
    const args = buildOmpArgs(baseOpts());
    expect(args.some((a) => a.startsWith('--api-key'))).toBe(false);
  });
});

describe('omp adapter - Seam B: stdout envelope parsing', () => {
  it('parses an object envelope with .result and .session_id', () => {
    const stdout = JSON.stringify({
      type: 'result',
      is_error: false,
      session_id: 'omp-s-1',
      result: 'hello world',
    });
    const o = interpretOmpForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });
    expect(o.isError).toBe(false);
    expect(o.sessionId).toBe('omp-s-1');
    expect(o.resultText).toBe('hello world');
    expect(o.errorText).toBeUndefined();
  });

  it('parses an array envelope and pulls the terminal result entry', () => {
    const stdout = JSON.stringify([
      { type: 'system', session_id: 'omp-s-2' },
      { type: 'assistant', text: 'thinking' },
      { type: 'result', is_error: false, session_id: 'omp-s-2', result: 'final' },
    ]);
    const o = interpretOmpForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });
    expect(o.isError).toBe(false);
    expect(o.sessionId).toBe('omp-s-2');
    expect(o.resultText).toBe('final');
  });

  it('flags is_error when the result entry sets it', () => {
    const stdout = JSON.stringify({
      type: 'result',
      is_error: true,
      session_id: 'omp-s-3',
      result: 'upstream rejected the prompt',
    });
    const o = interpretOmpForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });
    expect(o.isError).toBe(true);
    expect(o.errorText).toBe('upstream rejected the prompt');
  });

  it('returns parsed=null and surfaces stderr on garbage stdout', () => {
    const o = interpretOmpForTest({
      exitCode: 0,
      stdout: 'not json',
      stderr: 'parser boom',
      timedOut: false,
    });
    expect(o.parsed).toBeNull();
    expect(o.sessionId).toBeNull();
    expect(o.errorText).toBe('parser boom');
  });

  it('reports a timeout-shaped run without throwing', () => {
    const o = interpretOmpForTest({
      exitCode: 124,
      stdout: '',
      stderr: 'killed by watchdog',
      timedOut: true,
    });
    expect(o.parsed).toBeNull();
    expect(o.timedOut).toBe(true);
  });

  it('parses the real omp NDJSON event stream (fixture 2026-08-30)', () => {
    const fixture = fileURLToPath(
      new URL('./__fixtures__/omp-smoke-2026-08-30.jsonl', import.meta.url),
    );
    const stdout = readFileSync(fixture, 'utf8');
    const o = interpretOmpForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });
    expect(o.isError).toBe(false);
    expect(o.sessionId).toBe('01a05127-5cc0-7680-9853-7dc3c80a1477');
    expect(o.resultText).toBe('OK');
  });
});
