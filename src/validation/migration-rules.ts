/**
 * G3 static migration-analysis rule engine.
 * Line-oriented heuristic SQL scanning: not a parser. String literals and
 * comments are masked before dangerous-pattern matching so that SQL keywords
 * appearing inside strings/comments never fire.
 */
import type { Finding, Severity } from '../types.js';

export interface MigrationFile {
  path: string;
  sql: string;
}

export interface AnalyzeOptions {
  /** Paths of down-migration files; used by DA006. */
  downMigrations?: string[];
  dialect?: 'postgres' | 'generic';
}

// ---------- masking helpers ----------

interface MaskedSql {
  /** Text with string literals and comments replaced by spaces (newlines kept). */
  masked: string;
  /** Map from masked-text offset to original line number (1-based). */
  lineOf: (offset: number) => number;
}

/** Matches comments and single-quoted literals ('' escape). Alternation order
 * ensures a `--` inside a literal stays inside the literal match and vice versa. */
const MASKABLE_RE = /--[^\n]*|\/\*[\s\S]*?(?:\*\/|$)|'(?:[^']|'')*(?:'|$)/g;

function maskSql(sql: string): MaskedSql {
  const masked = sql.replace(MASKABLE_RE, (tok) =>
    tok.replace(/[^\n]/g, ' ')
  );

  // prefix sum of newlines for offset -> line lookup
  const lineStarts: number[] = [0];
  for (let k = 0; k < masked.length; k++) {
    if (masked[k] === '\n') lineStarts.push(k + 1);
  }
  return {
    masked,
    lineOf(offset: number): number {
      let lo = 0;
      let hi = lineStarts.length - 1;
      while (lo < hi) {
        const mid = (lo + hi + 1) >> 1;
        if ((lineStarts[mid] ?? 0) <= offset) lo = mid;
        else hi = mid - 1;
      }
      return lo + 1;
    },
  };
}

/** Collapse whitespace so multi-line statements match single regexes. */
function normalize(masked: string): string {
  return masked.replace(/\s+/g, ' ');
}

function finding(
  ruleId: string,
  severity: Severity,
  file: string | undefined,
  line: number | undefined,
  message: string
): Finding {
  return { ruleId, severity, message, ...(file !== undefined ? { file } : {}), ...(line !== undefined ? { line } : {}) };
}

// ---------- per-file rules ----------

const DROP_RE =
  /\b(DROP\s+(TABLE|COLUMN|DATABASE|SCHEMA)|TRUNCATE\s+[^;]*CASCADE)\b/i;

function ruleDa001(file: MigrationFile, m: MaskedSql): Finding[] {
  const findings: Finding[] = [];
  for (const stmt of splitStatements(m.masked)) {
    const match = DROP_RE.exec(stmt.text);
    if (match) {
      findings.push(
        finding('DA001', 'critical', file.path, m.lineOf(stmt.start + (match.index ?? 0)), `Destructive operation detected (${match[0].trim()}): ${firstToken(stmt.text)}`)
      );
    }
  }
  return findings;
}

interface Statement {
  text: string; // whitespace-normalized statement text
  start: number; // offset in masked source where the statement begins
}

function splitStatements(masked: string): Statement[] {
  const out: Statement[] = [];
  let start = 0;
  for (;;) {
    const semi = masked.indexOf(';', start);
    const end = semi === -1 ? masked.length : semi;
    const lead = masked.slice(start, end).search(/\S/);
    const textStart = lead === -1 ? start : start + lead;
    const norm = normalize(masked.slice(textStart, end));
    if (norm.trim().length > 0) {
      out.push({ text: norm, start: textStart });
    }
    if (semi === -1) break;
    start = semi + 1;
  }
  return out;
}

function firstToken(stmt: string): string {
  const t = stmt.trim();
  return t.length > 60 ? t.slice(0, 57) + '...' : t;
}

function ruleDa002(file: MigrationFile, m: MaskedSql): Finding[] {
  const findings: Finding[] = [];
  for (const stmt of splitStatements(m.masked)) {
    const typeMatch = /ALTER\s+COLUMN\s+(\w+)\s+TYPE\s+([\w\s()[\],]+?)(?=\b(?:USING|NOT\s+NULL|DEFAULT|COLLATE)\b|[;,]|$)/i.exec(stmt.text);
    if (!typeMatch) continue;
    const column = typeMatch[1] ?? '';
    const targetType = (typeMatch[2] ?? '').trim().replace(/\s+/g, ' ');

    // explicit narrowing targets
    if (/^(smallint|tinyint)$/i.test(targetType)) {
      findings.push(finding('DA002', 'critical', file.path, lineOfType(m, stmt), `Column "${column}" narrowed to ${targetType}`));
      continue;
    }
    // any numeric type narrowed to smallint/tinyint handled above; also varchar shrink:
    const varMatch = /^varchar\s*\(\s*(\d+)\s*\)$/i.exec(targetType);
    if (!varMatch) continue;
    // look for a previous wider definition of this column in the same file set is out of scope;
    // here detect shrinking relative to an earlier ALTER or CREATE in the same file.
    // We only flag when a prior width is known and larger.
    const prior = priorVarcharWidths.get(column.toLowerCase());
    if (prior !== undefined && Number(varMatch[1]) < prior) {
      findings.push(finding('DA002', 'critical', file.path, lineOfType(m, stmt), `varchar(${varMatch[1]}) shrinks "${column}" from varchar(${prior})`));
    }
  }
  return findings;
}

function lineOfType(m: MaskedSql, stmt: Statement): number {
  return m.lineOf(stmt.start);
}

const priorVarcharWidths = new Map<string, number>();

function collectVarcharWidths(files: MigrationFile[], allMasked: Map<string, MaskedSql>): void {
  priorVarcharWidths.clear();
  for (const file of files) {
    const m = allMasked.get(file.path);
    if (!m) continue;
    for (const stmt of splitStatements(m.masked)) {
      // CREATE TABLE ... "col" varchar(n)
      const createRe = /\b(\w+)\s+varchar\s*\(\s*(\d+)\s*\)/gi;
      let cm: RegExpExecArray | null;
      while ((cm = createRe.exec(stmt.text)) !== null) {
        priorVarcharWidths.set((cm[1] ?? '').toLowerCase(), Number(cm[2] ?? '0'));
      }
    }
  }
}

function ruleDa003(file: MigrationFile, m: MaskedSql, opts: AnalyzeOptions): Finding[] {
  if (opts.dialect === 'generic') return [];
  if (!isPostgresPath(file.path)) return [];
  const findings: Finding[] = [];
  for (const stmt of splitStatements(m.masked)) {
    const match = /CREATE\s+(UNIQUE\s+)?INDEX\s+(?!CONCURRENTLY)/i.exec(stmt.text);
    if (match) {
      findings.push(finding('DA003', 'high', file.path, m.lineOf(stmt.start + (match.index ?? 0)), 'CREATE INDEX without CONCURRENTLY'));
    }
  }
  return findings;
}

function isPostgresPath(path: string): boolean {
  return /\.sql$/i.test(path) || /\.pg\./i.test(path) || /postgres|(^|\/)pg(\/|$)/i.test(path);
}

function ruleDa004(file: MigrationFile, m: MaskedSql, indexes: Set<string>): Finding[] {
  const findings: Finding[] = [];
  for (const stmt of splitStatements(m.masked)) {
    const re = /ADD\s+(CONSTRAINT\s+\w+\s+)?FOREIGN\s+KEY\s*\(\s*([\w",\s]+?)\s*\)\s*REFERENCES\s+([\w."`[\]]+)/gi;
    const alterTableMatch = /ALTER\s+TABLE\s+([\w."`[\]]+)/i.exec(stmt.text);
    const sourceTable = ((alterTableMatch?.[1] ?? '').replace(/["`[\]]/g, '')).split('.').pop() ?? '';
    let match: RegExpExecArray | null;
    while ((match = re.exec(stmt.text)) !== null) {
      const columns = (match[2] ?? '')
        .split(',')
        .map((c) => c.replace(/["`[\]]/g, '').trim())
        .filter(Boolean);
      const table = sourceTable;
      for (const col of columns) {
        if (!indexes.has(`${table.toLowerCase()}.${col.toLowerCase()}`)) {
          findings.push(
            finding(
              'DA004',
              'high',
              file.path,
              m.lineOf(stmt.start + (match.index ?? 0)),
              `Foreign key on "${table}.${col}" has no matching index`
            )
          );
        }
      }
    }
  }
  return findings;
}

function ruleDa005(file: MigrationFile, m: MaskedSql): Finding[] {
  const findings: Finding[] = [];
  for (const stmt of splitStatements(m.masked)) {
    const re = /ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:(\w+)\.)?(\w+)\s+([\w()]+)([^;]*)/gi;
    let match: RegExpExecArray | null;
    while ((match = re.exec(stmt.text)) !== null) {
      const rest = match[4] ?? '';
      if (/\bNOT\s+NULL\b/i.test(rest) && !/\bDEFAULT\b/i.test(rest)) {
        findings.push(
          finding('DA005', 'high', file.path, m.lineOf(stmt.start + (match.index ?? 0)), `ADD COLUMN "${match[2] ?? ''}" NOT NULL without DEFAULT`)
        );
      }
    }
  }
  return findings;
}

function ruleDa007(file: MigrationFile, m: MaskedSql): Finding[] {
  const findings: Finding[] = [];
  const lines = file.sql.split('\n');
  for (const stmt of splitStatements(m.masked)) {
    const match = /SET\s+NOT\s+NULL/i.exec(stmt.text);
    if (!match || !/ALTER\s+COLUMN\s+\w+/i.test(stmt.text)) continue;
    // backfill marker may appear as a comment anywhere before the statement in the file,
    // or immediately above it. Heuristic: marker present anywhere earlier in the file counts.
    const upToStatementEnd = m.lineOf(stmt.start);
    let sawMarker = false;
    for (let ln = 0; ln < Math.min(upToStatementEnd, lines.length); ln++) {
      if (/--\s*devagent:backfilled\b/i.test(lines[ln] ?? '')) {
        sawMarker = true;
        break;
      }
    }
    if (!sawMarker) {
      findings.push(finding('DA007', 'high', file.path, m.lineOf(stmt.start + (match.index ?? 0)), 'SET NOT NULL without preceding "-- devagent:backfilled" marker'));
    }
  }
  return findings;
}

function ruleDa008(file: MigrationFile, m: MaskedSql): Finding[] {
  const findings: Finding[] = [];
  const lines = m.masked.split('\n');
  let inTx = false;
  let txStartLine = 0;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i] ?? '';
    const upper = line.toUpperCase();
    if (/\bBEGIN\s*(TRANSACTION|WORK)?\s*;?\s*$/.test(upper) && !inTx) {
      inTx = true;
      txStartLine = i + 1;
    } else if (/\bCOMMIT\b/.test(upper) && inTx) {
      inTx = false;
    }
    if (inTx && /CREATE\s+INDEX\s+CONCURRENTLY/i.test(line)) {
      findings.push(finding('DA008', 'medium', file.path, i + 1, 'CREATE INDEX CONCURRENTLY inside BEGIN/COMMIT block (starting line ' + txStartLine + ')'));
    }
  }
  return findings;
}

// ---------- DA006 ----------

function ruleDa006(files: MigrationFile[], downMigrations: string[]): Finding[] {
  const findings: Finding[] = [];
  const base = (p: string) => p.replace(/\.[^.]+$/, '').toLowerCase();
  const downBases = new Set(downMigrations.map(base));
  for (const file of files) {
    if (/\.down\./i.test(file.path)) continue;
    const b = base(file.path);
    // matching-name convention: "<name>.down.sql" pairs with "<name>.up.sql"
    const expectedDown = b.replace(/\.up$/, '') + '.down';
    if (!downBases.has(expectedDown) && !downBases.has(b + '.down')) {
      findings.push(finding('DA006', 'medium', file.path, undefined, 'No corresponding down-migration found'));
    }
  }
  return findings;
}

// ---------- entry point ----------

export function analyzeMigrations(files: MigrationFile[], opts: AnalyzeOptions = {}): Finding[] {
  const findings: Finding[] = [];

  const maskedByPath = new Map<string, MaskedSql>();
  for (const file of files) {
    maskedByPath.set(file.path, maskSql(file.sql));
  }

  // cross-file index registry for DA004
  const indexes = new Set<string>();
  for (const file of files) {
    const m = maskedByPath.get(file.path);
    if (!m) continue;
    for (const stmt of splitStatements(m.masked)) {
      const idxRe = /CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:[\w."`[\]]+\s+ON\s+)?([\w."`[\]]+)\s*(?:USING\s+\w+\s*)?\(\s*([^)]+?)\s*\)/gi;
      let im: RegExpExecArray | null;
      while ((im = idxRe.exec(stmt.text)) !== null) {
        const table = (im[1] ?? '').replace(/["`[\]]/g, '').split('.').pop() ?? '';
        for (const col of (im[2] ?? '').split(',')) {
          const clean = col.replace(/\s+(ASC|DESC)\b.*$/i, '').replace(/["`[\]\s]/g, '').toLowerCase();
          if (clean) indexes.add(`${table.toLowerCase()}.${clean}`);
        }
      }
    }
  }

  collectVarcharWidths(files, maskedByPath);

  for (const file of files) {
    const m = maskedByPath.get(file.path);
    if (!m) continue;
    findings.push(...ruleDa001(file, m));
    findings.push(...ruleDa002(file, m));
    findings.push(...ruleDa003(file, m, opts));
    findings.push(...ruleDa004(file, m, indexes));
    findings.push(...ruleDa005(file, m));
    findings.push(...ruleDa007(file, m));
    findings.push(...ruleDa008(file, m));
  }

  if (opts.downMigrations) {
    findings.push(...ruleDa006(files, opts.downMigrations));
  }

  return findings;
}
