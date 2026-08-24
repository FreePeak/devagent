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

/** Detect the repo's test command from conventional files. */
export function detectTestCommand(repoPath: string): TestCommand | null {
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
