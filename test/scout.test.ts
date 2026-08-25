import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildScoutPrompt, parseScoutOutput, runScoutOnce, readHeartbeat } from '../src/scout.js';
import type { DevAgentConfig } from '../src/config.js';

const baseConfig: DevAgentConfig = { worker: 'opencode', maxLoops: 3, timeoutMinutes: 30 };

function tmpRepo(): string {
  const d = mkdtempSync(join(tmpdir(), 'da-scout-'));
  // minimal repo shape scout reads
  mkdirSync(join(d, 'docs'), { recursive: true });
  writeFileSync(join(d, 'docs', 'PRD.md'), '# PRD\n## 4 Competitive Landscape\nfoo\n## 17 Roadmap\nbar\n');
  mkdirSync(join(d, '.selfbuild'), { recursive: true });
  writeFileSync(join(d, '.selfbuild', 'ledger.jsonl'), '{"loop":1,"status":"ok"}\n');
  writeFileSync(join(d, '.selfbuild', 'lessons.md'), '# Lessons\n');
  return d;
}

describe('parseScoutOutput', () => {
  it('parses valid TASK+PRD markers', () => {
    const raw = `---TASK---\nid: FEAT-1\ntitle: Add foo bar endpoint\n\ngoal: Goal: Add GET /foo returning JSON\ncriteria: returns 200; validates schema\n---PRD---\n# Add foo\n\n## Goal\nAdd endpoint.\n`;
    const p = parseScoutOutput(raw);
    expect(p).not.toBeNull();
    expect(p!.id).toBe('FEAT-1');
    expect(p!.goal).toMatch(/^Goal:/);
    expect(p!.criteria).toEqual(['returns 200', 'validates schema']);
    expect(p!.prdMarkdown).toContain('# Add foo');
  });

  it('returns null on missing markers or missing Goal:', () => {
    expect(parseScoutOutput('no markers')).toBeNull();
    expect(parseScoutOutput('---TASK---\nid: X\ntitle: t\ngoal: not starting with keyword\n---PRD---\n# hi')).toBeNull();
  });

  it('returns null when PRD empty', () => {
    expect(parseScoutOutput('---TASK---\nid: X\ntitle: t\ngoal: Goal: x\n---PRD---\n')).toBeNull();
  });
});

describe('buildScoutPrompt', () => {
  it('includes queue depth and roadmap reference', () => {
    const repo = tmpRepo();
    try {
      const prompt = buildScoutPrompt(repo, baseConfig);
      expect(prompt).toContain('Queue depth');
      expect(prompt).toContain('---TASK---');
      expect(prompt).toContain('---PRD---');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});

describe('runScoutOnce dryRun', () => {
  it('dry-run enqueues a fallback task, writes PRD + queue + heartbeat', async () => {
    const repo = tmpRepo();
    try {
      const result = await runScoutOnce({ repoPath: repo, dryRun: true }, baseConfig);
      expect(result.ok).toBe(true);
      expect(result.taskId).toBeDefined();
      expect(existsSync(result.prdPath!)).toBe(true);
      expect(existsSync(result.queuePath!)).toBe(true);
      const hb = readHeartbeat(repo);
      expect(hb).not.toBeNull();
      expect(hb!.lastStatus).toBe('ok');
      expect(readFileSync(result.prdPath!, 'utf8')).toContain('## Goal');
      // second dry-run should still succeed (new random id)
      const r2 = await runScoutOnce({ repoPath: repo, dryRun: true }, baseConfig);
      expect(r2.ok).toBe(true);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('respects maxQueued guard and skips with skipped heartbeat', async () => {
    const repo = tmpRepo();
    try {
      const cfg: DevAgentConfig = { ...baseConfig, scout: { maxQueued: 1 } };
      const r1 = await runScoutOnce({ repoPath: repo, dryRun: true }, cfg);
      expect(r1.ok).toBe(true);
      const r2 = await runScoutOnce({ repoPath: repo, dryRun: true }, cfg);
      // r2 used non-dryRun path would skip, but dryRun path bypasses guard? Check: guard runs even in dryRun per implementation — it skips.
      // Our implementation checks guard before dryRun branch, so second dryRun also hits guard and returns skipped.
      expect(r2.detail).toMatch(/skipped/);
      expect(readHeartbeat(repo)!.lastStatus).toBe('skipped');
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });

  it('handles missing ledger/lessons gracefully', async () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-scout-2-'));
    try {
      mkdirSync(join(repo, 'docs'), { recursive: true });
      writeFileSync(join(repo, 'docs', 'PRD.md'), '# PRD');
      const result = await runScoutOnce({ repoPath: repo, dryRun: true }, baseConfig);
      expect(result.ok).toBe(true);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});

describe('runScoutOnce live fallback paths', () => {
  it('dry-run still succeeds after previous mocks reset', async () => {
    const repo = tmpRepo();
    try {
      const r = await runScoutOnce({ repoPath: repo, dryRun: true }, baseConfig);
      expect(r.ok).toBe(true);
      expect(r.taskId).toMatch(/^SCOUT-/);
    } finally { rmSync(repo, { recursive: true, force: true }); }
  });
});
