import { chmodSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, describe, expect, it } from 'vitest';
import {
  LEANKG_TIMEOUT_MS,
  createLeanKgProvider,
  queryLeanKg,
} from '../src/leankg.js';
import {
  KG_CONTEXT_SUBHEADER,
  KNOWLEDGE_CONTEXT_HEADER,
  buildKnowledgeContext,
  buildPlannerPrompt,
} from '../src/prompt.js';
import { buildScoutPrompt } from '../src/scout.js';
import { RunLogger } from '../src/logger.js';
import type { DevAgentConfig } from '../src/config.js';

/**
 * FR-CTX-05: the real LeanKG client behind the `kgProvider` seam. Every call
 * runs `leankg <query|graph-query> <text> --json` under a hard 1s wall-clock
 * budget; timeout / missing binary / non-zero exit omit the KG layer and
 * surface the degraded mode in the run log. LeanKG's `retrieval: {rung,
 * reason}` + `freshness` provenance is consumed verbatim in the digest and
 * run log — no query-fallback ladder inside DevAgent.
 */

const dirs: string[] = [];

function tmpDir(prefix: string): string {
  const d = mkdtempSync(join(tmpdir(), prefix));
  dirs.push(d);
  return d;
}

/** Write an executable `#!/bin/sh` stub and return its path. */
function stub(name: string, body: string): string {
  const p = join(tmpDir('da-leankg-stub-'), name);
  writeFileSync(p, `#!/bin/sh\n${body}\n`);
  chmodSync(p, 0o755);
  return p;
}

/** RunLogger writing into a throwaway home dir; `entries()` reads it back. */
function captureLogger(): { log: RunLogger; entries: () => Record<string, unknown>[] } {
  const log = new RunLogger(tmpDir('da-leankg-home-'));
  return {
    log,
    entries: () => {
      // The JSONL file is created on the first write; no lines logged = [].
      let raw = '';
      try {
        raw = readFileSync(log.path, 'utf8');
      } catch {
        return [];
      }
      return raw
        .split('\n')
        .filter(Boolean)
        .map((l) => JSON.parse(l) as Record<string, unknown>);
    },
  };
}

const GOOD_JSON =
  '{"answer":"callers of extractScoutPayload: 3","retrieval":{"rung":"L2","reason":"pg_trgm fuzzy match"},"freshness":"possibly_stale"}';

afterEach(() => {
  while (dirs.length) {
    const d = dirs.pop();
    if (d) rmSync(d, { recursive: true, force: true });
  }
});

describe('queryLeanKg (FR-CTX-05 client)', () => {
  it('renders the response body plus retrieval/freshness provenance verbatim', () => {
    const bin = stub('leankg-ok', `echo '${GOOD_JSON}'`);
    const res = queryLeanKg({ repoPath: tmpDir('da-leankg-repo-'), query: 'extractScoutPayload', bin, timeoutMs: 5_000 });
    expect(res.degraded).toBeUndefined();
    expect(res.content).toContain('callers of extractScoutPayload: 3');
    // Provenance consumed as-is: rung, reason, and freshness all present.
    expect(res.content).toContain('retrieval: L2 (pg_trgm fuzzy match)');
    expect(res.content).toContain('freshness: possibly_stale');
    expect(res.provenance).toEqual({ rung: 'L2', reason: 'pg_trgm fuzzy match', freshness: 'possibly_stale' });
  });

  it('invokes the single router verb with JSON output — one call, no ladder', () => {
    const dir = tmpDir('da-leankg-argv-');
    const callsLog = join(dir, 'calls.log');
    const bin = stub('leankg-argv', `echo "$1|$2|$3" >> '${callsLog}'\necho '${GOOD_JSON}'`);
    // Generous budget here: this test pins argv + single-call discipline; the
    // 1s budget itself is asserted by the hanging-stub test below (full-suite
    // parallelism can push a trivial stub spawn past 1s under load).
    const res = queryLeanKg({ repoPath: dir, query: 'payments worker', subcommand: 'graph-query', bin, timeoutMs: 5_000 });
    expect(res.degraded).toBeUndefined();
    const calls = readFileSync(callsLog, 'utf8').trim().split('\n');
    // Exactly ONE leankg invocation: DevAgent never re-queries on a degraded reply.
    expect(calls).toHaveLength(1);
    expect(calls[0]).toBe('graph-query|payments worker|--json');
  });

  it('hanging stub trips the 1s wall-clock budget and degrades', () => {
    const bin = stub('leankg-hang', 'sleep 30');
    const started = Date.now();
    const res = queryLeanKg({ repoPath: tmpDir('da-leankg-repo-'), query: 'hang', bin });
    const elapsed = Date.now() - started;
    expect(res.degraded?.reason).toBe('timeout');
    expect(res.content).toBe('');
    // Hard budget: killed at ~1s, never anywhere near the stub's 30s sleep.
    expect(elapsed).toBeGreaterThanOrEqual(LEANKG_TIMEOUT_MS - 100);
    expect(elapsed).toBeLessThan(5_000);
  });

  it('missing binary degrades with a distinct reason', () => {
    const res = queryLeanKg({
      repoPath: tmpDir('da-leankg-repo-'),
      query: 'q',
      bin: join(tmpDir('da-leankg-nope-'), 'definitely-not-leankg'),
    });
    expect(res.degraded?.reason).toBe('missing-binary');
    expect(res.content).toBe('');
  });

  it('non-zero exit degrades with the exit code and stderr excerpt', () => {
    const bin = stub('leankg-fail', 'echo "postgres unreachable" >&2\nexit 3');
    const res = queryLeanKg({ repoPath: tmpDir('da-leankg-repo-'), query: 'q', bin, timeoutMs: 5_000 });
    expect(res.degraded?.reason).toBe('exit');
    expect(res.degraded?.detail).toContain('exited 3');
    expect(res.degraded?.detail).toContain('postgres unreachable');
  });

  it('non-JSON output degrades as parse failure', () => {
    const bin = stub('leankg-junk', 'echo "not json at all"');
    const res = queryLeanKg({ repoPath: tmpDir('da-leankg-repo-'), query: 'q', bin, timeoutMs: 5_000 });
    expect(res.degraded?.reason).toBe('parse');
    expect(res.content).toBe('');
  });
});

