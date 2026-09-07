package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo doc-sync (operator PRD-freshness fix, 2026-09-04): the 24/7 scout and
// the selfbuild loop select work from docs/PRD.md, but nothing refreshed the
// working tree from origin before reading it — a manual PRD update pushed
// from another machine (or committed locally by the operator) was invisible
// until some unrelated `git pull` happened to land, so the scout enqueued
// stale backlog items and the PO built the older doc version indefinitely.

// RepoSyncResult mirrors RepoSyncResult.
type RepoSyncResult struct {
	OK bool `json:"ok"`
	// AlreadyUpToDate: true when the tree already matched origin (no update pulled).
	AlreadyUpToDate bool `json:"alreadyUpToDate,omitempty"`
	// Diverged: true when local and origin histories share no fork point
	// progress — a fast-forward is impossible and only a rebase reconciles.
	Diverged bool `json:"diverged,omitempty"`
	// Dirty: true when the tracked work-selection docs are locally modified.
	Dirty bool `json:"dirty,omitempty"`
	// Detail is the human-readable outcome string (byte-parity with TS).
	Detail string `json:"detail"`
}

// WorkSelectionDocs are the files that gate work selection; a sync is only
// meaningful when they exist.
var WorkSelectionDocs = []string{"docs/PRD.md"}

// DocSyncOpts mirrors the opts parameter of syncWorkSelectionDocs.
type DocSyncOpts struct {
	// Branch to sync against. Default 'main'.
	Branch string
	// TimeoutMs is the per-command wall clock. Default 30s.
	TimeoutMs int
}

