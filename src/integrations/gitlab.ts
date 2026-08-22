/**
 * GitLab publisher (Phase 4): open a merge request via REST with a
 * project/group access token. Auth: PRIVATE-TOKEN header; git push uses any
 * username + token as password. Bot users cannot approve their own MRs,
 * which preserves the human-review gate.
 */

export interface GitlabCredentials {
  baseUrl: string; // e.g. https://gitlab.com or self-hosted root
  projectId: number | string; // numeric id or URL-encoded path
  token: string;
}

export interface CreateMrOptions {
  sourceBranch: string;
  targetBranch: string;
  title: string;
  description: string;
}

export async function createMergeRequest(
  creds: GitlabCredentials,
  opts: CreateMrOptions,
  http: { post: typeof fetch } = { post: fetch },
): Promise<string> {
  const url = `${creds.baseUrl}/api/v4/projects/${encodeURIComponent(String(creds.projectId))}/merge_requests`;
  const res = await http.post(url, {
    method: 'POST',
    headers: {
      'PRIVATE-TOKEN': creds.token,
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({
      source_branch: opts.sourceBranch,
      target_branch: opts.targetBranch,
      title: opts.title,
      description: opts.description,
    }),
  });
  if (!res.ok) {
    throw new Error(`GitLab MR creation failed: HTTP ${res.status}`);
  }
  const payload = (await res.json()) as { web_url?: string };
  if (!payload.web_url) {
    throw new Error('GitLab MR response missing web_url');
  }
  return payload.web_url;
}
