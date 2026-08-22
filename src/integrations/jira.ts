import type { TicketSpec } from '../types.js';

/**
 * Jira Cloud adapter (Phase 4): fetch a ticket via REST API v3 with
 * scoped API-token Basic auth. Mirrors the Linear adapter shape.
 * Auth: `email:apiToken` base64 — the token inherits the user's permissions,
 * so use a dedicated service account with minimal project scope.
 */

export interface JiraCredentials {
  domain: string; // e.g. acme.atlassian.net
  email: string;
  apiToken: string;
}

export const ADF_TO_TEXT = (node: unknown): string => {
  const n = node as { type?: string; text?: string; content?: unknown[] } | null;
  if (!n) return '';
  if (n.type === 'text') return n.text ?? '';
  if (n.type === 'hardBreak') return '\n';
  // block containers (doc, bulletList, ...) separate children by newline;
  // inline containers (paragraph) concatenate their children directly
  const sep = n.type === 'paragraph' ? '' : '\n';
  return (n.content ?? []).map(ADF_TO_TEXT).join(sep);
};

/** Extract `- [ ]` checklist items under an "Acceptance Criteria" heading; fall back to all checklist items. */
export function extractAcceptanceCriteria(markdown: string): string[] {
  const lines = markdown.split('\n');
  const inSection = /#{1,6}\s*(acceptance criteria|ac)\b/i;
  let inAc = false;
  const all: string[] = [];
  const fromAc: string[] = [];
  for (const line of lines) {
    if (inSection.test(line)) {
      inAc = true;
      continue;
    }
    if (/^#{1,6}\s/.test(line)) inAc = false;
    const m = line.match(/^\s*[-*]\s*\[( |x|X)\]\s*(.+)$/);
    if (m) {
      all.push(m[2]!.trim());
      if (inAc) fromAc.push(m[2]!.trim());
    }
  }
  return fromAc.length > 0 ? fromAc : all;
}

interface JiraIssuePayload {
  key: string;
  fields: {
    summary: string;
    description: unknown; // ADF document
    labels?: string[];
  };
}

export function parseJiraIssue(payload: JiraIssuePayload): TicketSpec {
  const description = ADF_TO_TEXT(payload.fields.description);
  return {
    id: payload.key,
    title: payload.fields.summary,
    description,
    labels: payload.fields.labels ?? [],
    acceptanceCriteria: extractAcceptanceCriteria(description),
    trackerInternalId: payload.key,
  };
}

export async function fetchJiraTicket(
  issueKey: string,
  creds: JiraCredentials,
  opts: { retries?: number; sleep?: (ms: number) => Promise<void> } = {},
): Promise<TicketSpec> {
  const retries = opts.retries ?? 3;
  const sleep = opts.sleep ?? ((ms) => new Promise<void>((r) => setTimeout(r, ms)));
  const auth = Buffer.from(`${creds.email}:${creds.apiToken}`).toString('base64');
  const url = `https://${creds.domain}/rest/api/3/issue/${encodeURIComponent(issueKey)}?fields=summary,description,labels`;

  let res: Response | null = null;
  for (let attempt = 0; attempt <= retries; attempt++) {
    res = await fetch(url, {
      headers: { Authorization: `Basic ${auth}`, Accept: 'application/json' },
    });
    if (res.status !== 429 || attempt === retries) break;
    const retryAfter = Number(res.headers.get('retry-after'));
    const delayMs =
      Number.isFinite(retryAfter) && retryAfter > 0
        ? retryAfter * 1000
        : Math.min(30_000, 1000 * 2 ** attempt) + Math.floor(Math.random() * 500);
    await sleep(delayMs);
  }

  if (!res!.ok) {
    throw new Error(`Jira API request failed: HTTP ${res!.status}`);
  }
  const payload = (await res!.json()) as JiraIssuePayload & { errorMessages?: string[] };
  if (!payload?.key) {
    throw new Error(`Jira issue not found: ${issueKey}`);
  }
  return parseJiraIssue(payload);
}
