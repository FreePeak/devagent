import type { TicketSpec } from '../types.js';
import { extractAcceptanceCriteria } from './jira.js';

/**
 * GitHub Issues adapter: fetch a webhook-dispatched issue via the REST API
 * so it can enter the same pipeline as Linear/Jira tickets.
 * Issue refs are shaped `owner/repo#n`; auth is a PAT passed as Bearer —
 * use a fine-grained token with read-only access to the target repos.
 */

export const GITHUB_ISSUE_REF = /^[\w.-]+\/[\w.-]+#\d+$/;

/** Parse an `owner/repo#n` ref into its parts; returns null when malformed. */
export function parseGitHubIssueRef(ref: string): { owner: string; repo: string; number: number } | null {
  const m = ref.match(/^([\w.-]+)\/([\w.-]+)#(\d+)$/);
  if (!m) return null;
  return { owner: m[1]!, repo: m[2]!, number: Number(m[3]) };
}

interface GitHubIssuePayload {
  number: number;
  title: string;
  body: string | null;
  labels?: Array<{ name?: string }>;
  html_url?: string;
}

export function parseGitHubIssue(ref: string, payload: GitHubIssuePayload): TicketSpec {
  const description = payload.body ?? '';
  return {
    id: ref,
    title: payload.title,
    description,
    labels: (payload.labels ?? []).map((l) => l.name ?? '').filter(Boolean),
    acceptanceCriteria: extractAcceptanceCriteria(description),
    trackerInternalId: ref,
    url: payload.html_url,
  };
}

export async function fetchGitHubTicket(
  issueRef: string,
  token: string,
  opts: { retries?: number; sleep?: (ms: number) => Promise<void> } = {},
): Promise<TicketSpec> {
  const parsed = parseGitHubIssueRef(issueRef);
  if (!parsed) throw new Error(`Invalid GitHub issue ref: ${issueRef} (expected owner/repo#n)`);

  const retries = opts.retries ?? 3;
  const sleep = opts.sleep ?? ((ms) => new Promise<void>((r) => setTimeout(r, ms)));
  const url = `https://api.github.com/repos/${parsed.owner}/${parsed.repo}/issues/${parsed.number}`;
  const headers = {
    Authorization: `Bearer ${token}`,
    Accept: 'application/vnd.github+json',
  };

  let res: Response | null = null;
  for (let attempt = 0; attempt <= retries; attempt++) {
    res = await fetch(url, { headers });
    // Retry on rate limits: 429 too-many-requests and 403 secondary rate limit
    if ((res.status !== 429 && res.status !== 403) || attempt === retries) break;
    const retryAfter = Number(res.headers.get('retry-after'));
    const delayMs =
      Number.isFinite(retryAfter) && retryAfter > 0
        ? retryAfter * 1000
        : Math.min(30_000, 1000 * 2 ** attempt) + Math.floor(Math.random() * 500);
    await sleep(delayMs);
  }

  if (!res!.ok) {
    throw new Error(`GitHub Issues API request failed: HTTP ${res!.status}`);
  }
  const payload = (await res!.json()) as GitHubIssuePayload & { message?: string };
  if (!payload?.number || typeof payload.title !== 'string') {
    throw new Error(`GitHub issue not found: ${issueRef}`);
  }
  return parseGitHubIssue(issueRef, payload);
}
