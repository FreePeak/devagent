import { describe, expect, it } from 'vitest';
import { execFileSync } from 'node:child_process';
import { buildAdjacentCategoryScanText, SCAN_CATEGORIES, FUNNEL_MISS_NOTE, ARCHITECTURE_GATE_HINT } from '../src/research/scan-text.js';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

const repoRoot = join(import.meta.dirname, '..');

describe('GRADIENT adjacent-category scan text (PRD Phase 4)', () => {
  it('declares all three adjacent categories', () => {
    expect([...SCAN_CATEGORIES]).toEqual(['sensors', 'MCP servers', 'harness tooling']);
  });

  it('renders deterministic text covering categories, funnel miss, and advisory gate hint', () => {
    const text = buildAdjacentCategoryScanText();
    expect(text).toBe(buildAdjacentCategoryScanText()); // canonical, no timestamps/randomness
    for (const category of SCAN_CATEGORIES) {
      expect(text).toContain(category);
    }
    expect(text).toContain(FUNNEL_MISS_NOTE);
    expect(text).toContain('sentrux');
    expect(text).toContain('2026-09-01');
    expect(text).toContain(ARCHITECTURE_GATE_HINT);
    // Q38: the scalar exit-code gate is out of scope — the text stays advisory.
    expect(text).toMatch(/advisory/i);
  });

  it('prints the canonical text via the scan-text subcommand (CLI smoke)', () => {
    const out = execFileSync('npx', ['tsx', 'src/cli.ts', 'scan-text'], {
      cwd: repoRoot,
      stdio: 'pipe',
      env: { PATH: process.env.PATH, HOME: process.env.HOME },
      timeout: 30_000,
    }).toString();
    // Machine-readable consumption: stdout is exactly the module text, so the
    // shell prompts can never drift from src/research/scan-text.ts.
    expect(out).toBe(buildAdjacentCategoryScanText() + '\n');
  });

  it('is consumed by the selfbuild loop via the subcommand, not a hand-copied string', () => {
    const script = readFileSync(join(repoRoot, 'scripts', 'selfbuild-loop.sh'), 'utf8');
    expect(script).toContain('${DEVAGENT[@]}" scan-text');
    // Both prompt consumers (RESEARCH_PROMPT and PO_PROMPT) interpolate the
    // captured variable on their own line — dropping either call site silently
    // hollows a prompt. Anchored to line-anchored sites: the capture and the
    // [ -n ... ] guard lines also mention the variable and must not count.
    expect(script.match(/^\$GRADIENT_SCAN_TEXT$/gm)?.length).toBe(2);
    // The hand-copy this guard exists for: the script must not embed the scan prose.
    expect(script).not.toContain('sentrux');
  });
});