describe('createLeanKgProvider run-log surfacing (FR-CTX-05)', () => {
  it('degraded call logs a warn line and yields empty provider content', () => {
    const { log, entries } = captureLogger();
    const bin = stub('leankg-hang', 'sleep 30');
    const provider = createLeanKgProvider({
      repoPath: tmpDir('da-leankg-repo-'),
      query: 'hang',
      bin,
      log,
      stage: 'implement',
    });
    expect(provider()).toBe('');
    const lines = entries();
    const degraded = lines.find((e) => e.level === 'warn');
    expect(degraded).toBeDefined();
    expect(String(degraded?.message)).toContain('kg=leankg degraded (timeout)');
    expect(String(degraded?.message)).toContain('KG layer omitted');
    expect(degraded?.stage).toBe('implement');
  });

  it('successful call logs the provenance verbatim in the run log', () => {
    const { log, entries } = captureLogger();
    const bin = stub('leankg-ok', `echo '${GOOD_JSON}'`);
    const provider = createLeanKgProvider({
      repoPath: tmpDir('da-leankg-repo-'),
      query: 'q',
      bin,
      log,
      stage: 'scout',
      timeoutMs: 5_000,
    });
    expect(provider()).toContain('callers of extractScoutPayload: 3');
    const info = entries().find((e) => e.level === 'info');
    expect(String(info?.message)).toContain('retrieval: L2 (pg_trgm fuzzy match)');
    expect(String(info?.message)).toContain('freshness: possibly_stale');
  });

  it('degraded provider leaves the digest baseline-only without throwing', () => {
    const repo = tmpDir('da-leankg-repo-');
    mkdirSync(join(repo, '.devagent', 'context'), { recursive: true });
    writeFileSync(join(repo, '.devagent', 'context', 'base.md'), 'BASELINE-LINE\n');
    const { log, entries } = captureLogger();
    const provider = createLeanKgProvider({
      repoPath: repo,
      query: 'q',
      bin: join(tmpDir('da-leankg-nope-'), 'missing-leankg'),
      log,
      stage: 'task',
    });
    const digest = buildKnowledgeContext(repo, { kg: 'leankg', kgProvider: provider });
    expect(digest).toContain('BASELINE-LINE');
    expect(digest).not.toContain(KG_CONTEXT_SUBHEADER);
    expect(entries().some((e) => e.level === 'warn' && String(e.message).includes('kg=leankg degraded (missing-binary)'))).toBe(true);
  });
});

describe('call-site wiring (FR-CTX-01/05, orchestrator-side only)', () => {
  const scoutConfig: DevAgentConfig = {
    worker: 'opencode',
    maxLoops: 3,
    timeoutMinutes: 30,
    context: { kg: 'leankg' },
  };

  it('scout prompt layers provider KG content under the digest', () => {
    const repo = tmpDir('da-leankg-repo-');
    mkdirSync(join(repo, '.devagent', 'context'), { recursive: true });
    writeFileSync(join(repo, '.devagent', 'context', 'base.md'), 'BASELINE-LINE\n');
    const prompt = buildScoutPrompt(repo, scoutConfig, { kgProvider: () => 'KG-SCOUT-LINE' });
    expect(prompt).toContain(KNOWLEDGE_CONTEXT_HEADER);
    expect(prompt).toContain('BASELINE-LINE');
    expect(prompt).toContain(KG_CONTEXT_SUBHEADER);
    expect(prompt).toContain('KG-SCOUT-LINE');
  });

  it('planner prompt layers provider KG content under the digest', () => {
    const repo = tmpDir('da-leankg-repo-');
    mkdirSync(join(repo, '.devagent', 'context'), { recursive: true });
    writeFileSync(join(repo, '.devagent', 'context', 'base.md'), 'BASELINE-LINE\n');
    const prompt = buildPlannerPrompt('Plan with KG', repo, {
      kg: 'leankg',
      kgProvider: () => 'KG-PLANNER-LINE',
    });
    expect(prompt).toContain(KG_CONTEXT_SUBHEADER);
    expect(prompt).toContain('KG-PLANNER-LINE');
  });

  it('kg=off never consults the client: no leankg call, no run-log line', () => {
    const dir = tmpDir('da-leankg-argv-');
    const callsLog = join(dir, 'calls.log');
    const bin = stub('leankg-count', `echo "$@" >> '${callsLog}'\necho '${GOOD_JSON}'`);
    const { log, entries } = captureLogger();
    const repo = tmpDir('da-leankg-repo-');
    mkdirSync(join(repo, '.devagent', 'context'), { recursive: true });
    writeFileSync(join(repo, '.devagent', 'context', 'base.md'), 'BASELINE-LINE\n');
    const prompt = buildScoutPrompt(repo, { ...scoutConfig, context: { kg: 'off' } }, {
      kgProvider: createLeanKgProvider({ repoPath: repo, query: 'q', bin, log }),
    });
    expect(prompt).toContain('BASELINE-LINE');
    expect(prompt).not.toContain(KG_CONTEXT_SUBHEADER);
    expect(entries()).toHaveLength(0);
    expect(() => readFileSync(callsLog, 'utf8')).toThrow();
  });
});
