# Herdr runtime support

DevAgent can run its worker launches inside [herdr](https://github.com/herdrdev/herdr), a
persistent terminal workspace manager for coding agents. Instead of invisible child
processes, each worker launch becomes a pane in a dedicated named herdr session:

- Runs are visible live in the herdr TUI (`herdr session attach devagent`).
- The session survives terminal disconnects and machine reboots (persistent server).
- Completed/failed runs can be kept for inspection instead of vanishing with their process.

The feature is opt-in. Without it, workers run as plain child processes exactly as before.

## Enablement

Config (repo `devagent.json`):

```json
{
  "worker": "opencode",
  "herdr": { "enabled": true, "session": "devagent" }
}
```

Environment override (applies to every worker spawn site — executor, planner, auditor,
deps, fanout):

```bash
DEVAGENT_HERDR=1            # force on; =0 forces off
DEVAGENT_HERDR_SESSION=x    # target session name (default: "devagent")
```

Requires the `herdr` binary on PATH. If herdr is missing or unreachable, worker launches
silently fall back to direct execution after a single warning — the runtime is a
visibility enhancement, never a hard dependency.

## How it works

For each worker attempt, `src/integrations/herdr.ts`:

1. Ensures the named session's headless server is running
   (`herdr --session <name> server`, started detached if needed).
2. Creates a labeled workspace in that session.
3. Writes the computed child environment to a source-only env file (mode 0600) because
   panes inherit the server daemon's env, not DevAgent's; the pane script removes it
   immediately, so secrets never appear on the command line or in scrollback.
4. Runs the worker CLI via `pane run` with stdout/stderr redirected to temp files plus an
   exit-code marker file. Adapters keep parsing exact stdout JSON — no pty scraping.
5. Polls the files. Captured-output growth drives the no-progress watchdog; wall-clock
   timeout interrupts via ctrl+c and closes the workspace.

Retry/backoff semantics of both adapters are unchanged: a timed-out herdr run is treated
identically to a timed-out direct spawn.

## Pane lifecycle

- Default hygiene: the run's workspace is closed as soon as output is captured.
- `DEVAGENT_HERDR_KEEP_PANES=1`: completed workspaces stay open in the session so you can
  inspect transcripts after the fact.
- Timeout path always closes the workspace.

## Tests

`test/herdr.test.ts` exercises the full protocol against a functional stub CLI
(`DEVAGENT_HERDR_BIN` injects the binary): stdout capture, exit-code propagation,
env injection without leakage, timeout teardown, keep-panes mode, fallback behavior,
and config validation.
