# Go migration baseline (FR-GO-01, issue #191)

Measured 2026-09-07 on the reference machine (Apple M2 Pro, macOS 26 arm64,
Go 1.25.x). These are the numbers the Go implementation must beat. Update
this file only with fresh measurements, never projections.

> **2026-09-08 (FR-GO-15 cutover, #204):** the production entrypoint flips to
> the Go binary at this cutover; the Node CLI becomes a legacy fallback until
> FR-GO-16. The measurements below remain the reference baseline from
> 2026-09-07.
>
> **2026-09-08 (FR-GO-16 cutover complete, #205):** the Node implementation
> (src/, test/, dist/, package.json, npm tooling) has been deleted — the Go
> binary is the one implementation. Build via `make build` → `./devagent-go`;
> tests are `go vet ./... && go test ./...`. The "Node baseline" rows below
> are kept verbatim as the historical reference they were measured to be.

## Node baseline

| Metric | Value | How measured |
|---|---|---|
| `devagent daemon` idle RSS | **16 MB** | `ps aux` RSS of the long-running daemon process (`node ~/.local/bin/devagent daemon`) |
| `devagent --version` cold start | **0.13 s** | `/usr/bin/time` over 3 runs (`node dist/src/cli.js --version`) |
| `devagent status` cold start | **0.11 s** | `/usr/bin/time` over 3 runs (`node dist/src/cli.js status`) |

## Worker-fleet context (unaffected by host language)

Worker child processes dominate steady-state memory; the Go migration does
not change them (external CLIs). Observed same day, for scale only:

| Process | RSS |
|---|---|
| omp workers (live, ~10 instances) | 235–473 MB each |

The Node daemon at 16 MB idle is not a memory hog by itself; the migration's
memory case rests on (a) the daemon + TUI + loop drivers being permanent
residents across many boards, (b) single-binary deployment removing the
dist-staleness class, and (c) cold-start tail latency.

## Go targets

| Metric | Target |
|---|---|
| Go daemon idle RSS | ≤ 50 MB (must not exceed the Node baseline by more than 3×; Go's runtime floor is ~2–8 MB) |
| Go CLI cold start (`--version`) | ≤ 0.05 s |
| Go binary size | ≤ 30 MB |

## Measurement procedure

```sh
# daemon RSS (run the Go daemon, then):
ps aux | grep 'devagent' | grep -v grep   # RSS column / 1024 = MB
# cold start:
for i in 1 2 3; do /usr/bin/time -p ./devagent --version 2>&1 | grep real; done
```
