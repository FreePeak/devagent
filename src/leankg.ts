import { execFileSync } from 'node:child_process';
import type { RunStage } from './types.js';

/**
 * FR-CTX-05: orchestrator-side LeanKG client behind the `kgProvider` seam
 * (src/prompt.ts). One call per digest build to the `leankg` CLI
 * (`query` / `graph-query`, JSON output) under a hard 1s wall-clock budget.
 * On timeout, missing binary, or non-zero exit the KG layer is omitted and
 * the degraded mode is surfaced in the run log — the pipeline never blocks
 * (FR-CTX-03). No query-fallback ladder lives here: LeanKG's single-tool
 * router degrades internally (L3 vectors → L2 fuzzy → L1 exact → L0 cold)
 * and stamps every response with `retrieval: {rung, reason}` +
 * `freshness`, which this client consumes verbatim in the digest and run
 * log. The client is orchestrator-side only and never reaches a worker
 * adapter (FR-CTX-04).
 */

/** Default binary name (the freepeak `leankg` server, never be-knowledge-graph). */
export const LEANKG_BIN = 'leankg';
/** Hard wall-clock budget for every KG client call (FR-CTX-05). */
export const LEANKG_TIMEOUT_MS = 1000;
/** CLI verbs the client speaks. One verb per call — no fallback ladder. */
export type KgSubcommand = 'query' | 'graph-query';

/** Why the KG layer was omitted from the digest. */
export type KgDegradedReason = 'timeout' | 'missing-binary' | 'exit' | 'parse' | 'error';

/** LeanKG's per-response provenance, consumed verbatim (never re-derived). */
export interface LeanKgProvenance {
  rung?: string;
  reason?: string;
  freshness?: string;
}

export interface LeanKgCallOptions {
  /** Repo the query runs against; passed as the spawn cwd. */
  repoPath: string;
  /** Natural-language query text handed to LeanKG's single-tool router. */
  query: string;
  /** Binary override (tests inject a stub script). Default `leankg`. */
  bin?: string;
  /** CLI verb. Default `query`. */
  subcommand?: KgSubcommand;
  /** Wall-clock budget in ms. Default 1000 (FR-CTX-05). */
  timeoutMs?: number;
}

export interface LeanKgResult {
  /** Rendered KG digest lines (content + provenance); '' when degraded. */
  content: string;
  /** Provenance extracted from the response, present on a parseable reply. */
  provenance?: LeanKgProvenance;
  /** Set whenever the KG layer must be omitted from the digest. */
  degraded?: { reason: KgDegradedReason; detail: string };
}

/** Minimal structural view of RunLogger (src/logger.ts) used for surfacing. */
export interface KgLogger {
  info(stage: RunStage, message: string, data?: Record<string, unknown>): void;
  warn(stage: RunStage, message: string, data?: Record<string, unknown>): void;
}

export interface LeanKgClientOptions extends LeanKgCallOptions {
  /** Run logger for the degraded / provenance lines. Optional: silent client. */
  log?: KgLogger;
  /** Stage label for emitted run-log lines. Default `implement`. */
  stage?: RunStage;
}

/** Render `retrieval: {rung, reason}` + `freshness` verbatim as one line. */
export function provenanceLine(p: LeanKgProvenance): string {
  const parts: string[] = [];
  if (p.rung !== undefined || p.reason !== undefined) {
    const reason = p.reason !== undefined ? ` (${p.reason})` : '';
    parts.push(`retrieval: ${p.rung ?? 'unknown'}${reason}`);
  }
  if (p.freshness !== undefined) parts.push(`freshness: ${p.freshness}`);
  return parts.join(' | ');
}

/** One digest line per result entry; unknown shapes stay verbatim JSON. */
function resultLine(e: unknown): string {
  if (typeof e === 'string') return e;
  if (e && typeof e === 'object') {
    const o = e as Record<string, unknown>;
    const label = ['name', 'title', 'file', 'id']
      .map((k) => (typeof o[k] === 'string' ? (o[k] as string) : undefined))
      .find(Boolean);
    const desc = ['description', 'summary', 'detail']
      .map((k) => (typeof o[k] === 'string' ? (o[k] as string) : undefined))
      .find(Boolean);
    if (label) return desc ? `${label}: ${desc}` : label;
  }
  return JSON.stringify(e);
}

/**
 * queryLeanKg — the single client call. Spawns
 * `leankg <subcommand> <query> --json` with a hard wall-clock budget
 * (SIGKILL on expiry), parses the JSON response, and renders the digest
 * lines with the response's own provenance stamp appended verbatim.
 * Never throws: every failure mode returns `content: ''` plus a `degraded`
 * descriptor the caller surfaces in the run log.
 */
