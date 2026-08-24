import { afterEach, describe, expect, it } from 'vitest';
import { PassThrough } from 'node:stream';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { startMcpServer } from '../src/server/mcp.js';

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

describe('devagent_board MCP tool', () => {
  const dirs: string[] = [];

  function tempRepo(): string {
    const dir = mkdtempSync(join(tmpdir(), 'da-mcp-board-'));
    dirs.push(dir);
    return dir;
  }

  afterEach(() => {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
    dirs.length = 0;
  });

  function start(): { input: PassThrough; output: PassThrough } {
    const input = new PassThrough();
    const output = new PassThrough();
    startMcpServer(input, output);
    return { input, output };
  }

  it('is advertised in tools/list', async () => {
    const { input, output } = start();
    const res = await rpc(input, output, { jsonrpc: '2.0', id: 1, method: 'tools/list' });
    const tools = (res.result as { tools: Array<{ name: string }> }).tools;
    expect(tools.map((t) => t.name)).toContain('devagent_board');
  });

  it('returns exists:false when no board file is present', async () => {
    const repo = tempRepo();
    const { input, output } = start();
    const res = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 2,
      method: 'tools/call',
      params: { name: 'devagent_board', arguments: { repoPath: repo } },
    });
    const text = (res.result as { content: Array<{ text: string }> }).content[0].text;
    expect(JSON.parse(text)).toEqual({ exists: false });
  });

  it('returns goal, counts and task list for an existing board', async () => {
    const repo = tempRepo();
    writeFileSync(
      join(repo, '.devagent-project.json'),
      JSON.stringify({
        goal: 'add export button',
        createdAt: '2026-08-24T00:00:00Z',
        updatedAt: '2026-08-24T01:00:00Z',
        roles: { planner: 'claude-code', executor: 'claude-code' },
        tasks: [
          { id: 'T1', title: 'schema', prompt: 'p', dependsOn: [], status: 'done', attempts: 1 },
          { id: 'T2', title: 'endpoint', prompt: 'p', dependsOn: ['T1'], status: 'ready', attempts: 0 },
          { id: 'T3', title: 'ui', prompt: 'p', dependsOn: ['T2'], status: 'blocked', attempts: 0 },
        ],
      }),
    );
    const { input, output } = start();
    const res = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 3,
      method: 'tools/call',
      params: { name: 'devagent_board', arguments: { repoPath: repo } },
    });
    const text = (res.result as { content: Array<{ text: string }> }).content[0].text;
    const board = JSON.parse(text) as {
      exists: boolean;
      goal: string;
      counts: Record<string, number>;
      tasks: Array<{ id: string; status: string; failureDetail?: string }>;
    };
    expect(board.exists).toBe(true);
    expect(board.goal).toBe('add export button');
    expect(board.counts).toEqual({ done: 1, ready: 1, blocked: 1 });
    expect(board.tasks.map((t) => t.id)).toEqual(['T1', 'T2', 'T3']);
  });

  it('surfaces audit verdicts and evidence gaps for audited tasks', async () => {
    const repo = tempRepo();
    writeFileSync(
      join(repo, '.devagent-project.json'),
      JSON.stringify({
        goal: 'audited work',
        createdAt: '2026-08-24T00:00:00Z',
        updatedAt: '2026-08-24T01:00:00Z',
        roles: { planner: 'claude-code', executor: 'claude-code', auditor: 'opencode' },
        tasks: [
          {
            id: 'T1',
            title: 'schema',
            prompt: 'p',
            dependsOn: [],
            status: 'done',
            attempts: 1,
            audit: {
              verdict: 'pass',
              integrity: 'clean',
              criteriaResults: [
                { criterion: 'table exists', met: true, evidence: 'psql \\dt shows it' },
                { criterion: 'index exists', met: true, evidence: '\\di shows idx' },
              ],
              summary: 'verified via psql',
            },
          },
          {
            id: 'T2',
            title: 'retry me',
            prompt: 'p',
            dependsOn: [],
            status: 'pending',
            attempts: 1,
            evidenceGaps: ['unmet: b holds — grep found nothing'],
          },
        ],
      }),
    );
    const { input, output } = start();
    const res = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 5,
      method: 'tools/call',
      params: { name: 'devagent_board', arguments: { repoPath: repo } },
    });
    const board = JSON.parse((res.result as { content: Array<{ text: string }> }).content[0].text) as {
      roles: { auditor?: string };
      tasks: Array<{ id: string; audit?: { verdict: string; integrity: string; unmetCriteria: string[] }; evidenceGaps?: string[] }>;
    };
    expect(board.roles.auditor).toBe('opencode');
    expect(board.tasks[0]!.audit).toEqual({ verdict: 'pass', integrity: 'clean', unmetCriteria: [] });
    expect(board.tasks[0]!.evidenceGaps).toBeUndefined();
    expect(board.tasks[1]!.audit).toBeUndefined();
    expect(board.tasks[1]!.evidenceGaps![0]).toContain('unmet: b holds');
  });

  it('lists pendingQuestions and answers them via devagent_answer', async () => {
    const repo = tempRepo();
    writeFileSync(
      join(repo, '.devagent-project.json'),
      JSON.stringify({
        goal: 'needs input',
        createdAt: '2026-08-24T00:00:00Z',
        updatedAt: '2026-08-24T01:00:00Z',
        roles: { planner: 'claude-code', executor: 'claude-code' },
        tasks: [
          {
            id: 'T1',
            title: 'blocked on human',
            prompt: 'p',
            dependsOn: [],
            status: 'ask',
            attempts: 1,
            failureDetail: 'needs human input: which DB should the migration target?',
          },
        ],
      }),
    );
    const { input, output } = start();

    const boardRes = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 6,
      method: 'tools/call',
      params: { name: 'devagent_board', arguments: { repoPath: repo } },
    });
    const before = JSON.parse((boardRes.result as { content: Array<{ text: string }> }).content[0].text) as {
      pendingQuestions: Array<{ taskId: string; title: string; question: string }>;
    };
    expect(before.pendingQuestions).toEqual([
      { taskId: 'T1', title: 'blocked on human', question: 'which DB should the migration target?' },
    ]);

    const ansRes = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 7,
      method: 'tools/call',
      params: { name: 'devagent_answer', arguments: { repoPath: repo, taskId: 'T1', answer: 'target the analytics replica' } },
    });
    const answered = JSON.parse((ansRes.result as { content: Array<{ text: string }> }).content[0].text) as { ok: boolean; note: string };
    expect(answered.ok).toBe(true);
    expect(answered.note).toContain('back in queue');

    // board reflects the requeue and the persisted answer context
    const after = JSON.parse(readFileSync(join(repo, '.devagent-project.json'), 'utf8')) as {
      tasks: Array<{ status: string; prompt: string }>;
    };
    // saveBoard recomputes readiness: no deps -> immediately ready again
    expect(after.tasks[0]!.status).toBe('ready');
    expect(after.tasks[0]!.prompt).toContain('analytics replica');
  });

  it('devagent_answer fails cleanly with no board or wrong task state', async () => {
    const empty = tempRepo();
    const { input, output } = start();
    const noBoard = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 8,
      method: 'tools/call',
      params: { name: 'devagent_answer', arguments: { repoPath: empty, taskId: 'T1', answer: 'yes' } },
    });
    expect(JSON.parse((noBoard.result as { content: Array<{ text: string }> }).content[0].text)).toEqual({
      ok: false,
      note: 'no project board for this repo',
    });

    const repo = tempRepo();
    writeFileSync(
      join(repo, '.devagent-project.json'),
      JSON.stringify({
        goal: 'g',
        createdAt: '2026-08-24T00:00:00Z',
        updatedAt: '2026-08-24T01:00:00Z',
        roles: { planner: 'claude-code', executor: 'claude-code' },
        tasks: [{ id: 'T1', title: 't', prompt: 'p', dependsOn: [], status: 'done', attempts: 1 }],
      }),
    );
    const notAsk = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 9,
      method: 'tools/call',
      params: { name: 'devagent_answer', arguments: { repoPath: repo, taskId: 'T1', answer: 'yes' } },
    });
    const r = JSON.parse((notAsk.result as { content: Array<{ text: string }> }).content[0].text) as { ok: boolean; note: string };
    expect(r.ok).toBe(false);
    expect(r.note).toContain("no task 'T1'");
  });

  it('reports tool error for a nonexistent repo path', async () => {
    const { input, output } = start();
    const res = await rpc(input, output, {
      jsonrpc: '2.0',
      id: 4,
      method: 'tools/call',
      params: { name: 'devagent_board', arguments: { repoPath: '/nonexistent/repo/path' } },
    });
    const result = res.result as { isError?: boolean; content: Array<{ text: string }> };
    // loadBoard tolerates missing dirs -> exists:false, or surfaces an error result; both are valid
    if (!result.isError) {
      expect(JSON.parse(result.content[0].text)).toEqual({ exists: false });
    } else {
      expect(result.content[0].text).toMatch(/error/i);
    }
  });

});
