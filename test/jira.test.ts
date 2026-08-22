import { afterEach, describe, expect, it, vi } from 'vitest';
import { ADF_TO_TEXT, extractAcceptanceCriteria, fetchJiraTicket, parseJiraIssue } from '../src/integrations/jira.js';

describe('ADF_TO_TEXT', () => {
  it('flattens paragraphs and text nodes', () => {
    const doc = {
      type: 'doc',
      content: [
        { type: 'paragraph', content: [{ type: 'text', text: 'Line one.' }] },
        { type: 'paragraph', content: [{ type: 'text', text: 'Line two ' }, { type: 'text', text: 'continued.' }] },
      ],
    };
    expect(ADF_TO_TEXT(doc)).toBe('Line one.\nLine two continued.');
  });

  it('handles hardBreak and null safely', () => {
    expect(ADF_TO_TEXT({ type: 'paragraph', content: [{ type: 'hardBreak' }, { type: 'text', text: 'x' }] })).toBe('\nx');
    expect(ADF_TO_TEXT(null)).toBe('');
  });
});

describe('extractAcceptanceCriteria', () => {
  it('prefers checklist under an acceptance heading', () => {
    const md = ['## Context', '- [ ] not AC', '', '## Acceptance Criteria', '- [ ] first', '- [X] second'].join('\n');
    expect(extractAcceptanceCriteria(md)).toEqual(['first', 'second']);
  });

  it('falls back to all checklist items without a heading', () => {
    expect(extractAcceptanceCriteria('- [ ] alpha\n- [x] beta')).toEqual(['alpha', 'beta']);
  });
});

describe('parseJiraIssue / fetchJiraTicket', () => {
  afterEach(() => vi.unstubAllGlobals());

  const payload = {
    key: 'PROJ-7',
    fields: {
      summary: 'Ship the thing',
      description: { type: 'doc', content: [{ type: 'paragraph', content: [{ type: 'text', text: '- [ ] works' }] }] },
      labels: ['backend'],
    },
  };

  it('maps fields into TicketSpec', () => {
    const t = parseJiraIssue(payload);
    expect(t.id).toBe('PROJ-7');
    expect(t.title).toBe('Ship the thing');
    expect(t.labels).toEqual(['backend']);
    expect(t.acceptanceCriteria).toEqual(['works']);
  });

  it('uses Basic auth and honors 429 Retry-After', async () => {
    const sleeps: number[] = [];
    let calls = 0;
    const fetchMock = vi.fn().mockImplementation(async () => {
      calls++;
      return {
        ok: true,
        status: calls === 1 ? 429 : 200,
        headers: { get: (k: string) => (k === 'retry-after' ? '3' : null) },
        json: async () => payload,
      };
    });
    vi.stubGlobal('fetch', fetchMock);

    const ticket = await fetchJiraTicket(
      'PROJ-7',
      { domain: 'acme.atlassian.net', email: 'bot@acme.io', apiToken: 'tok' },
      { retries: 2, sleep: async (ms) => sleeps.push(ms) },
    );
    expect(ticket.id).toBe('PROJ-7');
    expect(sleeps).toEqual([3000]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect((init.headers as Record<string, string>).Authorization).toMatch(/^Basic /);
  });

  it('throws on non-200 after retries exhausted', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 403, headers: { get: () => null } }),
    );
    await expect(
      fetchJiraTicket('PROJ-9', { domain: 'd', email: 'e', apiToken: 't' }, { retries: 1, sleep: async () => {} }),
    ).rejects.toThrow(/HTTP 403/);
  });
});
