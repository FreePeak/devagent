import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  AGENTS_MD_FILE,
  AGENTS_MD_SUBHEADER,
  COMPACT_CONTEXT_MARKER,
  KNOWLEDGE_CONTEXT_HEADER,
  TRUST_FILE,
  buildKnowledgeContext,
  buildPlannerPrompt,
  isAgentsMdTrusted,
  loadAgentsMd,
  trustAgentsMd,
} from '../src/prompt.js';
import { loadConfig } from '../src/config.js';

/**
 * PRD §18 Q11: `.devagent/AGENTS.md` auto-load behind a one-time per-repo
 * trust confirm. Config `context.agentsMd` (`ask` default | `on` | `off`)
 * gates the loader; `ask` injects nothing until `devagent trust agents-md`
 * writes `.devagent/trust.json`. Trusted content rides the knowledge digest
 * through the existing `spliceCompactContext` seam to worker/planner prompts.
 */

const repoRoot = join(import.meta.dirname, '..');
const dirs: string[] = [];

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-agents-'));
  dirs.push(d);
  return d;
}

/** Write <repo>/.devagent/AGENTS.md. */
function writeAgentsMd(repo: string, content: string): void {
  const dir = join(repo, '.devagent');
  mkdirSync(dir, { recursive: true });
  writeFileSync(join(dir, 'AGENTS.md'), content);
}

afterEach(() => {
  while (dirs.length) {
    const d = dirs.pop();
    if (d) rmSync(d, { recursive: true, force: true });
  }
});

describe('loadAgentsMd trust gate (Q11)', () => {
  it('ask (the default) injects nothing until the repo is trusted', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-SENTINEL\n');
    expect(loadAgentsMd(repo)).toBe('');
    expect(loadAgentsMd(repo, 'ask')).toBe('');
    const digest = buildKnowledgeContext(repo);
    expect(digest).not.toContain('AGENTS-SENTINEL');
    expect(digest).not.toContain(AGENTS_MD_SUBHEADER);
  });

  it('ask + trust record: content lands under the AGENTS.md sub-header', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-SENTINEL\nUse pnpm, never npm.\n');
    trustAgentsMd(repo);
    expect(loadAgentsMd(repo)).toContain('AGENTS-SENTINEL');
    const digest = buildKnowledgeContext(repo);
    expect(digest.startsWith(KNOWLEDGE_CONTEXT_HEADER)).toBe(true);
    expect(digest).toContain(AGENTS_MD_SUBHEADER);
    expect(digest).toContain('Use pnpm, never npm.');
  });

  it('on bypasses the trust gate', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-ON-SENTINEL\n');
    expect(isAgentsMdTrusted(repo)).toBe(false);
    expect(buildKnowledgeContext(repo, { agentsMd: 'on' })).toContain('AGENTS-ON-SENTINEL');
  });

  it('off never loads, even when trusted', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-OFF-SENTINEL\n');
    trustAgentsMd(repo);
    expect(loadAgentsMd(repo, 'off')).toBe('');
    expect(buildKnowledgeContext(repo, { agentsMd: 'off' })).not.toContain('AGENTS-OFF-SENTINEL');
  });

  it('missing AGENTS.md degrades to noop in every mode', () => {
    const repo = tmpRepo();
    trustAgentsMd(repo);
    expect(loadAgentsMd(repo, 'ask')).toBe('');
    expect(loadAgentsMd(repo, 'on')).toBe('');
    expect(buildKnowledgeContext(repo, { agentsMd: 'on' })).toBe('');
  });
});

