import { describe, expect, it } from 'vitest';
import { interpretForTest } from '../src/workers/claude-code.js';

describe('claude-code adapter envelope parsing', () => {
  it('extracts result + session_id from array-shaped json output', () => {
    const stdout = JSON.stringify([
      { type: 'system', subtype: 'init', session_id: 's-1' },
      { type: 'assistant', message: {} },
      {
        type: 'result',
        subtype: 'success',
        is_error: false,
        session_id: 's-1',
        result: '[{"id":"T1","title":"t","prompt":"p","dependsOn":[]}]',
      },
    ]);
    const o = interpretForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });
    expect(o.isError).toBe(false);
    expect(o.sessionId).toBe('s-1');
    expect(JSON.parse(o.errorText!)).toHaveLength(1);
  });

  it('still handles legacy single-object envelopes', () => {
    const stdout = JSON.stringify({
      type: 'result',
      is_error: false,
      session_id: 's-2',
      result: 'done',
    });
    const o = interpretForTest({ exitCode: 0, stdout, stderr: '', timedOut: false });
    expect(o.sessionId).toBe('s-2');
    expect(o.errorText).toBe('done');
  });

  it('falls back to unparsed on garbage without throwing', () => {
    const o = interpretForTest({ exitCode: 0, stdout: 'not json at all', stderr: '', timedOut: false });
    expect(o.parsed).toBeNull();
    expect(o.sessionId).toBeNull();
  });
});
