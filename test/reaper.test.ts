import { describe, expect, it } from 'vitest';
import { isDevagentWorkerCmd, ownAncestryPids, parseEtimeToMs } from '../src/resilience/reaper.js';

describe('parseEtimeToMs (macOS BSD ps regression: etimes column does not exist)', () => {
  it('parses plain seconds', () => {
    expect(parseEtimeToMs('45')).toBe(45_000);
  });

  it('parses mm:ss', () => {
    expect(parseEtimeToMs('05:30')).toBe(((5 * 60) + 30) * 1000);
  });

  it('parses hh:mm:ss', () => {
    expect(parseEtimeToMs('2:03:04')).toBe((((2 * 60) + 3) * 60 + 4) * 1000);
  });

  it('parses dd-hh:mm:ss', () => {
    expect(parseEtimeToMs('1-02:03:04')).toBe(((((1 * 24 + 2) * 60) + 3) * 60 + 4) * 1000);
  });

  it('returns 0 for malformed input instead of NaN', () => {
    expect(parseEtimeToMs('garbage')).toBe(0);
    expect(parseEtimeToMs('')).toBe(0);
  });
});

describe('ownAncestryPids (self-kill guard)', () => {
  it('includes this process and terminates at init (pid 1)', () => {
    const pids = ownAncestryPids();
    expect(pids.has(process.pid)).toBe(true);
    for (const pid of pids) {
      expect(pid).toBeGreaterThan(1);
    }
    expect(pids.size).toBeGreaterThan(1);
  });

  it('never contains the reaper target pattern blindly — every pid is finite', () => {
    for (const pid of ownAncestryPids()) {
      expect(Number.isFinite(pid)).toBe(true);
    }
  });
});

describe('isDevagentWorkerCmd (worker signature gate, 2026-08-30 review: reaper blind to omp)', () => {
  const eligible = [
    'claude -p do the thing --output-format json',
    'omp -p do the thing --mode json',
    'omp -p do the thing --mode json --model omniroute/bai/glm-5.3-flash',
    'omp --mode json -c Continue',
    'omp --mode=json -p hi',
    // devagent's seatbelt wrapper prefixes `omp` after `sandbox-exec -f profile.sb`.
    '/usr/bin/sandbox-exec -f /tmp/devagent-sb-x/worker.sb omp -p hi --mode json',
  ];
  it.each(eligible)('matches devagent headless argv: %s', (cmd) => {
    expect(isDevagentWorkerCmd(cmd)).toBe(true);
  });

  const ineligible = [
    // Bare interactive omp TUI — the live user case (4 sessions, ttys001/003/004/007).
    'omp',
    'omp --thinking high',
    // claude in non-devagent shapes still rejected.
    'claude',
    'claude -p do the thing', // no --output-format
    'claude --output-format json', // no -p/--print
    // A user's prompt merely containing "omp --mode json" must not match.
    'node dist/cli.js --prompt "explain omp --mode json" --output-format json',
    // Substring trap: a binary like "compiler" must not match.
    '/usr/bin/compiler --output-format json -p hi',
  ];
  it.each(ineligible)('rejects non-devagent argv: %s', (cmd) => {
    expect(isDevagentWorkerCmd(cmd)).toBe(false);
  });
});
