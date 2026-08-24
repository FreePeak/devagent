import { afterEach, describe, expect, it } from 'vitest';
import { PassThrough } from 'node:stream';
import { mkdtempSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { startMcpServer } from '../src/server/mcp.js';
import { appendAuditRecord, auditLedgerRecord } from '../src/orchestrator/ledger.js';

function rpc(serverInput: PassThrough, output: PassThrough, msg: Record<string, unknown>): Promise<Record<string, unknown>> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('MCP response timeout')), 5000);
    output.once('data', (chunk: Buffer) => {
      clearTimeout(timer);
      resolve(JSON.parse(chunk.toString()) as Record<string, unknown>);
    });
    serverInput.write(`${JSON.stringify(msg)}\n`);
  });
}

describe('devagent_ledger MCP tool', () => {
  const dirs: string[] = [];
  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  it('is advertised and returns persisted audit records', async () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-mcp-ledger-'));
    dirs.push(repo);
    appendAuditRecord(
      repo,
      auditLedgerRecord({ taskId: 'T1', attempt: 1, verdict: { verdict: 'fail', integrity: 'clean', criteriaResults: [{ criterion: 'b holds', met: false, evidence: 'none' }], summary: 'not done' }, ts: '2026-08-24T03:00:00Z' }),
    );
    appendAuditRecord(
      repo,
      auditLedgerRecord({ taskId: 'T1', attempt: 2, verdict: { verdict: 'pass', integrity: 'clean', criteriaResults: [{ criterion: 'b holds', met: true, evidence: 'test green' }], summary: 'verified' }, ts: '2026-08-24T03:05:00Z' }),
    );
    const input = new PassThrough();
    const output = new PassThrough();
    startMcpServer(input, output);

    const list = await rpc(input, output, { jsonrpc: '2.0', id: 1, method: 'tools/list' });
    expect((list.result as { tools: Array<{ name: string }> }).tools.map((t) => t.name)).toContain('devagent_ledger');

    const res = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 2,
      method: 'tools/call',
      params: { name: 'devagent_ledger', arguments: { repoPath: repo } },
    });
    const out = JSON.parse((res.result as { content: Array<{ text: string }> }).content[0].text) as {
      records: Array<{ taskId: string; attempt: number; verdict: string; unmetCriteria?: string[] }>;
      summary: { tasks: number; audits: number; resolved: number; meanAttemptsToPass: number | null };
    };
    expect(out.summary).toEqual({ tasks: 1, audits: 2, resolved: 1, meanAttemptsToPass: 2, unresolved: 0 });
    expect(out.records).toHaveLength(2);
    expect(out.records[0]).toMatchObject({ taskId: 'T1', attempt: 1, verdict: 'fail' });
    expect(out.records[0]!.unmetCriteria).toEqual(['b holds']);
    expect(out.records[1]).toMatchObject({ verdict: 'pass' });

    // task filter
    const none = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 3,
      method: 'tools/call',
      params: { name: 'devagent_ledger', arguments: { repoPath: repo, taskId: 'TX' } },
    });
    expect(JSON.parse((none.result as { content: Array<{ text: string }> }).content[0].text).records).toEqual([]);
  });

  it('returns empty records for repos without a ledger', async () => {
    const repo = mkdtempSync(join(tmpdir(), 'da-mcp-ledger-empty-'));
    dirs.push(repo);
    const input = new PassThrough();
    const output = new PassThrough();
    startMcpServer(input, output);
    const res = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 4,
      method: 'tools/call',
      params: { name: 'devagent_ledger', arguments: { repoPath: repo } },
    });
    const empty = JSON.parse((res.result as { content: Array<{ text: string }> }).content[0].text) as { records: unknown[] };
    expect(empty.records).toEqual([]);
  });
});
