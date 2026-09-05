/**
 * Inline session attach (FR-TUI-06): hand the terminal to a live worker pane
 * without leaving the TUI. The herdr server owns the pane PTYs (FR-VIS); the
 * TUI is a renderer over the FR-CTRL API and must never touch PTYs itself
 * (§20.3 anti-pattern) — so attaching means *suspending* the dashboard and
 * execing `herdr --session <session> agent attach <paneId>` with inherited
 * stdio, exactly what `devagent attach <task> --exec` does (FR-VIS-02).
 *
 * Suspend = leave the alternate screen + show cursor + drain raw mode, so the
 * child owns a clean interactive terminal; keystrokes go to the worker, not
 * the dashboard. Resume = the caller re-enters the alternate screen, redraws
 * from scratch, and re-arms raw input. The resolved pane id is recorded in the
 * orchestration ledger (`operator-attached`) like the CLI path.
 */
import { spawn } from 'node:child_process';
import { appendOperatorAttachRecord } from '../orchestrator/ledger.js';
import { herdrBin, resolveSession } from '../integrations/herdr.js';

/** Hand the terminal to the pane until the operator detaches (herdr detach). */
export function suspendToShell(paneId: string, taskId: string, repoPath: string): Promise<number> {
  const session = resolveSession();
  appendOperatorAttachRecord(repoPath, {
    ts: new Date().toISOString(),
    kind: 'event',
    event: 'operator-attached',
    taskId,
    attempt: 1,
    paneId,
    session,
  });
  let resolveCode!: (code: number) => void;
  const done = new Promise<number>((resolve) => {
    resolveCode = resolve;
  });
  const child = spawn(herdrBin(), ['--session', session, 'agent', 'attach', paneId], { stdio: 'inherit' });
  child.on('close', (code) => resolveCode(code ?? 0));
  child.on('error', () => resolveCode(1));
  return done;
}
