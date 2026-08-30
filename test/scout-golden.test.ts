import { describe, expect, it } from 'vitest';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { replayScoutFixtures } from '../src/scout.js';

// Golden fixture suite for extractScoutPayload: each entry in golden.json maps a
// captured worker output shape to the exact string extractScoutPayload must return
// (null for shapes it must reject). Covers claude array / single-line-array /
// object forms, opencode NDJSON, and null cases. Future Claude/OpenCode output
// format changes fail here (and via `devagent scout --replay`) instead of silently
// routing every scout cycle to the fallback task.
describe('scout golden fixtures', () => {
  const fixturesDir = join(import.meta.dirname, '..', 'src', 'scout', '__fixtures__');
  it('extracts the golden payload from each fixture exactly as recorded', () => {
    // Guard: every fixture on disk must have a golden entry, and vice versa, so a
    // new fixture without a recorded expectation fails loudly here.
    const golden = JSON.parse(readFileSync(join(fixturesDir, 'golden.json'), 'utf8')) as Record<string, { worker: string; expected: string | null }>;
    const onDisk = readdirSync(fixturesDir).filter((f) => f !== 'golden.json').sort();
    expect(onDisk).toEqual(Object.keys(golden).sort());
    for (const r of replayScoutFixtures(fixturesDir)) {
      expect(r.pass, `${r.name}: expected ${JSON.stringify(r.expected)}, got ${JSON.stringify(r.actual)}`).toBe(true);
    }
  });
});
