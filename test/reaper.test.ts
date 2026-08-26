import { describe, expect, it } from 'vitest';
import { parseEtimeToMs, ownAncestryPids } from '../src/resilience/reaper.js';

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
