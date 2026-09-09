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
	// TimeoutMs is the wall-clock budget for the whole sync, fetch ladder
	// included: the retry shares ONE window fixed at entry — each fetch
	// attempt runs under the remaining budget as its per-command timeout and
	// the 1s/3s backoffs count against it, so total wall clock never exceeds
	// a single legacy fetch ceiling. Default 30s.
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

// fetchRetryAttempts is the git-fetch retry budget: one initial attempt plus
// two retries — 3 attempts total, matching issue #239.
const fetchRetryAttempts = 3

// docFetchBackoffs are the pauses between fetch attempts (1s, 3s) — one
// fewer than fetchRetryAttempts. The bounds guard in docFetchOrigin also
// tolerates a shorter/empty ladder. Tests no-op docSyncSleep instead of
// touching the ladder, so the production values stay under test.
var docFetchBackoffs = []time.Duration{1 * time.Second, 3 * time.Second}

// docGitFn is the fetch-retry seam: docFetchOrigin goes through it so tests
// can stub fetch attempts (issue #239) while every other doc-sync command
// keeps running real git.
var docGitFn = docGit

// docSyncSleep pauses between fetch attempts; the sleepMs-style seam
// (internal/orchestrator/executor.go) lets tests no-op the 1s/3s backoff.
var docSyncSleep = time.Sleep

