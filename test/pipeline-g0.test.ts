import { describe, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runPipeline, type PipelineDeps } from '../src/pipeline.js';
import { RunLogger } from '../src/logger.js';
import { evaluateReadiness } from '../src/validation/readiness-gate.js';
import type { RunConfig, TicketSpec } from '../src/types.js';

const readyTicket: TicketSpec = {
  id: 'ENG-1',
  title: 'Add GET /health endpoint',
  description: 'Endpoint returns service status as JSON including uptime and version.',
  labels: [],
  acceptanceCriteria: ['returns 200', 'has integration test'],
};

const unreadyFixture: TicketSpec = {
  id: 'ENG-2',
  title: 'Fix it',
  description: '',
  labels: [],
  acceptanceCriteria: [],
};

const baseCfg = (over: Partial<RunConfig> = {}): RunConfig => ({
  ticketId: 'ENG-1',
  repoPath: '.',
  worker: 'claude-code',
  autoPr: false,
  interactive: true,
  maxLoops: 3,
  timeoutMs: 60_000,
  dryRun: false,
  ...over,
});

const okDeps = (over: Partial<PipelineDeps> = {}): PipelineDeps => ({
  fetchTicket: vi.fn().mockResolvedValue(readyTicket),
  runGateG3: vi.fn().mockReturnValue({ passed: true, findings: [] }),
  ...over,
});

const tmpLog = () => {
  const dir = mkdtempSync(join(tmpdir(), 'da-g0-'));
  return { log: new RunLogger(dir), dir };
};

/** Default-shape G0 gate over the real scorer, matching the buildDeps wiring. */
const realG0 = (
  t: TicketSpec,
  classification: Parameters<typeof evaluateReadiness>[0]['classification'],
) => evaluateReadiness({ ticket: t, classification });

describe('runPipeline G0 readiness gate', () => {
  it('dispatches the worker when G0 passes', async () => {
    const { log, dir } = tmpLog();
    try {
      const impl = vi.fn().mockResolvedValue({ ok: true, worker: 'claude-code' as const, attempts: 1 });
      const outcomes = await runPipeline(baseCfg(), okDeps({ runGateG0: realG0, implementStage: impl }), log);
      expect(impl).toHaveBeenCalledOnce();
      expect(outcomes.at(-1)).toMatchObject({ stage: 'publish' });
      expect(outcomes.filter((o) => o.stage === 'validate').length).toBeGreaterThanOrEqual(1);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('rejects an under-specified ticket before worker dispatch and reports failed', async () => {
    const { log, dir } = tmpLog();
    try {
      const impl = vi.fn();
      const postComment = vi.fn().mockResolvedValue(undefined);
      const outcomes = await runPipeline(
        baseCfg({ ticketId: 'ENG-2' }),
        okDeps({
          fetchTicket: vi.fn().mockResolvedValue({ ...unreadyFixture, trackerInternalId: 'uuid-2' }),
          runGateG0: realG0,
          postTicketComment: postComment,
          implementStage: impl,
        }),
        log,
      );
      expect(impl).not.toHaveBeenCalled();
      expect(outcomes.at(-1)).toMatchObject({ stage: 'failed', reason: /G0 readiness gate rejected/ });
      expect(outcomes.filter((o) => o.stage === 'implement')).toHaveLength(0);
      expect(postComment).toHaveBeenCalledOnce();
      expect(postComment.mock.calls[0]![1]).toContain('G0 readiness');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('posts the G0 findings detail to the tracker with score and threshold', async () => {
    const { log, dir } = tmpLog();
    try {
      const postComment = vi.fn().mockResolvedValue(undefined);
      await runPipeline(
        baseCfg({ ticketId: 'ENG-2' }),
        okDeps({
          fetchTicket: vi.fn().mockResolvedValue({ ...unreadyFixture, trackerInternalId: 'uuid-2' }),
          runGateG0: realG0,
          postTicketComment: postComment,
        }),
        log,
      );
      const body = postComment.mock.calls[0]![1] as string;
      expect(body).toMatch(/G0 readiness \d+\/100 \(threshold 60\)/);
      expect(body).toContain('G0-TITLE');
      expect(body).toContain('G0-DESCRIPTION');
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('continues without failing when the tracker comment post errors', async () => {
    const { log, dir } = tmpLog();
    try {
      const postComment = vi.fn().mockRejectedValue(new Error('Linear comment failed: HTTP 401'));
      const outcomes = await runPipeline(
        baseCfg({ ticketId: 'ENG-2' }),
        okDeps({
          fetchTicket: vi.fn().mockResolvedValue({ ...unreadyFixture, trackerInternalId: 'uuid-2' }),
          runGateG0: realG0,
          postTicketComment: postComment,
        }),
        log,
      );
      expect(outcomes.at(-1)).toMatchObject({ stage: 'failed', reason: /G0 readiness gate rejected/ });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('surfaces G0 in dry-run like checkSpec (plan-time quality gate; real dry-run deps omit the gate)', async () => {
    const { log, dir } = tmpLog();
    try {
      const g0 = vi.fn(realG0);
      const outcomes = await runPipeline(baseCfg({ dryRun: true }), okDeps({ runGateG0: g0 }), log);
      expect(g0).toHaveBeenCalledOnce();
      expect(outcomes[0]).toMatchObject({ stage: 'validate', passed: true });
      expect(outcomes[1]).toMatchObject({ stage: 'plan' });
      expect(outcomes).toHaveLength(2);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('treats a missing runGateG0 as no gate (back-compat: checkSpec still clarifies)', async () => {
    const { log, dir } = tmpLog();
    try {
      const impl = vi.fn();
      const outcomes = await runPipeline(
        baseCfg({ ticketId: 'ENG-2' }),
        okDeps({ fetchTicket: vi.fn().mockResolvedValue(unreadyFixture), implementStage: impl }),
        log,
      );
      // Unchanged pre-G0 behavior: checkSpec clarifies, worker never dispatched.
      expect(impl).not.toHaveBeenCalled();
      expect(outcomes.at(-1)).toMatchObject({ stage: 'clarify' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('skips honestly when the G0 gate returns skipped (unknown classification)', async () => {
    const { log, dir } = tmpLog();
    try {
      const impl = vi.fn().mockResolvedValue({ ok: true, worker: 'claude-code' as const, attempts: 1 });
      const outcomes = await runPipeline(
        baseCfg(),
        okDeps({
          runGateG0: () => evaluateReadiness({ ticket: readyTicket, classification: 'weird' as never }),
          implementStage: impl,
        }),
        log,
      );
      expect(impl).toHaveBeenCalledOnce();
      expect(outcomes.at(-1)).toMatchObject({ stage: 'publish' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
