import type { TicketSpec } from '../types.js';

const LINEAR_GRAPHQL_ENDPOINT = 'https://api.linear.app/graphql';

/** Linear rate limits: honor Retry-After on 429 with jittered backoff (max 3 retries). */
export async function fetchLinear(
  body: string,
  apiKey: string,
  opts: { retries?: number; sleep?: (ms: number) => Promise<void> } = {},
): Promise<Response> {
  const retries = opts.retries ?? 3;
  const sleep = opts.sleep ?? ((ms) => new Promise<void>((r) => setTimeout(r, ms)));
  let res: Response | null = null;
  for (let attempt = 0; attempt <= retries; attempt++) {
    res = await fetch(LINEAR_GRAPHQL_ENDPOINT, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: apiKey },
      body,
    });
    if (res.status !== 429 || attempt === retries) break;
    const retryAfter = Number(res.headers.get('retry-after'));
    const delayMs = Number.isFinite(retryAfter) && retryAfter > 0
      ? retryAfter * 1000
      : Math.min(30_000, 1000 * 2 ** attempt) + Math.floor(Math.random() * 500);
    await sleep(delayMs);
  }
  return res!;
}

/**
 * GraphQL query fetching a Linear issue by identifier (e.g. "ENG-204").
 * Exported so tests can assert on its shape without network access.
 */
export const LINEAR_ISSUE_QUERY = `
query Issue($id: String!) {
  issue(id: $id) {
    id
    title
    description
    url
    labels {
      nodes {
        name
      }
    }
  }
}
`.trim();

interface RawIssue {
  id?: unknown;
  title?: unknown;
  description?: unknown;
  url?: unknown;
  labels?: {
    nodes?: Array<{ name?: unknown } | null> | null;
  } | null;
}

interface GraphQLResponse {
  data?: { issue?: RawIssue | null } | null;
  errors?: Array<{ message?: unknown }> | null;
}

function isChecklistItem(line: string): boolean {
  return /^\s*-\s+\[[ xX]\]\s+/.test(line);
}

function checklistText(line: string): string {
  return line.replace(/^\s*-\s+\[[ xX]\]\s+/, '').trim();
}

/**
 * Extract acceptance criteria from a markdown description.
 * Preference order:
 *   1. Checklist items under any heading containing "acceptance" (case-insensitive)
 *      up to the next heading of the same-or-higher level.
 *   2. All checklist items anywhere in the document.
 *   3. Empty array.
 */
export function extractAcceptanceCriteria(description: string): string[] {
  const lines = description.split(/\r?\n/);

  const acceptanceHeadingIdx = lines.findIndex((line) =>
    /^#{1,6}\s+.*acceptance/i.test(line),
  );

  let scope: string[];
  if (acceptanceHeadingIdx >= 0) {
    const headingLine = lines[acceptanceHeadingIdx] ?? '';
    const headingLevel = (headingLine.match(/^#+/)?.[0] ?? '#').length;
    let end = lines.length;
    for (let i = acceptanceHeadingIdx + 1; i < lines.length; i++) {
      const line = lines[i];
      if (!line) continue;
      if (/^#{1,6}\s+/.test(line)) {
        const level = (line.match(/^#+/)?.[0] ?? '#').length;
        if (level <= headingLevel) {
          end = i;
          break;
        }
      }
    }
    scope = lines.slice(acceptanceHeadingIdx + 1, end);
  } else {
    scope = lines;
  }

  return scope.filter(isChecklistItem).map(checklistText);
}

/**
 * Pure mapping from the raw GraphQL issue payload to a TicketSpec.
 * Throws when the payload does not contain an issue with a usable title.
 */
export function parseLinearIssue(data: unknown): TicketSpec {
  const response = data as GraphQLResponse | null | undefined;
  const issue = response?.data?.issue;

  if (!issue || typeof issue.title !== 'string' || issue.title.length === 0) {
    throw new Error('Linear issue not found or malformed response');
  }

  const description =
    typeof issue.description === 'string' ? issue.description : '';

  const labels = (issue.labels?.nodes ?? [])
    .map((node) => (node && typeof node.name === 'string' ? node.name : null))
    .filter((name): name is string => name !== null);

  return {
    id: '',
    title: issue.title,
    description,
    labels,
    acceptanceCriteria: extractAcceptanceCriteria(description),
    url: typeof issue.url === 'string' ? issue.url : undefined,
    trackerInternalId: typeof issue.id === 'string' ? issue.id : undefined,
  };
}

const LINEAR_COMMENT_MUTATION = `
mutation Comment($input: CommentCreateInput!) {
  commentCreate(input: $input) {
    success
  }
}
`.trim();

/** Post a progress/clarification comment to a Linear issue (FR-TICKET-03). */
export async function postTicketComment(
  issueId: string,
  body: string,
  apiKey: string,
): Promise<void> {
  const res = await fetchLinear(
    JSON.stringify({
      query: LINEAR_COMMENT_MUTATION,
      variables: { input: { issueId, body } },
    }),
    apiKey,
  );

  if (!res.ok) {
    throw new Error(`Linear comment failed: HTTP ${res.status}`);
  }

  const gql = (await res.json()) as { errors?: Array<{ message?: string }> } | null;
  if (gql?.errors?.length) {
    throw new Error(`Linear comment GraphQL error: ${gql.errors[0]?.message ?? 'unknown'}`);
  }
}

/**
 * Fetch a Linear ticket by identifier and map it to a TicketSpec.
 * Uses Linear's API key auth scheme: `Authorization: <apiKey>` (no Bearer prefix).
 */
export async function fetchTicket(
  id: string,
  apiKey: string,
): Promise<TicketSpec> {
  const res = await fetchLinear(
    JSON.stringify({
      query: LINEAR_ISSUE_QUERY,
      variables: { id },
    }),
    apiKey,
  );

  if (!res.ok) {
    throw new Error(`Linear API request failed: HTTP ${res.status}`);
  }

  const body: unknown = await res.json();

  const gqlResponse = body as GraphQLResponse | null | undefined;
  if (gqlResponse?.errors && gqlResponse.errors.length > 0) {
    const first = gqlResponse.errors[0]?.message ?? 'unknown error';
    throw new Error(`Linear GraphQL error: ${first}`);
  }

  const ticket = parseLinearIssue(body);
  ticket.id = id;

  return ticket;
}
