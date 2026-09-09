# DevAgent Control (Tauri 2 desktop app)

The FR-UI cross-platform desktop control app (PRD §20.4, issue #181): a tray
app + dashboard that is a **thin client** of the FR-CTRL daemon
(`internal/daemon`). It holds no credentials beyond the daemon's own per-boot
token file and no business logic — losing the app degrades to CLI-only
operation (FR-UI-07).

## Run

Prereqs: Rust (stable), pnpm 9, the Tauri 2 prerequisites for your OS
(macOS: Xcode CLT; Linux: webkit2gtk-4.1 dev; Windows: MSVC + WebView2).

```sh
cd app
pnpm install
pnpm tauri dev        # dev run (needs a display)
# or, without bundling:
pnpm build            # esbuild → ui/main.js
cd src-tauri
cargo build           # debug binary
```

The daemon must be running (`devagent daemon`, default `127.0.0.1:7788`).
Connection defaults:

- Base URL: `http://127.0.0.1:7788` (override: `DEVAGENT_DAEMON_URL` env or
  in-app Settings sheet)
- Bearer token: read from `$DEVAGENT_HOME/daemon-token` (default
  `~/.devagent/daemon-token`, the file the daemon writes `0600` at boot).
  Override in the in-app Settings sheet — the token stays in memory for the
  session, it is never persisted by this app.

## Architecture (and why HTTP lives in Rust)

`app/src-tauri` is the Tauri 2 Rust core; `app/ui` is the static webview
bundle (plain TS + esbuild, no framework).

**All daemon I/O is in the Rust core** (ureq), not the webview: the daemon
rejects non-loopback `Origin` headers (drive-by CSRF guard, `originAllowed`),
and Windows/Linux webviews present `http://tauri.localhost` as origin — the
daemon would 403 every request. Rust-side requests carry no `Origin` header,
which the daemon treats as same-origin, on every OS. The webview talks to the
core via Tauri commands and `daemon://*` events only.

```
daemon (Go)  <──HTTP/SSE──  src-tauri (Rust: transport, tray, notifications)
                                  │ invoke / emit
                                  ▼
                             ui/ (webview: roster, dispatch sheet, approvals, DAG)
```

## FR-UI row → implementation map

| Row | Pri | Status | Where |
|---|---|---|---|
| FR-UI-01 tray aggregate state | M | functional | `src-tauri/src/state.rs` (running/idle/failed derivation from `GET /status`, circuit `open` → failed, offline/auth-failed distinct), `src-tauri/src/main.rs` `poll_loop` (5s poll) + `set_tray_icon` (per-state icons) + tray menu |
| FR-UI-02 dispatch sheet | M | functional | `ui/index.html` `#dispatch-sheet` (prompt, role, worker, repoPath, autoPr, budget), `ui-src/main.ts` submit → `dispatch` command → `POST /dispatch`; payload mirrors `parseDispatch` (endpoints.go) incl. `autoPr` only on explicit true |
| FR-UI-03 dashboard roster + live log tail | M | functional | `GET /agents` poll → roster/queue lists; `GET /events` SSE (core) → `daemon://event` → log tail (500-line capped, stick-to-bottom); `GET /history` → History tab |
| FR-UI-04 approval inbox + notifications | M | functional | `verdict:"ask"` rows (the same `pickPausedTask` fallback signal the TUI uses) → native notification + inbox item; Approve/Deny → `POST /approve` (free-text answer, y/n shorthand like the TUI; the `__kill__` sentinel is deliberately not offered from the app) |
| FR-UI-05 autostart + single instance | S | wired, best-effort | `tauri-plugin-autostart` (LaunchAgent on macOS, Run key/~/.config/autostart elsewhere) + `tauri-plugin-single-instance` (second launch focuses the dashboard). Not e2e-verified on every OS in this PR — exercise per-OS before trusting it |
| FR-UI-06 signed builds + auto-update | S | scaffolded, by design | `.github/workflows/app-build.yml` TODO notes list the exact secrets (APPLE_API_ISSUER/KEY, Windows PFX) and `tauri-plugin-updater` steps; signing is NOT faked |
| FR-UI-07 thin client | M | enforced | all daemon I/O in `src-tauri/src/daemon.rs`; no other credential source than the daemon's token file; no pipeline logic in the app — daemon down → the UI says "degrade to CLI" |
| FR-UI-08 pipeline DAG view | M | functional (minimal) | `src-tauri/src/pipeline.rs` folds SSE rows into a per-run stage timeline scout→plan→implement→gates→pr (G0–G5 collapse into `gates`; loopdriver `loop-phase` rows map onto the pipeline; per-stage status/elapsed/retries; unit-tested). Static fallback rebuilds from `GET /history` (`get_pipeline_from_history`) |
| FR-UI-09 3-OS CI parity gate | S | scaffolded, by design | `app-build.yml` has the real 3-OS matrix (macOS/Ubuntu/Windows, cargo check + debug build) but is not a required check until FR-UI-06 signing lands — the file documents the enabling steps |

## Not in this PR (honest scope notes)

- FR-UI-06 updater plugin (`tauri-plugin-updater`) is not added yet — it is
  useless without signed release artifacts and a `latest.json` feed; adding it
  now would be dead config.
- FR-UI-05 launch-at-login is wired but only manually exercised on macOS.
- Notifications on Windows rely on WebView2/Toast support in
  `tauri-plugin-notification`; verify on a real Windows box (CI scaffold
  cannot assert this).

## Tests

Rust: `cargo test` inside `app/src-tauri` — covers aggregate-state precedence
and malformed-body tolerance (`state.rs`) and the pipeline reducer (stage
mapping, gate collapse, ordering, retries, ts parsing) in `pipeline.rs`.
