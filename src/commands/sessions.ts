/**
 * Operator surface for visible worker sessions (PRD FR-VIS-02):
 * `devagent sessions` lists live worker panes in the herdr session and
 * `devagent attach <task>` prints (or runs) the jump-in command for one pane,
 * recording the attach in the orchestration ledger.
 */
import { spawn } from 'node:child_process';
import { attachCommandFor, herdrBin, listSessionPanes, resolveSession } from '../integrations/herdr.js';
import { appendOperatorAttachRecord } from '../orchestrator/ledger.js';

/** List live worker panes as a table (or raw JSON with --json). */
export async function runSessions(opts: { json?: boolean; repoPath?: string } = {}): Promise<void> {
  const panes = await listSessionPanes();
  if (opts.json) {
    console.log(JSON.stringify(panes, null, 2));
    return;
  }
  if (panes.length === 0) {
    console.log(`no worker panes in session "${resolveSession()}"`);
    return;
  }
  const rows = await Promise.all(
    panes.map(async (p) => {
      const attach = (await attachCommandFor(p.taskId)) ?? '-';
      return [p.paneId, p.label || p.worker, p.state, p.cwd || '-', attach] as string[];
    }),
  );
  const header = ['PANE', 'AGENT', 'STATUS', 'CWD', 'ATTACH'];
  const widths = header.map((h, col) => Math.max(h.length, ...rows.map((r) => r[col]?.length ?? 0)));
  console.log(header.map((h, col) => h.padEnd(widths[col] ?? 0)).join('  '));
  for (const r of rows) console.log(r.map((c, col) => c.padEnd(widths[col] ?? 0)).join('  '));
}

/**
 * Resolve the jump-in command for a task's pane, print it, record the attach
 * in the ledger, and with --exec run `herdr --session <s> agent attach <pane>`
 * with inherited stdio (exit code follows the child).
 */
export async function runAttach(taskId: string, opts: { exec?: boolean; repoPath?: string } = {}): Promise<void> {
  const repoPath = opts.repoPath ?? process.cwd();
  const session = resolveSession();
  const [panes, cmd] = await Promise.all([listSessionPanes(session), attachCommandFor(taskId, session)]);
  const pane = panes.find((p) => p.taskId === taskId);
  if (!cmd || !pane) {
    console.error(`no live pane found for task ${taskId} in session "${session}"`);
    process.exitCode = 1;
    return;
  }
  console.log(cmd);
  appendOperatorAttachRecord(repoPath, {
    ts: new Date().toISOString(),
    kind: 'event',
    event: 'operator-attached',
    taskId,
    attempt: 1,
    paneId: pane.paneId,
    session,
  });
  if (!opts.exec) return;
  const child = spawn(herdrBin(), ['--session', session, 'agent', 'attach', pane.paneId], { stdio: 'inherit' });
  const code = await new Promise<number | null>((resolve) => child.on('close', resolve));
  process.exitCode = code ?? 1;
}
