# TUI dashboard (`devagent tui`)

Full-screen terminal dashboard over the FR-CTRL daemon API (PRD §20.8 FR-TUI).
Works over SSH on any macOS/Linux/Windows terminal; zero browser, zero desktop
app. Non-TTY stdin (pipes, CI) degrades to a one-shot snapshot on stdout and
exit 0 — the smoke-testable path.

```bash
devagent tui   # that's it — attaches to a daemon, or embeds one for the session
```

It is also the **use command of the 1+1 handoff path (FR-HAND, #145)**: after
`devagent init`, press `n` and state your goal in one sentence — the dispatch
sheet posts it to `POST /dispatch` and a worker card appears. No third
terminal, no flag maze.

## Daemon modes (one command, three behaviors)

The glances standalone/client/server pattern: one command covers all cases.

| Invocation | Behavior |
| --- | --- |
| `devagent tui` | Probe `127.0.0.1:7788` (`/healthz`): running daemon → **attach**; none → **embed** an ephemeral in-process daemon on a random port (fresh token, no conflicts) and stop it when the TUI quits. The title bar marks embedded sessions with `· daemon:embedded`. |
| `devagent tui --attach-only` | Never spawn: attach or show `DAEMON UNREACHABLE` (glances `-c` analog). Also implied by an explicit `--url`/`--token`/`--uds-path`. |
| `devagent daemon` (separate command) | Long-lived shared daemon for the 24/7 factory, webhooks, and multi-client access (glances `-s` analog) — unchanged. |

Notes:
- An embedded daemon dispatches tasks exactly like the standalone one
  (same `devagent task` pipeline); dispatched workers are detached children
  and keep running after the TUI exits.
- Multiple concurrent `devagent tui` instances with no shared daemon each
  embed their own — run `devagent daemon` (or `--attach-only` against it)
  when several clients must see one state.

## Keyboard reference

| Key | Action |
| --- | --- |
| `n` | **dispatch sheet** (FR-HAND-02): type the goal in one line, `Enter` posts it to `POST /dispatch` (worker + repo come from your config; auto-PR on when `GITHUB_TOKEN` is set — FR-HAND-03). Esc cancels. |
| `g` | **answer a paused task** (FR-HAND-07): `y`/`n` approve/deny or free text, `Enter` posts it to `POST /approve`. The header shows a `paused for you — press g` cue when a task is waiting. Without a paused task, `g` still jumps to the first item. |
| `1` / `2` / `3` | switch view: **workers** / **sessions** / **live log** (`s` and `l` toggle back to workers) |
| `↑` `↓`, PgUp/PgDn | move the selection (workers, sessions) · scroll the log |
| `Home` / `G` | jump to first / last item (log: oldest / newest) |
| `f` | toggle follow-tail in the log view (Esc also snaps back to the tail) |
| `Enter` / `o` | expand the selected worker/queued card into a detail panel |
| `u` | upgrade hint — the self-hosted upgrade/rollback recipe (pilot's `u` key, FR-TUI-05) |
| `r` | refresh now |
| `k` | kill the running/selected task via `POST /approve` with the `__kill__` sentinel (goes through the same gate machinery as the CLI, FR-CTRL-03; daemon must advertise `kill-via-answer`) |
| `y` | confirm a pending kill — any other key cancels |
| `?` | toggle the help overlay |
| `q` / Ctrl+C | quit (the alternate screen and cursor are always restored) |

`k` stays bound to kill per FR-TUI-05, so list navigation uses arrow keys (the
htop default) rather than vi keys.

## The three views

- **Workers** — boxed worker cards (task id, status chip, elapsed, engine,
  cwd, `devagent attach` jump-in hint) plus queued cards, and the ledger
  history tail. The selection cursor (`▸`) marks the card `Enter` will expand
  and `k` will kill.
- **Sessions** — dense herdr-pane roster: pane id, task, status chip, elapsed, cwd.
- **Live log** (FR-TUI-03) — the daemon's SSE `/events` stream (run-log +
  repo orchestration rows, e.g. `loop-phase`), level-colored, scrollable,
  follow-tail by default, ~1k-line ring buffer, auto-reconnect with
  `Last-Event-ID` resume.

The header is shared by all views: an inverse title strip with the aggregate
`RUNNING/IDLE/FAILED` status and a braille spinner (animated only while work
is live), the iteration card (`iteration 82 · phase: task — …` from the newest
`loop-phase` ledger row), and a metrics line — uptime, active/failed runs, a
queue-depth meter, an activity sparkline sampled once per poll (~2 min window),
circuit state, herdr session, spawn visibility.

## Design notes — what was borrowed from the reference TUIs

- **pilot** — card layout, status chips, sparkline metric, the `u` upgrade hint.
- **htop** — proportional meters, arrow-key navigation with a visible cursor,
  an always-fits-the-terminal layout (panels trim instead of scrolling the
  screen), and flicker-free redraw.
- **Claude Code** — live log tail, Enter-to-expand detail panels, contextual
  footer key hints, a spinner only while busy.

## Architecture

The TUI remains a pure HTTP + SSE client of the daemon (§20.3 anti-pattern: no
PTY parsing, no second event system). `/status`, `/agents`, `/history`,
`/sessions` and `/events` are its only data sources — embedding changes who
*starts* the daemon, never how it is talked to (`ensureDaemon` in
`internal/tui` probes `/healthz`, then either attaches or starts an in-process
daemon on port 0 and tears it down on every exit path).

| Module | Role |
| --- | --- |
| `internal/tui/tui.go` | views, overlays, key handling, interactive loop, one-shot mode, daemon resolution (attach/embed) |
| `internal/tui/transport.go` | bearer-token HTTP + SSE subscriber (reconnect, Last-Event-ID resume) |
| `internal/tui/input.go` | raw-stdin key decoding (arrows/PgUp/Home as whole escape sequences) |
| `internal/tui/frame.go` | incremental frame differ: rewrites only changed rows, never clears the screen |
| `internal/tui/viz.go` | sparkline, meter bar, log-line parse/format primitives |

Rendering pipeline: `renderLines()` builds a plain line array → `renderFrame()`
diffs it against the previous frame and emits the minimal escape sequence
(identical rows are skipped with cursor-down; changed rows are rewritten with
erase-to-EOL; lines are clamped to the terminal width so nothing wraps and
desyncs the diff). The spinner ticker re-renders at ~7 fps while RUNNING — one
header-line rewrite per tick.

Token/cost cards (FR-TUI-02's budget panel) are intentionally absent: nothing
in the codebase records token usage yet; the dashboard refuses to fabricate
metrics it cannot source.

## Testing

The `internal/tui` tests render every view/overlay from fixed snapshots
(including terminal-height fitting and the never-cut log title), pin
escape-sequence decoding, cover the sparkline/meter/log-line/frame-diff math,
and exercise the SSE subscriber against a live daemon seeded with run-log
lines — run them with `go test ./internal/tui/...`.