// isTransientFetchError matches the network/TLS failure classes a flaky
// upstream (VN-ISP TLS handshakes to github.com, ~10-15% per #239) produces;
// these are the only fetch failures retried. Auth failures, missing
// refs/repos, and protocol errors fail immediately.
func isTransientFetchError(msg string) bool {
	patterns := []string{
		"could not resolve host", "connection", "timed out", "tls", "ssl",
		"handshake", "reset by peer", "early eof", "rpc failed",
	}
	lower := strings.ToLower(msg)
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// docFetchOrigin runs `git fetch origin <branch>` with transient-error retry:
// up to fetchRetryAttempts attempts (2 retries), retried only when the
// failure matches isTransientFetchError — that gate covers BOTH failure
// shapes the sync recognizes: non-zero exit and exit 0 with "fatal:" on
// stderr. Non-transient failures return on attempt 1.
//
// The retry budget lives inside the existing timeoutMs semantics as ONE
// shared window fixed at ladder entry: each attempt runs under the remaining
// budget (now+timeoutMs) as its per-command docGit timeout, and the backoffs
// count against the same window — total wall clock never exceeds the single
// fetch ceiling a legacy no-retry sync already had. When the remaining budget
// cannot cover the next backoff plus a fetch, the loop stops; docGit treats
// timeoutMs<=0 as "default 30s", so a non-positive remaining budget returns
// the last failure instead of arming a fresh window. Error text is only
// built on final failure, byte-parity with the pre-retry format
// ("git fetch failed: <message>").
func docFetchOrigin(branch string, repoPath string, timeoutMs int) (stderr string, err error) {
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	var lastStderr string
	var lastErr error
	for attempt := range fetchRetryAttempts {
		if attempt > 0 {
			backoff := time.Duration(0)
			if i := attempt - 1; i < len(docFetchBackoffs) {
				backoff = docFetchBackoffs[i]
			}
			if backoff > 0 {
				if remaining := time.Until(deadline); remaining <= backoff {
					break
				}
				docSyncSleep(backoff)
			}
		}
		budgetMs := int(time.Until(deadline).Milliseconds())
		if budgetMs <= 0 {
			break
		}
		_, stderrOut, err := docGitFn([]string{"fetch", "origin", branch}, repoPath, budgetMs)
		if err == nil && !strings.Contains(stderrOut, "fatal:") {
			return stderrOut, nil
		}
		lastStderr, lastErr = stderrOut, err
		// Exit 0 with "fatal:" on stderr: git itself signalled a hard
		// failure. One unified gate over both shapes — docErrMessage covers
		// the err==nil case — terminal unless the text also looks transient.
		if !isTransientFetchError(docErrMessage(stderrOut, err)) {
			break
		}
	}
	if lastStderr == "" && lastErr == nil {
		// Window exhausted before any fetch recorded a result: fail the sync
		// the way a timed-out fetch would, never as a fake success.
		lastErr = context.DeadlineExceeded
	}
	return lastStderr, lastErr
}

// SyncWorkSelectionDocs fetches origin and fast-forwards the current branch
// so work-selection docs are fresh before any scout/PO read. Refuses to run
// on a dirty tree for the tracked work-selection files (an operator mid-edit
// must never be clobbered); untracked files and state dirs (.devagent/
// .selfbuild) do not block the sync. Network/merge failures are reported,
// never thrown — callers decide whether a stale read is fatal (loop) or
// best-effort (scout heartbeat).

// Issue #245: dirt is gated over the WHOLE tracked tree, not just the
// work-selection docs — a dirty non-doc file used to slip past the gate,
// abort the ff merge ("local changes would be overwritten") and surface as
// a plain OK:false that the loop driver misclassified as provider-degraded
// (rc 1) instead of operator-degraded (rc 2). Refusals name the blocking
// files; a merge that aborts on local changes despite the pre-check is
// reclassified as dirty (defense in depth).
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

	fetchStderr, fetchErr := docFetchOrigin(branch, repoPath, timeoutMs)
	if fetchErr != nil {
		return RepoSyncResult{OK: false, Detail: "git fetch failed: " + truncate(docErrMessage(fetchStderr, fetchErr), 300)}
	}
	if strings.Contains(fetchStderr, "fatal:") {
		return RepoSyncResult{OK: false, Detail: "git fetch failed: " + truncate(strings.TrimSpace(fetchStderr), 300)}
	}

	localOut, _, localErr := docGit([]string{"rev-parse", "HEAD"}, repoPath, timeoutMs)
	remoteOut, _, remoteErr := docGit([]string{"rev-parse", "origin/" + branch}, repoPath, timeoutMs)
	dirtyOut, _, statusErr := docGit(append([]string{"status", "--porcelain", "--"}, WorkSelectionDocs...), repoPath, timeoutMs)
	treeOut, treeErr := docTreeDirtyFiles(repoPath, timeoutMs)
	if localErr != nil || remoteErr != nil || statusErr != nil || treeErr != nil {
		msg := docErrMessage("", firstNonNil(localErr, remoteErr, statusErr, treeErr))
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
	behind := base == local && local != remote

	dirtyTreeFiles := nonEmptyLines(strings.TrimSpace(treeOut))
	dirtyTree := len(dirtyTreeFiles) > 0
	// Whole-tree dirty gate (issue #245): a locally modified tracked file
	// outside the state dirs blocks a merge that would pull — git would
	// abort with "would be overwritten", which used to surface as a plain
	// OK:false and got misclassified as provider-degraded (rc 1) instead of
	// operator-degraded (rc 2). The docs gate below remains for the
	// strictly-ahead case, where the ff merge is a no-op and nothing can be
	// clobbered.
	if diverged && dirtyTree {
		return RepoSyncResult{OK: false, Diverged: true, Dirty: true,
			Detail: "refusing sync: histories diverged from origin/" + branch + " and tracked files locally modified — reconcile by hand (an autostash would lift the operator edit out of the tree): " +
				truncate(strings.Join(dirtyTreeFiles, ", "), 200)}
	}
	if dirtyTree && behind {
		return RepoSyncResult{OK: false, Dirty: true,
			Detail: "refusing sync: tracked files locally modified — commit or stash first (" +
				strconv.Itoa(len(dirtyTreeFiles)) + " file(s)): " + truncate(strings.Join(dirtyTreeFiles, ", "), 200)}
	}
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
			if mergeAbortedOnLocalChanges(msg) {
				return RepoSyncResult{OK: false, Dirty: true,
					Detail: "refusing sync: work tree locally modified — commit or stash first: " + truncate(msg, 300)}
			}
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

// docTreeDirtyFiles runs `git status --porcelain -uno -- .` with the loop
// state dirs excluded by pathspec (issue #245) and returns the raw porcelain
// output: one "XY <path>" line per locally modified tracked file. Untracked
// files never block the sync (--untracked-files=no); .devagent/ and
// .selfbuild/ are excluded so the loop's own state churn cannot block or
// misclassify a sync. The git-level pathspec filter (not post-hoc string
// filtering) is pinned by TestDocTreeDirtyFilesPathspecFilter.
func docTreeDirtyFiles(repoPath string, timeoutMs int) (string, error) {
	stdout, _, err := docGit([]string{"status", "--porcelain", "--untracked-files=no",
		"--", ".", ":(exclude).devagent", ":(exclude).selfbuild"}, repoPath, timeoutMs)
	return stdout, err
}

// mergeAbortedOnLocalChanges matches the git merge abort a dirty tree
// produces: "error: Your local changes to the following files would be
// overwritten by merge". Checked case-insensitively against the whole
// message (stderr plus any error text) so the wording cannot drift past
// this classifier (issue #245, defense in depth).
func mergeAbortedOnLocalChanges(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "would be overwritten by merge") &&
		strings.Contains(m, "local changes")
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
