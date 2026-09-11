package loopdriver

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/FreePeak/devagent/internal/git"
)

const (
	// StateBranch is the orphan branch mirroring .selfbuild/ to origin.
	StateBranch = "selfbuild/state"
	// RemoteStateRef is the local remote-tracking ref StateBranch fetches into.
	RemoteStateRef = "refs/remotes/origin/selfbuild-state"
	// networkTimeout bounds each git network op in the state sync (the bash
	// driver wrapped each selfbuild-state.sh call in `timeout 60`).
	networkTimeout = 60 * time.Second
)

// stateSync ports scripts/selfbuild-state.sh: pull merges
// origin/selfbuild/state into local .selfbuild/, push publishes the merged
// state back as a commit on the orphan branch. Merge policy: ledger rows
// keyed by "loop":<n> with the later "ts" winning; lessons is a ratchet-only
// union of unique lines.
type stateSync struct {
	repo   string
	stdout io.Writer
	now    func() time.Time
}

// gitEnv mirrors `${GIT_SSH_COMMAND:-ssh -o BatchMode=yes -o ConnectTimeout=10}`:
// keep an ambient GIT_SSH_COMMAND, otherwise pin the bounded one so a wedged
// ssh child can never hang the sync (2026-09-06 loop hang class).
func gitEnv() []string {
	if os.Getenv("GIT_SSH_COMMAND") != "" {
		return os.Environ()
	}
	return append(os.Environ(), "GIT_SSH_COMMAND="+git.BoundedGitSSHCommand)
}

func newStateSync(repo string, stdout io.Writer, now func() time.Time) *stateSync {
	if now == nil {
		now = time.Now
	}
	// state.sh startup: mkdir -p "$STATE"/{research,goals,logs,curation} —
	// .selfbuild must exist before the first ledger write (os.WriteFile
	// does not create parent dirs).
	for _, sub := range []string{"", "/research", "/goals", "/logs", "/curation"} {
		_ = os.MkdirAll(filepath.Join(repo, ".selfbuild"+sub), 0o755)
	}
	return &stateSync{repo: repo, stdout: stdout, now: now}
}

// git runs one state-sync git command, network-bounded by networkTimeout.
// Output() captures through a pipe, so the teardown is drain-bounded
// (issue #286): the ctx kill takes out the git child, but an ssh
// grandchild it spawned keeps the pipe write end open — without WaitDelay
// the copy goroutine pins Wait forever even after the deadline fired.
func (s *stateSync) git(captureStderr io.Writer, stdin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), networkTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.repo
	cmd.Env = gitEnv()
	cmd.Stderr = captureStderr
	cmd.WaitDelay = pipeDrainDelay
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.Output()
	// A drain cutoff (ErrWaitDelay) must not flip a git child that itself
	// exited 0 into an error — push/pull verdicts feed the ledger.
	if err != nil && cmd.ProcessState != nil && cmd.ProcessState.Success() {
		err = nil
	}
	return string(out), err
}

func (s *stateSync) ledgerPath() string  { return filepath.Join(s.repo, ".selfbuild", "ledger.jsonl") }
func (s *stateSync) lessonsPath() string { return filepath.Join(s.repo, ".selfbuild", "lessons.md") }

func (s *stateSync) fetchState() error {
	_, err := s.git(nil, "", "fetch", "--quiet", "origin", StateBranch+":"+RemoteStateRef)
	return err
}

// fileAtState mirrors file_at_state: remote blob content or "".
func (s *stateSync) fileAtState(path string) string {
	if _, err := s.git(nil, "", "rev-parse", "--verify", "--quiet", RemoteStateRef); err != nil {
		return ""
	}
	out, err := s.git(nil, "", "show", RemoteStateRef+":"+path)
	if err != nil {
		return ""
	}
	return out
}

func (s *stateSync) mergeLedger() {
	remote := s.fileAtState(".selfbuild/ledger.jsonl")
	local, _ := os.ReadFile(s.ledgerPath())
	merged := mergeLedgerLines(remote, string(local))
	if merged == "" {
		// Bash: `[ -s "$tmp" ] && mv || rm` — an empty merged set leaves the
		// local ledger untouched.
		return
	}
	_ = os.WriteFile(s.ledgerPath(), []byte(merged), 0o644)
}

var (
	loopFieldRe = regexp.MustCompile(`"loop":[0-9]+`)
	tsFieldRe   = regexp.MustCompile(`"ts":"[^"]*"`)
)

// mergeLedgerLines ports merge_ledger: drop blank lines, key each remaining
// line by its "loop":<n> field (lines without one are dropped), keep the
// entry with the greatest "ts" per loop (ties broken by the
// lexicographically greater line, matching GNU sort's last-resort whole-line
// comparison), output sorted by loop number.
func mergeLedgerLines(remote, local string) string {
	type entry struct{ ts, line string }
	last := map[string]entry{}
	nums := map[int]string{}
	for _, line := range strings.Split(remote+local, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		loopStr := extractField(loopFieldRe, line, `"loop":`)
		if loopStr == "" {
			continue
		}
		loop, err := strconv.Atoi(loopStr)
		if err != nil {
			continue
		}
		ts := extractField(tsFieldRe, line, `"ts":"`)
		if cur, ok := last[loopStr]; !ok {
			last[loopStr] = entry{ts: ts, line: line}
			nums[loop] = loopStr
		} else if ts > cur.ts || (ts == cur.ts && line > cur.line) {
			last[loopStr] = entry{ts: ts, line: line}
		}
	}
	keys := make([]int, 0, len(nums))
	for k := range nums {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(last[nums[k]].line)
		b.WriteString("\n")
	}
	return b.String()
}

