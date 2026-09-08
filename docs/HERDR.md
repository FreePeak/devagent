# Herdr runtime support

DevAgent can run its worker launches inside [herdr](https://github.com/herdrdev/herdr), a
persistent terminal workspace manager for coding agents. Instead of invisible child
processes, each worker launch becomes a pane in a dedicated named herdr session:

- Runs are visible live in the herdr TUI (`herdr session attach devagent`).
- The session survives terminal disconnects and machine reboots (persistent server).
- Completed/failed runs can be kept for inspection instead of vanishing with their process.

Since FR-VIS-01 (and the FR-HAND-05 init flow, #145) the feature is **default-on when
the `herdr` binary is present**: `devagent init` flips `herdr.enabled: true` in
`devagent.json` when herdr is found and the key is unset. Without it, workers run as
invisible child processes with a loud one-line advisory — never silently.

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
DEVAGENT_HERDR_SWEEP=0      # herdr-sweep master toggle (default on)
DEVAGENT_HERDR_SWEEP_ORPHANS=0  # orphan class; overrides a caller's --orphans either way
```

Requires the `herdr` binary on PATH. If herdr is missing or unreachable, worker launches
silently fall back to direct execution after a single warning — the runtime is a
visibility enhancement, never a hard dependency.

## How it works

For each worker attempt, `internal/workers/herdrruntime.go`:

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

### pane-run (FR-VIS-06)

`devagent pane-run --cwd <dir> --timeout <secs> --out <file> --err <file> --done <file> -- <cmd> [args...]`
runs one command inside a new pane of the devagent session and captures
stdout/stderr/exit code to files (the same contract the loop driver's direct
dispatch uses). The loop's research and PO phases dispatch through it, so
every agent role is operator-visible by default — not just coding workers.
Exit code 3 means the herdr pane runtime was unavailable; callers fall back
to their own direct dispatch so visibility never becomes a hard dependency.

### Sweep safety (FR-VIS-07, FR-VIS-10)

`devagent herdr-sweep` closes only panes that satisfy both per-pane guards:

1. **Automation ownership**: pane cwd sits inside `.devagent-worktrees/` —
   operator scratch panes in the same session are never listed, let alone
   closed.
2. **No live dispatch**: `pane process-info` shows the foreground process; a
   pane running `omp`/`pi`/`claude`/`opencode` is mid-run and skipped. This
   guard exists because a busy pane can still report `agent_status: idle`
   (the pane wrapper polls the done-marker, not the agent state machine) —
   the 2026-09-05 in-flight-close regression.

The session name alone is not a safety property (PRD §18 Q23), so two
operator-side bounds sit on top of the per-pane checks:

3. **Operator-attach exemption**: a pane the FR-VIS-02 roster reports as live
   (`state: running`) is never closed, and neither is anything in the session
   while `DEVAGENT_OPERATOR_ATTACHED` is set in the sweep's environment — an
   operator is at the wheel. Spared panes are still reported, with
   `reason=operator-attached` (CLI line prefix `[spared]`), so a dry-run
   explains why a pane survived. The exemption outranks the `--orphans` class.

4. **Managed deny toggle** (`devagent.json`):

```json
{
  "herdr": {
    "enabled": true,
    "session": "devagent",
    "sweep": { "enabled": true, "orphans": false, "denySessions": ["personal"] }
  }
}
```

   `sweep.enabled=false` (or `DEVAGENT_HERDR_SWEEP=0`) stops the sweep before it
   lists a single pane; `sweep.denySessions` names sessions it must never touch
   even when they are the resolved target. `sweep.orphans` is the orphan class
   default and, like the master toggle, env wins over config — so
   `DEVAGENT_HERDR_SWEEP_ORPHANS=0` reins in a loop driver that always passes
   `--orphans` without editing the script. Unset, every default is today's
   behavior: sweep on, orphan class only when the caller asks for it. An invalid
   `herdr.sweep` block fails closed — the sweep refuses to run rather than
   sweep with an unknown deny list.

`devagent herdr-sweep --dry-run` prints the same report without closing
anything, and states the bound when the sweep is disabled or denied.

### Orphaned worker-helper class (`--orphan-brokers`)

The per-pane guards above cannot see a different leak class: omp's
`__omp_worker_daemon_broker` worker outliving its session. When an omp process
dies without taking its broker down, the broker reparents to launchd (ppid 1)
and keeps an `lsp_mux` and a `gopls serve` fleet alive underneath it — four such
brokers (etime 13 h to 8 days) pinned ~1.3 GiB in the 2026-09-08 memory-pressure
diagnosis, entirely invisible to a pane-scoped sweep.

`devagent herdr-sweep --orphan-brokers` reaps them. Detection is process-scoped
and evidence-only: the same bounded `ps` ppid walk `--orphans` uses must return a
length-1 ancestry (direct child of launchd) whose command still names
`__omp_worker_daemon_broker`. A live session's broker always has its `omp` (and
the pane's shell / herdr server) above it, so it never matches — verified against
four live brokers while reaping a real orphan. `pgrep`/`ps` failure yields no
candidates, and without the flag the class is inert.

**Why the loop driver does not pass it:** a broker's children die or reparent
with it, and a `devagent daemon` started from inside that session tree is one of
those children — killing the orphan took the daemon down on 2026-09-08. This
class is operator-invoked (or scripted with a `:7788/status` check afterwards),
not automatic.

## Tests

The `internal/herdr` and `internal/workers` tests exercise the full protocol
against a functional stub CLI (`DEVAGENT_HERDR_BIN` injects the binary): stdout
capture, exit-code propagation, env injection without leakage, timeout teardown,
keep-panes mode, fallback behavior, and config validation — plus the sweep guards
above (FR-VIS-07 per-pane checks, the FR-VIS-10 deny toggle, the operator-attach
exemption, `herdr.sweep` parsing/validation/env precedence). The orphaned-broker
class is tested against a real child process that traps SIGTERM, so the
SIGTERM→SIGKILL escalation is proven rather than stubbed.
