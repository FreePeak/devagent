/**
 * Pipeline visualization model (FR-UI-08, issue #181): fold the SSE event
 * stream (run-log JSONL + orchestration rows, no second event system) into a
 * per-scope DAG of the ticket flow  scout → plan → implement → gates G0–G5
 * → PR. Pure and DOM-free; tested in test/desktop-pipeline.test.ts.
 */
import type { EventLine } from "./protocol.js";

export const PIPELINE_STAGES = ["scout", "plan", "implement", "G0", "G1", "G2", "G3", "G4", "G5", "pr"] as const;
export type StageId = (typeof PIPELINE_STAGES)[number];

export type StageStatus = "pending" | "running" | "pass" | "fail" | "skip";

export interface StageState {
  id: StageId;
  status: StageStatus;
  firstTs?: string;
  lastTs?: string;
  /** last - first event timestamp within the stage (0 when single event). */
  elapsedMs: number;
  /** Re-entries after a failure (retry counts, FR-UI-08). */
  retries: number;
  /** Newest event message for the stage (gate detail, worker name, PR url). */
  detail?: string;
}

export interface PipelineModel {
  /**
   * Scope key: taskId when events carry one, else the runId, else "" for
   * unscoped events (the newest non-empty scope is the "current run").
   */
  scope: string;
  stages: StageState[];
  /** Set by a `stage: 'failed'` event; carries the reason. */
  failedReason?: string;
  prUrl?: string;
}

/** Canonical stage for one event, or null when it maps to no DAG stage. */
function stageFor(line: EventLine): StageId | null {
  const stage = line.stage ?? line.event;
  if (!stage) return null;
  if (stage === "scout") return "scout";
  // 'clarify' is the G0 readiness/spec gate's own stage name in pipeline.ts.
  if (stage === "clarify") return "G0";
  if (stage === "plan") return "plan";
  if (stage === "implement") return "implement";
  if (stage === "publish") return "pr";
  if (stage === "validate") {
    // Gate events announce themselves: "G0 passed", "G2 failed: ...",
    // "G3 skipped". Unnumbered validate rows default to G1 (the test gate,
    // the validate stage's baseline in the flow).
    const m = /^G([0-5])\b/.exec(line.message);
    if (m) return `G${m[1]}` as StageId;
    return "G1";
  }
  return null;
}

function statusFor(line: EventLine, fallback: StageStatus): StageStatus {
  const text = line.message.toLowerCase();
  if (/\bskipped\b/.test(text)) return "skip";
  if (/\bfailed\b|\brejected\b/.test(text)) return "fail";
  if (/\bpassed\b|\baccepted\b/.test(text)) return "pass";
  return fallback;
}

function tsMs(ts?: string): number {
  if (!ts) return 0;
  const t = Date.parse(ts);
  return Number.isFinite(t) ? t : 0;
}

function emptyStage(id: StageId): StageState {
  return { id, status: "pending", elapsedMs: 0, retries: 0 };
}

/** A later stage starting proves earlier ones passed through. */
function markPriorStages(model: PipelineModel, reached: StageId): void {
  const reachedIdx = PIPELINE_STAGES.indexOf(reached);
  for (const s of model.stages) {
    const idx = PIPELINE_STAGES.indexOf(s.id);
    if (idx >= reachedIdx) break;
    if (s.status === "running" || s.status === "pending") s.status = "pass";
  }
}

/**
 * Fold events (chronological) into per-scope pipeline models. Progression
 * marks earlier stages pass; explicit pass/fail/skip wording in gate
 * messages wins; retries count re-entries after a failure or skip.
 */
export function buildPipelines(events: EventLine[]): PipelineModel[] {
  const byScope = new Map<string, PipelineModel>();
  for (const line of events) {
    const scope = line.taskId ?? line.runId ?? "";
    let model = byScope.get(scope);
    if (!model) {
      model = { scope, stages: PIPELINE_STAGES.map((id) => emptyStage(id)) };
      byScope.set(scope, model);
    }
    const id = stageFor(line);
    if (id) {
      const s = model.stages.find((x) => x.id === id)!;
      const wasFail = s.status === "fail";
      if (s.status === "fail" || s.status === "skip") s.retries += 1;
      // A publish event carrying the PR url is a pass by definition.
      const isPublishUrl = id === "pr" && /https:\/\//.test(line.message);
      s.status = isPublishUrl ? "pass" : statusFor(line, "running");
      if (!s.firstTs) s.firstTs = line.ts;
      s.lastTs = line.ts;
      s.elapsedMs = Math.max(0, tsMs(s.lastTs) - tsMs(s.firstTs));
      s.detail = line.message;
      // failedReason tracks the LATEST failing gate: a retry that passes
      // clears it, so the badge never shows a healed failure.
      if (s.status === "fail") model.failedReason = line.message;
      else if (wasFail) model.failedReason = undefined;
    }
    const pr = /https:\/\/[^\s"']+$/.exec(line.message);
    if (line.stage === "publish" && pr) model.prUrl = pr[0];
    if (id) markPriorStages(model, id);
  }
  return [...byScope.values()];
}

/**
 * Current stage badge per scope (FR-UI-08 roster badge): the last stage that
 * is not pending, or "pr" when the flow reached a published PR.
 */
export function stageBadge(model: PipelineModel | undefined): string {
  if (!model) return "";
  const active = [...model.stages].reverse().find((s) => s.status !== "pending");
  if (!active) return "";
  if (active.id === "pr" && active.status === "pass") return "pr";
  return active.id;
}
