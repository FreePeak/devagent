import { describe, expect, it, vi } from 'vitest';
import {
  createMergeRequest,
  postMrNote,
  waitForMrPipeline,
  type GitlabCredentials,
  type GitlabHttpClient,
} from '../src/integrations/gitlab.js';

const creds: GitlabCredentials = { baseUrl: 'https://gitlab.com', projectId: 42, token: 'glpat-x' };

/** Fake http client recording calls and replaying scripted responses. */
function fakeClient(responses: Array<{ status: number; body?: unknown }>) {
  const calls: Array<{ url: string; init: { method: string; headers: Record<string, string>; body?: string } }> = [];
  const http: GitlabHttpClient = {
    request: async (url, init) => {
      calls.push({ url, init });
      const next = responses.shift();
      if (!next) throw new Error('unexpected extra HTTP call');
      return {
        ok: next.status >= 200 && next.status < 300,
        status: next.status,
        json: async () => next.body,
        text: async () => (next.body === undefined ? '' : JSON.stringify(next.body)),
      };
    },
  };
  return { calls, http };
}

describe('createMergeRequest', () => {
  it('posts PRIVATE-TOKEN MR and returns web_url', async () => {
    const { http, calls } = fakeClient([
      { status: 201, body: { web_url: 'https://gitlab.com/g/p/-/merge_requests/3', iid: 3 } },
    ]);
    const url = await createMergeRequest(
      creds,
      { sourceBranch: 'devagent/E-1', targetBranch: 'main', title: 't', description: 'd' },
      http,
    );
    expect(url).toBe('https://gitlab.com/g/p/-/merge_requests/3');
    expect(calls[0]?.url).toContain('/api/v4/projects/42/merge_requests');
    expect(calls[0]?.init.headers['PRIVATE-TOKEN']).toBe('glpat-x');
    const body = JSON.parse(String(calls[0]?.init.body)) as Record<string, string>;
    expect(body.source_branch).toBe('devagent/E-1');
    expect(body.target_branch).toBe('main');
  });

  it('URL-encodes project path ids', async () => {
    const { http, calls } = fakeClient([{ status: 201, body: { web_url: 'u' } }]);
    await createMergeRequest(
      { ...creds, projectId: 'group/proj' },
      { sourceBranch: 'a', targetBranch: 'b', title: 't', description: 'd' },
      http,
    );
    expect(calls[0]?.url).toContain(encodeURIComponent('group/proj'));
  });

  it('throws with HTTP status and API error body on failure', async () => {
    const { http } = fakeClient([{ status: 401, body: { message: 'invalid token' } }]);
    await expect(
      createMergeRequest(creds, { sourceBranch: 'a', targetBranch: 'b', title: 't', description: 'd' }, http),
    ).rejects.toThrow(/HTTP 401: invalid token/);
  });

  it('throws when web_url is missing', async () => {
    const { http } = fakeClient([{ status: 201, body: {} }]);
    await expect(
      createMergeRequest(creds, { sourceBranch: 'a', targetBranch: 'b', title: 't', description: 'd' }, http),
    ).rejects.toThrow(/web_url/);
  });
});

describe('postMrNote', () => {
  it('posts note body to the MR notes endpoint', async () => {
    const { http, calls } = fakeClient([{ status: 201, body: { id: 9 } }]);
    await postMrNote(creds, 3, 'validation evidence: all gates green', http);
    expect(calls[0]?.url).toBe('https://gitlab.com/api/v4/projects/42/merge_requests/3/notes');
    expect(JSON.parse(String(calls[0]?.init.body)).body).toContain('validation evidence');
  });

  it('throws with API error detail on failure', async () => {
    const { http } = fakeClient([{ status: 404, body: { error: '404 Not Found' } }]);
    await expect(postMrNote(creds, 3, 'x', http)).rejects.toThrow(/HTTP 404: 404 Not Found/);
  });
});

describe('waitForMrPipeline', () => {
  const noSleep = async () => {};

  it('resolves true when the latest pipeline succeeds', async () => {
    const { http } = fakeClient([{ status: 200, body: [{ id: 5, status: 'success' }] }]);
    await expect(waitForMrPipeline(creds, 3, http, { sleep: noSleep })).resolves.toBe(true);
  });

  it('polls until terminal state, sleeping between attempts', async () => {
    const sleeps: number[] = [];
    const { http } = fakeClient([
      { status: 200, body: [{ id: 1, status: 'running' }] },
      { status: 200, body: [{ id: 1, status: 'pending' }] },
      { status: 200, body: [{ id: 1, status: 'success' }] },
    ]);
    await expect(waitForMrPipeline(creds, 3, http, { pollIntervalMs: 2_000, sleep: async (ms) => { sleeps.push(ms); } })).resolves.toBe(true);
    expect(sleeps).toEqual([2_000, 2_000]);
  });

  it('throws listing pipeline statuses on terminal failure', async () => {
    const { http } = fakeClient([{ status: 200, body: [{ id: 6, status: 'failed' }, { id: 5, status: 'success' }] }]);
    await expect(waitForMrPipeline(creds, 3, http, { sleep: noSleep })).rejects.toThrow(/#6:failed/);
  });

  it('times out loudly while pipelines stay non-terminal', async () => {
    const { http } = fakeClient(Array.from({ length: 30 }, () => ({ status: 200 as const, body: [{ id: 1, status: 'running' }] })));
    await expect(waitForMrPipeline(creds, 3, http, { timeoutMs: 0, pollIntervalMs: 1, sleep: noSleep })).rejects.toThrow(/timed out/);
  });

  it('waits (not fails) when no pipelines exist yet', async () => {
    const { http } = fakeClient([
      { status: 200, body: [] },
      { status: 200, body: [{ id: 2, status: 'success' }] },
    ]);
    await expect(waitForMrPipeline(creds, 3, http, { pollIntervalMs: 1, sleep: noSleep })).resolves.toBe(true);
  });
});