// extractField returns the match contents after the marker prefix is
// stripped, mirroring the awk substr extraction (RSTART+len(marker)).
func extractField(re *regexp.Regexp, line, marker string) string {
	loc := re.FindStringIndex(line)
	if loc == nil {
		return ""
	}
	return strings.TrimPrefix(line[loc[0]:loc[1]], marker)
}

func (s *stateSync) mergeLessons() {
	remote := s.fileAtState(".selfbuild/lessons.md")
	local, _ := os.ReadFile(s.lessonsPath())
	_ = os.MkdirAll(filepath.Dir(s.lessonsPath()), 0o755)
	// Bash touches the lessons file first, so an empty merge still leaves an
	// (empty) file behind — mirror that.
	merged := dedupeLines(remote + string(local))
	_ = os.WriteFile(s.lessonsPath(), []byte(merged), 0o644)
}

// dedupeLines ports `awk '!seen[$0]++'`: keep the first occurrence of every
// line. Only the final empty element produced by a trailing newline is
// dropped; interior blank lines are ordinary lines to awk and stay.
func dedupeLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range lines {
		if seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// Pull merges origin/selfbuild/state into local .selfbuild/. A missing
// remote branch is the normal first-run path, not an error.
func (s *stateSync) Pull() error {
	if err := s.fetchState(); err != nil {
		_, _ = fmt.Fprintf(s.stdout, "[state] no remote state yet (%s) — starting fresh\n", StateBranch)
		return nil
	}
	s.mergeLedger()
	s.mergeLessons()
	_, _ = fmt.Fprintf(s.stdout, "[state] pulled %d ledger entries into %s\n", countNewlines(s.ledgerPath()), s.ledgerPath())
	return nil
}

// Push publishes the merged .selfbuild/ to origin/selfbuild/state. On a
// push race it re-fetches, re-merges and retries exactly once, mirroring the
// bash driver.
func (s *stateSync) Push() error {
	if _, err := os.Stat(s.ledgerPath()); err != nil {
		_, _ = fmt.Fprintf(s.stdout, "[state] nothing to push: %s missing\n", s.ledgerPath())
		return nil
	}
	if err := s.fetchState(); err != nil {
		_, _ = fmt.Fprintf(s.stdout, "[state] no remote state yet — creating %s\n", StateBranch)
	}
	s.mergeLedger()
	s.mergeLessons()
	commit, err := s.commitMergedState()
	if err == nil {
		err = s.pushRef(commit, nil)
	}
	if err == nil {
		_, _ = fmt.Fprintf(s.stdout, "[state] pushed %d ledger entries to %s\n", countNewlines(s.ledgerPath()), StateBranch)
		return nil
	}
	// Another run raced us: re-pull, re-merge, retry once.
	var stderr bytes.Buffer
	_ = s.fetchState()
	s.mergeLedger()
	s.mergeLessons()
	commit2, cerr := s.commitMergedState()
	if cerr != nil {
		return cerr
	}
	if err := s.pushRef(commit2, &stderr); err != nil {
		_, _ = fmt.Fprintf(s.stdout, "[state] push failed after retry:\n%s", stderr.String())
		return err
	}
	_, _ = fmt.Fprintf(s.stdout, "[state] pushed %d ledger entries to %s\n", countNewlines(s.ledgerPath()), StateBranch)
	return nil
}

func (s *stateSync) commitMergedState() (string, error) {
	tree, err := s.buildTree()
	if err != nil {
		return "", err
	}
	parent := ""
	if out, err := s.git(nil, "", "rev-parse", "--verify", "--quiet", RemoteStateRef); err == nil {
		parent = strings.TrimSpace(out)
	}
	msg := fmt.Sprintf("self-build state sync %s\n", s.now().UTC().Format(rowTimeFormat))
	args := []string{"commit-tree", tree}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	out, err := s.git(nil, msg, args...)
	if err != nil {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// buildTree ports build_tree: blob both .selfbuild files (lessons only when
// it exists locally), mktree them into a subtree, then wrap that subtree in
// a root tree under .selfbuild — the committed layout the merge side reads
// back via `git show <ref>:.selfbuild/<file>`.
func (s *stateSync) buildTree() (string, error) {
	files := []string{"ledger.jsonl"}
	if _, err := os.Stat(s.lessonsPath()); err == nil {
		files = append(files, "lessons.md")
	}
	var sub strings.Builder
	for _, f := range files {
		path := filepath.Join(s.repo, ".selfbuild", f)
		sha, err := s.git(nil, "", "hash-object", "-w", path)
		if err != nil {
			return "", fmt.Errorf("hash-object %s: %w", f, err)
		}
		_, _ = fmt.Fprintf(&sub, "100644 blob %s\t%s\n", strings.TrimSpace(sha), f)
	}
	subtree, err := s.git(nil, sub.String(), "mktree")
	if err != nil {
		return "", fmt.Errorf("mktree: %w", err)
	}
	root := fmt.Sprintf("040000 tree %s\t.selfbuild\n", strings.TrimSpace(subtree))
	tree, err := s.git(nil, root, "mktree")
	if err != nil {
		return "", fmt.Errorf("mktree: %w", err)
	}
	return strings.TrimSpace(tree), nil
}

func (s *stateSync) pushRef(commit string, stderrBuf io.Writer) error {
	_, err := s.git(stderrBuf, "", "push", "--quiet", "origin", commit+":refs/heads/"+StateBranch)
	return err
}

// countNewlines mirrors `wc -l` (newline count, not line count).
func countNewlines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(data, []byte("\n"))
}
