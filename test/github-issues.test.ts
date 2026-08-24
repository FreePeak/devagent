import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  GITHUB_ISSUE_REF,
  fetchGitHubTicket,
  parseGitHubIssue,
  parseGitHubIssueRef,
} from '../src/integrations/github-issues.js';
import { buildDeps } from '../src/deps.js';
import { RunLogger } from '../src/logger.js';

describe('parseGitHubIssueRef', () => {
  it('parses owner/repo#n refs', () => {
    expect(parseGitHubIssueRef('acme/widget#42')).toEqual({ owner: 'acme', repo: 'widget', number: 42 });
  });

  it('rejects malformed refs', () => {
    expect(parseGitHubIssueRef('ENG-204')).toBeNull();
    expect(parseGitHubIssueRef('acme/widget#abc')).toBeNull();
    expect(GITHUB_ISSUE_REF.test('ENG-204')).toBe(false);
    expect(GITHUB_ISSUE_REF.test('acme.co/repo.x#7')).toBe(true);
  });
});

describe('parseGitHubIssue / fetchGitHubTicket', () => {
  afterEach(() => vi.unstubAllGlobals());

  const payload = {
    number: 42,
    title: 'Ship the thing',
    body: '## Acceptance Criteria\n- [ ] works\n- [X] tested',
    labels: [{ name: 'backend' }, { name: '' }],
    html_url: 'https://github.com/acme/widget/issues/42',
  };

  it('maps fields into TicketSpec', () => {
    const t = parseGitHubIssue('acme/widget#42', payload);
    expect(t.id).toBe('acme/widget#42');
    expect(t.title).toBe('Ship the thing');
    expect(t.labels).toEqual(['backend']);
    expect(t.acceptanceCriteria).toEqual(['works', 'tested']);
    expect(t.trackerInternalId).toBe('acme/widget#42');
    expect(t.url).toBe('https://github.com/acme/widget/issues/42');
  });

  it('calls the REST endpoint with Bearer auth and honors 429 Retry-After', async () => {
    const sleeps: number[] = [];
    let calls = 0;
    const fetchMock = vi.fn().mockImplementation(async () => {
      calls++;
      return {
        ok: true,
        status: calls === 1 ? 429 : 200,
        headers: { get: (k: string) => (k === 'retry-after' ? '5' : null) },
        json: async () => payload,
      };
    });
    vi.stubGlobal('fetch', fetchMock);

    const ticket = await fetchGitHubTicket('acme/widget#42', 'tok', {
      retries: 2,
      sleep: async (ms) => sleeps.push(ms),
    });
    expect(ticket.id).toBe('acme/widget#42');
    expect(sleeps).toEqual([5000]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe('https://api.github.com/repos/acme/widget/issues/42');
    const headers = init.headers as Record<string, string>;
    expect(headers.Authorization).toBe('Bearer tok');
    expect(headers.Accept).toBe('application/vnd.github+json');
  });

  it('retries on 403 secondary rate limits then succeeds', async () => {
    let calls = 0;
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(async () => {
        calls++;
        return {
          ok: true,
          status: calls === 1 ? 403 : 200,
          headers: { get: () => null },
          json: async () => payload,
        };
      }),
    );
    const ticket = await fetchGitHubTicket('acme/widget#42', 'tok', { retries: 1, sleep: async () => {} });
    expect(ticket.title).toBe('Ship the thing');
  });

  it('throws on non-ok after retries exhausted', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({ ok: false, status: 404, headers: { get: () => null } }),
    );
    await expect(
      fetchGitHubTicket('acme/widget#404', 'tok', { retries: 1, sleep: async () => {} }),
    ).rejects.toThrow(/HTTP 404/);
  });

  it('rejects malformed refs before calling the API', async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    await expect(fetchGitHubTicket('ENG-204', 'tok')).rejects.toThrow(/owner\/repo#n/);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('buildDeps fetchTicket routing', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    delete process.env.JIRA_DOMAIN;
    delete process.env.JIRA_EMAIL;
    delete process.env.JIRA_API_TOKEN;
  });

  const cfg = { repoPath: '.', maxLoops: 1, timeoutMs: 1000, worker: 'claude-code' as const, autoPr: false };
  const log = new RunLogger();

  it('routes owner/repo#n refs to GitHub when a token is set', async () => {
    let calledUrl = '';
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        calledUrl = url;
        return { ok: false, status: 500, headers: { get: () => null } };
      }),
    );
    const d = buildDeps({ linearApiKey: 'x', githubToken: 'ghtok' }, cfg, log);
    await expect(d.fetchTicket('acme/widget#42')).rejects.toThrow(/GitHub Issues API request failed/);
    expect(calledUrl).toBe('https://api.github.com/repos/acme/widget/issues/42');
  });

  it('throws a clear error for GitHub refs without a token', async () => {
    const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
    // throws synchronously before any promise exists
    expect(() => d.fetchTicket('acme/widget#42')).toThrow(/GITHUB_TOKEN/);
  });

  it('keeps Linear routing for non-GitHub ids', async () => {
    let calledUrl = '';
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        calledUrl = url;
        return { ok: false, status: 401, headers: { get: () => null } };
      }),
    );
    const d = buildDeps({ linearApiKey: 'x' }, cfg, log);
    await expect(d.fetchTicket('ENG-204')).rejects.toThrow(/Linear API request failed/);
    expect(calledUrl).not.toMatch(/api\.github\.com/);
  });
});
