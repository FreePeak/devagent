import { describe, expect, it, vi } from 'vitest';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { runPipeline, type PipelineDeps } from '../src/pipeline.js';
import { RunLogger } from '../src/logger.js';
import type { RunConfig, TicketSpec } from '../src/types.js';

const goodTicket: TicketSpec = {
  id: 'ENG-1',
  title: 'Add GET /health endpoint',
  description: 'Endpoint returns service status as JSON including uptime and version.',
  labels: [],
  acceptanceCriteria: ['returns 200'],
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
  fetchTicket: vi.fn().mockResolvedValue(goodTicket),
  runGateG3: vi.fn().mockReturnValue({ passed: true, findings: [] }),
  ...over,
});

const tmpLog = () => {
  const dir = mkdtempSync(join(tmpdir(), 'da-pipe-'));
  return { log: new RunLogger(dir), dir };
};

describe('runPipeline', () => {
  it('clarifies on insufficient spec and posts comment', async () => {
    const { log, dir } = tmpLog();
    try {
      const postTicketComment = vi.fn().mockResolvedValue(undefined);
      const postComment = postTicketComment;
      const outcomes = await runPipeline(
        baseCfg(),
        okDeps({
          postTicketComment,
          fetchTicket: vi.fn().mockResolvedValue({
            ...goodTicket,
            trackerInternalId: 'uuid-123',
            description: '',
            acceptanceCriteria: [],
          }),
        }),
        log,
      );
      expect(outcomes[0]).toMatchObject({ stage: 'clarify' });
      expect(postComment).toHaveBeenCalledOnce();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('stops after plan in dry-run', async () => {
    const { log, dir } = tmpLog();
    try {
      const outcomes = await runPipeline(baseCfg({ dryRun: true }), okDeps(), log);
      expect(outcomes).toHaveLength(1);
      expect(outcomes[0]).toMatchObject({ stage: 'plan' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('fails cleanly when no implementStage is configured', async () => {
    const { log, dir } = tmpLog();
    try {
      const outcomes = await runPipeline(baseCfg(), okDeps(), log);
      expect(outcomes.at(-1)).toMatchObject({ stage: 'failed' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('dispatches implementStage then validates', async () => {
    const { log, dir } = tmpLog();
    try {
      const impl = vi.fn().mockResolvedValue({ ok: true, worker: 'claude-code' as const, attempts: 2 });
      const g3 = vi.fn().mockReturnValue({ passed: true, findings: [], detail: 'ok' });
      const outcomes = await runPipeline(baseCfg({ autoPr: true }), okDeps({ implementStage: impl, runGateG3: g3 }), log);
      expect(outcases(outcomes)).toContain('implement');
      expect(impl).toHaveBeenCalledOnce();
      expect(outcomes.at(-1)).toMatchObject({ stage: 'publish', note: 'no publisher configured' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('publishes via publishStage in auto-pr mode', async () => {
    const { log, dir } = tmpLog();
    try {
      const publish = vi.fn().mockResolvedValue('https://github.com/o/r/pull/1');
      const outcomes = await runPipeline(
        baseCfg({ autoPr: true }),
        okDeps({
          implementStage: vi.fn().mockResolvedValue({ ok: true, worker: 'claude-code' as const, attempts: 1 }),
          publishStage: publish,
        }),
        log,
      );
      expect(publish).toHaveBeenCalledOnce();
      expect(outcomes.at(-1)).toMatchObject({ stage: 'publish', prUrl: 'https://github.com/o/r/pull/1' });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });

  it('fails when the gate blocks', async () => {
    const { log, dir } = tmpLog();
    try {
      const outcomes = await runPipeline(
        baseCfg(),
        okDeps({
          implementStage: vi.fn().mockResolvedValue({ ok: true, worker: 'opencode' as const, attempts: 1 }),
          runGateG3: vi.fn().mockReturnValue({ passed: false, findings: [{ ruleId: 'DA001' }] }),
        }),
        log,
      );
      expect(outcomes.at(-1)).toMatchObject({ stage: 'failed', reason: /migration static gate/ });
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});

function outcases(outcomes: Array<{ stage: string }>): string[] {
  return outcomes.map((o) => o.stage);
}
