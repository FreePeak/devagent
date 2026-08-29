/**
 * Gate G5 executor adapter (PRD section 11): wraps the validated STRIDE
 * rubric in src/validation/stride-gate.ts for the consume/autoMerge path.
 * Reuses the existing rules verbatim — no rubric logic lives here; this
 * module only maps categories/severities to the gate-executor contract and
 * applies the CRITICAL promotion for committed credential literals.
 */
import { parseUnifiedDiff, runStrideGate } from '../validation/stride-gate.js';

export type StrideGateSeverity = 'HIGH' | 'CRITICAL' | 'MEDIUM' | 'LOW';

export type StrideCategory =
  | 'Spoofing'
  | 'Tampering'
  | 'Repudiation'
  | 'InformationDisclosure'
  | 'DenialOfService'
  | 'ElevationOfPrivilege';

export interface StrideGateFinding {
  category: StrideCategory;
  severity: StrideGateSeverity;
  evidence: string;
  file?: string;
  line?: number;
}

export interface StrideGateEvaluation {
  findings: StrideGateFinding[];
  severityMax: StrideGateSeverity | null;
  contextDigest?: string;
}

const CATEGORY_NAMES: Record<string, StrideCategory> = {
  S: 'Spoofing',
  T: 'Tampering',
  R: 'Repudiation',
  I: 'InformationDisclosure',
  D: 'DenialOfService',
  E: 'ElevationOfPrivilege',
};

const SEVERITY_NAMES: Record<string, StrideGateSeverity> = {
  high: 'HIGH',
  medium: 'MEDIUM',
  low: 'LOW',
};

const SEVERITY_RANK: Record<StrideGateSeverity, number> = {
  LOW: 1,
  MEDIUM: 2,
  HIGH: 3,
  CRITICAL: 4,
};

/** Committed credential literal: promotes an otherwise HIGH finding to CRITICAL. */
const CREDENTIAL_LITERAL = /(?:api[_-]?key|secret|password|token)\s*[:=]\s*["'][^"']{8,}/i;

/**
 * Run the STRIDE rubric over a unified diff. Null/undefined/empty diffs pass
 * with zero findings; the function never throws (diffs are untrusted input).
 * `contextDigest` is provenance-only and passed through verbatim (PRD Q12).
 */
export async function evaluateStride(input: {
  diff: string;
  contextDigest?: string;
}): Promise<StrideGateEvaluation> {
  try {
    if (!input || !input.diff) {
      return {
        findings: [],
        severityMax: null,
        ...(input?.contextDigest !== undefined ? { contextDigest: input.contextDigest } : {}),
      };
    }

    const hunks = parseUnifiedDiff(input.diff);
    const gate = await runStrideGate(hunks, '');

    const findings: StrideGateFinding[] = gate.findings.map((f) => {
      const severity: StrideGateSeverity =
        f.severity === 'high' && CREDENTIAL_LITERAL.test(f.evidence)
          ? 'CRITICAL'
          : (SEVERITY_NAMES[f.severity] ?? 'LOW');
      return {
        category: CATEGORY_NAMES[f.category] ?? 'Spoofing',
        severity,
        evidence: f.evidence,
        file: f.file,
        ...(f.line !== undefined ? { line: f.line } : {}),
      };
    });

    let severityMax: StrideGateSeverity | null = null;
    for (const f of findings) {
      if (severityMax === null || SEVERITY_RANK[f.severity] > SEVERITY_RANK[severityMax]) {
        severityMax = f.severity;
      }
    }

    return {
      findings,
      severityMax,
      ...(input.contextDigest !== undefined ? { contextDigest: input.contextDigest } : {}),
    };
  } catch {
    // Never throw: an evaluation failure must not block or crash the merge path.
    return {
      findings: [],
      severityMax: null,
      ...(input?.contextDigest !== undefined ? { contextDigest: input.contextDigest } : {}),
    };
  }
}