export function queryLeanKg(opts: LeanKgCallOptions): LeanKgResult {
  const bin = opts.bin ?? LEANKG_BIN;
  const sub = opts.subcommand ?? 'query';
  const budgetMs = opts.timeoutMs ?? LEANKG_TIMEOUT_MS;

  let stdout: string;
  try {
    stdout = execFileSync(bin, [sub, opts.query, '--json'], {
      cwd: opts.repoPath,
      encoding: 'utf8',
      timeout: budgetMs,
      killSignal: 'SIGKILL',
      stdio: ['ignore', 'pipe', 'pipe'],
    });
  } catch (err) {
    const e = err as { code?: unknown; status?: unknown; stderr?: unknown; message?: unknown };
    if (e.code === 'ETIMEDOUT') {
      return { content: '', degraded: { reason: 'timeout', detail: `${bin} ${sub} exceeded ${budgetMs}ms budget (killed)` } };
    }
    if (e.code === 'ENOENT') {
      return { content: '', degraded: { reason: 'missing-binary', detail: `${bin} binary not found` } };
    }
    if (typeof e.status === 'number') {
      const stderr = typeof e.stderr === 'string' ? e.stderr.trim().slice(0, 200) : '';
      return {
        content: '',
        degraded: { reason: 'exit', detail: `${bin} ${sub} exited ${e.status}${stderr ? `: ${stderr}` : ''}` },
      };
    }
    return { content: '', degraded: { reason: 'error', detail: `${bin} ${sub} failed: ${String(e.message ?? 'unknown error')}` } };
  }

  const raw = (stdout ?? '').trim();
  if (!raw) return { content: '', degraded: { reason: 'parse', detail: `${bin} ${sub} returned empty output` } };

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return { content: '', degraded: { reason: 'parse', detail: `${bin} ${sub} response is not valid JSON` } };
  }
  const res = (parsed && typeof parsed === 'object' ? parsed : {}) as Record<string, unknown>;

  const prov: LeanKgProvenance = {};
  const retrieval = res.retrieval as Record<string, unknown> | undefined;
  if (retrieval && typeof retrieval === 'object') {
    if (typeof retrieval.rung === 'string' || typeof retrieval.rung === 'number') prov.rung = String(retrieval.rung);
    if (typeof retrieval.reason === 'string' || typeof retrieval.reason === 'number') prov.reason = String(retrieval.reason);
  }
  if (typeof res.freshness === 'string' || typeof res.freshness === 'number') prov.freshness = String(res.freshness);

  // Body: prefer the router's own text fields; fall back to the raw response.
  // This is shape consumption of ONE reply, not a re-query ladder.
  let body: string[];
  if (typeof res.answer === 'string' && res.answer.trim()) {
    body = res.answer.trim().split('\n').map((l) => l.trimEnd()).filter(Boolean);
  } else if (Array.isArray(res.results)) {
    body = (res.results as unknown[]).map(resultLine).filter((l) => l.trim());
  } else {
    body = raw.split('\n').map((l) => l.trimEnd()).filter(Boolean);
  }

  const stamp = provenanceLine(prov);
  const content = [...body, ...(stamp ? [stamp] : [])].join('\n');
  return { content, ...(Object.keys(prov).length ? { provenance: prov } : {}) };
}

/**
 * createLeanKgProvider — adapts the client to the synchronous `kgProvider`
 * seam in buildKnowledgeContext. Each provider invocation performs exactly
 * one leankg call (no fallback ladder) and surfaces the outcome in the run
 * log: a warn line with the degraded reason when the layer is omitted, an
 * info line carrying the response's `retrieval`/`freshness` provenance
 * verbatim when it is not. Never throws.
 */
export function createLeanKgProvider(opts: LeanKgClientOptions): LeanKgProvider {
  const stage: RunStage = opts.stage ?? 'implement';
  const provider: LeanKgProvider = () => {
    const res = queryLeanKg(opts);
    provider.last = res;
    if (res.degraded) {
      opts.log?.warn(
        stage,
        `kg=leankg degraded (${res.degraded.reason}): ${res.degraded.detail} — KG layer omitted from digest`,
        { kg: 'leankg', reason: res.degraded.reason },
      );
      return '';
    }
    const stamp = res.provenance ? provenanceLine(res.provenance) : '';
    opts.log?.info(
      stage,
      `kg=leankg digest attached${stamp ? `: ${stamp}` : ''}`,
      { kg: 'leankg', ...(res.provenance ?? {}) },
    );
    return res.content;
  };
  return provider;
}

/**
 * The `kgProvider` seam plus the last raw result. The extra property is
 * structurally invisible to `() => string` consumers, so every existing
 * digest site keeps compiling; the merge path reads `last` to capture the
 * provenance the run actually consumed instead of re-querying (PRD Q28).
 */
export type LeanKgProvider = (() => string) & { last?: LeanKgResult };

/** The one LeanKG freshness value that clears the stale-evidence gate. */
export const KG_FRESHNESS_FRESH = 'fresh';

/** Verbatim KG provenance excerpt of one digest build (PRD Q28). */
export interface KgEvidence {
  /** `retrieval: … | freshness: …`, rendered verbatim by `provenanceLine`. */
  excerpt: string;
  /** LeanKG's own freshness stamp, never re-derived. */
  freshness?: string;
}

/**
 * Capture the run's verbatim provenance excerpt from a provider that has
 * already been called by `buildKnowledgeContext`. Returns undefined when the
 * KG layer was absent, degraded, or answered without provenance — the caller
 * then persists nothing.
 */
export function captureKgEvidence(provider?: LeanKgProvider): KgEvidence | undefined {
  const prov = provider?.last?.provenance;
  if (!prov) return undefined;
  const excerpt = provenanceLine(prov);
  if (!excerpt) return undefined;
  return { excerpt, ...(prov.freshness !== undefined ? { freshness: prov.freshness } : {}) };
}

/**
 * FR-CTX-05 freshness gate: only a `fresh` stamp may persist across runs.
 * `stale`, `possibly_stale`, `cold`, and an absent stamp are all omitted so a
 * later digest never learns from evidence LeanKG itself distrusts.
 */
export function isFreshKgEvidence(evidence?: KgEvidence): evidence is KgEvidence & { freshness: string } {
  return evidence?.freshness === KG_FRESHNESS_FRESH;
}
