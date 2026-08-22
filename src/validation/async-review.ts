import { existsSync, readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import type { Finding } from '../types.js';

/**
 * Gate G4 (FR-VALID-03): heuristic async/concurrency review for
 * consumer-classified tickets. Regex-based — advisory by design; catches the
 * common Node hazards, not a substitute for type-aware linting.
 */

const SOURCE_EXT = /\.(ts|js|mts|mjs|tsx|jsx)$/i;
const SKIP_DIRS = ['node_modules', 'dist', '.git', '.devagent-worktrees'];

export interface AsyncSourceFile {
  path: string;
  content: string;
}

export function collectSourceFiles(repoPath: string, maxFiles = 200): AsyncSourceFile[] {
  const files: AsyncSourceFile[] = [];
  const walk = (dir: string): void => {
    if (files.length >= maxFiles) return;
    let entries: string[];
    try {
      entries = readdirSync(dir, { withFileTypes: true }).map((e) => e.name);
    } catch {
      return;
    }
    for (const name of entries) {
      if (SKIP_DIRS.includes(name)) continue;
      const full = join(dir, name);
      // only readdir; isDirectory would need withFileTypes plumbing — cheap enough to try/catch
      try {
        if (!SOURCE_EXT.test(name)) {
          walk(full);
          continue;
        }
        files.push({ path: full.slice(repoPath.length + 1), content: readFileSync(full, 'utf8') });
      } catch {
        walk(full); // directory
      }
    }
  };
  walk(repoPath);
  return files;
}

/** Analyze source text for concurrency hazards. Exported pure for tests. */
export function analyzeAsyncHazards(files: AsyncSourceFile[]): Finding[] {
  const findings: Finding[] = [];

  for (const file of files) {
    const lines = file.content.split('\n');
    lines.forEach((line, i) => {
      const lineNo = i + 1;
      const stripped = stripComment(line);

      // DA101 high: .then( chain without a .catch( anywhere on the same line/statement
      if (/\.then\s*\(/.test(stripped) && !/\.catch\s*\(/.test(stripped)) {
        findings.push({
          ruleId: 'DA101',
          severity: 'high',
          message: 'promise chain uses .then() without .catch(); rejections will be unhandled',
          file: file.path,
          line: lineNo,
        });
      }

      // DA102 medium: async callback passed to forEach (concurrent mutation hazard)
      if (/forEach\s*\(\s*(async\b|\([^)]*\)\s*=>)/.test(stripped) && /forEach\s*\(\s*async\b/.test(stripped)) {
        findings.push({
          ruleId: 'DA102',
          severity: 'medium',
          message: 'async callback in forEach(): iterations run concurrently; use for..of + await',
          file: file.path,
          line: lineNo,
        });
      }

      // DA103 high: setInterval without clearInterval in the same file
      if (/\bsetInterval\s*\(/.test(stripped)) {
        if (!/\bclearInterval\s*\(/.test(file.content)) {
          findings.push({
            ruleId: 'DA103',
            severity: 'high',
            message: 'setInterval() with no clearInterval() in this file; timer leaks across reloads',
            file: file.path,
            line: lineNo,
          });
        }
      }

      // DA104 high: void-cast call (fire-and-forget) swallows rejections
      if (/^\s*void\s+[A-Za-z_$][\w$]*\s*\(/.test(line)) {
        findings.push({
          ruleId: 'DA104',
          severity: 'high',
          message: 'void-cast call discards the promise; errors are silently swallowed',
          file: file.path,
          line: lineNo,
        });
      }
    });
  }

  return findings;
}

function stripComment(line: string): string {
  const idx = line.indexOf('//');
  return idx >= 0 ? line.slice(0, idx) : line;
}

/**
 * Files changed by this run: diff of the worktree branch against its
 * merge-base with the repo default branch, so pre-existing hazards in the
 * codebase never block a ticket.
 */
export async function collectChangedSourceFiles(
  repoPath: string,
  baseBranch: string,
  runGit: (args: string[]) => Promise<{ exitCode: number; stdout: string }>,
): Promise<AsyncSourceFile[]> {
  const mb = await runGit(['merge-base', baseBranch, 'HEAD']);
  if (mb.exitCode !== 0) return [];
  const diff = await runGit(['diff', '--name-only', mb.stdout.trim(), 'HEAD']);
  if (diff.exitCode !== 0) return [];
  const files: AsyncSourceFile[] = [];
  for (const rel of diff.stdout.split('\n').map((s) => s.trim()).filter(Boolean)) {
    if (!SOURCE_EXT.test(rel)) continue;
    try {
      files.push({ path: rel, content: readFileSync(join(repoPath, rel), 'utf8') });
    } catch {
      // deleted or unreadable: skip
    }
  }
  return files;
}