describe('trust record (.devagent/trust.json)', () => {
  it('trustAgentsMd writes agentsMd=true and preserves sibling keys', () => {
    const repo = tmpRepo();
    mkdirSync(join(repo, '.devagent'), { recursive: true });
    writeFileSync(join(repo, TRUST_FILE), JSON.stringify({ other: 'kept' }));
    const p = trustAgentsMd(repo);
    const rec = JSON.parse(readFileSync(p, 'utf8'));
    expect(rec.agentsMd).toBe(true);
    expect(rec.other).toBe('kept');
    expect(typeof rec.agentsMdTrustedAt).toBe('string');
    expect(isAgentsMdTrusted(repo)).toBe(true);
  });

  it('absent, malformed, or negative trust files read as untrusted', () => {
    const repo = tmpRepo();
    expect(isAgentsMdTrusted(repo)).toBe(false);
    mkdirSync(join(repo, '.devagent'), { recursive: true });
    writeFileSync(join(repo, TRUST_FILE), 'not json{{');
    expect(isAgentsMdTrusted(repo)).toBe(false);
    writeFileSync(join(repo, TRUST_FILE), JSON.stringify({ agentsMd: false }));
    expect(isAgentsMdTrusted(repo)).toBe(false);
  });
});

describe('spliceCompactContext seam (worker/planner injection)', () => {
  it('planner prompt carries trusted AGENTS.md content through the knowledge digest', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-PLANNER-SENTINEL\n');
    trustAgentsMd(repo);
    const prompt = buildPlannerPrompt('Ship Q11', repo);
    expect(prompt).toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(prompt).toContain(AGENTS_MD_SUBHEADER);
    expect(prompt).toContain('AGENTS-PLANNER-SENTINEL');
  });

  it('planner prompt is unchanged while ask is unapproved', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-UNTRUSTED-SENTINEL\n');
    const prompt = buildPlannerPrompt('Ship Q11', repo);
    expect(prompt).not.toContain('AGENTS-UNTRUSTED-SENTINEL');
    expect(prompt.endsWith(COMPACT_CONTEXT_MARKER)).toBe(true);
  });
});

describe('config context.agentsMd (Q11)', () => {
  it('defaults to unset: the loader applies ask semantics', () => {
    expect(loadConfig(tmpRepo()).context?.agentsMd).toBeUndefined();
  });

  it('parses context.agentsMd from devagent.json', () => {
    const dir = tmpRepo();
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ context: { agentsMd: 'on' } }));
    expect(loadConfig(dir).context?.agentsMd).toBe('on');
  });

  it('rejects an unknown mode', () => {
    const dir = tmpRepo();
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ context: { agentsMd: 'yes' } }));
    expect(() => loadConfig(dir)).toThrow(/Invalid context\.agentsMd/);
  });
});

describe('devagent trust agents-md CLI (Q11 smoke)', () => {
  function runTrust(repo: string): { status: number | null; out: string } {
    const r = spawnSync('npx', ['tsx', join(repoRoot, 'src/cli.ts'), 'trust', 'agents-md', '--repo', repo], {
      cwd: repoRoot,
      encoding: 'utf8',
      env: { PATH: process.env.PATH, HOME: process.env.HOME },
      timeout: 60_000,
    });
    return { status: r.status, out: `${r.stdout}${r.stderr}` };
  }

  it('exit 0: writes the trust record so ask-mode loading picks the file up', () => {
    const repo = tmpRepo();
    writeAgentsMd(repo, 'AGENTS-CLI-SENTINEL\n');
    const r = runTrust(repo);
    expect(r.status).toBe(0);
    expect(isAgentsMdTrusted(repo)).toBe(true);
    // Default (ask) loading now injects through the digest.
    expect(buildKnowledgeContext(repo)).toContain('AGENTS-CLI-SENTINEL');
  });

  it('trusting a repo without AGENTS.md still records the confirm and notes the absence', () => {
    const repo = tmpRepo();
    const r = runTrust(repo);
    expect(r.status).toBe(0);
    expect(r.out).toContain('not present');
    expect(isAgentsMdTrusted(repo)).toBe(true);
    expect(loadAgentsMd(repo, 'ask')).toBe('');
  });
});
