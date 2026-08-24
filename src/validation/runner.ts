import { readdirSync, readFileSync } from 'node:fs';
import { join, basename } from 'node:path';
import type { Finding, GateResult, TicketClass } from '../types.js';
import { analyzeMigrations, type MigrationFile } from './migration-rules.js';

/**
 * Gate orchestration. Loop 1 implements G3 (static migration analysis) fully;
 * G1/G2/G4 are wired but require a live sandbox and are gated behind config.
 */

export interface GateContext {
  repoPath: string;
  classification: TicketClass;
  /** Directories to scan for migration files, relative to repoPath */
  migrationDirs?: string[];
}

const MIGRATION_DIR_DEFAULTS = ['migrations', 'db/migrations', 'prisma/migrations', 'drizzle'];
const MIGRATION_EXT = /\.(sql|up\.sql|down\.sql)$/i;

function collectMigrationFiles(repoPath: string, dirs: string[]): MigrationFile[] {
  const files: MigrationFile[] = [];
  for (const dir of dirs) {
    const abs = join(repoPath, dir);
    let entries: string[];
    try {
      entries = readdirSync(abs, { recursive: true }) as unknown as string[];
    } catch {
      continue; // dir not present in this repo
    }
    for (const rel of entries) {
      const full = join(abs, rel);
      if (!MIGRATION_EXT.test(basename(full))) continue;
      try {
        files.push({ path: rel, sql: readFileSync(full, 'utf8') });
      } catch {
        // unreadable file: skip, do not fail the gate on collection errors
      }
    }
  }
  return files;
}

/** Down-migration pairing check: an up file `NNN_name.up.sql` pairs with `NNN_name.down.sql`. */
export function findUnpairedUpMigrations(files: MigrationFile[]): string[] {
  const paths = new Set(files.map((f) => f.path));
  return files
    .filter((f) => /\.up\.sql$/i.test(f.path))
    .map((f) => f.path)
    .filter((up) => !paths.has(up.replace(/\.up\.sql$/i, '.down.sql')));
}

/** Gate G3: static migration analysis over the diff/repo's migration directories. */
export function runMigrationStaticGate(ctx: GateContext): GateResult {
  if (ctx.classification !== 'migration-required') {
    return { gate: 'G3-migration-static', passed: true, skipped: true, findings: [], detail: 'skipped: no migrations in this ticket' };
  }

  const dirs = ctx.migrationDirs ?? MIGRATION_DIR_DEFAULTS.filter((d) => existsInRepo(ctx.repoPath, d));
  const files = collectMigrationFiles(ctx.repoPath, dirs);

  if (files.length === 0) {
    return {
      gate: 'G3-migration-static',
      passed: false,
      findings: [],
      detail: 'classified as migration-required but no migration files found',
    };
  }

  const unpaired = findUnpairedUpMigrations(files);
  const findings: Finding[] = [
    ...analyzeMigrations(files),
    ...unpaired.map(
      (path): Finding => ({
        ruleId: 'DA006',
        severity: 'medium',
        message: 'up-migration has no matching down-migration',
        file: path,
      }),
    ),
  ];

  const blocking = findings.some((f) => f.severity === 'critical');
  return {
    gate: 'G3-migration-static',
    passed: !blocking,
    findings,
    detail: `${files.length} migration file(s) analyzed, ${findings.length} finding(s)`,
  };
}

function existsInRepo(repoPath: string, rel: string): boolean {
  try {
    readdirSync(join(repoPath, rel));
    return true;
  } catch {
    return false;
  }
}
