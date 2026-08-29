/**
 * Gate G5 (STRIDE threat modeling): static, regex-based rubric over parsed
 * diff hunks. No LLM, no network, no new dependencies. Each rubric rule maps
 * a regex over a whole added (or, for one rule, removed) diff line to a
 * STRIDE category + severity; the first matching rule wins per line.
 */

export type StrideCategory = 'S' | 'T' | 'R' | 'I' | 'D' | 'E';

export type StrideSeverity = 'low' | 'medium' | 'high';

export interface StrideFinding {
  id: string;
  category: StrideCategory;
  severity: StrideSeverity;
  title: string;
  evidence: string;
  file: string;
  line?: number;
  recommendation: string;
}

/**
 * One hunk of a parsed unified diff. `added` is the newline-joined '+' lines
 * of the hunk; `removed` is the '-' lines. `line` is the 1-indexed new-file
 * line where the hunk starts, when known.
 */
export interface ParsedDiffHunk {
  file: string;
  line?: number;
  added: string;
  removed?: string;
}

export interface StrideGateResult {
  gate: 'G5-stride';
  passed: boolean;
  findings: StrideFinding[];
  severityMax: StrideSeverity | null;
  detail: string;
}

const STRIDE_NAMES: Record<StrideCategory, string> = {
  S: 'Spoofing',
  T: 'Tampering',
  R: 'Repudiation',
  I: 'Information disclosure',
  D: 'Denial of service',
  E: 'Elevation of privilege',
};

interface StrideRule {
  category: StrideCategory;
  severity: StrideSeverity;
  title: string;
  recommendation: string;
  pattern: RegExp;
  /** Match against removed lines instead of added lines (default: added). */
  onRemoved?: boolean;
}

/**
 * Static rubric, iterated in order; first match wins per line. Severity note:
 * console.log of request objects (req/req.body/req.params) is high, while
 * console.log of a sensitive *variable name* (password/token/secret) is
 * medium — the latter is advisory so it does not block the gate.
 */
