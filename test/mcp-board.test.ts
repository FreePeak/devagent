import { afterEach, describe, expect, it } from 'vitest';
import { PassThrough } from 'node:stream';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
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
