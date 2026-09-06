import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, utimesSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  COMPACT_CONTEXT_MARKER,
  KG_CONTEXT_SUBHEADER,
  KNOWLEDGE_CONTEXT_DIR,
  KNOWLEDGE_CONTEXT_HEADER,
  buildKnowledgeContext,
  buildPlannerPrompt,
  buildRepairPrompt,
} from '../src/prompt.js';
import { buildScoutPrompt } from '../src/scout.js';
import { loadConfig } from '../src/config.js';
import type { DevAgentConfig } from '../src/config.js';
import { LESSONS_MAX_CHARS } from '../src/lessons/guard.js';
import { planFromTicket } from '../src/planner.js';

/**
 * FR-CTX-01..04 baseline slice: an always-on knowledge-context digest built
 * from `.devagent/context/*.md`, spliced into the scout/planner/repair prompts
 * at the existing `COMPACT_CONTEXT_MARKER` seam, ratchet-capped at the shared
 * `lessonsMaxChars` budget (oldest entries dropped whole, never split). The
 * KG tier is opt-in via config `context.kg` and never blocks: off or
 * unreachable degrades to the baseline-only digest.
 */

const dirs: string[] = [];

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-kctx-'));
  dirs.push(d);
  return d;
}

/** Write one baseline context file; `ageDays` backdates mtime for ratchet order. */
function writeContext(repo: string, name: string, content: string, ageDays = 0): string {
  const dir = join(repo, KNOWLEDGE_CONTEXT_DIR);
  mkdirSync(dir, { recursive: true });
  const p = join(dir, name);
  writeFileSync(p, content);
  if (ageDays > 0) {
    const t = new Date(Date.now() - ageDays * 86_400_000);
    utimesSync(p, t, t);
  }
  return p;
}

/** Character count of the digest body (everything under the section header). */
function bodyOf(section: string): string {
  return section.slice(KNOWLEDGE_CONTEXT_HEADER.length + 1);
}

afterEach(() => {
  while (dirs.length) {
    const d = dirs.pop();
    if (d) rmSync(d, { recursive: true, force: true });
  }
});

describe('knowledge digest assembly (FR-CTX-02)', () => {
  it('renders the baseline markdown from every .devagent/context/*.md file', () => {
    const repo = tmpRepo();
    writeContext(repo, 'a-arch.md', 'Postgres is the system of record.\n', 2);
    writeContext(repo, 'b-rails.md', 'Sidekiq retries are idempotent-only.\n', 1);
    const digest = buildKnowledgeContext(repo);
    expect(digest.startsWith(KNOWLEDGE_CONTEXT_HEADER)).toBe(true);
    expect(digest).toContain('Postgres is the system of record.');
    expect(digest).toContain('Sidekiq retries are idempotent-only.');
  });

  it('ignores non-markdown files in the context dir', () => {
    const repo = tmpRepo();
    mkdirSync(join(repo, KNOWLEDGE_CONTEXT_DIR), { recursive: true });
    writeFileSync(join(repo, KNOWLEDGE_CONTEXT_DIR, 'notes.txt'), 'not context\n');
    expect(buildKnowledgeContext(repo)).toBe('');
  });

  it('returns an empty string when the context dir is absent (degrades to noop)', () => {
    expect(buildKnowledgeContext(tmpRepo())).toBe('');
  });
});

