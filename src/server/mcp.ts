import { createInterface } from 'node:readline';
import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';

/**
 * Minimal MCP (Model Context Protocol) stdio server exposing DevAgent as
 * tools for MCP-capable hosts (Orca, Claude Desktop, any MCP client).
 * Zero dependencies: JSON-RPC 2.0, one message per line on stdin/stdout.
 *
 *   devagent mcp
 *
 * Tools:
 *   devagent_dispatch  — run a prompt-driven task headlessly (task mode)
 *   devagent_status    — list recent runs from the runs directory
 *   devagent_log       — read one run's JSONL log tail
 *   devagent_board     — read the durable orchestration board (goal + tasks + statuses)
 */

interface JsonRpcRequest {
  jsonrpc: '2.0';
  id?: number | string | null;
  method: string;
  params?: Record<string, unknown>;
}

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

const TOOLS: ToolDef[] = [
  {
    name: 'devagent_dispatch',
    description:
      'Run a prompt-driven implementation task through the DevAgent pipeline in an isolated git worktree with a test gate. Returns the worktree path and result note.',
    inputSchema: {
      type: 'object',
      properties: {
        prompt: { type: 'string', description: 'Task description; first line becomes the title' },
        repoPath: { type: 'string', description: 'Absolute path to the target git repository' },
        autoPr: { type: 'boolean', description: 'Push branch and open a PR when tests pass (default false)' },
      },
      required: ['prompt', 'repoPath'],
    },
  },
  {
    name: 'devagent_status',
    description: 'List recent DevAgent runs (run id, timestamp, first event) from the local runs directory.',
    inputSchema: { type: 'object', properties: {}, additionalProperties: false },
  },
  {
    name: 'devagent_log',
    description: 'Read the last N entries of a run JSONL log.',
    inputSchema: {
      type: 'object',
      properties: {
        runId: { type: 'string' },
        tail: { type: 'number', description: 'Number of trailing entries (default 10)' },
      },
      required: ['runId'],
    },
  },
  {
    name: 'devagent_board',
    description:
      'Read the durable orchestration board (.devagent-project.json): goal, planner/executor roles, and every task with status, dependencies, attempts, and failure detail. Lets MCP hosts observe shared pipeline state.',
    inputSchema: {
      type: 'object',
      properties: {
        repoPath: { type: 'string', description: 'Absolute path to the git repository holding the board file' },
      },
      required: ['repoPath'],
    },
  },
];

function runsDir(): string {
  return process.env.DEVAGENT_HOME || join(process.env.HOME || '.', '.devagent');
}

function listRuns(): string {
  const dir = join(runsDir(), 'runs');
  if (!existsSync(dir)) return JSON.stringify({ runs: [] });
  const files = readdirSync(dir).filter((f) => f.endsWith('.jsonl')).sort().slice(-20);
  const runs = files.map((f) => {
    try {
      const first = readFileSync(join(dir, f), 'utf8').split('\n')[0] ?? '';
      return { runId: f.replace('.jsonl', ''), startedAt: safeTs(first) };
    } catch {
      return { runId: f.replace('.jsonl', ''), startedAt: null };
    }
  });
  return JSON.stringify({ runs });
}

function safeTs(firstLine: string): string | null {
  try {
    return (JSON.parse(firstLine) as { ts?: string }).ts ?? null;
  } catch {
    return null;
  }
}

function readRunLog(runId: string, tail: number): string {
  // Path safety: runId must be a bare filename component
  if (!/^[A-Za-z0-9_-]+$/.test(runId)) {
    throw new Error(`invalid runId: ${runId}`);
  }
  const file = join(runsDir(), 'runs', `${runId}.jsonl`);
  const lines = readFileSync(file, 'utf8').trim().split('\n');
  return lines.slice(-Math.max(1, tail)).join('\n');
}

