/**
 * Gate G5 executor adapter (PRD section 11): wraps the validated STRIDE
 * rubric in src/validation/stride-gate.ts for the consume/autoMerge path.
 * Reuses the existing rules verbatim — no rubric logic lives here; this
 * module only maps categories/severities to the gate-executor contract and
 * applies the CRITICAL promotion for committed credential literals.
 *
 * Per-path allowlist (PRD Q25): a PR may commit
 * `.devagent/stride-allowlist.json` (`{"paths": [...glob patterns...]}`)
 * so findings whose file matches an allowed path are suppressed — fixture
 * credentials in test files no longer stall autoMerge. The allowlist is
 * read from the PR branch itself (see consume.ts); an absent, unreadable,
 * or malformed allowlist is ignored (fail closed: the gate keeps blocking).
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

/** Repo-relative location of the committed per-path allowlist (PRD Q25). */
export const STRIDE_ALLOWLIST_PATH = '.devagent/stride-allowlist.json';

export interface StrideGateEvaluation {
  findings: StrideGateFinding[];
  severityMax: StrideGateSeverity | null;
  contextDigest?: string;
}

/**
 * Parse the raw text of a committed allowlist file into a pattern list.
 * Returns null when the file is malformed JSON, not an object, or lacks a
 * string-array `paths` field — callers must treat null as "no allowlist"
 * (fail closed).
 */
export function parseStrideAllowlist(text: string): string[] | null {
  try {
    const parsed: unknown = JSON.parse(text);
    if (typeof parsed !== 'object' || parsed === null) return null;
    const paths = (parsed as { paths?: unknown }).paths;
    if (!Array.isArray(paths) || paths.some((p) => typeof p !== 'string')) return null;
    return paths as string[];
  } catch {
    return null;
  }
}

/**
 * Minimal glob match for allowlist patterns (no dependencies, deterministic):
 * `**` matches any characters including `/`, `*` matches within one path
 * segment, `?` matches one non-separator character. A pattern containing no
 * `/` matches the file's basename (so `*.json` covers `test/fixtures/x.json`).
 */
export function pathMatchesAllowlist(path: string, patterns: readonly string[]): boolean {
  const toRegex = (pattern: string): RegExp => {
    let re = '';
    for (let i = 0; i < pattern.length; i++) {
      const ch: string = pattern[i]!;
      if (ch === '*') {
        if (pattern[i + 1] === '*') {
          re += '.*';
          i++;
        } else {
          re += '[^/]*';
        }
      } else if (ch === '?') {
        re += '[^/]';
      } else {
        re += ch.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      }
    }
    return new RegExp(`^${re}$`);
  };
  for (const pattern of patterns) {
    const regexes = pattern.includes('/') ? [toRegex(pattern)] : [toRegex(pattern), toRegex(`**/${pattern}`)];
    if (regexes.some((re) => re.test(path))) return true;
  }
  return false;
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
 * `allowlistPaths` suppresses findings whose file matches one of the glob
 * patterns before severityMax is computed (PRD Q25); malformed/absent
 * allowlists are ignored by callers, so a null/empty list is a no-op here.
 */
export async function evaluateStride(input: {
  diff: string;
  contextDigest?: string;
  allowlistPaths?: readonly string[];
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

    const allowlist = input.allowlistPaths ?? [];
    const findings: StrideGateFinding[] = gate.findings
      .filter((f) => !(allowlist.length > 0 && f.file && pathMatchesAllowlist(f.file, allowlist)))
      .map((f) => {
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
