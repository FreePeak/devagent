import { describe, expect, it } from 'vitest';
import { createHmac, randomBytes } from 'node:crypto';
import {
  verifyAndParse,
  DeliveryDedup,
  timingSafeEqualHex,
  SignatureError,
} from '../src/server/webhook.js';

const secret = 'whsec_test_' + randomBytes(8).toString('hex');

function signed(body: string): Record<string, string> {
  const sig = createHmac('sha256', secret).update(body).digest('hex');
  return {
    'linear-delivery': 'delivery-1',
    'linear-signature': sig,
    'linear-event': 'AgentSessionEvent',
  };
}

describe('verifyAndParse', () => {
  const body = JSON.stringify({ type: 'AgentSessionEvent' });

  it('accepts a valid signed delivery', () => {
    const v = verifyAndParse({ headers: signed(body), rawBody: Buffer.from(body) }, secret);
    expect(v.deliveryId).toBe('delivery-1');
    expect(v.event).toBe('AgentSessionEvent');
    expect(v.payload).toEqual({ type: 'AgentSessionEvent' });
  });

  it('rejects tampered bodies', () => {
    const headers = signed(body);
    expect(() =>
      verifyAndParse({ headers, rawBody: Buffer.from(JSON.stringify({ evil: true })) }, secret),
    ).toThrow(SignatureError);
  });

  it('rejects missing signature and missing delivery id', () => {
    const h = signed(body);
    delete (h as Record<string, string>)['linear-signature'];
    expect(() => verifyAndParse({ headers: h, rawBody: Buffer.from(body) }, secret)).toThrow(/signature/);

    const h2 = signed(body);
    delete (h2 as Record<string, string>)['linear-delivery'];
    expect(() => verifyAndParse({ headers: h2, rawBody: Buffer.from(body) }, secret)).toThrow(/delivery/);
  });

  it('supports github-style sha256= prefix header', () => {
    const sig = createHmac('sha256', secret).update(body).digest('hex');
    const v = verifyAndParse(
      {
        headers: { 'x-github-delivery': 'gh-1', 'x-hub-signature-256': `sha256=${sig}` },
        rawBody: Buffer.from(body),
      },
      secret,
    );
    expect(v.deliveryId).toBe('gh-1');
  });
});

describe('DeliveryDedup', () => {
  it('returns true once per delivery id', () => {
    const d = new DeliveryDedup();
    expect(d.isFirst('a')).toBe(true);
    expect(d.isFirst('a')).toBe(false);
    expect(d.isFirst('b')).toBe(true);
  });

  it('evicts old entries at capacity', () => {
    const d = new DeliveryDedup(10);
    for (let i = 0; i < 12; i++) d.isFirst(`k${i}`);
    // oldest evicted; k0 seen again is "first" now
    expect(d.isFirst('k11')).toBe(false);
    expect(typeof d.isFirst('k0')).toBe('boolean');
  });
});

describe('timingSafeEqualHex', () => {
  it('compares correctly without short-circuit', () => {
    expect(timingSafeEqualHex('abcd', 'abcd')).toBe(true);
    expect(timingSafeEqualHex('abcd', 'abce')).toBe(false);
    expect(timingSafeEqualHex('abc', 'abcd')).toBe(false);
  });
});

describe('parseGithubIssueEvent', () => {
  it('maps issue opened events to owner/repo#n', async () => {
    const { parseGithubIssueEvent } = await import('../src/server/webhook.js');
    const d = parseGithubIssueEvent('issues', {
      action: 'opened',
      issue: { number: 5, title: 'Fix login' },
      repository: { full_name: 'acme/api' },
    });
    expect(d).toEqual({ issueIdentifier: 'acme/api#5', title: 'Fix login' });
  });

  it('ignores non-issue events, PRs, and malformed payloads', async () => {
    const { parseGithubIssueEvent } = await import('../src/server/webhook.js');
    expect(parseGithubIssueEvent('push', {})).toBeNull();
    expect(parseGithubIssueEvent('issues', { action: 'opened', issue: { number: 1, title: 'x', pull_request: {} }, repository: { full_name: 'a/b' } })).toBeNull();
    expect(parseGithubIssueEvent('issues', { action: 'closed', issue: { number: 1, title: 'x' }, repository: { full_name: 'a/b' } })).toBeNull();
    expect(parseGithubIssueEvent('issues', { action: 'opened', issue: { title: 'no number' }, repository: { full_name: 'a/b' } })).toBeNull();
  });
});
