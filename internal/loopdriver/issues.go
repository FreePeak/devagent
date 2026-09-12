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

// prViewResult is one `gh pr view --json state,mergedAt,baseRefName,headRefName`
// read. The state half answers the OPEN gates, the merge stamp answers the
// landing-evidence window, and the two ref names answer the CLOSED-but-unmerged
// reopen gate (run.go): a closed pull request is only reopenable while it still
// targets main and its head ref is still on origin.
type prViewResult struct {
	State       string `json:"state"`
	MergedAt    string `json:"mergedAt"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
}

// prView reads gh's view of pull request num: its state (OPEN | MERGED |
// CLOSED, "" when gh cannot say), its merge stamp mergedAt (second-precision
// RFC3339, empty when it has not merged), and the base/head ref names the
// reopen gate needs. One `gh pr view --json …` per call, so the
// landing-evidence readers share a read instead of one gh call per field.
// Decoded rather than substring-matched: the verdicts gate a merge, gh's JSON
// shape is not contractual, and decoding is this file's own convention
// (pickIssue).
func (d *driver) prView(num int) prViewResult {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "pr", "view", strconv.Itoa(num),
		"--repo", d.cfg.GHRepo, "--json", "state,mergedAt,baseRefName,headRefName")
	cmd.Dir = d.cfg.Repo
	cmd.WaitDelay = pipeDrainDelay
	out, err := cmd.Output()
	// A drain cutoff (ErrWaitDelay) must not read the view as failed when
	// gh itself exited 0 (#286).
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		return prViewResult{}
	}
	var view prViewResult
	if err := json.Unmarshal(out, &view); err != nil {
		return prViewResult{}
	}
	return view
}

// prState is the state-only half of prView — the reader the OPEN gates use.
// A verdict that has to tell "merged before this iteration" from "merged
// inside it" reads prView instead (the landing-evidence certification in
// run.go).
func (d *driver) prState(num int) string {
	return d.prView(num).State
}

// prMerged reports whether pull request num has merged — the publish evidence
// for a verify-and-merge dispatch (issue #301), which lands an existing PR and
// so never prints a "PR opened:" line. Anything else reads as not shipped: the
// issue stays open for re-pick (#238 semantics).
func (d *driver) prMerged(num int) bool { return d.prState(num) == "MERGED" }

// mergedAtTime parses gh's mergedAt stamp; ok=false is the cannot-say branch
// (empty string, JSON null, an unparseable stamp) and every caller treats it
// as no evidence: a stamp the driver cannot read must never certify a ship.
func mergedAtTime(mergedAt string) (time.Time, bool) {
	if mergedAt == "" {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, mergedAt)
	if err != nil {
		return time.Time{}, false
	}
	return at, true
}

// ghTimelineEvent mirrors the cross-referenced entries of the issue timeline
// REST response (GET /repos/{owner}/{repo}/issues/{n}/timeline).
type ghTimelineEvent struct {
	Event  string `json:"event"`
	Source *struct {
		Issue *struct {
			Number      int       `json:"number"`
			PullRequest *struct{} `json:"pull_request,omitempty"`
		} `json:"issue"`
	} `json:"source"`
}

// openPROfIssue returns the number of the pull request cross-referenced on
// issue issueNum's timeline that is currently OPEN — the deterministic
// detection half of merged = shipped: an issue whose own PR is open must
// route to a verify-and-merge dispatch, not a fresh rewrite that cannot open
// a second PR (#323 Case B: three no-pr rows re-running #316 while its PR sat
// open). The research prose parse stays as the richer fallback; this only
// fires when research did not name a PR. 0 when gh cannot say or no open PR
// is linked.
func (d *driver) openPROfIssue(issueNum int) int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "api",
		"repos/"+d.cfg.GHRepo+"/issues/"+strconv.Itoa(issueNum)+"/timeline?per_page=100")
	cmd.Dir = d.cfg.Repo
	cmd.WaitDelay = pipeDrainDelay
	out, err := cmd.Output()
	// Drain-cutoff tolerance, same contract as prState (#286).
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		return 0
	}
	var events []ghTimelineEvent
	if err := json.Unmarshal(out, &events); err != nil {
		return 0
	}
	for _, ev := range events {
		if ev.Event != "cross-referenced" || ev.Source == nil || ev.Source.Issue == nil {
			continue
		}
		if ev.Source.Issue.PullRequest == nil {
			continue // an issue cross-reference, not a pull request
		}
		if num := ev.Source.Issue.Number; num != issueNum && d.prState(num) == "OPEN" {
			return num
		}
	}
	return 0
}