async function callTool(name: string, args: Record<string, unknown>): Promise<string> {
  switch (name) {
    case 'devagent_status':
      return listRuns();
    case 'devagent_log':
      return readRunLog(String(args.runId), Number(args.tail) || 10);
    case 'devagent_board': {
      const { loadBoard } = await import('../orchestrator/store.js');
      const board = loadBoard(String(args.repoPath));
      if (!board) return JSON.stringify({ exists: false });
      const counts: Record<string, number> = {};
      for (const t of board.tasks) counts[t.status] = (counts[t.status] ?? 0) + 1;
      return JSON.stringify({
        exists: true,
        goal: board.goal,
        updatedAt: board.updatedAt,
        roles: board.roles,
        counts,
        tasks: board.tasks.map((t) => ({
          id: t.id,
          title: t.title,
          status: t.status,
          dependsOn: t.dependsOn,
          attempts: t.attempts,
          ...(t.audit
            ? {
                audit: {
                  verdict: t.audit.verdict,
                  integrity: t.audit.integrity,
                  unmetCriteria: t.audit.criteriaResults.filter((c) => !c.met).map((c) => c.criterion),
                },
              }
            : {}),
          ...(t.evidenceGaps?.length ? { evidenceGaps: t.evidenceGaps } : {}),
          ...(t.failureDetail ? { failureDetail: t.failureDetail } : {}),
        })),
      });
    }
    case 'devagent_dispatch': {
      const { spawnCli } = await import('../workers/spawn-utils.js');
      const argv = ['src/cli.ts', 'task', '--prompt', String(args.prompt), '--repo', String(args.repoPath)];
      if (args.autoPr) argv.push('--auto-pr');
      const r = await spawnCli('npx', ['tsx', ...argv], {
        cwd: '.',
        timeoutMs: 30 * 60_000,
      });
      return JSON.stringify({
        exitCode: r.exitCode,
        timedOut: r.timedOut,
        note: (r.stdout || r.stderr).trim().slice(-2000),
      });
    }
    default:
      throw new Error(`unknown tool: ${name}`);
  }
}

/** Handle one JSON-RPC request; returns null for notifications. */
export async function handleRpc(req: JsonRpcRequest): Promise<Record<string, unknown> | null> {
  if (req.method === 'initialize') {
    return {
      jsonrpc: '2.0',
      id: req.id,
      result: {
        protocolVersion: '2024-11-05',
        capabilities: { tools: {} },
        serverInfo: { name: 'devagent', version: '0.3.0' },
      },
    };
  }
  if (req.method === 'tools/list') {
    return { jsonrpc: '2.0', id: req.id, result: { tools: TOOLS } };
  }
  if (req.method === 'tools/call') {
    const name = String(req.params?.name);
    const args = (req.params?.arguments as Record<string, unknown>) ?? {};
    try {
      return {
        jsonrpc: '2.0',
        id: req.id,
        result: { content: [{ type: 'text', text: await callTool(name, args) }] },
      };
    } catch (err) {
      return {
        jsonrpc: '2.0',
        id: req.id,
        result: { content: [{ type: 'text', text: `error: ${(err as Error).message}` }], isError: true },
      };
    }
  }
  if (req.method === 'ping') {
    return { jsonrpc: '2.0', id: req.id, result: {} };
  }
  // Notifications (no id) and unknown methods: nothing to answer
  return null;
}

export function startMcpServer(
  input: NodeJS.ReadableStream = process.stdin,
  output: NodeJS.WritableStream = process.stdout,
): void {
  const rl = createInterface({ input });
  rl.on('line', (line) => {
    const trimmed = line.trim();
    if (!trimmed) return;
    let req: JsonRpcRequest;
    try {
      req = JSON.parse(trimmed) as JsonRpcRequest;
    } catch {
      return; // not JSON: ignore line
    }
    void handleRpc(req).then((res) => {
      if (res) output.write(`${JSON.stringify(res)}\n`);
    });
  });
}
