package loopdriver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/orchestrator"
)

// landedWithoutArtifactStatus is the ledger row for a merge verdict whose
// artifacts did not follow: gh read the pull request MERGED, but the files it
// touched never reached main (loop 276's class — #342 merged only as an
// auto-cleanup snapshot while its new test file stayed absent). The status is
// deliberately outside the productive set: the iteration must not read as
// shipped, the queue claim and tracker issue stay live, and the starvation
// gate counts the row toward its halt.
const landedWithoutArtifactStatus = "landed-without-artifact"

// preflightRejectedDetail is the failure detail stamped on a queue claim
// retired by the landing-evidence preflight: a fingerprint match on a
// landed-without-artifact row. The goal is NOT refusable (the work is not on
// main), so unlike goalRejectedDetail the claim is retired `failed` rather
// than the goal text — the operator who enqueued it sees on the task row why
// the loop will not re-dispatch this exact goal against the same lying MERGED
// stamp, and the retirement follows the shape gate's precedent (run_test.go:
// the unretired claim class that re-claimed every lease and walked the loop
// into the starvation halt).
const preflightRejectedDetail = "goal fingerprint matches a landed-without-artifact row (landing-evidence preflight)"

// landedArtifactsVerified reports whether pull request pr's touched
// implementation files exist on main — the artifact half of a merge verdict
// (the 276/277/278 streak's root cause: merge/close decisions running on
// status stamps, not artifacts-on-main). The check runs over the GitHub files
// API: the PR's touched-file list and main's tree, both JSON-decoded like
// prState. Anything gh cannot say (down, auth failure, unparseable body, a
// truncated tree listing) passes with a log line — the gate is evidence
// against false merges, not a second availability gate.
func (d *driver) landedArtifactsVerified(logF io.Writer, pr int) bool {
	files, ok := d.prFiles(pr)
	if !ok {
		_, _ = fmt.Fprintf(logF, "[verify] cannot list PR #%d's files — artifact gate skipped\n", pr)
		return true
	}
	tree, ok := d.mainTreePaths()
	if !ok {
		_, _ = fmt.Fprintln(logF, "[verify] cannot read main's tree — artifact gate skipped")
		return true
	}
	var missing []string
	for _, f := range files {
		if !implementationPath(f) || tree[f] {
			continue
		}
		missing = append(missing, f)
	}
	if len(missing) == 0 {
		_, _ = fmt.Fprintf(logF, "[verify] PR #%d's implementation files verified on main\n", pr)
		return true
	}
	_, _ = fmt.Fprintf(logF, "[verify] PR #%d merged but absent from main: %s\n", pr, strings.Join(missing, " "))
	return false
}

// implementationPath reports whether a touched path carries an
// implementation claim. Docs surfaces (docs/, *.md) are excluded: a
// PRD-regeneration PR legitimately changes nothing else, and the class this
// gate targets — a merge that lost its code — always has code files.
func implementationPath(p string) bool {
	return !strings.HasPrefix(p, "docs/") && !strings.HasSuffix(p, ".md")
}

// prFiles lists the implementation paths pull request num touched
// (GET /repos/{owner}/{repo}/pulls/{n}/files). A file the PR DELETED is not
// an artifact claim — main is supposed to lack it — so `removed` entries are
// filtered out here, before the tree comparison can call them missing.
// ponytail: first page only (per_page=100) — a PR wider than a hundred files
// is judged on its first hundred; full coverage wants the pagination loop and
// no shipped devagent PR has come near the ceiling.
func (d *driver) prFiles(num int) ([]string, bool) {
	var rows []struct {
		Filename string `json:"filename"`
		Status   string `json:"status"`
	}
	if !d.ghAPIDecode(fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", d.cfg.GHRepo, num), &rows) {
		return nil, false
	}
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Status == "removed" {
			continue
		}
		names = append(names, r.Filename)
	}
	return names, true
}

// mainTreePaths returns the blob paths on main
// (GET /repos/{owner}/{repo}/git/trees/main?recursive=1) as a set. A
// TRUNCATED listing (GitHub caps huge trees) is not evidence of absence —
// every file past the cap would read as missing — so it reports cannot-say
// and the gate passes rather than inventing an artifact-less merge.
func (d *driver) mainTreePaths() (map[string]bool, bool) {
	var tree struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if !d.ghAPIDecode(fmt.Sprintf("repos/%s/git/trees/main?recursive=1", d.cfg.GHRepo), &tree) {
		return nil, false
	}
	if tree.Truncated {
		return nil, false
	}
	paths := make(map[string]bool, len(tree.Tree))
	for _, e := range tree.Tree {
		if e.Type == "blob" {
			paths[e.Path] = true
		}
	}
	return paths, true
}

// ghAPIDecode runs one gh api call the way openPROfIssue does (the repo rides
// in the URL path, drained teardown per #286) and JSON-decodes the body.
// False when gh cannot answer or the body is not the expected shape.
func (d *driver) ghAPIDecode(path string, v any) bool {
	if d.cfg.GHRepo == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.cfg.GhBin, "api", path)
	cmd.Dir = d.cfg.Repo
	cmd.WaitDelay = pipeDrainDelay
	out, err := cmd.Output()
	if err != nil && (cmd.ProcessState == nil || !cmd.ProcessState.Success()) {
		return false
	}
	return json.Unmarshal(out, v) == nil
}

// landedArtifactKeyLen is the fingerprint length the landing-evidence
// preflight greps for: the Q27 guard's rule-1 key (firstChars 60), which
// survives the 160-char ledger row cap and is stable across the
// deterministic per-issue goal templates.
const landedArtifactKeyLen = 60

// ledgerLandingFingerprint greps the ledger for the goal's fingerprint on a
// landed-without-artifact row and returns the matching raw line, "" when
// none. The pick-time preflight's match: a goal whose previous landing
// recorded artifact-less must not re-dispatch, because the merge behind that
// row already read MERGED — the re-run would re-read the same stamp and
// re-record the same nothing. Shipped rows are deliberately NOT matched
// here: the Q27 guard owns that verdict and its consequences.
func ledgerLandingFingerprint(goal string, rows []string) string {
	key := truncateRunes(orchestrator.NormalizeGoalText(goal), landedArtifactKeyLen)
	if key == "" {
		return ""
	}
	for _, raw := range rows {
		if !strings.Contains(raw, `"status":"`+landedWithoutArtifactStatus+`"`) {
			continue
		}
		if strings.Contains(orchestrator.NormalizeGoalText(raw), key) {
			return raw
		}
	}
	return ""
}
