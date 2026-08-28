import { describe, expect, it, vi, beforeEach } from 'vitest';
import { mkdtempSync, rmSync, existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { buildScoutPrompt, parseScoutOutput, extractScoutPayload, runScoutOnce, readHeartbeat } from '../src/scout.js';
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

describe('extractScoutPayload', () => {
  const payload = 'preamble text\n---TASK---\nid: FEAT-9\ntitle: T\ngoal: Goal: g\ncriteria: c\n---PRD---\n# T\n## Goal\nx\n';

  it('extracts from claude single-line JSON array (the loop-55 unparseable-fallback bug)', () => {
    // claude -p --output-format json emits ONE line: an array of message objects,
    // with the answer in the final {type:"result"} element. The old line-splitting
    // parser JSON.parsed the whole array as one value, obj.result was undefined
    // (arrays have no .result), and scout fell back to the stub task every cycle.
    const raw = JSON.stringify([
      { type: 'system', subtype: 'init' },
      { type: 'assistant', message: { role: 'assistant' } },
      { type: 'result', result: payload },
    ]);
    expect(extractScoutPayload(raw, 'claude-code')).toBe(payload);
  });

  it('extracts from claude single-object JSON with .result', () => {
    expect(extractScoutPayload(JSON.stringify({ type: 'result', result: payload }), 'claude-code')).toBe(payload);
  });

  it('extracts from opencode NDJSON lines with .text', () => {
    const ndjson = `{"type":"step_start"}\n{"type":"text","text":${JSON.stringify(payload)}}\n`;
    expect(extractScoutPayload(ndjson, 'opencode')).toBe(payload);
  });

  it('returns null when no markers anywhere', () => {
    expect(extractScoutPayload('{"type":"result","result":"no markers here"}', 'claude-code')).toBeNull();
    expect(extractScoutPayload('[{"type":"result","result":"nope"}]', 'claude-code')).toBeNull();
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

describe('scout single-instance lock', () => {
  it('acquires when free, refuses while holder alive, releases cleanly', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-scoutlock-'));
    try {
      const { acquireScoutLock, releaseScoutLock } = await import('../src/scout.js');
      expect(acquireScoutLock(dir)).toBe(true);
      expect(acquireScoutLock(dir)).toBe(false);
      releaseScoutLock(dir);
      expect(acquireScoutLock(dir)).toBe(true);
      releaseScoutLock(dir);

      // corrupt lock file behaves as stale
      mkdirSync(join(dir, '.devagent'), { recursive: true });
      writeFileSync(join(dir, '.devagent', 'scout.lock'), 'not json');
      expect(acquireScoutLock(dir)).toBe(true);
      releaseScoutLock(dir);

      // dead foreign pid is treated as stale
      writeFileSync(join(dir, '.devagent', 'scout.lock'), JSON.stringify({ pid: 99999999, at: Date.now() }));
      expect(acquireScoutLock(dir)).toBe(true);
      releaseScoutLock(dir);
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });

  it('runScoutLoop exits immediately without cycling when the lock is held', async () => {
    const dir = mkdtempSync(join(tmpdir(), 'da-scoutlock2-'));
    try {
      const { runScoutLoop } = await import('../src/scout.js');
      mkdirSync(join(dir, '.devagent'), { recursive: true });
      writeFileSync(join(dir, '.devagent', 'scout.lock'), JSON.stringify({ pid: process.pid, at: Date.now() }));
      let cycles = 0;
      await runScoutLoop({ repoPath: dir, intervalMinutes: 1 }, baseConfig, () => { cycles += 1; });
      expect(cycles).toBe(0);
      // holder's lock file is untouched by the refused loop
      const lock = JSON.parse(readFileSync(join(dir, '.devagent', 'scout.lock'), 'utf8')) as { pid: number };
      expect(lock.pid).toBe(process.pid);
    } finally { rmSync(dir, { recursive: true, force: true }); }
  });
});
