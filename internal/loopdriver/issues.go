package loopdriver

import (
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// issues.go ports the issue-first tracker surface (pick_issue / close_issue,
// bash lines 167-184): a deterministic priority-ranked pick, and a
// best-effort close with an evidence comment.

// ghIssue mirrors the `gh issue list --json number,title,labels` rows.
type ghIssue struct {
	Number int       `json:"number"`
	Title  string    `json:"title"`
	Labels []ghLabel `json:"labels"`
}

type ghLabel struct {
	Name string `json:"name"`
}

// priorityRank mirrors the deterministic tracker ranking: priority:P0 >
// priority:P1 > everything else (unlabeled = P2).
func priorityRank(labels []ghLabel) int {
	for _, l := range labels {
		if l.Name == "priority:P0" {
			return 0
		}
	}
	for _, l := range labels {
		if l.Name == "priority:P1" {
			return 1
		}
	}
	return 2
}

var (
	gitAtRe = regexp.MustCompile(`^git@[^:]+:`)
	httpsRe = regexp.MustCompile(`^https?://[^/]+/`)
)

// ghRepoFromRemote derives owner/repo from `git remote get-url origin`
// (the SELFBUILD_GH_REPO fallback), stripping the git@ / https:// prefixes
// and a trailing .git.
func ghRepoFromRemote(remoteURL string) string {
	s := gitAtRe.ReplaceAllString(remoteURL, "")
	s = httpsRe.ReplaceAllString(s, "")
	return strings.TrimSuffix(s, ".git")
}

// pickIssue ports pick_issue(): list open issues with the tracker label and
// pick the deterministic top (priority rank, then oldest issue number).
// Returns "NUM\tTITLE" (title tabs folded to spaces) or "" — any failure
// (gh down, bad JSON, empty tracker) degrades to "".
func (d *driver) pickIssue() string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "issue", "list",
		"--repo", d.cfg.GHRepo, "--state", "open", "--label", d.cfg.IssueLabel,
		"--limit", itoa(d.cfg.IssueMax), "--json", "number,title,labels")
	cmd.Dir = d.cfg.Repo
	// Drain-bounded like every other pipe capture in the driver (#286): the
	// ctx kill reaches gh, not a grandchild left holding the write end.
	cmd.WaitDelay = pipeDrainDelay
	out, err := cmd.Output()
	// A drain cutoff (ErrWaitDelay) must not read the listing as failed
	// when gh itself exited 0 (#286).
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		return ""
	}
	var items []ghIssue
	if err := json.Unmarshal(out, &items); err != nil {
		return ""
	}
	sort.SliceStable(items, func(i, j int) bool {
		ri, rj := priorityRank(items[i].Labels), priorityRank(items[j].Labels)
		if ri != rj {
			return ri < rj
		}
		return items[i].Number < items[j].Number
	})
	if len(items) == 0 {
		return ""
	}
	top := items[0]
	return strconv.Itoa(top.Number) + "\t" + strings.ReplaceAll(top.Title, "\t", " ")
}

// closeIssue ports close_issue(): best effort, never fatal.
func (d *driver) closeIssue(num int, comment string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "issue", "close", strconv.Itoa(num), "--comment", comment)
	cmd.Dir = d.cfg.Repo
	_ = cmd.Run()
}

// prState returns gh's state for pull request num — OPEN | MERGED | CLOSED —
// or "" when gh cannot say. Decoded rather than substring-matched: the verdict
// gates a merge, gh's JSON shape is not contractual, and decoding is this
// file's own convention (pickIssue).
func (d *driver) prState(num int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "pr", "view", strconv.Itoa(num),
		"--repo", d.cfg.GHRepo, "--json", "state")
	cmd.Dir = d.cfg.Repo
	cmd.WaitDelay = pipeDrainDelay
	out, err := cmd.Output()
	// A drain cutoff (ErrWaitDelay) must not read the view as failed when
	// gh itself exited 0 (#286).
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		return ""
	}
	var view struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return ""
	}
	return view.State
}

// prMerged reports whether pull request num has merged — the publish evidence
// for a verify-and-merge dispatch (issue #301), which lands an existing PR and
// so never prints a "PR opened:" line. Anything else reads as not shipped: the
// issue stays open for re-pick (#238 semantics).
func (d *driver) prMerged(num int) bool { return d.prState(num) == "MERGED" }