describe('knowledge digest budget ratchet (FR-CTX-01)', () => {
  it('drops oldest entries whole until the block fits the budget', () => {
    const repo = tmpRepo();
    const oldLines = Array.from({ length: 4 }, (_, i) => `OLD-LINE-${i}-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`);
    writeContext(repo, 'old.md', `${oldLines.join('\n')}\n`, 2);
    writeContext(repo, 'new.md', 'NEW-LINE-KEPT\n', 1);
    const digest = buildKnowledgeContext(repo, { maxChars: 60 });
    const body = bodyOf(digest);
    expect(body.length).toBeLessThanOrEqual(60);
    expect(body).toContain('NEW-LINE-KEPT');
    expect(body).not.toContain('OLD-LINE');
    // Survivors are whole lines: nothing truncated mid-entry.
    for (const line of body.split('\n')) {
      expect(line.endsWith('a'.repeat(6)) || line === 'NEW-LINE-KEPT').toBe(true);
    }
  });

  it('defaults to the shared lessonsMaxChars budget (4000)', () => {
    const repo = tmpRepo();
    const oldLines = Array.from({ length: 200 }, (_, i) => `OLD-BULK-${i}-${'x'.repeat(60)}`);
    writeContext(repo, 'old.md', `${oldLines.join('\n')}\n`, 2);
    writeContext(repo, 'new.md', 'CTX-NEWEST-SENTINEL\n', 1);
    const digest = buildKnowledgeContext(repo);
    expect(bodyOf(digest).length).toBeLessThanOrEqual(LESSONS_MAX_CHARS);
    expect(digest).toContain('CTX-NEWEST-SENTINEL');
    expect(digest).not.toContain('OLD-BULK-0-');
  });

  it('keeps a single newest oversized line whole rather than splitting it', () => {
    const repo = tmpRepo();
    writeContext(repo, 'old.md', 'OLD-SHORT\n', 2);
    const long = `NEW-LONG-${'y'.repeat(100)}`;
    writeContext(repo, 'new.md', `${long}\n`, 1);
    const digest = buildKnowledgeContext(repo, { maxChars: 60 });
    expect(digest).toContain(long);
    expect(digest).not.toContain('OLD-SHORT');
  });
});

describe('KG layer is opt-in and never blocks (FR-CTX-03)', () => {
  it('off by default: the provider is never consulted', () => {
    const repo = tmpRepo();
    writeContext(repo, 'base.md', 'BASELINE-LINE\n');
    let consulted = false;
    const digest = buildKnowledgeContext(repo, {
      kgProvider: () => {
        consulted = true;
        return 'KG-LINE';
      },
    });
    expect(consulted).toBe(false);
    expect(digest).toContain('BASELINE-LINE');
    expect(digest).not.toContain(KG_CONTEXT_SUBHEADER);
  });

  it('unreachable provider degrades to baseline-only without throwing', () => {
    const repo = tmpRepo();
    writeContext(repo, 'base.md', 'BASELINE-LINE\n');
    const digest = buildKnowledgeContext(repo, {
      kg: 'leankg',
      kgProvider: () => {
        throw new Error('ECONNREFUSED leankg');
      },
    });
    expect(digest).toBe(buildKnowledgeContext(repo));
    expect(digest).toContain('BASELINE-LINE');
  });

  it('absent provider under kg=leankg still yields the baseline digest', () => {
    const repo = tmpRepo();
    writeContext(repo, 'base.md', 'BASELINE-LINE\n');
    expect(buildKnowledgeContext(repo, { kg: 'leankg' })).toContain('BASELINE-LINE');
  });

  it('leankg content layers under its sub-header within the shared budget', () => {
    const repo = tmpRepo();
    writeContext(repo, 'base.md', 'BASELINE-LINE\n');
    const digest = buildKnowledgeContext(repo, {
      kg: 'leankg',
      kgProvider: () => 'callers of extractScoutPayload: 3',
    });
    expect(digest).toContain('BASELINE-LINE');
    expect(digest).toContain(KG_CONTEXT_SUBHEADER);
    expect(digest).toContain('callers of extractScoutPayload: 3');
  });
});

