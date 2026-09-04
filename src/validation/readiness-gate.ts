import type { Finding, Severity, TicketClass, TicketSpec } from '../types.js';

/**
 * Gate G0 (issue readiness): deterministic, type-specific scoring of an
 * incoming ticket against ready-for-dev criteria, evaluated BEFORE worker
 * dispatch so under-specified tickets never burn worker credits.
 *
 * Shape mirrors the G5 STRIDE gate (src/validation/stride-gate.ts): a pure
 * regex/length rubric over the ticket text — no LLM, no network, never
 * throws on missing fields. Every unmet criterion becomes a Finding and the
 * summed score decides pass/reject against the class threshold.
 */

/** Minimum score (0-100) for a ticket to be dispatched. Uniform across classes. */
export const G0_READINESS_THRESHOLD = 60;

export interface ReadinessGateResult {
  gate: 'G0-readiness';
  passed: boolean;
  /** True when no rubric applies (unknown classification) — never treat as verified-green. */
  skipped?: boolean;
  /** 0-100 readiness score. */
  score: number;
  /** Minimum passing score for the classification. */
  threshold: number;
  findings: Finding[];
  detail: string;
}

interface ReadinessRule {
  ruleId: string;
  severity: Severity;
  /** Points awarded when the pattern matches anywhere in the ticket text. */
  weight: number;
  pattern: RegExp;
  /** Human-facing gap description used as the finding message when unmatched. */
  gap: string;
}

/**
 * Type-specific ready-for-dev signals, one rubric per FR-PLAN-03
 * classification. Weights: common criteria (title/description/criteria)
 * carry 65 points; the two class signals carry 35.
 */
const CLASS_RULES: Record<TicketClass, readonly ReadinessRule[]> = {
  'endpoint-only': [
    {
      ruleId: 'G0-ENDPOINT-SURFACE',
      severity: 'medium',
      weight: 20,
      pattern: /\b(get|post|put|patch|delete|head|options)\b[^.\n]{0,60}\/[a-z0-9._~-]|\/api\/|\/v\d+\//i,
      gap: 'name the HTTP surface (method + path, e.g. GET /api/things)',
    },
    {
      ruleId: 'G0-VERIFICATION',
      severity: 'medium',
      weight: 15,
      pattern: /\b(test|tests|spec|specs|curl|expect|expects|assert|verif|coverage|integration|unit)\b|returns? \d{3}|status code/i,
      gap: 'state how the endpoint is verified (tests, expected status/body)',
    },
  ],
  'migration-required': [
    {
      ruleId: 'G0-SCHEMA-ENTITY',
      severity: 'medium',
      weight: 20,
      pattern: /\b(table|column|schema|index|constraint|foreign key|primary key|migration)\b/i,
      gap: 'name the affected schema entities (table/column/index)',
    },
    {
      ruleId: 'G0-REVERSIBILITY',
      severity: 'medium',
      weight: 15,
      pattern: /\b(down migration|down\.sql|rollback|revert|reverse|reversible|back out|backout)\b/i,
      gap: 'state the down-migration/rollback expectation',
    },
  ],
  'consumer-only': [
    {
      ruleId: 'G0-TRANSPORT',
      severity: 'medium',
      weight: 20,
      pattern: /\b(queue|topic|consumer|publisher|subscriber|subscription|kafka|sqs|rabbitmq|webhook|stream)\b/i,
      gap: 'name the transport (queue/topic/webhook and its name)',
    },
    {
      ruleId: 'G0-DELIVERY',
      severity: 'medium',
      weight: 15,
      pattern: /\b(idempoten|duplicate|at.least.once|exactly.once|dedup|retry|retries|redeliver|offset|ack)\b/i,
      gap: 'state delivery semantics (idempotency, duplicate/retry handling)',
    },
  ],
};

const SEVERITY_TAG: Record<Severity, string> = { critical: 'C', high: 'H', medium: 'M', low: 'L' };

/** Deterministic markdown block for the string detail channel. */
function formatDetail(
  passed: boolean,
  score: number,
  threshold: number,
  classification: TicketClass,
  findings: Finding[],
): string {
  const head = `G0 readiness ${score}/100 (threshold ${threshold}) for ${classification}: ${passed ? 'pass' : 'rejected'}`;
  if (findings.length === 0) return head;
  return [head, ...findings.map((f) => `- [${SEVERITY_TAG[f.severity]}] ${f.ruleId}: ${f.message}`)].join('\n');
}

/**
 * Score a ticket against the readiness rubric for its classification.
 * Tolerates missing/undefined fields (tracker adapters return partial data);
 * unknown classifications skip honestly like the other gates.
 */
export function evaluateReadiness(input: {
  ticket: Pick<TicketSpec, 'id' | 'title' | 'description' | 'labels' | 'acceptanceCriteria'>;
  classification: TicketClass;
}): ReadinessGateResult {
  const cls = CLASS_RULES[input.classification] ? input.classification : null;
  if (!cls) {
    return {
      gate: 'G0-readiness',
      passed: true,
      skipped: true,
      score: 0,
      threshold: 0,
      findings: [],
      detail: 'skipped: unknown ticket classification',
    };
  }

  const ticket = input.ticket ?? { id: '', title: '', description: '', labels: [], acceptanceCriteria: [] };
  const title = (ticket.title ?? '').trim();
  const description = (ticket.description ?? '').trim();
  const criteria = (ticket.acceptanceCriteria ?? []).filter((c) => typeof c === 'string' && c.trim());
  const text = [title, description, (ticket.labels ?? []).join(' '), criteria.join('\n')].join('\n');

  const findings: Finding[] = [];
  let score = 0;

  // Common criterion: substantive title.
  if (title.length >= 10) {
    score += 20;
  } else {
    findings.push({
      ruleId: 'G0-TITLE',
      severity: 'high',
      message: `title too short (${title.length} chars, need >= 10); state what is delivered`,
    });
  }

  // Common criterion: substantive description.
  if (description.length >= 40) {
    score += 20;
  } else {
    findings.push({
      ruleId: 'G0-DESCRIPTION',
      severity: 'high',
      message: `description too short (${description.length} chars, need >= 40); add context, scope, and expected behavior`,
    });
  }

  // Common criterion: machine-checkable acceptance criteria (tiered).
  if (criteria.length >= 2) {
    score += 25;
  } else if (criteria.length === 1) {
    score += 15;
  } else {
    findings.push({
      ruleId: 'G0-ACCEPTANCE',
      severity: 'high',
      message: 'no acceptance criteria; add machine-checkable completion signals',
    });
  }

  // Type-specific ready-for-dev signals.
  for (const rule of CLASS_RULES[cls]) {
    if (rule.pattern.test(text)) {
      score += rule.weight;
    } else {
      findings.push({ ruleId: rule.ruleId, severity: rule.severity, message: rule.gap });
    }
  }

  const threshold = G0_READINESS_THRESHOLD;
  const passed = score >= threshold;
  return {
    gate: 'G0-readiness',
    passed,
    score,
    threshold,
    findings,
    detail: formatDetail(passed, score, threshold, cls, findings),
  };
}