// docGit runs git the way doc-sync.ts does — plain execFile, NOT the hardened
// spawn path (the TS module predates the fallback-PATH lesson and its error
// messages are built from raw exec output). Timeout via context deadline.
func docGit(args []string, cwd string, timeoutMs int) (string, string, error) {
	if timeoutMs <= 0 {
		timeoutMs = 30_000
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

// docErrMessage builds the error text the TS module embeds in Detail.
// Divergence note: Node's execFile error message is
// "Command failed: git <args>\n<stderr>"; Go has no such wrapper, so we use
// the command's stderr (falling back to the error text). The Detail prefix
// (e.g. "git fetch failed: ") is identical across runtimes.
func docErrMessage(stderr string, err error) string {
	s := strings.TrimSpace(stderr)
	if s != "" {
		return s
	}
	return strings.TrimSpace(err.Error())
}

// SyncWorkSelectionDocs fetches origin and fast-forwards the current branch
// so work-selection docs are fresh before any scout/PO read. Refuses to run
// on a dirty tree for the tracked work-selection files (an operator mid-edit
// must never be clobbered); untracked files and state dirs (.devagent/
// .selfbuild) do not block the sync. Network/merge failures are reported,
// never thrown — callers decide whether a stale read is fatal (loop) or
// best-effort (scout heartbeat).
//
// Divergence (Q41 degradation surface): when histories diverge and the
// tracked docs are clean, the local branch is rebased onto origin/<branch>
// with --autostash (clean abort on conflict, tree untouched). When the
// tracked docs are dirty AND histories diverged, the sync refuses with
// ok:false + diverged:true — a stash/autostash would momentarily lift the
// operator's edit out of the tree, so the operator must reconcile by hand.
func SyncWorkSelectionDocs(repoPath string, opts *DocSyncOpts) RepoSyncResult {
	branch := "main"
	timeoutMs := 30_000
	if opts != nil {
		if opts.Branch != "" {
			branch = opts.Branch
		}
		if opts.TimeoutMs > 0 {
			timeoutMs = opts.TimeoutMs
		}
	}

	_, fetchStderr, fetchErr := docGit([]string{"fetch", "origin", branch}, repoPath, timeoutMs)
	if fetchErr != nil {
		return RepoSyncResult{OK: false, Detail: "git fetch failed: " + truncate(docErrMessage(fetchStderr, fetchErr), 300)}
	}
	if strings.Contains(fetchStderr, "fatal:") {
		return RepoSyncResult{OK: false, Detail: "git fetch failed: " + truncate(strings.TrimSpace(fetchStderr), 300)}
	}

	localOut, _, localErr := docGit([]string{"rev-parse", "HEAD"}, repoPath, timeoutMs)
	remoteOut, _, remoteErr := docGit([]string{"rev-parse", "origin/" + branch}, repoPath, timeoutMs)
	dirtyOut, _, statusErr := docGit(append([]string{"status", "--porcelain", "--"}, WorkSelectionDocs...), repoPath, timeoutMs)
	if localErr != nil || remoteErr != nil || statusErr != nil {
		msg := docErrMessage("", firstNonNil(localErr, remoteErr, statusErr))
		return RepoSyncResult{OK: false, Detail: "git rev-parse/status failed: " + truncate(msg, 300)}
	}
	local := strings.TrimSpace(localOut)
	remote := strings.TrimSpace(remoteOut)
	dirty := dirtyOut

	dirtyDocs := strings.TrimSpace(dirty) != ""
	if local == remote {
		return RepoSyncResult{OK: true, AlreadyUpToDate: true, Dirty: dirtyDocs, Detail: "work-selection docs already at origin"}
	}

	// Divergence classification (PRD §17 defect + Q41) BEFORE the dirty gate:
	// merge-base prints the fork point A. base === local → strictly behind, a
	// plain fast-forward still applies; base === remote → strictly ahead,
	// nothing to pull; anything else (including merge-base exit 1 = unrelated
	// histories) means the histories diverged and a fast-forward is impossible.
	base := ""
	baseOut, _, baseErr := docGit([]string{"merge-base", "HEAD", "origin/" + branch}, repoPath, timeoutMs)
	if baseErr == nil {
		base = strings.TrimSpace(baseOut)
	}
	diverged := base != local && base != remote

	docsList := strings.Join(WorkSelectionDocs, ", ")
	if dirtyDocs {
		if diverged {
			return RepoSyncResult{OK: false, Diverged: true, Dirty: true,
				Detail: "refusing sync: histories diverged from origin/" + branch + " and " + docsList +
					" locally modified — reconcile by hand (an autostash would lift the operator edit out of the tree)"}
		}
		count := len(nonEmptyLines(strings.TrimSpace(dirty)))
		return RepoSyncResult{OK: false, Dirty: true,
			Detail: "refusing sync: " + docsList + " locally modified — commit or stash first (" +
				strconv.Itoa(count) + " file(s))"}
	}

	if !diverged {
		// Linear history: the old fast-forward path (covers strictly-behind and
		// strictly-ahead — an ff-only merge of an ancestor is a no-op).
		stdout, stderr, err := docGit([]string{"merge", "--ff-only", "origin/" + branch}, repoPath, timeoutMs)
		if err != nil {
			msg := docErrMessage(stderr, err)
			return RepoSyncResult{OK: false,
				Detail: "fast-forward to origin/" + branch + " failed: " + truncate(msg, 300)}
		}
		detail := strings.TrimSpace(stdout)
		if detail == "" {
			detail = "fast-forwarded"
		}
		return RepoSyncResult{OK: true, Dirty: false, Detail: truncate(detail, 200)}
	}

	// Diverged + clean tracked docs: rebase onto origin with --autostash.
	// On conflict, abort cleanly — a checkout left mid-rebase
	// (.git/rebase-merge) would poison every later status/merge/worktree op.
	stdout, stderr, err := docGit([]string{"rebase", "--autostash", "origin/" + branch}, repoPath, timeoutMs)
	if err != nil {
		_, _, _ = docGit([]string{"rebase", "--abort"}, repoPath, timeoutMs)
		msg := docErrMessage(stderr, err)
		return RepoSyncResult{OK: false, Diverged: true, Dirty: false,
			Detail: "diverged from origin/" + branch + ": rebase failed, aborted cleanly — reconcile by hand: " + truncate(msg, 300)}
	}
	detail := strings.TrimSpace(stdout)
	if detail == "" {
		detail = "rebased onto origin/" + branch
	}
	return RepoSyncResult{OK: true, AlreadyUpToDate: false, Diverged: true, Dirty: false,
		Detail: truncate(detail, 200) + " (diverged history reconciled)"}
}

// PrdStatResult is PRD.md stat as the staleness join key (doc-level, not
// tree-level). nil = the file does not exist.
type PrdStatResult struct {
	Size    int64 `json:"size"`
	MtimeMs int64 `json:"mtimeMs"`
}

// PrdStat mirrors prdStat.
func PrdStat(repoPath string) *PrdStatResult {
	st, err := os.Stat(filepath.Join(repoPath, "docs", "PRD.md"))
	if err != nil {
		return nil
	}
	return &PrdStatResult{Size: st.Size(), MtimeMs: st.ModTime().UnixMilli()}
}

func firstNonNil(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