describe('injection at COMPACT_CONTEXT_MARKER (FR-CTX-01)', () => {
  const plan = planFromTicket({
    id: 'CTX-1',
    title: 'Inject knowledge context',
    description: 'Digest .devagent/context into prompts.',
    labels: [],
    acceptanceCriteria: ['digest present'],
  });

  it('planner prompt carries the digest below the goal section', () => {
    const repo = tmpRepo();
    writeContext(repo, 'facts.md', 'CTX-PLANNER-SENTINEL\n');
    const prompt = buildPlannerPrompt('Ship the context digest', repo);
    expect(prompt).toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(prompt).toContain('CTX-PLANNER-SENTINEL');
    expect(prompt.indexOf(KNOWLEDGE_CONTEXT_HEADER)).toBeGreaterThan(prompt.indexOf('## Goal'));
  });

  it('planner marker slot merges prior trails and the knowledge digest', () => {
    const repo = tmpRepo();
    const trailDir = join(repo, '.selfbuild', 'trails', 'loop-ctx');
    mkdirSync(trailDir, { recursive: true });
    writeFileSync(join(trailDir, 'T1.jsonl'), '{"note":"TRAIL-SENTINEL"}\n');
    writeContext(repo, 'facts.md', 'CTX-PLANNER-SENTINEL\n');
    const prompt = buildPlannerPrompt('Both layers', repo, { loopId: 'loop-ctx', taskId: 'T1' });
    expect(prompt).toContain(COMPACT_CONTEXT_MARKER);
    expect(prompt).toContain('### Trail for T1');
    expect(prompt).toContain('TRAIL-SENTINEL');
    expect(prompt).toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(prompt).toContain('CTX-PLANNER-SENTINEL');
  });

  it('planner prompt is unchanged when no context exists', () => {
    const repo = tmpRepo();
    const prompt = buildPlannerPrompt('No context here', repo);
    expect(prompt).not.toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(prompt.endsWith(COMPACT_CONTEXT_MARKER)).toBe(true);
  });

  it('repair prompt carries the digest and stays byte-identical without it', () => {
    const repo = tmpRepo();
    writeContext(repo, 'facts.md', 'CTX-REPAIR-SENTINEL\n');
    const knowledge = buildKnowledgeContext(repo);
    const withDigest = buildRepairPrompt(plan, 2, 'test suite failed', 'lesson text', knowledge);
    expect(withDigest).toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(withDigest).toContain('CTX-REPAIR-SENTINEL');
    // Digest lands after the lessons section (fixed trailing offset).
    expect(withDigest.indexOf(KNOWLEDGE_CONTEXT_HEADER)).toBeGreaterThan(withDigest.indexOf('## Lessons from previous runs'));
    // Absence degrades to noop: the 4-arg shape is untouched.
    expect(buildRepairPrompt(plan, 2, 'test suite failed', 'lesson text', '')).toBe(
      buildRepairPrompt(plan, 2, 'test suite failed', 'lesson text'),
    );
  });

  it('scout prompt carries the digest and stays byte-identical without it', () => {
    const config: DevAgentConfig = { worker: 'opencode', maxLoops: 3, timeoutMinutes: 30 };
    const bare = tmpRepo();
    expect(buildScoutPrompt(bare, config)).not.toContain(KNOWLEDGE_CONTEXT_HEADER);

    const repo = tmpRepo();
    writeContext(repo, 'facts.md', 'CTX-SCOUT-SENTINEL\n');
    const prompt = buildScoutPrompt(repo, config);
    expect(prompt).toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(prompt).toContain('CTX-SCOUT-SENTINEL');
  });
});

describe('config context.kg (FR-CTX-03)', () => {
  it('defaults to off: context is unset without a config file', () => {
    const cfg = loadConfig(tmpRepo());
    expect(cfg.context?.kg).toBeUndefined();
  });

  it('parses context.kg from devagent.json', () => {
    const dir = tmpRepo();
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ context: { kg: 'leankg' } }));
    expect(loadConfig(dir).context?.kg).toBe('leankg');
  });

  it('rejects an unknown provider', () => {
    const dir = tmpRepo();
    writeFileSync(join(dir, 'devagent.json'), JSON.stringify({ context: { kg: 'neo4j' } }));
    expect(() => loadConfig(dir)).toThrow(/Invalid context\.kg/);
  });
});

describe('orchestrator-side only (FR-CTX-04)', () => {
  it('no worker adapter references the knowledge digest or the KG seam', () => {
    const workersDir = join(process.cwd(), 'src', 'workers');
    const offenders = readdirSync(workersDir)
      .filter((f) => f.endsWith('.ts'))
      .filter((f) => {
        const src = readFileSync(join(workersDir, f), 'utf8');
        return src.includes('buildKnowledgeContext') || src.includes('kgProvider') || src.includes('KNOWLEDGE_CONTEXT');
      });
    expect(offenders).toEqual([]);
  });
});
