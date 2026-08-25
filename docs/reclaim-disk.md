# Disk reclaim

Local disk space is limited. Finished agent work must not leave worktrees,
installed libs, or build caches behind. `scripts/reclaim-disk.sh` finds and
removes them.

## Usage

```bash
scripts/reclaim-disk.sh report                  # sizes only, no changes
scripts/reclaim-disk.sh clean                   # dry-run removal plan
scripts/reclaim-disk.sh clean --yes             # apply all categories
scripts/reclaim-disk.sh clean --yes caches      # single category
npm run reclaim                                 # dry-run via npm
```

Scan root defaults to `~/work/harvey/freepeak`; override with
`DEVAGENT_SCAN_ROOT=/some/root`.

## Categories

| Category      | Removes |
|---------------|---------|
| `worktrees`   | Git worktrees whose tree is clean and branch is fully merged into main/master (handles Claude-locked agent worktrees) |
| `builds`      | `node_modules` / `.venv` / `target` inside any worktree (rebuildable) |
| `caches`      | Rebuildable tool caches: `~/.cache/cargo-target`, `codex-runtimes`, `kilo`, `opencode` |
| `transcripts` | Claude Code project transcripts older than 30 days, shell snapshots older than 7 days |

Safety rules, enforced before any deletion:

- A worktree with **any** uncommitted or untracked file is skipped (`dirty`).
- A worktree with unmerged commits on its branch is skipped and the count shown.
- Never touches opencode session DBs, cursor/opencode chat history, or anything
  under a dirty worktree — those are data, not cache.

## Known non-cache hogs (manual decision required)

- `~/.local/share/opencode/opencode.db` (~5 GB): opencode session history.
  Shrink by deleting old sessions from within opencode, not by removing the file.

## 2026-08-24 run

First application freed ~62 GB: 48 GB cargo-target cache, 673 MB merged 9router
worktree, 5 locked Alamofire agent worktrees, token-lens build dirs, codex/kilo/
opencode caches. Disk went 125 GB free to 187 GB free.
