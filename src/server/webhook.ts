import { createHmac } from 'node:crypto';
import type { IncomingMessage, ServerResponse } from 'node:http';

/**
 * Webhook receiver core (FR-TICKET-04): raw-body collection, HMAC-SHA256
 * signature verification, and delivery-ID dedup. Transport-agnostic so tests
 * drive it without opening a socket; server.ts wires it to node:http.
 *
 * Responds 2xx within the provider deadline (Linear: 5s) — handlers run async.
 */

export interface WebhookRequest {
  headers: Record<string, string | string[] | undefined>;
  rawBody: Buffer;
}

export interface VerifiedWebhook {
  /** Provider delivery id for dedup (Linear-Delivery / X-GitHub-Delivery) */
  deliveryId: string;
  event: string;
  /** GitHub's X-GitHub-Event header when present (issues, push, ...) */
  githubEvent?: string;
  payload: unknown;
}

export class SignatureError extends Error {}

const MAX_BODY_BYTES = 1 << 20; // 1 MiB

export function verifyAndParse(
  req: WebhookRequest,
  signingSecret: string,
): VerifiedWebhook {
  const deliveryId = header(req.headers['linear-delivery']) ?? header(req.headers['x-github-delivery']);
  if (!deliveryId) throw new SignatureError('missing delivery id');

  const provided = header(req.headers['linear-signature']) ?? header(req.headers['x-hub-signature-256']);
  if (!provided) throw new SignatureError('missing signature');

  const expected = createHmac('sha256', signingSecret).update(req.rawBody).digest('hex');
  const providedHex = stripPrefix(provided);
  if (!timingSafeEqualHex(expected, providedHex)) {
    throw new SignatureError('signature mismatch');
  }

  let payload: unknown;
  try {
    payload = JSON.parse(req.rawBody.toString('utf8'));
  } catch {
    throw new SignatureError('invalid JSON body');
  }

  return {
    deliveryId,
    event: header(req.headers['linear-event']) ?? 'unknown',
    githubEvent: header(req.headers['x-github-event']) ?? undefined,
    payload,
  };
}

/** In-memory dedup window; a real deployment swaps this for the run store. */
export class DeliveryDedup {
  private readonly seen = new Set<string>();

  constructor(private readonly maxEntries = 10_000) {}

  /** Returns true on first sight of a delivery id. */
  isFirst(deliveryId: string): boolean {
    if (this.seen.has(deliveryId)) return false;
    if (this.seen.size >= this.maxEntries) {
      // drop oldest ~10% rather than growing unbounded
      const drop = Math.floor(this.maxEntries / 10);
      let i = 0;
      for (const k of this.seen) {
        this.seen.delete(k);
        if (++i >= drop) break;
      }
    }
    this.seen.add(deliveryId);
    return true;
  }
}

/** Express-style adapter over node:http primitives. */
export function handleWebhook(
  req: IncomingMessage,
  res: ServerResponse,
  opts: { signingSecret: string; dedup?: DeliveryDedup; onEvent(v: VerifiedWebhook): void },
): void {
  const chunks: Buffer[] = [];
  let size = 0;
  req.on('data', (chunk: Buffer) => {
    size += chunk.length;
    if (size > MAX_BODY_BYTES) {
      res.statusCode = 413;
      res.end();
      req.destroy();
      return;
    }
    chunks.push(chunk);
  });
  req.on('end', () => {
    try {
      const verified = verifyAndParse({ headers: req.headers, rawBody: Buffer.concat(chunks) }, opts.signingSecret);
      if (!verified.deliveryId || (opts.dedup && !opts.dedup.isFirst(verified.deliveryId))) {
        res.statusCode = 200;
        res.end('duplicate');
        return;
      }
      res.statusCode = 200;
      res.end('accepted'); // respond inside the provider deadline...
      setImmediate(() => opts.onEvent(verified)); // ...then process async
    } catch (err) {
      res.statusCode = err instanceof SignatureError ? 401 : 400;
      res.end((err as Error).message);
    }
  });
}

function header(value: string | string[] | undefined): string | null {
  if (Array.isArray(value)) return value[0] ?? null;
  return value ?? null;
}

/**
 * Extract a dispatchable ticket identifier from a Linear AgentSessionEvent
 * payload. Treats all fields as untrusted source data (PRD risk R5).
 */
export interface AgentSessionDispatch {
  issueIdentifier: string;
  issueId: string;
  commentBody: string | null;
}

export function parseAgentSessionEvent(payload: unknown): AgentSessionDispatch | null {
  const p = payload as { type?: unknown; agentSession?: { issue?: { identifier?: unknown; id?: unknown } }; comment?: { body?: unknown } } | null;
  if (!p || p.type !== 'AgentSessionEvent') return null;
  const issue = p.agentSession?.issue;
  if (typeof issue?.identifier !== 'string' || typeof issue?.id !== 'string') return null;
  return {
    issueIdentifier: issue.identifier,
    issueId: issue.id,
    commentBody: typeof p.comment?.body === 'string' ? p.comment.body : null,
  };
}

/**
 * Extract a dispatchable ticket from a GitHub issues webhook event
 * (event header "issues", action opened/labeled). Untrusted-data discipline:
 * only well-typed fields are read.
 */
export interface GithubIssueDispatch {
  /** "owner/repo#123" composite key */
  issueIdentifier: string;
  title: string;
}

export function parseGithubIssueEvent(eventHeader: string | undefined, payload: unknown): GithubIssueDispatch | null {
  const ghEvent = (payload as { action?: unknown; issue?: { number?: unknown; title?: unknown; pull_request?: unknown }; repository?: { full_name?: unknown } } | null);
  if (eventHeader !== 'issues' || !ghEvent || ghEvent.action !== 'opened') return null;
  const issue = ghEvent.issue;
  // PRs also fire "issues" events — skip those (they have their own pipeline)
  if (!issue || 'pull_request' in issue) return null;
  if (typeof issue.number !== 'number' || typeof issue.title !== 'string') return null;
  if (typeof ghEvent.repository?.full_name !== 'string') return null;
  return {
    issueIdentifier: `${ghEvent.repository.full_name}#${issue.number}`,
    title: issue.title,
  };
}

function stripPrefix(sig: string): string {
  return sig.startsWith('sha256=') ? sig.slice(7) : sig;
}

export function timingSafeEqualHex(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}
