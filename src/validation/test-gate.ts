import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { GateResult } from '../types.js';
import { spawnCli } from '../workers/spawn-utils.js';

/**
 * Gate G1 (lightweight variant): run the target repo's own test suite in the
 * worktree. Docker-based sandbox execution arrives with loop 5; this variant
 * covers repos where tests are runnable directly (npm/go conventions).
 */

export interface TestCommand {
  cmd: string;
  args: string[];
}

const CONFIG_FILENAMES = ['devagent.json', '.devagent.json'];

/**
 * Detect the repo's test command: declarative devagent.json override first,
 * then conventional files (package.json, go.mod, pyproject.toml).
 */
export function detectTestCommand(repoPath: string): TestCommand | null {
  for (const name of CONFIG_FILENAMES) {
    const p = join(repoPath, name);
    if (existsSync(p)) {
      let testCommand: unknown;
      try {
        testCommand = JSON.parse(readFileSync(p, 'utf8'))?.testCommand;
      } catch {
        // malformed config JSON: fall through to convention detection
      }
      if (testCommand !== undefined) {
        if (typeof testCommand !== 'string' || testCommand.length === 0) {
          throw new Error(`Invalid testCommand "${String(testCommand)}" in ${p}: expected a non-empty string`);
        }
        const parts = testCommand.trim().split(/\s+/);
        return { cmd: parts[0]!, args: parts.slice(1) };
      }
      break;
    }
  }
  const pkg = join(repoPath, 'package.json');
  if (existsSync(pkg)) {
    try {
      const scripts = JSON.parse(readFileSync(pkg, 'utf8'))?.scripts;
      if (scripts && typeof scripts.test === 'string' && scripts.test.length > 0) {
        return { cmd: 'npm', args: ['test'] };
      }
    } catch {
      // malformed package.json: fall through to go detection
    }
  }
  if (existsSync(join(repoPath, 'go.mod'))) {
    return { cmd: 'go', args: ['test', './...'] };
  }
  if (existsSync(join(repoPath, 'pyproject.toml'))) {
    return { cmd: 'python3', args: ['-m', 'pytest'] };
  }
  return null;
}

export async function runTestGate(
  worktreePath: string,
  timeoutMs: number,
): Promise<GateResult> {
  const detected = detectTestCommand(worktreePath);
  if (!detected) {
    return {
      gate: 'G1-tests',
      passed: true,
      skipped: true,
      findings: [],
      detail: 'skipped: no runnable test command detected (Docker sandbox not yet configured)',
    };
  }

  const result = await spawnCli(detected.cmd, detected.args, { cwd: worktreePath, timeoutMs });
  const tail = result.stdout.split('\n').slice(-15).join('\n');
  return {
    gate: 'G1-tests',
    passed: !result.timedOut && result.exitCode === 0,
    findings: [],
    detail: `${detected.cmd} ${detected.args.join(' ')} -> exit ${result.exitCode}${result.timedOut ? ' (TIMED OUT)' : ''}\n${tail}`,
  };
}