const RUBRIC: readonly StrideRule[] = [
  // ---- Spoofing ----------------------------------------------------------
  {
    category: 'S',
    severity: 'high',
    title: 'Hardcoded API key or secret',
    recommendation: 'Load the credential from an environment variable instead of committing it.',
    pattern: /api[_-]?key\s*[:=]/i,
  },
  {
    category: 'S',
    severity: 'high',
    title: 'Hardcoded bearer token',
    recommendation: 'Read the token from config/env; never inline it in source.',
    pattern: /bearer\s+[A-Za-z0-9._-]{20,}/i,
  },
  {
    category: 'S',
    severity: 'high',
    title: 'Private key material committed',
    recommendation: 'Store key material outside the repository and reference it by path.',
    pattern: /-----BEGIN [A-Z ]*PRIVATE KEY-----/,
  },
  {
    category: 'S',
    severity: 'medium',
    title: 'Authentication check skipped or bypassed',
    recommendation: 'Keep auth middleware short-circuiting to an error, never an unconditional next().',
    pattern: /(?:skip|bypass)\s+auth|(?:auth|jwt|session)[^\n]{0,40}\bnext\s*\(\s*\)/i,
  },

  // ---- Tampering ---------------------------------------------------------
  {
    category: 'T',
    severity: 'high',
    title: 'SQL query built by string interpolation',
    recommendation: 'Use parameterized queries or an ORM binding instead of interpolating input.',
    pattern: /`?\s*(SELECT|INSERT|UPDATE|DELETE)\s+.*\$\{/i,
  },
  {
    category: 'T',
    severity: 'high',
    title: 'Command execution fed by request data',
    recommendation: 'Avoid shelling out with user input; use execFile with an argument array and validation.',
    pattern: /\b(?:exec|execSync|spawn(?:Sync)?)\s*\([^)]*(?:req\.|body|params)/i,
  },
  {
    category: 'T',
    severity: 'high',
    title: 'Unsafe deserialization into eval/Function',
    recommendation: 'Never pass parsed request data to eval or the Function constructor.',
    pattern: /\b(?:eval|new\s+Function)\s*\([^)]*(?:JSON\.parse|req\.|body)/i,
  },

  // ---- Repudiation -------------------------------------------------------
  {
    category: 'R',
    severity: 'medium',
    title: 'Audit or log call commented out',
    recommendation: 'Restore the audit/log call or route it through the project logger.',
    pattern: /^\s*(?:\/\/|\/\*).*\b(log|audit)\b/i,
  },

  // ---- Information disclosure -------------------------------------------
  {
    category: 'I',
    severity: 'high',
    title: 'Request data logged to console',
    recommendation: 'Drop the console.log or redact request payloads before logging.',
    pattern: /console\.log\s*\([^)]*\breq\b/i,
  },
  {
    category: 'I',
    severity: 'medium',
    title: 'Sensitive variable logged to console',
    recommendation: 'Remove the log or redact the sensitive value.',
    pattern: /console\.log\s*\([^)]*(?:password|passwd|token|secret)/i,
  },
  {
    category: 'I',
    severity: 'medium',
    title: 'Raw error stack or message returned to client',
    recommendation: 'Return a generic error to the client and log the details server-side.',
    pattern: /\bres\.(?:status|json|send)\s*\([^)]*err(?:or)?\.(?:stack|message)|err(?:or)?\.stack/i,
  },

  // ---- Denial of service -------------------------------------------------
  {
    category: 'D',
    severity: 'medium',
    title: 'Unbounded timer or infinite loop',
    recommendation: 'Bound the loop/timer with a clear exit condition or cancellation.',
    pattern: /\bsetInterval\s*\(|\bsetTimeout\s*\(|while\s*\(\s*true\s*\)/i,
  },
  {
    category: 'D',
    severity: 'medium',
    title: 'Blocking file read of a user-controlled path',
    recommendation: 'Validate/sandbox the path and prefer async fs APIs.',
    pattern: /\breadFileSync\s*\([^)]*(?:req\.|input|body|params)/i,
  },
  {
    category: 'D',
    severity: 'high',
    title: 'Outbound HTTP call without timeout',
    recommendation: 'Set an explicit timeout or wire an AbortController before awaiting the call.',
    pattern: /\b(?:fetch|axios(?:\.\w+)?|https?\.request|https?\.get)\s*\([^)]*(?:req\.|body|params|input)(?![^)]*(?:timeout|signal|AbortController))/i,
  },

  // ---- Elevation of privilege --------------------------------------------
  {
    category: 'E',
    severity: 'high',
    title: 'Process environment or global state assignment',
    recommendation: 'Keep configuration read-only at startup; never assign into globalThis from a handler.',
    pattern: /^\s*(?:globalThis\.\w+\s*=[^=]|Object\.assign\s*\(\s*global|process\.env\.\w+\s*=[^=])|\brequire\s*\(\s*["']fs["']\s*\)[^;\n]*(?:req\.|body|input|params)/i,
  },
  {
    category: 'E',
    severity: 'medium',
    title: 'Role or permission check removed',
    recommendation: 'Re-add the authorization check or document the delegated guard.',
    pattern: /\b(role|permission|isAdmin|authorize)\b/i,
    onRemoved: true,
  },
];

/** Severity ranking used to compute severityMax and the detail ordering. */
const SEVERITY_RANK: Record<StrideSeverity, number> = { low: 1, medium: 2, high: 3 };

const SEVERITY_TAG: Record<StrideSeverity, string> = { high: 'H', medium: 'M', low: 'L' };

/**
 * Deterministic markdown block for the string detail channel, sorted
 * severity-first (H before M before L), then by category letter.
 */
function formatDetail(passed: boolean, findings: StrideFinding[]): string {
  if (findings.length === 0) {
    return passed ? 'STRIDE pass: no diff to analyze' : 'STRIDE gate failed: no findings';
  }
  const lines = [...findings]
    .sort(
      (a, b) =>
        SEVERITY_RANK[b.severity] - SEVERITY_RANK[a.severity] ||
        a.category.localeCompare(b.category),
    )
    .map(
      (f) =>
        `- [${SEVERITY_TAG[f.severity]}] ${STRIDE_NAMES[f.category]} — ${f.file}${
          f.line !== undefined ? `:${f.line}` : ''
        } — ${f.title} (${f.evidence})`,
    );
  const header = passed ? 'STRIDE pass (advisory):\n## STRIDE G5 findings' : '## STRIDE G5 findings';
  return [header, ...lines].join('\n');
}

/**
 * Run the STRIDE rubric over parsed diff hunks. Async so it composes with the
 * existing awaited G1-G4 gate pipeline. `worktree` is accepted for future
 * file-content enrichment but unused in this revision. Null/undefined/empty
 * input is a pass with zero findings (first run may have no diff yet) — never
 * throws.
 */
export async function runStrideGate(
  parsed: ParsedDiffHunk[] | null | undefined,
  worktree: string,
): Promise<StrideGateResult> {
  void worktree;
  if (!parsed || parsed.length === 0) {
    return {
      gate: 'G5-stride',
      passed: true,
      findings: [],
      severityMax: null,
      detail: 'STRIDE pass: no diff to analyze',
    };
  }

  const findings: StrideFinding[] = [];
  let matchCounter = 0;
  for (const hunk of parsed) {
    // Iterate lines, then rules: the first matching rule wins per line, and
    // every matching line yields its own finding (duplicates never collapse).
    const addedLines = hunk.added ? hunk.added.split('\n') : [];
    const removedLines = hunk.removed ? hunk.removed.split('\n') : [];
    for (const line of addedLines) {
      const rule = RUBRIC.find((r) => !r.onRemoved && r.pattern.test(line));
      if (!rule) continue;
      matchCounter++;
      findings.push({
        id: `${rule.category}-${hunk.file}-${hunk.line ?? 0}-${matchCounter}`,
        category: rule.category,
        severity: rule.severity,
        title: rule.title,
        evidence: line.trim(),
        file: hunk.file,
        ...(hunk.line !== undefined ? { line: hunk.line } : {}),
        recommendation: rule.recommendation,
      });
    }
    for (const line of removedLines) {
      const rule = RUBRIC.find((r) => r.onRemoved && r.pattern.test(line));
      if (!rule) continue;
      matchCounter++;
      findings.push({
        id: `${rule.category}-${hunk.file}-${hunk.line ?? 0}-${matchCounter}`,
        category: rule.category,
        severity: rule.severity,
        title: rule.title,
        evidence: line.trim(),
        file: hunk.file,
        ...(hunk.line !== undefined ? { line: hunk.line } : {}),
        recommendation: rule.recommendation,
      });
    }
  }

  let severityMax: StrideSeverity | null = null;
  for (const f of findings) {
    if (severityMax === null || SEVERITY_RANK[f.severity] > SEVERITY_RANK[severityMax]) {
      severityMax = f.severity;
    }
  }

  const passed = severityMax !== 'high';
  return {
    gate: 'G5-stride',
    passed,
    findings,
    severityMax,
    detail: formatDetail(passed, findings),
  };
}

/**
 * Minimal unified-diff parser producing {@link ParsedDiffHunk}s. Tracks the
 * current file from `+++` headers and hunk starts from `@@ -a,b +c,d @@`.
 */
export function parseUnifiedDiff(diffText: string): ParsedDiffHunk[] {
  const hunks: ParsedDiffHunk[] = [];
  let file = '';
  let current: ParsedDiffHunk | null = null;

  for (const raw of diffText.split('\n')) {
    const fileMatch = /^\+\+\+ b\/(.*)$/.exec(raw);
    if (fileMatch && fileMatch[1]) {
      file = fileMatch[1];
      continue;
    }
    const hunkMatch = /^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@/.exec(raw);
    if (hunkMatch) {
      current = { file, line: Number(hunkMatch[1]), added: '', removed: '' };
      hunks.push(current);
      continue;
    }
    if (!current) continue;
    if (raw.startsWith('+')) current.added += `${raw.slice(1)}\n`;
    else if (raw.startsWith('-')) current.removed += `${raw.slice(1)}\n`;
  }

  return hunks;
}
