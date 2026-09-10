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
	out, err := cmd.Output()
	if err != nil {
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

// prMerged ports the merge-pick verification (issue #301): `gh pr view <pr>
// --json state` == MERGED. Best-effort — any gh failure (offline, unknown PR)
// reads as false, leaving the iteration non-productive so the issue is
// re-picked rather than falsely shipped.
func (d *driver) prMerged(prNum int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "pr", "view", strconv.Itoa(prNum), "--repo", d.cfg.GHRepo, "--json", "state")
	cmd.Dir = d.cfg.Repo
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	var v struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return false
	}
	return v.State == "MERGED"
}
