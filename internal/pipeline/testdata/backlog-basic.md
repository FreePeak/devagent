## 17. Roadmap

### Phase 4 — Expansion (post-v1)

#### Phase 4 — current backlog (2026-09-08, curation run 25)

~~- **Orphan-pane sweep hardening** — driver-only orphan detection with a ppid ancestry walk; landed as PR #183 (Q33).~~
- **Run-lock registry races** — atomic acquire via `wx` create, pid-liveness probe before breaking stale locks, heartbeat mtime touches (Q44).
- **Cross-board retry memory beyond the SHA guard** — carry the prior board's failure class onto the re-bridged goal so the scout deprioritizes until the root-cause fix lands (Q27 family; trailing).
- **Board-level merged-result oracle** — gate the integrated tree after each merge on base + all open devagent heads (Q46).

## 18. Open Questions

> **Completed post-v0.3 (2026-09-02, curation run 23):** Operator hardening — research moved to local-evidence-only (5d8a319), NDJSON assistant-text extraction (d3adf17), all roles defaulted to omp (799fd86).
> **Completed 2026-09-07 (curation run 24):** herdr orphan-pane class fixed — `devagent herdr-sweep --orphans` closes panes whose ancestry has no live selfbuild driver (5d74a19).
