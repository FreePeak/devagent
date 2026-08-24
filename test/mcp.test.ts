import { describe, expect, it } from 'vitest';
import { handleRpc, startMcpServer } from '../src/server/mcp.js';
import { PassThrough } from 'node:stream';

describe('handleRpc', () => {
  it('initializes with protocol version and server info', async () => {
    const res = await handleRpc({ jsonrpc: '2.0', id: 1, method: 'initialize' });
    expect(res).toMatchObject({
      id: 1,
      result: { protocolVersion: '2024-11-05', serverInfo: { name: 'devagent' } },
    });
  });

  it('lists all tools', async () => {
    const res = await handleRpc({ jsonrpc: '2.0', id: 2, method: 'tools/list' });
    const tools = (res as { result: { tools: { name: string }[] } }).result.tools;
    expect(tools.map((t) => t.name)).toEqual(['devagent_dispatch', 'devagent_status', 'devagent_log', 'devagent_board', 'devagent_ledger', 'devagent_answer']);
  });

  it('answers ping', async () => {
    expect(await handleRpc({ jsonrpc: '2.0', id: 3, method: 'ping' })).toEqual({ jsonrpc: '2.0', id: 3, result: {} });
  });

  it('returns null for notifications (no id)', async () => {
    expect(await handleRpc({ jsonrpc: '2.0', method: 'notifications/initialized' })).toBeNull();
  });

  it('wraps tool errors as isError results instead of throwing', async () => {
    const res = await handleRpc({
      jsonrpc: '2.0',
      id: 4,
      method: 'tools/call',
      params: { name: 'devagent_log', arguments: { runId: '../etc/passwd' } },
    });
    const r = (res as { result: { isError?: boolean; content: { text: string }[] } }).result;
    expect(r.isError).toBe(true);
    expect(r.content[0]!.text).toContain('invalid runId');
  });
});

describe('startMcpServer', () => {
  it('responds to newline-delimited JSON-RPC on a stream', async () => {
    const input = new PassThrough();
    const output = new PassThrough();
    startMcpServer(input, output);

    let buffer = '';
    output.on('data', (d) => (buffer += String(d)));
    input.write('{"jsonrpc":"2.0","id":1,"method":"initialize"}\n');
    input.write('not-json-at-all\n'); // ignored silently
    input.write('\n'); // empty line ignored

    await new Promise<void>((resolve) => {
      const check = () => {
        if (buffer.includes('"id":1')) resolve();
        else setTimeout(check, 10);
      };
      check();
    });
    expect(buffer).toContain('"protocolVersion"');
    expect(buffer.split('\n').filter(Boolean)).toHaveLength(1); // only the valid request answered
  });
});
