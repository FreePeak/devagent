/**
 * GitLab publisher (Phase 4): open a merge request via REST with a
 * project/group access token. Auth: PRIVATE-TOKEN header; git push uses any
 * username + token as password. Bot users cannot approve their own MRs,
 * which preserves the human-review gate.
 *
 * Closed-loop support: postMrNote attaches validation evidence to an MR
 * (trust per PR), waitForMrPipeline polls CI until a terminal state so
 * callers can refuse to hand off unverified changes.
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

export type HttpMethod = 'GET' | 'POST';

export interface GitlabHttpClient {
  request: (url: string, init: { method: HttpMethod; headers: Record<string, string>; body?: string }) => Promise<{
    ok: boolean;
    status: number;
    json: () => Promise<unknown>;
    text: () => Promise<string>;
  }>;
}

const defaultHttp: GitlabHttpClient = {
  request: async (url, init) => {
    const res = await fetch(url, init);
    return { ok: res.ok, status: res.status, json: () => res.json(), text: () => res.text() };
  },
};

function projectApiRoot(creds: GitlabCredentials): string {
  return `${creds.baseUrl}/api/v4/projects/${encodeURIComponent(String(creds.projectId))}`;
}

/** Extract GitLab's `{ message | error }` body for actionable failure output. */
async function apiErrorDetail(res: { json: () => Promise<unknown>; text: () => Promise<string> }): Promise<string> {
  try {
    const payload = (await res.json()) as { message?: unknown; error?: unknown };
    const detail = payload.message ?? payload.error;
    if (typeof detail === 'string' && detail) return detail;
    if (detail !== undefined && detail !== null) return JSON.stringify(detail);
  } catch {
    // fall through to raw text
  }
  try {
    const text = (await res.text()).trim();
    if (text) return text.slice(0, 300);
  } catch {
    // no body available
  }
  return '';
}

function headers(token: string): Record<string, string> {
  return { 'PRIVATE-TOKEN': token, 'Content-Type': 'application/json' };
}

export async function createMergeRequest(
  creds: GitlabCredentials,
  opts: CreateMrOptions,
  http: GitlabHttpClient = defaultHttp,
): Promise<string> {
  const url = `${projectApiRoot(creds)}/merge_requests`;
  const res = await http.request(url, {
    method: 'POST',
    headers: headers(creds.token),
    body: JSON.stringify({
      source_branch: opts.sourceBranch,
      target_branch: opts.targetBranch,
      title: opts.title,
      description: opts.description,
    }),
  });
  if (!res.ok) {
    throw new Error(`GitLab MR creation failed: HTTP ${res.status}${await apiErrorDetail(res).then((d) => (d ? `: ${d}` : ''))}`);
  }
  const payload = (await res.json()) as { web_url?: string; iid?: number };
  if (!payload.web_url) {
    throw new Error('GitLab MR response missing web_url');
  }
  return payload.web_url;
}

/**
 * Post a note (comment) on a merge request. Used to attach validation
 * evidence so reviewers see gate results without leaving GitLab.
 */
export async function postMrNote(
  creds: GitlabCredentials,
  mrIid: number,
  body: string,
  http: GitlabHttpClient = defaultHttp,
): Promise<void> {
  const url = `${projectApiRoot(creds)}/merge_requests/${mrIid}/notes`;
  const res = await http.request(url, {
    method: 'POST',
    headers: headers(creds.token),
    body: JSON.stringify({ body }),
  });
  if (!res.ok) {
    throw new Error(`GitLab MR note failed: HTTP ${res.status}${await apiErrorDetail(res).then((d) => (d ? `: ${d}` : ''))}`);
  }
}

export interface MrPipelineSummary {
  id: number;
  status: string;
}

type TerminalWaitState =
  | { done: true; outcome: 'success' }
  | { done: true; outcome: 'failed'; pipelines: MrPipelineSummary[] }
  | { done: false };

function evaluatePipelines(pipelines: MrPipelineSummary[]): TerminalWaitState {
  const latest = pipelines[0]; // GitLab returns newest first
  if (!latest) return { done: false };
  if (latest.status === 'success') return { done: true, outcome: 'success' };
  const terminalFailure = ['failed', 'canceled', 'skipped'];
  if (terminalFailure.includes(latest.status)) {
    return { done: true, outcome: 'failed', pipelines };
  }
  return { done: false };
}

/**
 * Poll the MR's pipelines until the latest reaches a terminal state or the
 * deadline passes. Resolves true on success; throws with pipeline statuses
 * on failure or timeout so callers can fail loudly instead of shipping red.
 */
export async function waitForMrPipeline(
  creds: GitlabCredentials,
  mrIid: number,
  http: GitlabHttpClient = defaultHttp,
  options: { timeoutMs?: number; pollIntervalMs?: number; sleep?: (ms: number) => Promise<void> } = {},
): Promise<boolean> {
  const timeoutMs = options.timeoutMs ?? 10 * 60_000;
  const pollIntervalMs = options.pollIntervalMs ?? 15_000;
  const sleep = options.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)));
  const deadline = Date.now() + timeoutMs;

  for (;;) {
    const url = `${projectApiRoot(creds)}/merge_requests/${mrIid}/pipelines`;
    const res = await http.request(url, { method: 'GET', headers: headers(creds.token) });
    if (!res.ok) {
      throw new Error(`GitLab pipeline poll failed: HTTP ${res.status}${await apiErrorDetail(res).then((d) => (d ? `: ${d}` : ''))}`);
    }
    const pipelines = (await res.json()) as MrPipelineSummary[];
    const state = evaluatePipelines(Array.isArray(pipelines) ? pipelines : []);
    if (state.done) {
      if (state.outcome === 'success') return true;
      const summary = state.pipelines
        .slice(0, 3)
        .map((p) => `#${p.id}:${p.status}`)
        .join(', ');
      throw new Error(`GitLab pipeline did not pass (${summary})`);
    }
    if (Date.now() >= deadline) {
      throw new Error(`GitLab pipeline wait timed out after ${timeoutMs}ms for MR !${mrIid}`);
    }
    await sleep(pollIntervalMs);
  }
}
