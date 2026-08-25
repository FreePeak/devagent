# git-cleanup-merged.sh

Auto-cleanup of local git branches and worktrees whose MR (GitLab) / PR (GitHub) was
merged, across **all nested repos** under a scan root — default `~/work`
(~270 repos: `git.begroup.team`, `github.com`, personal GitHub aliases).

Complements the existing tools, which cover narrower scopes:

| Tool | Scope |
|------|-------|
| `scripts/reclaim-disk.sh` | disk space, freepeak only, no forge awareness |
| `scripts/orca-selfbuild-cleanup.sh` | Orca selfbuild sessions only |
| `OmniRoute/scripts/ops/prune-stale-worktrees.sh` | single repo, `port-*` only |

## Usage

```bash
scripts/git-cleanup-merged.sh                    # dry-run report over ~/work (default)
scripts/git-cleanup-merged.sh --root ~/work/be   # limit the scan root
scripts/git-cleanup-merged.sh --apply            # actually delete worktrees + branches
scripts/git-cleanup-merged.sh --fetch            # git fetch --prune each repo first (slower, fresher refs)
scripts/git-cleanup-merged.sh --install-launchagent   # weekly auto-run (--apply --fetch) via launchd
scripts/git-cleanup-merged.sh --uninstall-launchagent
```

Output lines: `WOULD` (dry-run candidate), `DEL` (applied), `KEEP` (with reason),
`SKIP` (with reason). Summary counters at the end.

## How it decides

A local branch is deleted only when **all** gates pass:

1. **Merged signal**
   - Forge API confirms a merged MR/PR for that exact source branch:
     `glab mr list --source-branch=<br> -M` / `gh pr list --state merged --head <br>`.
     This catches **squash merges**, which plain `git branch --merged` cannot see.
   - Fallback when the forge can't be queried (no remote, offline, unknown host):
     branch tip is an ancestor of `origin/<default>` (true merges only).
2. **Not protected**: `main`, `master`, `develop*`, `staging`, `production`, `release*`.
3. **Not checked out** in the main checkout or any surviving worktree.
4. **Clean worktree** — any attached worktree must have zero uncommitted/untracked files.
5. **Nothing unpushed** — if a live upstream exists, the branch may not be ahead of it.
   A stale upstream (remote deleted after merge) is ignored.

Deletions use `git worktree remove` then `git branch -D` (`-D` is deliberate:
squash-merged branches are not ancestors of main; gates 1–5 already ran).

## Forge detection

Remote origin URL decides which CLI queries it:

| URL contains | CLI | Auth |
|---|---|---|
| `github` | `gh` | `GITHUB_TOKEN` / keyring |
| `gitlab` or host in `$GIT_CLEANUP_GITLAB_HOSTS` (default `git.begroup.team`) | `glab` | `GITLAB_TOKEN` / keyring |
| anything else | none | ancestry fallback only |

## Automation

```bash
scripts/git-cleanup-merged.sh --install-launchagent              # weekly, 168h default
scripts/git-cleanup-merged.sh --install-launchagent --interval-hours 24
```

Installs `~/Library/LaunchAgents/com.devagent.git-cleanup-merged.plist` running
`--apply --fetch` on the timer. Log: `~/.local/var/git-cleanup-merged.log`.
Always dry-run manually first to review what would go.

## Performance notes

- Repos with no candidate local branches never touch the network.
- Per-branch targeted API queries (~0.5–1s each) instead of paginating all merged
  MRs per repo (a busy GitLab repo can have 1500+ merged MRs → 30s+ just paging).
- Build-artifact mirrors of repos (`.build/`, `dist/`, `out/`, …) are pruned and
  never scanned.
- First full dry-run over `~/work` without `--fetch` (2026-08-25):
  **52 repos with candidates, 305 merged branches found, 285 kept, 17 dirty-worktree
  skips, 337s**. Weekly LaunchAgent runs add fetch time but run in the background.

## Env overrides

- `GIT_CLEANUP_ROOT` — default scan root (default `$HOME/work`)
- `GIT_CLEANUP_GITLAB_HOSTS` — extra comma-separated GitLab hosts to recognize
