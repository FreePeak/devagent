# Windows support (NFR-05 §22.2, FR-GO-14 / [#203](https://github.com/FreePeak/devagent/issues/203))

Status of the Windows path after the Go migration wave. macOS and Linux
remain the reference platforms; this page records what works on Windows,
what is deliberately deferred, and the exact gaps a Windows maintainer picks
up next.

## Install

1. Install [Go](https://go.dev/dl/) 1.25+, [git](https://git-scm.com/), and
   optionally the [gh CLI](https://cli.github.com/) on PATH.
2. Build the CLI:

   ```powershell
   go build -o "$env:DEVAGENT_HOME\bin\devagent.exe" .\cmd\devagent
   ```

   `%DEVAGENT_HOME%` defaults to `%USERPROFILE%\.devagent` — set it to move
   the whole state tree (ledger, runs, daemon token, worktrees live in the
   repo's `.devagent-worktrees\`, not here).
3. Smoke: `devagent config`, `devagent init`.

## Paths: differences from macOS/Linux

| Concept | macOS/Linux | Windows |
|---|---|---|
| State home | `$DEVAGENT_HOME` or `~/.devagent` | `%DEVAGENT_HOME%` or `%USERPROFILE%\.devagent` |
| Daemon endpoint | `http://127.0.0.1:7788` (TCP) or UDS path | TCP, or a named pipe (`\\.\pipe\devagent`) |
| Credential file | `$DEVAGENT_HOME/daemon-token` (0600) | `%DEVAGENT_HOME%\daemon-token` (user-profile ACL) |
| 24/7 automation | LaunchAgents (`launchctl`) | Task Scheduler (`scripts/install-windows-task.ps1`) |
| Sandbox | `sandbox-exec` (seatbelt, darwin) | **not implemented** — see below |
| herdr panes | `herdr` binary on PATH | **no-op** — see below |

## Daemon IPC: named pipe (FR-CTRL-05)

FR-CTRL-05: the daemon can listen on a Unix-domain socket, "Windows
equivalent: named pipe". The Go port owns that seam in
`internal/platform`:

- **unix** — `platform.Listen` binds a real unix socket (with the same
  stale-socket unlink the Node daemon performs) and `platform.DialContext`
  dials it; covered by tests.
- **Windows** — `platform.DialContext` and `platform.Listen` are
  **documented stubs returning `ErrNotImplemented`**. Rationale: the only
  shipped Windows daemon is the Node one, which already serves named pipes
  natively (Node's `server.listen('\\\\.\\pipe\\name')`), so nothing needs a
  Go pipe listener yet; and a hand-rolled winio-style listener
  (`CreateNamedPipeW` + overlapped `ConnectNamedPipe` + `CancelIoEx`) cannot
  be runtime-verified by this CI (no Windows test runner). The Go TUI on
  Windows therefore uses the default TCP endpoint; a configured pipe endpoint
  fails loudly (Status 0) rather than silently falling back to TCP.

Implementing the pipe side (for the FR-CTRL daemon port, tracked at #200):
hand-roll the winio recipe against stdlib only —
`syscall.CreateNamedPipe(name, PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE,
PIPE_TYPE_BYTE, PIPE_UNLIMITED_INSTANCES, ...)` per pending instance,
`ConnectNamedPipe` with an `OVERLAPPED` event so `Accept` can honor
`Close`/deadlines via `CancelIoEx`, wrap the connected handle with
`os.NewFile`, and expose `\\.\pipe\<name>` where `<name>` is the last path
element of the configured socket path (`.sock` suffix dropped). Verify on a
real Windows runner before landing; the `windows-cross` CI job only proves
it compiles.

## herdr: documented no-op

The herdr runtime (`internal/herdr`) is a visibility enhancement, never a
hard dependency (docs/HERDR.md). On Windows it degrades exactly as on any
machine without the `herdr` binary: `HerdrBin()` fails `exec.LookPath`,
worker launches fall back to direct child processes after a single warning,
and `herdr-sweep` becomes a no-op sweep. The pane-detach syscall seam is
already split (`proc_unix.go` setsid / `proc_other.go` no-op), so if a
Windows herdr build ever exists, `DEVAGENT_HERDR=1` needs no Go change.

## Sandbox: the seatbelt gap

The worker sandbox has two layers in the TS source (`src/workers/sandbox.ts`):
env scrubbing (default, pure map filtering — portable) and seatbelt
confinement (`DEVAGENT_SANDBOX=seatbelt`, **darwin only** —
`sandbox-exec` + an SBPL profile). There is no Windows equivalent wired:

- `DEVAGENT_SANDBOX=seatbelt` on Windows fails loudly (the port mirrors the
  TS behavior of refusing non-darwin), never silently unconfined.
- The native Windows successor (Job Objects / AppContainer confinement for
  worker processes) is **unimplemented and untracked** — a maintainer picking
  this up should open a dedicated FR before writing code; do not approximate
  with a partial shim.
- Env scrubbing applies unchanged on Windows: credential-shaped variables
  are stripped from worker environments on every platform.

## Task Scheduler automation

`scripts/install-windows-task.ps1` replaces the LaunchAgents for 24/7
operation:

```powershell
# review the registration without touching the machine
.\scripts\install-windows-task.ps1 -RepoPath C:\src\devagent -DryRun
# install + start (per-user task, no admin, restart-on-failure KeepAlive)
.\scripts\install-windows-task.ps1 -RepoPath C:\src\devagent
# validate / uninstall
.\scripts\install-windows-task.ps1 -RepoPath C:\src\devagent -Validate
.\scripts\install-windows-task.ps1 -Uninstall
```

The script is **reviewed-by-construction**: CI has no Windows runner that
executes it, so correctness rests on the dry-run review surface plus the
`-Validate`/read-back assertions inside the script itself. It routes daemon
output to `%DEVAGENT_HOME%\logs\daemon.log` (the
`StandardOutPath`/`StandardErrorPath` analogue) and asserts after
registration that the task action actually points at `devagent.exe`.

## CI

`.github/workflows/ci-go.yml` gained two jobs (FR-GO-14):

- `windows-cross` (ubuntu runner, `GOOS=windows`): `go build ./...` +
  `go vet ./...`. vet type-checks the `//go:build windows` test files too,
  which a plain build does not — this is the guard that keeps windows-only
  files compiling.
- `windows-build` (windows-latest runner): build + vet on the real OS,
  **no `go test`**.

Per-package test-skip justification (the issue allows skips with recorded
justification): `internal/platform` skips its unix round-trip tests on
Windows until the pipe listener lands (the stub returns
`ErrNotImplemented`); every other package's suite is untested on Windows by
design in this wave — building + vetting on windows-latest is the recorded
scope, keeping CI minutes sane.
