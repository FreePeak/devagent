import { describe, expect, it, vi } from 'vitest';
import { createMergeRequest, type GitlabCredentials } from '../src/integrations/gitlab.js';

const creds: GitlabCredentials = { baseUrl: 'https://gitlab.com', projectId: 42, token: 'glpat-x' };

function okResponse(webUrl = 'https://gitlab.com/g/p/-/merge_requests/3'): Response {
  return {
    ok: true,
    status: 201,
    json: async () => ({ web_url: webUrl }),
  } as unknown as Response;
}

describe('createMergeRequest', () => {
  it('posts PRIVATE-TOKEN MR and returns web_url', async () => {
    const post = vi.fn().mockResolvedValue(okResponse());
    const url = await createMergeRequest(
      creds,
      { sourceBranch: 'devagent/E-1', targetBranch: 'main', title: 't', description: 'd' },
      { post: post as unknown as typeof fetch },
    );
    expect(url).toBe('https://gitlab.com/g/p/-/merge_requests/3');
    const [calledUrl, init] = post.mock.calls[0] as unknown as [string, RequestInit];
    expect(calledUrl).toContain('/api/v4/projects/42/merge_requests');
    expect((init.headers as Record<string, string>)['PRIVATE-TOKEN']).toBe('glpat-x');
    const body = JSON.parse(String(init.body)) as Record<string, string>;
    expect(body.source_branch).toBe('devagent/E-1');
    expect(body.target_branch).toBe('main');
  });

  it('URL-encodes project path ids', async () => {
    const post = vi.fn().mockResolvedValue(okResponse());
    await createMergeRequest(
      { ...creds, projectId: 'group/proj' },
      { sourceBranch: 'a', targetBranch: 'b', title: 't', description: 'd' },
      { post: post as unknown as typeof fetch },
    );
    const [calledUrl] = post.mock.calls[0] as unknown as [string];
    expect(calledUrl).toContain(encodeURIComponent('group/proj'));
  });

  it('throws with HTTP status on failure', async () => {
    const post = vi.fn().mockResolvedValue({ ok: false, status: 401 } as unknown as Response);
    await expect(
      createMergeRequest(creds, { sourceBranch: 'a', targetBranch: 'b', title: 't', description: 'd' }, { post: post as unknown as typeof fetch }),
    ).rejects.toThrow(/HTTP 401/);
  });

  it('throws when web_url is missing', async () => {
    const post = vi.fn().mockResolvedValue({
      ok: true,
      status: 201,
      json: async () => ({}),
    } as unknown as Response);
    await expect(
      createMergeRequest(creds, { sourceBranch: 'a', targetBranch: 'b', title: 't', description: 'd' }, { post: post as unknown as typeof fetch }),
    ).rejects.toThrow(/web_url/);
  });
});
